package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
//
// 登记表是「事件 id → 这次请求是谁说的」的凭证，只在计划生命周期内被查。它按
// **保留期 + 上限**修剪：不修剪的话，每说一句话就多一条，长期运行会线性增长
// （内存与 requests-index.jsonl 都长）。修剪有一条安全边界：仍被公告板引用的
// requestId 一律保留，且保留期必须远大于任何计划的可能寿命——否则一条还在挂起的
// request 会因「查不到来源」被判成 event，绕过「对话发起必须审批」，那是把 GC
// 做成了安全缺口。
type originStore struct {
	mu      sync.RWMutex
	path    string
	byEvent map[string]originRecord

	retention  time.Duration
	maxEntries int
	now        func() time.Time
	// keep 返回 true 表示这条凭证仍被引用（公告板上的待批卡），不能因为超期而丢。
	keep func(originRecord) bool
	// sinceCompact 是距上次修剪的新增条数：不必每登记一条就重写整份文件。
	sinceCompact int
}

const (
	// defaultOriginRetention 是 origin 凭证的缺省保留期。
	//
	// 7 天远大于卡的缺省寿命（15 分钟）与任何正常审批等待；把它调低到卡寿命附近
	// 会让「挂了很久才批」的计划失去来源凭证。到期且未被公告板引用的才丢。
	defaultOriginRetention = 7 * 24 * time.Hour
	// defaultOriginMaxEntries 是硬上限（防止保留期内被灌爆）。超出时从最旧的
	// 未引用记录开始丢——宁可丢来源凭证也不能让宿主内存无界增长。
	defaultOriginMaxEntries = 50000
	// originCompactEvery 是「每新增多少条触发一次修剪」的粒度。
	originCompactEvery = 256
)

func newOriginStore(path string) (*originStore, error) {
	st := &originStore{
		path: path, byEvent: map[string]originRecord{},
		retention: defaultOriginRetention, maxEntries: defaultOriginMaxEntries, now: time.Now,
	}
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

// compactNow 在装配完成后调用一次：启动时就把过期的旧账修掉（keep 必须先设置好）。
func (st *originStore) compactNow() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.compactLocked(); err != nil {
		// 修剪失败不影响服务：登记表本身仍可读，下一次 Register 会再试。
		log.Printf("controlplane: origin 登记表启动修剪失败（不影响服务）：%v", err)
	}
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
	s.sinceCompact++
	if s.sinceCompact >= originCompactEvery || len(s.byEvent) > s.maxEntries {
		if err := s.compactLocked(); err != nil {
			// 修剪失败不退回「登记失败」：凭证已经 append 落盘，它在本次运行内仍然有效；
			// 下一次登记会继续尝试重写。
			log.Printf("controlplane: origin 登记表修剪失败（条目 %d，稍后重试）：%v", len(s.byEvent), err)
		}
	}
	return nil
}

// compactLocked 按保留期与上限修剪，并原子重写登记表（调用方持写锁）。
func (st *originStore) compactLocked() error {
	now := st.now()
	type entry struct {
		rec originRecord
		at  time.Time
	}
	kept := make([]entry, 0, len(st.byEvent))
	dropped := 0
	for _, rec := range st.byEvent {
		protected := st.keep != nil && st.keep(rec)
		at, err := time.Parse(time.RFC3339, rec.At)
		if !protected && err == nil && now.Sub(at) > st.retention {
			dropped++
			continue
		}
		kept = append(kept, entry{rec: rec, at: at})
	}
	// 最旧的排在前面：上限裁剪从头部丢，重写出的文件也按时间有序（便于人看）。
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].at.Equal(kept[j].at) {
			return kept[i].rec.EventID < kept[j].rec.EventID
		}
		return kept[i].at.Before(kept[j].at)
	})
	for len(kept) > st.maxEntries {
		idx := -1
		for i := range kept {
			if st.keep == nil || !st.keep(kept[i].rec) {
				idx = i
				break
			}
		}
		if idx < 0 {
			break // 全被引用：宁可超上限也不丢还在用的凭证
		}
		kept = append(kept[:idx], kept[idx+1:]...)
		dropped++
	}
	if dropped == 0 {
		st.sinceCompact = 0
		return nil
	}
	records := make([]originRecord, 0, len(kept))
	for _, e := range kept {
		records = append(records, e.rec)
	}
	if err := st.rewriteLocked(records); err != nil {
		return err
	}
	next := make(map[string]originRecord, len(records))
	for _, rec := range records {
		next[rec.EventID] = rec
	}
	st.byEvent = next
	st.sinceCompact = 0
	log.Printf("controlplane: origin 登记表修剪 %d 条（保留 %d 条；保留期 %s，上限 %d）",
		dropped, len(next), st.retention, st.maxEntries)
	return nil
}

