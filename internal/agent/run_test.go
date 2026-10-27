package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenon.dev/internal/spec"
)

// 本文件的全部用例都用假 LLM + httptest 桩服务：不发真实模型请求、不花 token、
// 不依赖任何外部进程（设计 §6 测试策略）。

// ---------- 桩宿主 ----------

// fakeHost 扮演控制平面：注册表、策略、授权、审计、共驾帧。
//
// 它同时是**闸门轨迹的记录者**：每次收到请求就把闸门名写进共享的 trace，于是
// TestGateOrder 断言的是真实请求顺序，而不是运行时对自我调用的声明。
type fakeHost struct {
	*httptest.Server
	t     *testing.T
	trace *[]string
	mu    sync.Mutex

	// InventoryURL 是注册表里 inventory 的地址（由 newFakeEnv 注入）。
	InventoryURL string
	// ExtraModules 追加到注册表响应里（例如 enabled=false 的条目）。
	ExtraModules []moduleEntry

	PolicyOutcome string
	PolicyReasons []string
	// PolicyApprovers 是 policies:evaluate 响应里的 approvers。
	//
	// nil 表示用缺省的一个角色：approve 的用例要验的是挂起机制本身，不该被审批人校验
	// 拦住。显式赋一个非 nil 的切片（空切片也在内）才是在测「没给审批人角色」那条
	// 独立守卫（见 TestNoApproversDoesNotSuspend）。
	PolicyApprovers []string
	GrantAllowed    bool
	GrantReason     string
	FailAuditPost   bool

	AuditRecords []map[string]any
	IntentFrames []map[string]any
}

func newFakeHost(t *testing.T, trace *[]string) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, trace: trace, PolicyOutcome: outcomeAuto, GrantAllowed: true}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *fakeHost) appendTrace(name string) {
	*h.trace = append(*h.trace, name)
}

func (h *fakeHost) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/tenon/modules":
		mods := []moduleEntry{{ID: "inventory", BaseURL: h.InventoryURL, Enabled: true}}
		mods = append(mods, h.ExtraModules...)
		writeTestJSON(w, http.StatusOK, map[string]any{"modules": mods})

	case "/tenon/policies:evaluate":
		h.appendTrace("policies:evaluate")
		reasons := h.PolicyReasons
		if len(reasons) == 0 {
			reasons = []string{"TEST_RULE"}
		}
		approvers := h.PolicyApprovers
		if approvers == nil {
			// 缺省给一个角色：approve 的用例验的是挂起机制，不是审批人校验。
			// 「策略要求审批却没给审批人角色」由 TestNoApproversDoesNotSuspend 显式覆盖。
			approvers = []string{"role:approver"}
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"outcome":   h.PolicyOutcome,
			"decidedBy": []string{"pol-test"},
			"approvers": approvers,
			"reasons":   reasons,
		})

	case "/tenon/grants:evaluate":
		h.appendTrace("grants:evaluate")
		if !h.GrantAllowed {
			writeTestJSON(w, http.StatusOK, map[string]any{"allowed": false, "reason": h.GrantReason})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"allowed":        true,
			"matchedGrantId": "gr-test001",
			"quota":          map[string]any{"remaining": 100, "unit": "calls"},
		})

	case "/tenon/audit":
		var rec map[string]any
		if err := json.Unmarshal(readTestBody(h.t, r), &rec); err != nil {
			h.t.Errorf("审计请求体不是 JSON：%v", err)
		}
		stage, _ := rec["stage"].(string)
		h.appendTrace("audit:" + stage)
		if h.FailAuditPost {
			writeTestError(w, http.StatusServiceUnavailable, "retryable", "AUDIT_WRITE_FAILED", "注入的审计写失败")
			return
		}
		h.mu.Lock()
		h.AuditRecords = append(h.AuditRecords, rec)
		h.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "/tenon/intents":
		var frame map[string]any
		if err := json.Unmarshal(readTestBody(h.t, r), &frame); err != nil {
			h.t.Errorf("Intent 帧不是 JSON：%v", err)
		}
		h.mu.Lock()
		h.IntentFrames = append(h.IntentFrames, frame)
		h.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": 1})

	default:
		http.NotFound(w, r)
	}
}

// ---------- 桩模块 ----------

// fakeModule 扮演一个模块侧车：manifest、动作执行、可选的 dry-run。
type fakeModule struct {
	*httptest.Server
	mu    sync.Mutex
	trace *[]string

	ModuleID string
	Actions  []string
	// Templates 是 action id → 人话模板；空表示该动作没声明（渲染侧走 R2）。
	Templates map[string]string
	// Meta 是 action id → manifest 里的额外键（当前用于 requiredArgs）。
	Meta map[string]map[string]any

	DryRunStatus  string // 默认 verified
	DryRunMissing bool   // 声明「不支持预演」：回 404 空体

	RetryableTimes int    // 前 n 次执行返回 retryable
	AlwaysReject   string // 非空则每次执行都返回该错误码的 business_rejection

	ManifestCalls   int
	ExecuteCalls    int
	WriteCalls      int // 带幂等键的执行次数（= 写调用次数）
	IdempotencyKeys []string
	executed        int
}

func newFakeModule(t *testing.T, id string, actions []string, trace *[]string) *fakeModule {
	t.Helper()
	m := &fakeModule{ModuleID: id, Actions: actions, trace: trace, DryRunStatus: dryRunVerified}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Server.Close)
	return m
}

func (m *fakeModule) appendTrace(name string) {
	*m.trace = append(*m.trace, name)
}

