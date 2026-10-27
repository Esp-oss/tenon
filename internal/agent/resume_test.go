package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 恢复路径的固定时间轴（§0.1.1：夹具一律注入固定时间，绝不 time.Now()）。
//
// 远端与上古两个常量分别表达「还没到期」和「已经过期」：两者都取到不可能自然越过的
// 位置，所以这套用例不会因为过了某一天自己变红。计划正文里的 2026-10-27T09:30:03Z
// 是「今天下午就过期」的常量，写进用例等于给未来的自己埋一颗定时炸弹。
const (
	resumeExpiresFuture = "2099-01-01T00:00:00Z"
	resumeExpiresPast   = "2000-01-01T00:00:00Z"
	resumeCreatedAt     = "2026-10-27T09:15:03Z"
	resumeConfirmAt     = "2026-10-27T09:20:00Z"
)

// resumeEnv 是恢复路径的测试装配：复用 run_test.go 的假 host + 假模块，再加一个
// t.TempDir() 的状态目录。
//
// 恢复路径**不规划**（plan 原文就在 pending 文件里），所以这里不碰 env.LLM：
// 一旦哪天恢复路径偷偷调了规划器，这条用例集合会先因为「多出一次 policies:evaluate
// 之外的请求」而变红。
type resumeEnv struct {
	*fakeEnv
	Store *StateStore
}

func newResumeEnv(t *testing.T) *resumeEnv {
	t.Helper()
	env := newFakeEnv(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	return &resumeEnv{fakeEnv: env, Store: store}
}

// SeedPending 造一份还没过期的 pending，并用 suspend 落盘（走完 I7 的
// pending → dispatched 审计 → 回写标记，与真实挂起同一条路径）。
func (e *resumeEnv) SeedPending(t *testing.T, planID string) Pending {
	t.Helper()
	return e.seedPending(t, planID, resumeExpiresFuture,
		testStep{id: "s1", action: "inventory.DeductStock"})
}

// SeedPendingExpired 造一份已经过期的 pending：ExpiresAt 是过去的固定常量。
func (e *resumeEnv) SeedPendingExpired(t *testing.T, planID string) Pending {
	t.Helper()
	return e.seedPending(t, planID, resumeExpiresPast,
		testStep{id: "s1", action: "inventory.DeductStock"})
}

func (e *resumeEnv) seedPending(t *testing.T, planID, expiresAt string, steps ...testStep) Pending {
	t.Helper()
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON(planID, steps...)), &plan); err != nil {
		t.Fatalf("构造计划失败：%v", err)
	}
	p := Pending{
		PlanID:         planID,
		State:          StateAwaitingApproval,
		TriggerEventID: "evt_test0001",
		// subject 必须落盘（Step 0c）：恢复后的审计 effects.entities 靠它。
		Subject:           json.RawMessage(`"quote:Quote/Q-1"`),
		Origin:            "event",
		By:                "user:alice",
		Plan:              plan,
		Labels:            []string{"inventory.DeductStock（STD-1002 × 12）"},
		Summary:           "从标准件仓扣掉 12 件",
		RequiredRoles:     []string{"role:warehouse_manager"},
		DispatchedAuditID: "aud_" + planID,
		CreatedAt:         resumeCreatedAt,
		ExpiresAt:         expiresAt,
	}
	if err := suspend(context.Background(), e.Store, e.Deps(), p); err != nil {
		t.Fatalf("造 pending 失败（suspend）：%v", err)
	}
	got, ok, err := e.Store.Load(planID)
	if err != nil || !ok {
		t.Fatalf("pending 没落盘：ok=%v err=%v", ok, err)
	}
	return got
}

// Resume 走一次恢复；Deps 与造 pending 时同一份（宿主、模块都是真的 HTTP 桩）。
func (e *resumeEnv) Resume(t *testing.T, c Confirm) error {
	t.Helper()
	return Resume(context.Background(), e.Deps(), e.Store, c)
}

// confirmFrame 是一条 tenon.confirm 帧的 agent 侧投影。时间用固定值。
func confirmFrame(planID, decision, by string, roles []string) Confirm {
	return Confirm{
		ConfirmID: "cf_" + planID,
		PlanID:    planID,
		Decision:  decision,
		By:        by,
		Roles:     roles,
		At:        resumeConfirmAt,
	}
}