// rewriteLocked 原子重写登记表：先写临时文件再改名，半份文件等于把凭证弄丢。
func (st *originStore) rewriteLocked(records []originRecord) error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	w := bufio.NewWriter(f)
	for _, rec := range records {
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	ok = true
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
	frame, perr := normalizeInboundFrame(body, claims.Subject, claims.Roles, s.origin)
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
	requestID, _ := frame["requestId"].(string)
	syntheticEventID, _ := frame["syntheticEventId"].(string)
	writeJSON(w, http.StatusOK, inboundEnqueueResponse{
		OK: true, Seq: seq, Durable: durable,
		RequestID: requestID, SyntheticEventID: syntheticEventID,
	})
}

// inboundEnqueueResponse 是 POST /tenon/requests 的响应。
//
// 比出站的 enqueueResponse 多两个字段：
//
//   - durable：出站是内存队列、丢了只是界面提示；入站承载的是人说过的话，
//     客户端需要能区分「已落盘」与「只在内存里」。
//   - requestId：宿主签发的那一个。前端要把它和自己刚说出的那句话配上，
//     否则「我说了什么 → agent 后来提了什么」在界面上无法对应（发布公告板上的
//     pending 条目本来就带 requestId，回给客户端只是把同一条线接上）。
type inboundEnqueueResponse struct {
	OK        bool   `json:"ok"`
	Seq       int64  `json:"seq"`
	Durable   bool   `json:"durable"`
	RequestID string `json:"requestId,omitempty"`
	// SyntheticEventID 是宿主为这句人话合成的触发事件 id。
	//
	// 它是**来源凭证**：`policies:evaluate` 带它才会被宿主判成 `origin=request`，从而命中
	// 「对话发起」类规则（`plat-request-needs-human`）。签发时不给调用方，调用方就只能
	// 空着传，于是这类规则永远不命中——症状是「人在对话里要的事被判无策略匹配」。
	SyntheticEventID string `json:"syntheticEventId,omitempty"`
}

// inboundFrame 不再单独定义类型：规范化后的帧就是 map[string]any（保留原字段 + 宿主签发字段）。

// normalizeInboundFrame 把客户端提交的帧规范化成**只有宿主能认证的那些事实**才算数的形状。
//
// subject 与 roles 都是**已验签 token** 里的 claims：两者都必须覆写客户端自报的字段，
// 因为「谁说的」与「他有什么权限」恰恰是客户端最有动机伪造的两件事。
func normalizeInboundFrame(body []byte, subject string, roles []string, origin *originStore) (map[string]any, *protocol.Error) {
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
		// 审批人角色只能来自宿主验证过的 token：客户端自报的 roles 先删掉（**无论有没有**），
		// 再写入 claims.Roles。**先删后写**而不是「有就保留」——保留等于让客户端夹带一个
		// 高权限角色进来，把「谁批的」降级成「谁点了」。
		//
		// 没有角色时写入空集合而不是删掉字段：消费方要能区分「通道没接上（缺键）」与
		// 「这个人没有角色（空集）」，而后者在 agent 侧由 rolesIntersect 兑现成
		// fail-closed 的 403——绝不补一个默认角色（补了就是人人都能批）。
		delete(raw, "roles")
		raw["roles"] = emptyIfNilRoles(roles)
		return raw, nil
	default:
		return nil, newError(protocol.ClassContractViolation, "UNKNOWN_FRAME_TYPE",
			"入站只接受 tenon.request 或 tenon.confirm，收到 "+typ)
	}
}

// emptyIfNilRoles 把 nil 角色集规范成空数组：帧里出现 "roles":null 时，消费方分不清
// 「查不到」与「一个都没有」，而这两者在这里必须是同一个结论。
func emptyIfNilRoles(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
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