func (m *fakeModule) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/tenon" {
		m.mu.Lock()
		m.ManifestCalls++
		m.mu.Unlock()
		actions := make([]map[string]any, 0, len(m.Actions))
		for _, id := range m.Actions {
			item := map[string]any{"id": id, "version": "0.1.0", "title": id, "kind": "write"}
			if tpl, ok := m.Templates[id]; ok {
				item["summaryTemplate"] = tpl
			}
			for k, v := range m.Meta[id] {
				item[k] = v
			}
			actions = append(actions, item)
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"tenon":   "0.1",
			"module":  map[string]any{"id": m.ModuleID, "version": "0.1.0", "title": "测试模块"},
			"actions": actions,
			"events":  []any{},
		})
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/tenon/actions/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, isDryRun := strings.CutSuffix(rest, ":dry-run"); isDryRun {
		m.appendTrace("dry-run")
		if m.DryRunMissing {
			// 「没有预演能力」必须是可判定的信号：404 空体（interfaces.md §3.3）。
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// 真实模块把报告包在 {"dryRun": ...} 里（internal/module 的 wrapDryRun，
		// interfaces.md §3.3）：桩必须同形，否则测不到真正的解析路径。
		writeTestJSON(w, http.StatusOK, map[string]any{"dryRun": map[string]any{"status": m.DryRunStatus}})
		return
	}

	m.appendTrace("execute")
	key := r.Header.Get("Idempotency-Key")
	m.mu.Lock()
	m.ExecuteCalls++
	if key != "" {
		m.WriteCalls++
		m.IdempotencyKeys = append(m.IdempotencyKeys, key)
	}
	m.executed++
	executed := m.executed
	m.mu.Unlock()

	if m.AlwaysReject != "" {
		writeTestError(w, http.StatusUnprocessableEntity, "business_rejection", m.AlwaysReject, "可用 3，需求 12")
		return
	}
	if executed <= m.RetryableTimes {
		writeTestError(w, http.StatusServiceUnavailable, "retryable", "UPSTREAM_BUSY", "瞬时故障")
		return
	}
	writeTestJSON(w, http.StatusOK, map[string]any{"onHand": 0})
}

// ---------- 环境 ----------

type fakeEnv struct {
	t      *testing.T
	spec   *spec.Set
	Host   *fakeHost
	Module *fakeModule
	LLM    *fakeLLM
	Trace  []string
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	env := &fakeEnv{t: t, spec: testSpec(t)}
	// &env.Trace 在 env 是堆指针时地址稳定：桩服务与 Deps 拿到的是同一个切片。
	env.Module = newFakeModule(t, "inventory", []string{"inventory.GetStock", "inventory.DeductStock"}, &env.Trace)
	env.Host = newFakeHost(t, &env.Trace)
	env.Host.InventoryURL = env.Module.URL
	env.LLM = &fakeLLM{}
	return env
}

// Deps 组装一次 Run 的依赖，Trace 指向 env 自己的轨迹切片。
func (e *fakeEnv) Deps() Deps {
	return Deps{
		Host:       e.Host.URL,
		AgentID:    "ag_test",
		OnBehalfOf: "user:alice",
		Planner:    &Planner{LLM: e.LLM, Spec: e.spec},
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		Skills:     EmptySkills(),
		Trace:      &e.Trace,
	}
}

// PlannerCalls 是本轮实际发生的规划次数（含重规划）。
func (e *fakeEnv) PlannerCalls() int { return e.LLM.Calls }

// equalSeq 比较两个字符串序列是否逐项相同。
func equalSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------- 用例 ----------

func TestGateOrder(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_gate0001", testStep{id: "s1", action: "inventory.DeductStock"})}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	want := []string{"policies:evaluate", "grants:evaluate", "dry-run", "audit:dispatched", "execute", "audit:result"}
	if !equalSeq(env.Trace, want) {
		t.Fatalf("闸门顺序不对：\n  实际 %v\n  期望 %v", env.Trace, want)
	}
	if res.Outcome != OutcomeCompleted {
		t.Fatalf("期望 completed，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if res.PlanID != "pln_gate0001" || len(res.ActionsTaken) != 1 {
		t.Fatalf("结局字段不对：%+v", res)
	}
}

func TestGateOrderWithReadStep(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_gate0002",
		testStep{id: "s1", action: "inventory.GetStock", kind: "read", args: `{"sku":"STD-1002"}`},
		testStep{id: "s2", action: "inventory.DeductStock", dependsOn: `["s1"]`},
	)}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	// 授权要先把整份计划的每个动作都过一遍（不能执行一半才撞拒绝）；
	// 两段审计则与各自的执行紧邻，所以顺序是「dispatched → execute → result」×2。
	want := []string{
		"policies:evaluate", "grants:evaluate", "grants:evaluate", "dry-run",
		"audit:dispatched", "execute", "audit:result",
		"audit:dispatched", "execute", "audit:result",
	}
	if !equalSeq(env.Trace, want) {
		t.Fatalf("闸门顺序不对：\n  实际 %v\n  期望 %v", env.Trace, want)
	}
	if res.Outcome != OutcomeCompleted {
		t.Fatalf("期望 completed，得到 %q（%s）", res.Outcome, res.Detail)
	}
	// 读动作没有幂等语义：只有那个写动作算一次写调用。
	if env.Module.ExecuteCalls != 2 || env.Module.WriteCalls != 1 {
		t.Fatalf("执行次数不对：execute=%d write=%d", env.Module.ExecuteCalls, env.Module.WriteCalls)
	}
}

func TestTraceOrderAgainstStubRequestsOnly(t *testing.T) {
	// 轨迹由桩服务写入（而不是运行时自报），这条用例固定「谁在写轨迹」这个前提：
	// 如果哪天有人在运行时里 append 轨迹，这条会先红。
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_gate0003", testStep{id: "s1", action: "inventory.DeductStock"})}
	_ = Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if len(env.Trace) == 0 {
		t.Fatal("轨迹为空：桩服务没有写入，TestGateOrder 就失去意义")
	}
	if env.Module.ManifestCalls != 1 {
		t.Fatalf("能力发现应当只读一次 manifest，实际 %d 次", env.Module.ManifestCalls)
	}
}