// PlanResultAudit 返回计划级 result 段（auditId = 挂起时那条 dispatchedAuditId，
// stepId 为 null）。步骤级 result 段各有自己的 auditId，不会混进来。
func (e *resumeEnv) PlanResultAudit(auditID string) map[string]any {
	for _, rec := range e.Host.AuditRecords {
		if rec["auditId"] != auditID || rec["stage"] != auditStageResult {
			continue
		}
		return rec
	}
	return nil
}

// ResultApprovedBy 读计划级 result 段里的 decision.approvedBy。
func (e *resumeEnv) ResultApprovedBy(auditID string) string {
	rec := e.PlanResultAudit(auditID)
	if rec == nil {
		return ""
	}
	decision, _ := rec["decision"].(map[string]any)
	by, _ := decision["approvedBy"].(string)
	return by
}

// ExecutedActions 是**真的执行成功**的动作：以步骤级 result 审计（status=ok）为准，
// 而不是以「发出过调用」为准——业务拒绝（G4 失效）也发过调用，但什么都没改。
func (e *resumeEnv) ExecutedActions() []string {
	out := []string{}
	for _, rec := range e.Host.AuditRecords {
		if rec["stage"] != auditStageResult || rec["stepId"] == nil {
			continue
		}
		result, _ := rec["result"].(map[string]any)
		if result["status"] != auditStatusOK {
			continue
		}
		if action, ok := rec["action"].(string); ok && action != "" {
			out = append(out, action)
		}
	}
	return out
}

// StepAudit 找第一条 stepId 非空的指定 stage 记录（即步骤级审计）。
func (e *resumeEnv) StepAudit(stage string) map[string]any {
	for _, rec := range e.Host.AuditRecords {
		if rec["stage"] == stage && rec["stepId"] != nil {
			return rec
		}
	}
	return nil
}

// ArchivedState 返回归档件状态；没归档返回空串。
func (e *resumeEnv) ArchivedState(planID string) PendingState {
	p, err := e.Store.LoadArchived(planID)
	if err != nil {
		return ""
	}
	return p.State
}

// ---------- 用例 ----------

