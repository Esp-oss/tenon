package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	// bearerClaims 失败时已经把 401 写好了：这里只 return，再写一次会让响应失效。
	claims, ok := s.bearerClaims(w, r)
	if !ok {
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
	writeJSON(w, http.StatusOK, enqueueResponse{OK: true, Seq: seq})
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
func (s *Server) serveInboundPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
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
func (s *Server) serveInboundAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
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
