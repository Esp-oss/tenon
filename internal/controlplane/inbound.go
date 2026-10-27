package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"tenon.dev/internal/protocol"
)

// 入站帧类型：本切片只收这两种（tenon.context 不入站，见 spec §3.2）。
const (
	inboundTypeRequest = "tenon.request"
	inboundTypeConfirm = "tenon.confirm"
)

// originRecord 是宿主签发合成事件 id 时留下的凭证：只用来判定 origin，不参与执行。
type originRecord struct {
	EventID   string `json:"eventId"`
	RequestID string `json:"requestId"`
	By        string `json:"by"`
	At        string `json:"at"`
}

// originStore 是 origin 登记表。**必须独立落盘**（I8）：若跟着内存队列丢掉，
// 重启后未完成的 request 会被判成 event，从而绕过「对话发起必须审批」。
type originStore struct {
	mu      sync.RWMutex
	path    string
	byEvent map[string]originRecord
}

func newOriginStore(path string) (*originStore, error) {
	st := &originStore{path: path, byEvent: map[string]originRecord{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: 打开 origin 登记表失败：%w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var rec originRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		st.byEvent[rec.EventID] = rec
	}
	return st, sc.Err()
}

// Register 追加一条登记并落盘。落盘失败即返回错误：调用方不该在凭证没留下时就入队。
func (s *originStore) Register(rec originRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.byEvent[rec.EventID] = rec
	return nil
}

// IsRequest 回答「这枚事件 id 是不是人发起的」。未登记即视为事件驱动。
func (s *originStore) IsRequest(eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.byEvent[eventID]
	return ok
}

func (s *originStore) Get(eventID string) (originRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byEvent[eventID]
	return rec, ok
}

// originRegistry 只供测试与内部读用。
func (s *Server) originRegistry() *originStore { return s.origin }

// serveInboundEnqueue 处理 POST /tenon/requests。
//
// 三件必须做的事，缺一即失去治理意义：
//  1. 验 token（缺失/坏/过期一律 401，且不入队）；
//  2. 用 token 的 sub 覆写帧里的 by（客户端自称一律不认）；
//  3. 为 request 签发 requestId 与合成 eventId 并登记 origin。
func (s *Server) serveInboundEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	// 鉴权先于 preflight：未认证的调用不该拿到「你的 Content-Type 不对」这种端点细节。
	// bearerClaims 失败时已经把 401 写好了：这里只 return，再写一次会让响应失效。
	claims, ok := s.bearerClaims(w, r)
	if !ok {
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	frame, perr := normalizeInboundFrame(body, claims.Subject, s.origin)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		writeTenonErrorStatus(w, http.StatusInternalServerError,
			*newError(protocol.ClassContractViolation, "ENCODE_FAILED", err.Error()))
		return
	}
	seq := s.inbound.Push(raw)
	// 落盘失败时不能假装持久化：帧仍在内存队列里（poll 拿得到），但重启会丢。
	// 这里回 200 而非 500 是有意的——500 会诱导客户端重试，而重试会造出第二条用户请求
	// （requestId 由宿主签发，重试等于重发），比「热情地告知未落盘」更坏。
	durable := s.inbound.Broken() == nil
	if !durable {
		log.Printf("controlplane: 入站帧已入内存队列但未落盘（seq=%d）——重启会丢，请检查磁盘", seq)
	}
	writeJSON(w, http.StatusOK, inboundEnqueueResponse{OK: true, Seq: seq, Durable: durable})
}

// inboundEnqueueResponse 是 POST /tenon/requests 的响应。
//
// 比出站的 enqueueResponse 多一个 durable：出站是内存队列、丢了只是界面提示；
// 入站承载的是人说过的话，客户端需要能区分「已落盘」与「只在内存里」。
type inboundEnqueueResponse struct {
	OK      bool  `json:"ok"`
	Seq     int64 `json:"seq"`
	Durable bool  `json:"durable"`
}

// inboundFrame 不再单独定义类型：规范化后的帧就是 map[string]any（保留原字段 + 宿主签发字段）。