// IsRetryable 是「这一帧立刻 ack 掉，还是留到下一 tick 重放」的唯一判据。
//
// 每条子用例都走真实的 Resume，而不是手捏一个 resumeError：谓词要判的正是 Resume
// 各出口给出的那个错误，捏一个同形状的值只能证明「字段读得出来」。
func TestIsRetryable(t *testing.T) {
	approveAsBob := []string{"role:warehouse_manager"}

	t.Run("nil", func(t *testing.T) {
		if IsRetryable(nil) {
			t.Fatal("没有错误就没有可重试的事（已处理完，含幂等重放）")
		}
	})

	// 确定性拒绝：重放一万次也是同一个结论，必须立刻 ack。
	rejects := []struct {
		name string
		run  func(t *testing.T, env *resumeEnv)
	}{
		{"角色不匹配", func(t *testing.T, env *resumeEnv) {
			p := env.SeedPending(t, "pln_retry_r1")
			err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:alice", []string{"role:warehouse_clerk"}))
			if err == nil {
				t.Fatal("角色不匹配必须报错")
			}
			if IsRetryable(err) {
				t.Fatalf("角色不匹配是确定性拒绝（换个人批是另一帧的事）：%v", err)
			}
		}},
		{"过期", func(t *testing.T, env *resumeEnv) {
			p := env.SeedPendingExpired(t, "pln_retry_r2")
			err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", approveAsBob))
			if err == nil {
				t.Fatal("过期必须报错")
			}
			if IsRetryable(err) {
				t.Fatalf("过期是确定性拒绝（时间不会倒流）：%v", err)
			}
		}},
		{"未知 planId", func(t *testing.T, env *resumeEnv) {
			err := env.Resume(t, confirmFrame("pln_retry_missing", "approve", "user:bob", approveAsBob))
			if err == nil {
				t.Fatal("未知 planId 必须报错")
			}
			if IsRetryable(err) {
				t.Fatalf("未知 planId 是确定性拒绝（等多久都不会出现）：%v", err)
			}
		}},
		{"已归档非 done", func(t *testing.T, env *resumeEnv) {
			p := env.SeedPending(t, "pln_retry_r3")
			if err := env.Store.Archive(p.PlanID, StateRejected); err != nil {
				t.Fatalf("造归档件失败：%v", err)
			}
			err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", approveAsBob))
			if err == nil {
				t.Fatal("已归档非 done 必须报错")
			}
			if IsRetryable(err) {
				t.Fatalf("已归档非 done 是确定性拒绝（重放只会重复敲同一扇门）：%v", err)
			}
		}},
		{"未知处置", func(t *testing.T, env *resumeEnv) {
			p := env.SeedPending(t, "pln_retry_r4")
			err := env.Resume(t, confirmFrame(p.PlanID, "maybe", "user:bob", approveAsBob))
			if err == nil {
				t.Fatal("未知处置必须报错")
			}
			if IsRetryable(err) {
				t.Fatalf("未知处置是确定性拒绝（amend 属规划路径，不在恢复里）：%v", err)
			}
		}},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newResumeEnv(t)) })
	}

	// 瞬时故障：审计/策略/能力发现/网络类，pending 与审计都还完整，必须留着重放。
	t.Run("瞬时故障可重试", func(t *testing.T) {
		env := newResumeEnv(t)
		p := env.SeedPending(t, "pln_retry_r5")
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		deps := env.Deps()
		deps.Host = deadURL
		err := Resume(context.Background(), deps, env.Store,
			confirmFrame(p.PlanID, "approve", "user:bob", approveAsBob))
		if err == nil {
			t.Fatal("宿主不可达必须报错")
		}
		if !IsRetryable(err) {
			t.Fatalf("宿主不可达是瞬时故障，必须可重试：%v", err)
		}
		if _, ok, _ := env.Store.Load(p.PlanID); !ok {
			t.Fatal("可重试的故障不许把 pending 弄丢")
		}
	})

	// 包装链不该改变结论：errors.As 要能穿过 fmt.Errorf("%w")。
	t.Run("包装后的错误", func(t *testing.T) {
		retry := fmt.Errorf("恢复失败：%w", resumeError{Msg: "宿主不可达", Retryable: true})
		if !IsRetryable(retry) {
			t.Fatalf("包装后的可重试错误必须仍是可重试：%v", retry)
		}
		deny := fmt.Errorf("恢复失败：%w", resumeError{Msg: "未知 planId"})
		if IsRetryable(deny) {
			t.Fatalf("包装后的确定性拒绝必须仍不可重试：%v", deny)
		}
		// 不是 resumeError 的错误按可重试处理：没人给它定性时，宁可多来一次，
		// 也不要把一条可能成功的 confirm 当确定性拒绝丢掉。
		if !IsRetryable(errors.New("某个没包 resumeError 的错误")) {
			t.Fatal("非 resumeError 的错误按可重试处理（丢帧的代价更高）")
		}
	})
}

