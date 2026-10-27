package controlplane

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"tenon.dev/internal/protocol"
)

// pendingStateAwaitingApproval 是公告板条目的缺省状态。
//
// 「待批」是协议层的缺省语义：不推 state 的老客户端送进来的条目必须是待批，
// 空串既不像待批也不像终态，页面会把这张卡弄丢。
const pendingStateAwaitingApproval = "awaiting_approval"

// pendingItem 是公告板上一行：只放展示所需信息，不放完整 plan。
type pendingItem struct {
	PlanID        string           `json:"planId"`
	RequestID     string           `json:"requestId,omitempty"`
	By            string           `json:"by,omitempty"`
	Summary       string           `json:"summary"`
	Items         []map[string]any `json:"items,omitempty"`
	RequiredRoles []string         `json:"requiredRoles"`
	ExpiresAt     string           `json:"expiresAt"`
	// State 区分「待批」与「刚刚处理完」：页面按它决定怎么显示，协议层不替页面过滤。
	// 缺省 awaiting_approval —— 老客户端不推这个字段时不能被当成终态，也不能留空串。
	State string `json:"state"`
	Mine  bool   `json:"mine"`
}

// pendingBoard 是待批公告板：**只读 read-model**，权威状态在 agent 盘上的 pending 文件。
// host 不判断、不执行、不授权；GET 的 mine 只用于页面显示。
//
// 刻意不落盘：公告板可丢、可重建（agent 每 tick 重推即自愈），持久化反而会造出
// 第二份「权威状态」，与 agent 盘上的 pending 文件互相漂移。
type pendingBoard struct {
	mu    sync.RWMutex
	items map[string]pendingItem
}

func newPendingBoard() *pendingBoard {
	return &pendingBoard{items: map[string]pendingItem{}}
}

// Upsert 幂等写入（同 planId 覆盖）。agent 每 tick 重推即靠它自愈。
//
// 缺省 state 在这里补，而不是在 handler 里：Upsert 是写公告板的**唯一**入口，
// 补在入口上才不会有第二条路绕过去（补在 handler 里，未来任何内部调用都会漏）。
func (b *pendingBoard) Upsert(it pendingItem) {
	if it.State == "" {
		it.State = pendingStateAwaitingApproval
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[it.PlanID] = it
}

// List 返回按角色过滤后的列表：过期条目顺手剔除。
//
// mine 只表示「当前 token 的角色与 requiredRoles 有交集」，供页面高亮；
// 它不是授权判定——真正的审批权限校验在 agent 恢复时做（权威侧）。
// expiresAt 解析不出来的条目会留下（展示层的宽松读，不构成放行）。
//
// state **不参与**这里的取舍：无论 awaiting_approval 还是 done/rejected/expired/stale，
// 只要没过期就返回。按状态过滤是页面的事——协议层一旦开始按状态筛，终端就再也看不到
// 「刚刚处理完」的卡片，终态覆盖就白推了。
func (b *pendingBoard) List(roles []string, now time.Time) []pendingItem {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]pendingItem, 0, len(b.items))
	for id, it := range b.items {
		if exp, err := time.Parse(time.RFC3339, it.ExpiresAt); err == nil && now.After(exp) {
			delete(b.items, id)
			continue
		}
		it.Mine = rolesIntersect(roles, it.RequiredRoles)
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out
}

// rolesIntersect 回答「持有 have 的角色是否与 need 有交集」。
// need 为空即无人匹配：公告板条目没有指定审批角色，不该被任何人认领成「我的」。
func rolesIntersect(have, need []string) bool {
	if len(need) == 0 {
		return false
	}
	for _, n := range need {
		for _, h := range have {
			if h == n {
				return true
			}
		}
	}
	return false
}

// servePending 处理 POST（agent 推摘要）与 GET（前端按角色取列表）。
func (s *Server) servePending(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		// bearerClaims 失败时已经把 401 写好了：这里只 return，再写一次会让响应失效。
		claims, ok := s.bearerClaims(w, r)
		if !ok {
			return
		}
		if perr := preflight(r); perr != nil {
			writeTenonError(w, *perr)
			return
		}
		// 写公告板只允许 agent：普通用户若能写，就能给审批人塞一张伪造的待批卡片。
		if !claims.hasRole(roleAgent) {
			writeTenonErrorStatus(w, http.StatusForbidden,
				*newError(protocol.ClassPolicyDenied, "FORBIDDEN_ROLE", "写公告板只允许 agent 服务账号"))
			return
		}
		body, perr := readBody(r)
		if perr != nil {
			writeTenonError(w, *perr)
			return
		}
		var it pendingItem
		if err := json.Unmarshal(body, &it); err != nil {
			writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
				"公告板条目不是合法 JSON："+err.Error()))
			return
		}
		if it.PlanID == "" {
			writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH", "planId 必填"))
			return
		}
		// by 一律用 token 的 sub 覆写：条目里的自报身份只是展示文案，不构成来源凭证。
		it.By = claims.Subject
		s.pending.Upsert(it)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodGet:
		claims, ok := s.bearerClaims(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": s.pending.List(claims.Roles, time.Now().UTC()),
		})
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}