func TestAuditDispatchedFailureAbortsExecution(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_audit0001", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.FailAuditPost = true

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeFailed {
		t.Fatalf("期望 failed，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("dispatched 审计写失败时必须拒绝执行，实际 write=%d execute=%d",
			env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
	want := []string{"policies:evaluate", "grants:evaluate", "dry-run", "audit:dispatched"}
	if !equalSeq(env.Trace, want) {
		t.Fatalf("审计失败后应当立即终止：实际 %v，期望 %v", env.Trace, want)
	}
}

func TestPolicyDeniedGoesNeedsHumanWithoutWorkaround(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_deny0001", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "deny"
	env.Host.PolicyReasons = []string{"NO_POLICY_MATCH_DEFAULT_DENY"}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("期望 needs_human，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("被策略拒绝后不得尝试替代路径：write=%d execute=%d", env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
	if env.PlannerCalls() != 1 {
		t.Fatalf("policy_denied 不许重新规划（换个计划就是换路绕过），实际规划 %d 次", env.PlannerCalls())
	}
	want := []string{"policies:evaluate"}
	if !equalSeq(env.Trace, want) {
		t.Fatalf("被策略拒绝后不应再碰任何闸门：实际 %v，期望 %v", env.Trace, want)
	}
	if !strings.Contains(res.Detail, "NO_POLICY_MATCH_DEFAULT_DENY") {
		t.Fatalf("结局必须带上理由码，实际：%s", res.Detail)
	}
}

// approve 命中、但**没有挂起通道**（Deps.State 为 nil）→ 仍转人工，且不带 pendingPlanId。
//
// 这是 M0.5 行为的保留路径：没有状态目录时 approve 只能是「人来按」，不能假装能挂起。
// 「没有挂起通道」的唯一表达就是 State 为 nil——空串会被 NewStateStore 拒绝（§0.1.6）。
func TestPolicyApproveGoesNeedsHumanWithoutApprovalChannel(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_approve01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "approve"

	deps := env.Deps()
	deps.State = nil
	res := Run(context.Background(), deps, RunInput{Event: quoteCompleted(12)})

	// 三态封闭：转人工就是 needs_human，不新增第四态。
	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("approve 必须转人工，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if res.PendingPlanID != nil {
		t.Fatalf("没有挂起通道时不该报 pendingPlanId，得到 %q", *res.PendingPlanID)
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("没有人批准就不能执行写动作，实际 write=%d", env.Module.WriteCalls)
	}
	// 授权与预演必须在「挂起还是转人工」之前跑完（fail-closed 要早）：一份注定过不了
	// 授权或预演的计划，不该挂上公告板等人白批一次。
	want := []string{"policies:evaluate", "grants:evaluate", "dry-run"}
	if !equalSeq(env.Trace, want) {
		t.Fatalf("approve 未放行时仍要先跑完授权与预演：实际 %v，期望 %v", env.Trace, want)
	}
	// 事件回合的 Request 必须保持 nil：多一个空请求块，prompt 就把「事实」读成「指令」。
	if strings.Contains(env.LLM.LastUser, "用户请求") {
		t.Fatalf("事件回合不该出现用户请求块：\n%s", env.LLM.LastUser)
	}
}

// approve 且配置了状态目录 → 挂起：结局仍是 needs_human，但带 pendingPlanId。
func TestPolicyApproveSuspendsWhenChannelConfigured(t *testing.T) {
	env := newFakeEnv(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	env.LLM.replies = []string{planJSON("pln_appr01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "approve"

	deps := env.Deps()
	deps.State = store
	res := Run(context.Background(), deps, RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("挂起后结局仍是 needs_human（三态封闭），得到 %q", res.Outcome)
	}
	if res.PendingPlanID == nil || *res.PendingPlanID != "pln_appr01" {
		t.Fatalf("必须带 pendingPlanId，得到 %+v", res.PendingPlanID)
	}
	p, ok, _ := store.Load("pln_appr01")
	if !ok {
		t.Fatalf("pending 未落盘")
	}
	if env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("挂起不是执行：write=%d execute=%d", env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
	// pending 必须填全：漏 ExpiresAt 会被 Expired() 的 fail-closed 立刻判过期（§0.1.7），
	// 漏 labels 则审批人看到的是一张没有白活的卡。
	if p.ExpiresAt == "" || p.Expired(time.Now()) {
		t.Fatalf("expiresAt 必须是未来时刻，得到 %q", p.ExpiresAt)
	}
	if p.CreatedAt == "" || p.By != "user:alice" || p.TriggerEventID != "evt_test0001" {
		t.Fatalf("pending 的身份锚点没填全：%+v", p)
	}
	if p.Origin != "event" || p.RequestID != nil {
		t.Fatalf("事件回合的 pending 必须是 origin=event 且 requestId=null：%+v", p)
	}
	if len(p.Labels) != 1 || !strings.Contains(p.Labels[0], "inventory.DeductStock") {
		t.Fatalf("labels 要逐 step 渲染：%+v", p.Labels)
	}
	if p.Summary == "" {
		t.Fatalf("summary 不能为空：%+v", p)
	}
	if p.Plan.PlanID != "pln_appr01" || len(p.Plan.Steps) != 1 {
		t.Fatalf("plan 要存全文（恢复时按原计划执行）：%+v", p.Plan)
	}
	if p.AuditSettled != true || p.DispatchedAuditID == "" {
		t.Fatalf("I7 三次落盘没走完：%+v", p)
	}
}

// 策略要求审批、却没给审批人角色 → **不许挂起**，当场转人工。
//
// requiredRoles 为空时 rolesIntersect 一律 false（fail-closed），这样的计划谁点都是 403：
// 挂上去只是一张永远批不掉的卡，占着公告板和审批窗口，还让人白点一次。
func TestNoApproversDoesNotSuspend(t *testing.T) {
	env := newFakeEnv(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	env.LLM.replies = []string{planJSON("pln_noappr01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "approve"
	env.Host.PolicyApprovers = []string{} // 策略要求审批，却没给审批人角色

	deps := env.Deps()
	deps.State = store
	res := Run(context.Background(), deps, RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("没有审批人角色必须转人工，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if res.PendingPlanID != nil {
		t.Fatalf("没有审批人角色就不许挂起，得到 pendingPlanId=%q", *res.PendingPlanID)
	}
	if items, _ := store.List(); len(items) != 0 {
		t.Fatalf("不该落任何 pending：%+v", items)
	}
	if archived, _ := store.ListArchived(); len(archived) != 0 {
		t.Fatalf("也不该留下归档件（压根没挂起过）：%+v", archived)
	}
	if !strings.Contains(res.Detail, "requiredRoles 为空") || !strings.Contains(res.Detail, "403") {
		t.Fatalf("Detail 必须写明「没给审批人角色，谁批都会 403」，得到 %q", res.Detail)
	}
	// 守卫必须在 I7 的三次落盘之前：走到 suspend 会先写一条 dispatched 审计，
	// 于是盘上留一条谁也不认识的审计幽灵。
	want := []string{"policies:evaluate", "grants:evaluate", "dry-run"}
	if !equalSeq(env.Trace, want) {
		t.Fatalf("闸门次序不对（守卫应当紧接在预演之后、写审计之前）：实际 %v，期望 %v", env.Trace, want)
	}
	if env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("没挂起就不许执行：write=%d execute=%d", env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
}

// deny 仍然直接转人工，不产生 pending。
func TestPolicyDenyDoesNotSuspend(t *testing.T) {
	env := newFakeEnv(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	env.LLM.replies = []string{planJSON("pln_deny01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "deny"

	deps := env.Deps()
	deps.State = store
	res := Run(context.Background(), deps, RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman || res.PendingPlanID != nil {
		t.Fatalf("deny 不该挂起：%+v", res)
	}
	if items, _ := store.List(); len(items) != 0 {
		t.Fatalf("deny 不该写 pending：%+v", items)
	}
	// deny 之后不许再跑授权与预演：拒绝就该立刻停，别给「换个计划再试」留余地。
	if want := []string{"policies:evaluate"}; !equalSeq(env.Trace, want) {
		t.Fatalf("deny 之后不该再碰闸门：实际 %v，期望 %v", env.Trace, want)
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("deny 不得执行，实际 write=%d", env.Module.WriteCalls)
	}
}

// 未知的策略处置必须 fail-closed：绝不能因为「不认识这个值」就掉进执行。
//
// policy.schema.json 是封闭枚举，但宿主的响应不受本包控制（新枚举、被篡改的响应、
// 中间层塞进来的调试值都可能出现）。这是 approve 走挂起之后留下的一处 fail-open 口子：
// 只要判断写成「不是 deny 就往下走」，未知值就会一路走到执行。
func TestUnknownPolicyOutcomeDoesNotExecute(t *testing.T) {
	env := newFakeEnv(t)
	env.Host.PolicyOutcome = "maybe" // 未来的新枚举 / 被篡改的响应
	env.LLM.replies = []string{planJSON("pln_unknown", testStep{id: "s1", action: "inventory.DeductStock"})}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("未知处置应转人工，得到 %q（%s）", res.Outcome, res.Detail)
	}
	// 只走到策略闸门，不许碰授权/预演/执行。
	if want := []string{"policies:evaluate"}; !equalSeq(env.Trace, want) {
		t.Fatalf("闸门次序不对：实际 %v，期望 %v", env.Trace, want)
	}
	if env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("未知处置绝不许执行：write=%d execute=%d", env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
}

// request 回合命中 approve → 同样挂起，只是 origin=request、requestId 有值
// （spec §6.3：「等审批」只有一条实现路径，不分来源）。
//
// 同时锁住 Run 把 in.Request 传给规划器这条接线：漏了它，prompt 分流永远不生效
// （PlanInput.Request 恒 nil，用户原话进不了「指令」块）。
func TestRequestTurnSuspendsWithOriginRequest(t *testing.T) {
	env := newFakeEnv(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	env.LLM.replies = []string{planJSON("pln_req01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "approve"

	deps := env.Deps()
	deps.State = store
	in := RunInput{
		Event:   DomainEvent{EventID: "evt_req_1", Type: "tenon.request", CorrelationID: "cor:req/req_1"},
		Request: &UserRequest{RequestID: "req_1", Text: "把 Q-1 的 12 件扣掉", By: "user:alice"},
	}
	res := Run(context.Background(), deps, in)

	if res.Outcome != OutcomeNeedsHuman || res.PendingPlanID == nil || *res.PendingPlanID != "pln_req01" {
		t.Fatalf("request 回合命中 approve 也要挂起：%+v", res)
	}
	p, ok, _ := store.Load("pln_req01")
	if !ok {
		t.Fatalf("pending 未落盘")
	}
	if p.Origin != "request" || p.RequestID == nil || *p.RequestID != "req_1" {
		t.Fatalf("request 回合的 pending 必须带 requestId：%+v", p)
	}
	if !strings.Contains(env.LLM.LastUser, "用户请求（这是指令") {
		t.Fatalf("Run 必须把 in.Request 传给规划器（否则 prompt 分流永远不生效）：\n%s", env.LLM.LastUser)
	}
}

// request 回合的 skill 正文来自 SelectSkills 的候选集（而不是 tenon.subscribes）：
// 只按文本粗筛命中的 skill 必须进 prompt。
func TestRequestTurnUsesSelectedSkills(t *testing.T) {
	t.Run("正文命中", func(t *testing.T) {
		env := newFakeEnv(t)
		env.LLM.replies = []string{planJSON("pln_sel01", testStep{id: "s1", action: "inventory.DeductStock"})}
		deps := env.Deps()
		// 订阅的是一个与本次事件无关的类型：request 回合的候选集只按文本粗筛，不看订阅。
		deps.Skills = &SkillStore{skills: []Skill{{
			Name:       "stock-first",
			Subscribes: []string{"quote.completed"},
			Body:       "扣库存之前先核对 inventory 的在库数。",
		}}}

		res := Run(context.Background(), deps, RunInput{
			Event:   DomainEvent{EventID: "evt_req_sel", Type: "tenon.request", CorrelationID: "cor:req/req_sel"},
			Request: &UserRequest{RequestID: "req_sel", Text: "inventory 差 12 件，先扣现货", By: "user:alice"},
		})
		if res.Outcome != OutcomeCompleted {
			t.Fatalf("期望 completed，得到 %q（%s）", res.Outcome, res.Detail)
		}
		if !strings.Contains(env.LLM.LastUser, "## skill stock-first") {
			t.Fatalf("request 回合必须把选中的 skill 带进 prompt：\n%s", env.LLM.LastUser)
		}
		if !strings.Contains(env.LLM.LastUser, "扣库存之前先核对 inventory 的在库数。") {
			t.Fatalf("选中 skill 的正文必须逐字进 prompt：\n%s", env.LLM.LastUser)
		}
	})

	// 只靠订阅命中、正文为空的 skill：会被选中，但**绝不许**渲染成「没有匹配」——
	// 那等于「选了 1 份却告诉模型一份都没有」。
	t.Run("空正文渲染占位", func(t *testing.T) {
		env := newFakeEnv(t)
		env.LLM.replies = []string{planJSON("pln_sel02", testStep{id: "s1", action: "inventory.DeductStock"})}
		deps := env.Deps()
		deps.Skills = &SkillStore{skills: []Skill{{
			Name:       "subscription-only",
			Subscribes: []string{"inventory"}, // 命中靠这里，正文为空
			Body:       "",
		}}}

		res := Run(context.Background(), deps, RunInput{
			Event:   DomainEvent{EventID: "evt_req_sel2", Type: "tenon.request", CorrelationID: "cor:req/req_sel2"},
			Request: &UserRequest{RequestID: "req_sel2", Text: "inventory 差 12 件，先扣现货", By: "user:alice"},
		})
		if res.Outcome != OutcomeCompleted {
			t.Fatalf("期望 completed，得到 %q（%s）", res.Outcome, res.Detail)
		}
		if !strings.Contains(env.LLM.LastUser, "（无正文，仅按订阅命中）") {
			t.Fatalf("空正文必须渲染成显式占位：\n%s", env.LLM.LastUser)
		}
		if strings.Contains(env.LLM.LastUser, "本次没有匹配的 skill 正文") {
			t.Fatalf("选中的 skill 不许渲染成「没有匹配」——选了却告诉模型没有，候选集就被吞了：\n%s",
				env.LLM.LastUser)
		}
	})
}

// 事件回合一个字都不许变：skill 正文仍只按 tenon.subscribes 取（TextFor），不经过粗筛。
func TestEventTurnTextStillSubscription(t *testing.T) {
	env := newFakeEnv(t)
	plan := planJSON("pln_evt01", testStep{id: "s1", action: "inventory.DeductStock"})
	env.LLM.replies = []string{plan, plan, plan}

	run := func(skills *SkillStore) string {
		deps := env.Deps()
		deps.Skills = skills
		res := Run(context.Background(), deps, RunInput{Event: quoteCompleted(12)})
		if res.Outcome != OutcomeCompleted {
			t.Fatalf("回合失败：%q（%s）", res.Outcome, res.Detail)
		}
		return env.LLM.LastUser
	}

	baseline := run(EmptySkills())
	// ① 一份「按文本一定被 SelectSkills 命中」的 skill，但它没订阅这个事件：
	//    prompt 必须与基线**逐字相同**（多一个字都说明事件回合被粗筛污染了）。
	leak := run(&SkillStore{skills: []Skill{{
		Name:       "text-only",
		Subscribes: []string{"tenon.request"},
		Body:       "inventory 的业务语义参考（只该在 request 回合出现）。",
	}}})
	if leak != baseline {
		t.Fatalf("事件回合的 prompt 必须逐字不变（粗筛不许漏进事件回合）：\n 基线 %q\n 实际 %q",
			baseline, leak)
	}
	if strings.Contains(leak, "text-only") {
		t.Fatalf("没订阅该事件的 skill 不许进事件回合的 prompt：\n%s", leak)
	}
	// ② 订阅了事件的 skill 仍然按 TextFor 进 prompt——哪怕正文里没有任何模块名
	//    （SelectSkills 永远不会选它，事件回合也不该由粗筛决定）。
	subscribed := run(&SkillStore{skills: []Skill{{
		Name:       "quote-skill",
		Subscribes: []string{"quote.completed"},
		Body:       "报价完成后的业务语义参考，正文里没有任何模块名。",
	}}})
	if !strings.Contains(subscribed, "## skill quote-skill") {
		t.Fatalf("订阅了事件的 skill 必须按 TextFor 进 prompt：\n%s", subscribed)
	}
	if subscribed == baseline {
		t.Fatalf("订阅了事件的 skill 必须改变 prompt（TextFor 仍在用）")
	}
}

func TestGrantsDeniedGoesNeedsHuman(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_grant0001", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.GrantAllowed = false
	env.Host.GrantReason = "SUBJECT_NOT_PERMITTED"

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("期望 needs_human，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("授权未通过时不得执行，实际 write=%d", env.Module.WriteCalls)
	}
	if !strings.Contains(res.Detail, "SUBJECT_NOT_PERMITTED") {
		t.Fatalf("结局必须带上拒绝原因，实际：%s", res.Detail)
	}
}

func TestDryRunUnsafeGoesNeedsHuman(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_dry0001", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Module.DryRunStatus = "unsafe"

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("预演 unsafe 必须转人工，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("预演不安全时不得执行，实际 write=%d", env.Module.WriteCalls)
	}
}

// TestDryRunNotFoundForcesHuman 锁住 fail-closed 语义：
// 模块没实现预演（404）不等于免检。设计文档 §8 的降级矩阵写的是
// 「无 dry-run（unsupported）或 unsafe → 强制审批」。
//
// 曾经的实现把它当「忽略并继续」，那会形成一个供应商可自助开启的旁路：
// 删掉 :dry-run 端点就能绕过 G1。这条用例就是那个旁路的回归护栏。
func TestDryRunNotFoundForcesHuman(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_dry0002", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Module.DryRunMissing = true

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("dry-run 404 应转人工（fail-closed），得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("预演不可用时不得执行，实际 write=%d", env.Module.WriteCalls)
	}
	if !strings.Contains(res.Detail, "未实现预演") {
		t.Fatalf("Detail 应说明原因，得到 %q", res.Detail)
	}
}

func TestBusinessRejectionReplansAtMostTwice(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{
		planJSON("pln_br0001", testStep{id: "s1", action: "inventory.DeductStock"}),
		planJSON("pln_br0002", testStep{id: "s1", action: "inventory.DeductStock", args: `{"sku":"STD-1002","qty":3,"referenceNo":"Q-1"}`}),
		planJSON("pln_br0003", testStep{id: "s1", action: "inventory.DeductStock", args: `{"sku":"STD-1002","qty":1,"referenceNo":"Q-1"}`}),
		planJSON("pln_br0004", testStep{id: "s1", action: "inventory.DeductStock"}),
	}
	env.Module.AlwaysReject = "STOCK_INSUFFICIENT"

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("期望 needs_human，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if got := env.PlannerCalls(); got != 3 {
		t.Fatalf("期望 1 次初始 + 2 次重规划 = 3 次规划，实际 %d 次", got)
	}
	if env.Module.WriteCalls != 3 {
		t.Fatalf("三轮各执行一次写，期望 3 次写调用，实际 %d", env.Module.WriteCalls)
	}
	if !strings.Contains(res.Detail, "重新规划 2 轮") {
		t.Fatalf("结局必须说明重规划上限已用尽，实际：%s", res.Detail)
	}
	if env.LLM.prompts[1] == env.LLM.prompts[0] {
		t.Fatal("重规划的 prompt 必须带上上一轮被拒的原因，否则就是拿同一份 prompt 撞三次墙")
	}
	if !strings.Contains(env.LLM.prompts[1], "STOCK_INSUFFICIENT") {
		t.Fatalf("重规划 prompt 必须包含业务拒绝的错误码，实际：\n%s", env.LLM.prompts[1])
	}
}

func TestRetryableIsRetriedWithSameIdempotencyKey(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_retry0001", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Module.RetryableTimes = 1

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeCompleted {
		t.Fatalf("retryable 后重试成功应当 completed，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 2 {
		t.Fatalf("期望「失败一次 + 重试成功」= 2 次写调用，实际 %d", env.Module.WriteCalls)
	}
	keys := env.Module.IdempotencyKeys
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("重试期间幂等键必须不变，实际 %v", keys)
	}
	if want := "tenon:pln_retry0001/s1"; keys[0] != want {
		t.Fatalf("幂等键必须是 %q（protocol.IdempotencyKey 的约定），实际 %q", want, keys[0])
	}
}

func TestHostUnreachableFailsClosed(t *testing.T) {
	env := newFakeEnv(t)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	deps := env.Deps()
	deps.Host = deadURL
	res := Run(context.Background(), deps, RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeFailed {
		t.Fatalf("host 不可达必须 failed（fail-closed），得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("host 不可达时不得执行任何写：write=%d execute=%d", env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
	if env.PlannerCalls() != 0 {
		t.Fatalf("连模块清单都拿不到就不该规划，实际规划 %d 次", env.PlannerCalls())
	}
}

func TestCapabilityGapGoesNeedsHuman(t *testing.T) {
	env := newFakeEnv(t)
	// 规划里出现了清单外的动作（例如模型硬要买料，而采购模块已被摘除）。
	env.LLM.replies = []string{planJSON("pln_gap0001", testStep{id: "s1", action: "procurement.CreatePurchaseRequest"})}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("能力不足必须 needs_human（不是 failed），得到 %q（%s）", res.Outcome, res.Detail)
	}
	if res.PlanID != "pln_gap0001" {
		t.Fatalf("结局要能指出是哪份计划缺口，实际 planId=%q", res.PlanID)
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("清单外的动作绝不能被执行，实际 write=%d", env.Module.WriteCalls)
	}
	if env.PlannerCalls() != 1 {
		t.Fatalf("能力不足不重试规划，实际 %d 次", env.PlannerCalls())
	}
}

func TestEnabledModulesAreFiltered(t *testing.T) {
	env := newFakeEnv(t)
	// 注册表是管理面视图：摘除的模块仍然在列表里，但 enabled=false。
	disabled := newFakeModule(t, "procurement", []string{"procurement.CreatePurchaseRequest"}, &env.Trace)
	env.Host.ExtraModules = []moduleEntry{{ID: "procurement", BaseURL: disabled.URL, Enabled: false}}
	env.LLM.replies = []string{planJSON("pln_filter001", testStep{id: "s1", action: "inventory.DeductStock"})}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeCompleted {
		t.Fatalf("期望 completed，得到 %q（%s）", res.Outcome, res.Detail)
	}
	if disabled.ManifestCalls != 0 {
		t.Fatalf("enabled=false 的模块不该被做能力发现，实际读了 %d 次 manifest", disabled.ManifestCalls)
	}
	if strings.Contains(env.LLM.LastUser, "procurement") {
		t.Fatalf("摘除模块的动作绝不能出现在 prompt 里：\n%s", env.LLM.LastUser)
	}
}

func TestAuditRecordsTwoSegments(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_audit0002", testStep{id: "s1", action: "inventory.DeductStock"})}
	_ = Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if len(env.Host.AuditRecords) != 2 {
		t.Fatalf("期望 dispatched + result 两段，实际 %d 条", len(env.Host.AuditRecords))
	}
	dispatched, result := env.Host.AuditRecords[0], env.Host.AuditRecords[1]
	if dispatched["stage"] != "dispatched" || result["stage"] != "result" {
		t.Fatalf("两段顺序不对：%v / %v", dispatched["stage"], result["stage"])
	}
	if dispatched["auditId"] != result["auditId"] {
		t.Fatalf("同一笔调用的两段必须共用 auditId：%v / %v", dispatched["auditId"], result["auditId"])
	}
	if dispatched["planId"] != "pln_audit0002" || dispatched["stepId"] != "s1" {
		t.Fatalf("审计缺少计划锚点：%v", dispatched)
	}
	if got := dispatched["idempotencyKey"]; got != "tenon:pln_audit0002/s1" {
		t.Fatalf("写动作的审计必须带幂等键，实际 %v", got)
	}
	for _, field := range []string{"trigger", "inputHash", "tokenUsage", "actor", "effects"} {
		if _, ok := dispatched[field]; !ok {
			t.Fatalf("审计记录缺少必填字段 %q（audit-event.schema.json）：%v", field, dispatched)
		}
	}
	decision, _ := dispatched["decision"].(map[string]any)
	if decision["outcome"] != outcomeAuto {
		t.Fatalf("审计的 decision.outcome 应当是闸门处置 %q，实际 %v", outcomeAuto, decision["outcome"])
	}
	// 这里锁一个曾经写错过的语义：policyId 必须是「作出该处置的策略 id」，
	// 不是处置值本身（把 outcome 抄进 policyId 会看起来有策略却答不出是哪一条）。
	if decision["policyId"] != "pol-test" {
		t.Fatalf("audit.decision.policyId 应当是策略 id（pol-test），实际 %v", decision["policyId"])
	}
	if decision["grantId"] != "gr-test001" {
		t.Fatalf("audit.decision.grantId 应当是命中的授权，实际 %v", decision["grantId"])
	}
	res, _ := dispatched["result"].(map[string]any)
	if res["status"] != "unknown" {
		t.Fatalf("dispatched 段的结果必须未定（unknown），实际 %v", res["status"])
	}
}

// F1 的反面：auto 路径没有人批准过，step 级两段的 decision.approvedBy 必须是 null。
// 恢复路径给 step 级审计填审批人时，若把 gate（以及 auditBase）的取值搞混，这条会先红。
func TestAuditAutoPathKeepsApprovedByNull(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_auto0003", testStep{id: "s1", action: "inventory.DeductStock"})}
	_ = Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if len(env.Host.AuditRecords) != 2 {
		t.Fatalf("期望 dispatched + result 两段，实际 %d 条", len(env.Host.AuditRecords))
	}
	for _, rec := range env.Host.AuditRecords {
		decision, _ := rec["decision"].(map[string]any)
		if decision["outcome"] != outcomeAuto {
			t.Fatalf("auto 路径的 decision.outcome 应为 %q，实际 %v", outcomeAuto, decision["outcome"])
		}
		if got := decision["approvedBy"]; got != nil {
			t.Fatalf("auto 路径的 decision.approvedBy 必须是 null（那条路径确实没人批），实际 %#v", got)
		}
	}
}

// F2：人工请求回合的业务链锚点必须是**合法**的 cor: 值。
// 判据在 schema（common.schema.json#/$defs/CorrelationId 的 ^cor:\S+$），不在实现里；
// 原先直接把 by（user:alice）当锚点，落进审计 trigger.correlationId 就是一条不合规记录。
func TestAuditRequestCorrelationIDIsSchemaValid(t *testing.T) {
	// 与 common.schema.json 的 CorrelationId 模式逐字相同。
	correlationIDPattern := regexp.MustCompile(`^cor:\S+$`)

	for _, requestID := range []string{"req_01JB0TEST", "req_FAKE"} {
		got := RequestCorrelationID(requestID)
		if got != "cor:request/"+requestID {
			t.Fatalf("锚点形状应为 cor:request/<requestId>，实际 %q", got)
		}
		if !correlationIDPattern.MatchString(got) {
			t.Fatalf("锚点 %q 不满足 CorrelationId 模式 ^cor:\\S+$", got)
		}
		// 业务键必须来自 requestId：「谁问的」不在这里重复表达。
		if strings.Contains(got, "user:") {
			t.Fatalf("锚点不许拿身份当业务键，实际 %q", got)
		}
	}
}

// request 回合把锚点一路带进审计：run.go 用 RunInput.CorrelationID 覆盖 plan.Trigger，
// 审计 trigger 也取自它。这里断言审计里是 cor:request/<requestId>，而「谁问的」仍在
// actor.onBehalfOf——两者不互相冒充。
func TestAuditRequestTurnCorrelationIsCorAnchor(t *testing.T) {
	env := newFakeEnv(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	const requestID = "req_cor01"
	anchor := RequestCorrelationID(requestID)
	env.LLM.replies = []string{planJSON("pln_cor01", testStep{id: "s1", action: "inventory.GetStock", kind: "read"})}

	deps := env.Deps()
	deps.State = store
	res := Run(context.Background(), deps, RunInput{
		Event:         DomainEvent{EventID: "evt_req_cor01", Type: "tenon.request", CorrelationID: anchor},
		CorrelationID: anchor,
		Request:       &UserRequest{RequestID: requestID, Text: "查一下 STD-1002 的库存", By: "user:alice"},
	})
	if res.Outcome != OutcomeCompleted {
		t.Fatalf("期望 completed，得到 %q（%s）", res.Outcome, res.Detail)
	}

	correlationIDPattern := regexp.MustCompile(`^cor:\S+$`)
	if len(env.Host.AuditRecords) != 2 {
		t.Fatalf("一步计划应写 dispatched + result 两段，实际 %d 条", len(env.Host.AuditRecords))
	}
	for _, rec := range env.Host.AuditRecords {
		trigger, _ := rec["trigger"].(map[string]any)
		got, _ := trigger["correlationId"].(string)
		if got != anchor {
			t.Fatalf("审计 trigger.correlationId 应为 %q，实际 %q", anchor, got)
		}
		if !correlationIDPattern.MatchString(got) {
			t.Fatalf("审计 trigger.correlationId %q 不满足 ^cor:\\S+$", got)
		}
		actor, _ := rec["actor"].(map[string]any)
		if actor["onBehalfOf"] != "user:alice" {
			t.Fatalf("「谁问的」必须仍由 actor.onBehalfOf 承载，实际 %v", actor["onBehalfOf"])
		}
	}
}

func TestIntentFrameSentOnCompleted(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{planJSON("pln_intent001", testStep{id: "s1", action: "inventory.DeductStock"})}
	_ = Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if len(env.Host.IntentFrames) != 1 {
		t.Fatalf("完成一回合应当发一帧 Intent，实际 %d 帧", len(env.Host.IntentFrames))
	}
	frame := env.Host.IntentFrames[0]
	if frame["type"] != frameTypeIntent || frame["planId"] != "pln_intent001" {
		t.Fatalf("Intent 帧字段不对：%v", frame)
	}
	actions, _ := frame["actions"].([]any)
	if len(actions) != 2 {
		t.Fatalf("completed 应当给 open + highlight，实际 %v", actions)
	}
	first, _ := actions[0].(map[string]any)
	if first["kind"] != "open" || first["target"] != "quote:Quote/Q-1" {
		t.Fatalf("第一帧动作不对：%v", first)
	}
}
func TestPlanSchemaFailureIsFailedNotNeedsHuman(t *testing.T) {
	env := newFakeEnv(t)
	env.LLM.replies = []string{`{"planId":"pln_broken1","steps":"oops"}`}

	res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

	if res.Outcome != OutcomeFailed {
		t.Fatalf("模型输出坏了必须 failed（不是 needs_human），得到 %q（%s）", res.Outcome, res.Detail)
	}
	if env.PlannerCalls() != maxPlanAttempts {
		t.Fatalf("坏输出应当在规划器内部重试一次（共 %d 次），实际 %d 次", maxPlanAttempts, env.PlannerCalls())
	}
	if env.Module.WriteCalls != 0 {
		t.Fatalf("没有合法计划就不得执行，实际 write=%d", env.Module.WriteCalls)
	}
}

// ---------- 测试用 HTTP 工具 ----------

func readTestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("读取请求体失败：%v", err)
	}
	return body
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeTestError(w http.ResponseWriter, status int, class, code, message string) {
	writeTestJSON(w, status, map[string]any{
		"tenonError": map[string]any{
			"class":     class,
			"code":      code,
			"message":   message,
			"retryable": class == "retryable",
		},
	})
}

// newRunnerForDiscover 造一个只够跑 discover 的 runner：宿主注册表指向假模块。
func newRunnerForDiscover(t *testing.T, moduleBase string) *runner {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenon/modules" {
			http.NotFound(w, r)
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"modules": []map[string]any{{"id": "inventory", "baseURL": moduleBase, "enabled": true}},
		})
	}))
	t.Cleanup(srv.Close)
	return &runner{host: srv.URL, client: srv.Client()}
}

// 模板必须随能力发现一起到 agent 手里：它是渲染白话的唯一来源，
// 少了它就只能退回 R2（标题 + 参数），卡上读不到「扣减 … 库存 3 件」这种话。
func TestDiscoverCarriesActionMeta(t *testing.T) {
	mod := newFakeModule(t, "inventory", []string{"inventory.DeductStock"}, new([]string))
	mod.Templates = map[string]string{
		"inventory.DeductStock": "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）",
	}
	r := newRunnerForDiscover(t, mod.Server.URL)

	got, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("能力发现失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 个模块，得到 %d", len(got))
	}
	meta, ok := got[0].ActionMeta["inventory.DeductStock"]
	if !ok {
		t.Fatalf("ActionMeta 缺 inventory.DeductStock：%+v", got[0])
	}
	if meta.SummaryTemplate != "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）" {
		t.Fatalf("模板不符：%q", meta.SummaryTemplate)
	}
	if meta.Title == "" {
		t.Fatalf("Title 也要带走（R2 兜底要用）")
	}
}

// 挂起时渲染一次并落盘：labels 是白话、details 是技术原文、summary 是白话拼接。
//
// 注意 runner.modules 是 map[string]string（模块 → 基地址），**不携带**模板；
// 发现结果（[]ModuleDigest）只在 run() 里以局部变量 digest 存在，所以元信息
// 是以参数传进 buildPending 的，不挂在 runner 上。
func TestBuildPendingRendersPlainLanguage(t *testing.T) {
	meta := map[string]ActionMeta{
		"inventory.DeductStock": {Title: "扣减库存", SummaryTemplate: "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）"},
	}
	r := &runner{
		deps:  Deps{OnBehalfOf: "user:alice"},
		event: DomainEvent{Subject: json.RawMessage(`"inventory:SKU/STD-1002"`)},
	}
	plan := Plan{PlanID: "pln_1", Trigger: PlanTrigger{EventID: "evt_1"}, Steps: []PlanStep{{
		StepID: "s1", Action: "inventory.DeductStock", Kind: "write",
		Args: json.RawMessage(`{"sku":"STD-1002","qty":3,"referenceNo":"Q-2026-0417"}`),
	}}}
	got, err := r.buildPending(plan, policyEvaluation{Approvers: []string{"role:warehouse_manager"}}, meta)
	if err != nil {
		t.Fatalf("buildPending 失败：%v", err)
	}
	if want := "扣减 STD-1002 库存 3 件（单据 Q-2026-0417）"; got.Labels[0] != want {
		t.Fatalf("label = %q，期望 %q", got.Labels[0], want)
	}
	if len(got.Details) != 1 || !strings.Contains(got.Details[0], "inventory.DeductStock") {
		t.Fatalf("details 必须带 action id：%v", got.Details)
	}
	if got.Summary != got.Labels[0] {
		t.Fatalf("单步计划的 summary 应等于该步白话：%q", got.Summary)
	}
}

// 发现结果必须带上 requiredArgs：它是 prompt 渲染必填字段的唯一来源。
func TestDiscoverCarriesRequiredArgs(t *testing.T) {
	mod := newFakeModule(t, "inventory", []string{"inventory.DeductStock"}, new([]string))
	mod.Meta = map[string]map[string]any{
		"inventory.DeductStock": {"requiredArgs": []string{"sku", "qty"}},
	}
	r := newRunnerForDiscover(t, mod.Server.URL)

	got, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("能力发现失败：%v", err)
	}
	meta := got[0].ActionMeta["inventory.DeductStock"]
	if len(meta.RequiredArgs) != 2 || meta.RequiredArgs[0] != "sku" {
		t.Fatalf("requiredArgs 没带回来：%+v", meta)
	}
}