// 合法审批人 → 执行全部步骤 → 计划级 result 段带 approvedBy → 归档 done。
func TestResumeExecutesAllStepsWithApprovedBy(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r1")
	// suspend 写过一条计划级 dispatched 审计；恢复路径的轨迹从它之后开始算。
	seeded := len(env.Trace)

	if err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"})); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if got := env.ExecutedActions(); len(got) != 1 || got[0] != "inventory.DeductStock" {
		t.Fatalf("应执行 1 步，得到 %v", got)
	}
	if by := env.ResultApprovedBy(p.DispatchedAuditID); by != "user:bob" {
		t.Fatalf("result 段 approvedBy 应为 user:bob，得到 %q", by)
	}
	// 闸门处置仍是 approve（人批的），不许改写成 auto（Step 0d）。
	rec := env.PlanResultAudit(p.DispatchedAuditID)
	if rec == nil {
		t.Fatalf("缺计划级 result 段")
	}
	if decision, _ := rec["decision"].(map[string]any); decision["outcome"] != outcomeApprove {
		t.Fatalf("计划级 decision.outcome 必须保持 %q，得到 %v", outcomeApprove, decision["outcome"])
	}
	if result, _ := rec["result"].(map[string]any); result["status"] != auditStatusOK {
		t.Fatalf("成功路径的计划级 result.status 应为 ok，得到 %v", result["status"])
	}
	if st := env.ArchivedState(p.PlanID); st != StateDone {
		t.Fatalf("应归档 done，得到 %q", st)
	}
	// Step 0c：subject 要跟着 pending 回来，否则 effects.entities 退化成空数组。
	dispatched := env.StepAudit(auditStageDispatched)
	if dispatched == nil {
		t.Fatalf("缺步骤级 dispatched 审计")
	}
	effects, _ := dispatched["effects"].(map[string]any)
	entities, _ := effects["entities"].([]any)
	if len(entities) != 1 || entities[0] != "quote:Quote/Q-1" {
		t.Fatalf("恢复执行的审计必须带实体寻址（subject 丢了？），得到 %v", entities)
	}
	// 恢复不复用 round()：不重新规划、不重跑预演，闸门各一次、步骤两段、计划级一段收尾。
	want := []string{
		"policies:evaluate", "grants:evaluate",
		"audit:dispatched", "execute", "audit:result",
		"audit:result",
	}
	if got := env.Trace[seeded:]; !equalSeq(got, want) {
		t.Fatalf("恢复路径的请求次序不对：\n  实际 %v\n  期望 %v", got, want)
	}
}

// 审批人角色不匹配 → 不执行、pending 不动。
func TestResumeRejectsWrongApprover(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r2")

	err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:alice", []string{"role:warehouse_clerk"}))

	if err == nil {
		t.Fatalf("角色不匹配必须报错")
	}
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("不许执行")
	}
	got, ok, _ := env.Store.Load(p.PlanID)
	if !ok || got.State != StateAwaitingApproval {
		t.Fatalf("pending 必须保持待批：ok=%v state=%q", ok, got.State)
	}
	// 也不许写计划级 result 段：那条 dispatched/result 的 pair 要留给之后真正批的人。
	if rec := env.PlanResultAudit(p.DispatchedAuditID); rec != nil {
		t.Fatalf("审批人校验失败不该写终态 result 段：%v", rec)
	}
}

// 过期 → 归档 expired、零执行，并把计划级 result 段收尾（spec §5.3）。
func TestResumeExpiredDoesNotExecute(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPendingExpired(t, "pln_r3")

	err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))

	if err == nil {
		t.Fatalf("过期必须报错")
	}
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("不许执行")
	}
	if st := env.ArchivedState(p.PlanID); st != StateExpired {
		t.Fatalf("应归档 expired，得到 %q", st)
	}
	rec := env.PlanResultAudit(p.DispatchedAuditID)
	if rec == nil {
		t.Fatalf("过期也要写计划级 result 段（否则 dispatched 永远停在 unknown）")
	}
	result, _ := rec["result"].(map[string]any)
	if result["status"] != auditStatusError || result["errorClass"] != "policy_denied" {
		t.Fatalf("过期段应为 error/policy_denied，得到 %v/%v", result["status"], result["errorClass"])
	}
}

// G4 失效（模块在恢复执行时判前置条件不成立）→ 不硬跑，旧 plan 归档 stale。
func TestResumeG4FailureArchivesStale(t *testing.T) {
	env := newResumeEnv(t)
	env.Module.AlwaysReject = "STOCK_INSUFFICIENT" // 审批期间库存被别人扣掉了：422
	p := env.SeedPending(t, "pln_r4")

	err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))

	if err == nil {
		t.Fatalf("G4 失效必须报错")
	}
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("G4 失效不许留下成功执行的动作")
	}
	if st := env.ArchivedState(p.PlanID); st != StateStale {
		t.Fatalf("应归档 stale，得到 %q", st)
	}
	// 不硬重试：业务拒绝只打一次调用，不重规划也不再试。
	if env.Module.WriteCalls != 1 {
		t.Fatalf("业务拒绝不该重试，实际写调用 %d 次", env.Module.WriteCalls)
	}
	// 错误信息必须说清是「恢复时前置条件失效」，而不是笼统的「执行失败」。
	if !strings.Contains(err.Error(), "前置条件失效") || !strings.Contains(err.Error(), "库存") {
		t.Fatalf("错误信息要写明恢复时前置条件失效（如库存被改）：%v", err)
	}
	rec := env.PlanResultAudit(p.DispatchedAuditID)
	if rec == nil {
		t.Fatalf("缺计划级 result 段")
	}
	if result, _ := rec["result"].(map[string]any); result["errorClass"] != "business_rejection" {
		t.Fatalf("G4 失效的计划级 errorClass 应为 business_rejection，得到 %v", result["errorClass"])
	}
	if by := env.ResultApprovedBy(p.DispatchedAuditID); by != "user:bob" {
		t.Fatalf("人批过的事实必须留痕，得到 %q", by)
	}
}