func normalizeInboundFrame(body []byte, subject string, origin *originStore) (map[string]any, *protocol.Error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH", "帧不是 JSON 对象："+err.Error())
	}
	typ, _ := raw["type"].(string)
	switch typ {
	case inboundTypeRequest:
		requestID := newRequestID()
		eventID := newSyntheticEventID()
		raw["requestId"] = requestID
		raw["syntheticEventId"] = eventID
		raw["by"] = subject
		raw["at"] = time.Now().UTC().Format(time.RFC3339)
		if err := origin.Register(originRecord{
			EventID: eventID, RequestID: requestID, By: subject,
			At: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return nil, newError(protocol.ClassContractViolation, "ORIGIN_REGISTER_FAILED",
				"登记合成事件 id 失败，拒绝入队："+err.Error())
		}
		return raw, nil
	case inboundTypeConfirm:
		raw["by"] = subject
		return raw, nil
	default:
		return nil, newError(protocol.ClassContractViolation, "UNKNOWN_FRAME_TYPE",
			"入站只接受 tenon.request 或 tenon.confirm，收到 "+typ)
	}
}

// serveInboundPoll 处理 GET /tenon/requests/poll?after=。
//
// 要求 token：入站队列里装的是用户说过的话，未鉴权的读等于把别人的话摊开给人看。
// 消费方是 agent，它用服务账号登录（见 config/users.dev.json 与 spec §3.7）。
func (s *Server) serveInboundPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	// 消费侧只允许 agent：普通用户 token 能读队列就读得到别人说过的话。
	claims, ok := s.bearerClaims(w, r)
	if !ok {
		return
	}
	if !claims.hasRole(roleAgent) {
		writeTenonErrorStatus(w, http.StatusForbidden,
			*newError(protocol.ClassPolicyDenied, "FORBIDDEN_ROLE", "入站队列只允许 agent 服务账号消费"))
		return
	}
	after, perr := parseAfter(r.URL.Query().Get("after"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	frames, cursor, dropped := s.inbound.Since(after)
	writeJSON(w, http.StatusOK, pollResponse{Frames: frames, NextCursor: cursor, Dropped: dropped})
}

// serveInboundAck 处理 POST /tenon/requests/ack。
//
// 要求 token：未鉴权的 ack 能把还没被处理的用户请求直接吞掉（游标一推进，
// poll 就再也回放不出来），这是拒绝服务而不只是越权。
func (s *Server) serveInboundAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	// 只允许 agent：ack 一推进，未被处理的用户请求就再也回放不出来（拒绝服务）。
	claims, ok := s.bearerClaims(w, r)
	if !ok {
		return
	}
	if !claims.hasRole(roleAgent) {
		writeTenonErrorStatus(w, http.StatusForbidden,
			*newError(protocol.ClassPolicyDenied, "FORBIDDEN_ROLE", "入站队列只允许 agent 服务账号确认"))
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req ackRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {\"upto\": <序号>}："+err.Error()))
		return
	}
	upto := s.inbound.Highest()
	if req.Upto != nil {
		// 与出站的 serveIntentAck 保持同一校验：负游标既无意义，也不该回一个负的 acked。
		if *req.Upto < 0 {
			writeTenonError(w, *newError(protocol.ClassContractViolation, "INVALID_CURSOR",
				"upto 不允许为负"))
			return
		}
		upto = *req.Upto
	}
	writeJSON(w, http.StatusOK, ackResponse{OK: true, Acked: s.inbound.Ack(upto)})
}

// newRequestID / newSyntheticEventID 用纳秒+计数器生成，字符集满足 req_/evt_ 的词法约束。
func newRequestID() string        { return "req_" + newID("") }
func newSyntheticEventID() string { return "evt_" + newID("") }

// idSeq 保证同一纳秒内的多次签发不撞号。
var (
	idMu  sync.Mutex
	idSeq uint64
)

// newID 生成 base36 纳秒+序号；不含分隔符，满足 req_/evt_ 后续字符集。
func newID(prefix string) string {
	idMu.Lock()
	idSeq++
	n := idSeq
	idMu.Unlock()
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatUint(n, 36)
}