// 恢复时 policy 收紧 → 不执行，但计划级 result 段仍记 approvedBy。
func TestResumePolicyTightenedStillRecordsApprover(t *testing.T) {
	env := newResumeEnv(t)
	env.Host.PolicyOutcome = "deny" // 挂起时是 approve，恢复时策略已收紧
	p := env.SeedPending(t, "pln_r5")
	seeded := len(env.Trace)

	if err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"})); err == nil {
		t.Fatalf("策略收紧必须报错")
	}

	if len(env.ExecutedActions()) != 0 || env.Module.WriteCalls != 0 || env.Module.ExecuteCalls != 0 {
		t.Fatalf("policy 收紧不许执行：write=%d execute=%d", env.Module.WriteCalls, env.Module.ExecuteCalls)
	}
	if by := env.ResultApprovedBy(p.DispatchedAuditID); by != "user:bob" {
		t.Fatalf("人批过的事实必须留痕，得到 %q", by)
	}
	rec := env.PlanResultAudit(p.DispatchedAuditID)
	if rec == nil {
		t.Fatalf("缺计划级 result 段")
	}
	result, _ := rec["result"].(map[string]any)
	if result["status"] != auditStatusError || result["errorClass"] != "policy_denied" {
		t.Fatalf("收紧段应为 error/policy_denied，得到 %v/%v", result["status"], result["errorClass"])
	}
	if st := env.ArchivedState(p.PlanID); st != StateStale {
		t.Fatalf("应归档 stale，得到 %q", st)
	}
	// 收紧即停：授权与预演都不该再跑（fail-closed 要早），只留一条收尾的 result 审计。
	want := []string{"policies:evaluate", "audit:result"}
	if got := env.Trace[seeded:]; !equalSeq(got, want) {
		t.Fatalf("policy 收紧之后不许再碰其它闸门：实际 %v，期望 %v", got, want)
	}
}

// 同一 confirm 重放 → 第二次返回既有结局，不重复执行、不重复写审计。
func TestResumeIdempotentOnReplay(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r6")
	cf := confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"})

	if err := env.Resume(t, cf); err != nil {
		t.Fatalf("首次恢复失败：%v", err)
	}
	audits := len(env.Host.AuditRecords)
	if err := env.Resume(t, cf); err != nil {
		t.Fatalf("重放应返回既有结局而不是报错：%v", err)
	}

	if n := len(env.ExecutedActions()); n != 1 {
		t.Fatalf("副作用必须只发生一次，实际执行 %d 次", n)
	}
	if env.Module.WriteCalls != 1 {
		t.Fatalf("幂等键没了？实际写调用 %d 次", env.Module.WriteCalls)
	}
	if got := len(env.Host.AuditRecords); got != audits {
		t.Fatalf("重放不该再写审计：%d → %d 条", audits, got)
	}
}

// 宿主不可达是**瞬时**故障：不许归档（那等于把一张还在有效期内的待批件弄丢），
// 也不许执行；错误必须标成可重试，下一 tick 会重放同一帧。
func TestResumeTransientFailureKeepsPending(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r9")

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	deps := env.Deps()
	deps.Host = deadURL
	err := Resume(context.Background(), deps, env.Store,
		confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))

	if err == nil {
		t.Fatalf("宿主不可达必须报错")
	}
	var re resumeError
	if !errors.As(err, &re) || !re.Retryable {
		t.Fatalf("宿主不可达必须标成可重试（否则这一帧会被当成确定性拒绝丢掉）：%v", err)
	}
	if _, ok, _ := env.Store.Load(p.PlanID); !ok {
		t.Fatalf("瞬时故障不许归档：pending 必须留着等下一 tick")
	}
	if st := env.ArchivedState(p.PlanID); st != "" {
		t.Fatalf("瞬时故障不许归档，得到 %q", st)
	}
	if len(env.ExecutedActions()) != 0 || env.Module.WriteCalls != 0 {
		t.Fatalf("宿主不可达时不许执行，实际写调用 %d 次", env.Module.WriteCalls)
	}
}

// 多步计划里后一步被业务拒绝：**部分执行**是真实状态，不能写成整体失败，
// 也不能假装什么都没发生——归档 stale，但审计要标 partially_failed、错误要说清落地了几步。
func TestResumePartialExecutionArchivesStale(t *testing.T) {
	env := newResumeEnv(t)
	// 第二个模块在审批期间「坏了」：它的动作一律业务拒绝（例如库存已被占掉）。
	broken := newFakeModule(t, "stockcheck", []string{"stockcheck.Reserve"}, &env.Trace)
	broken.AlwaysReject = "STOCK_INSUFFICIENT"
	env.Host.ExtraModules = []moduleEntry{{ID: "stockcheck", BaseURL: broken.URL, Enabled: true}}

	p := env.seedPending(t, "pln_r10", resumeExpiresFuture,
		testStep{id: "s1", action: "inventory.DeductStock"},
		testStep{id: "s2", action: "stockcheck.Reserve", dependsOn: `["s1"]`},
	)

	err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))

	if err == nil {
		t.Fatalf("后一步业务拒绝必须报错")
	}
	if got := env.ExecutedActions(); len(got) != 1 || got[0] != "inventory.DeductStock" {
		t.Fatalf("只有第一步该成功落地，得到 %v", got)
	}
	if st := env.ArchivedState(p.PlanID); st != StateStale {
		t.Fatalf("应归档 stale，得到 %q", st)
	}
	rec := env.PlanResultAudit(p.DispatchedAuditID)
	if rec == nil {
		t.Fatalf("缺计划级 result 段")
	}
	if result, _ := rec["result"].(map[string]any); result["status"] != auditStatusPartiallyFailed {
		t.Fatalf("部分执行必须记 partially_failed（写成 error 会让对账少算已落地的那步），得到 %v",
			result["status"])
	}
	if !strings.Contains(err.Error(), "已执行 1/2 步") {
		t.Fatalf("错误信息要说清已落地几步：%v", err)
	}
}

// reject → 归档 rejected、零执行，也不写计划级 result 段（终态覆盖由 Task 9 补）。
func TestResumeRejectArchivesRejected(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r7")

	if err := env.Resume(t, confirmFrame(p.PlanID, "reject", "user:bob", []string{"role:warehouse_manager"})); err != nil {
		t.Fatalf("拒绝应当成功归档而不是报错：%v", err)
	}
	if len(env.ExecutedActions()) != 0 || env.Module.WriteCalls != 0 {
		t.Fatalf("被拒绝的计划不许执行")
	}
	if st := env.ArchivedState(p.PlanID); st != StateRejected {
		t.Fatalf("应归档 rejected，得到 %q", st)
	}
	if rec := env.PlanResultAudit(p.DispatchedAuditID); rec != nil {
		t.Fatalf("reject 分支不写 result 段（本任务不推公告板、不收尾），却写了一条：%v", rec)
	}
}

// 不认识的处置绝不按 approve 处理（与 Step 0 同一口径的 fail-closed）。
func TestResumeUnknownDecisionDoesNotExecute(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r8")

	err := env.Resume(t, confirmFrame(p.PlanID, "maybe", "user:bob", []string{"role:warehouse_manager"}))

	if err == nil {
		t.Fatalf("未知处置必须报错")
	}
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("未知处置不许执行")
	}
	got, ok, _ := env.Store.Load(p.PlanID)
	if !ok || got.State != StateAwaitingApproval {
		t.Fatalf("未知处置不该动 pending：ok=%v state=%q", ok, got.State)
	}
	if st := env.ArchivedState(p.PlanID); st != "" {
		t.Fatalf("未知处置不该归档，得到 %q", st)
	}
}
