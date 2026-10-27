// run.go 实现 agent 的执行循环：一份过了 schema 的 Plan 要按固定顺序过闸门、留下两段
// 审计，才允许产生副作用。
//
// 顺序**不可调换**（TestGateOrder 锁定）：
//
//	1 规划（Plan 过 plan.schema.json）
//	2 policies:evaluate   → deny 直接转人工（不得换路绕过）；approve 只记下「需要审批」，
//	  继续跑 3、4，最后才挂起。授权与预演必须早于「决定挂起」：一份注定过不了授权/
//	  预演的计划，不该挂上公告板等人白批一次（fail-closed 要早）。没有挂起通道
//	  （Deps.State 为 nil）时 approve 仍转人工——这是 M0.5 行为的保留路径
//	3 grants:evaluate     → 逐 action 全部先过（不允许即转人工）
//	4 每个 write 的 :dry-run（**404 也转人工**：模块不实现预演不等于免检；
//	  结论不是 verified 同样转人工——fail-closed，见设计文档 §8 降级矩阵）
//	5 需要审批 → 挂起（I7 三次落盘；结局仍是 needs_human + PendingPlanID，
//	  三态封闭不新增第四态）。否则每步执行前写 stage=dispatched 审计
//	  （非 200 立即终止，本次调用数必须为 0）
//	6 执行（write 带 Idempotency-Key = protocol.IdempotencyKey(planId, stepId)）
//	7 写 stage=result 审计
//	8 按错误分类处理：retryable 退避重试（幂等键不变）；business_rejection 重新规划；
//	  policy_denied 立即转人工（不换路）；其余当 bug 失败
//	9 发 Intent 帧（open + highlight；没有前端接入时帧丢失而业务结果不变）
//
// 三条 fail-closed 规则贯穿全文件：
//   - 宿主不可达 → 不做任何写，结局 failed（宁可不做，也不要做出没有审计的写）；
//   - dispatched 审计写失败 → 拒绝执行（§5「dispatched 审计写失败 = 不执行」）；
//   - 结局只有三种：completed / needs_human / failed，都带人能读懂的原因。
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"tenon.dev/internal/protocol"
)

const (
	// maxReplans 是 business_rejection 触发的**重新规划**上限：即最多 1 次初始规划 +
	// 2 次重规划。它是「撞同一面墙」的止损，不是重试次数。
	maxReplans = 2
	// maxRetries 是 retryable 的退避重试上限：同一步最多执行 1+maxRetries 次，
	// 每次都用同一个幂等键，所以重复执行不会产生重复副作用（semantics.md §5）。
	maxRetries = 3
	// retryBaseDelay 是退避的基数：第 n 次重试等 base<<(n-1)。
	// 50ms 量级足够让瞬时故障过去，也让单测总时长保持在毫秒级。
	retryBaseDelay = 50 * time.Millisecond
	// defaultHTTPTimeout 是没有注入 http.Client 时的兜底超时。模块侧动作的
	// timeoutMs 缺省是 30s（internal/module），所以这里必须比它长。
	defaultHTTPTimeout = 60 * time.Second
	// maxResponseBytes 是响应体上限（模块与宿主的响应都是 KB 量级）。
	maxResponseBytes = 1 << 20
	// snippetLimit 是错误信息里保留的响应体字符数。
	snippetLimit = 300
	// badgeLimit 是 Intent 帧 badge 的字符上限（frames.schema.json：maxLength 120）。
	badgeLimit = 120

	// 以下取值来自 spec/0.1 的 schema（policy / plan / frames），本文件只引用字符串，
	// 不复制它们的语义定义。
	outcomeAuto     = "auto"     // policy.schema.json 的 outcome：可自动执行
	outcomeApprove  = "approve"  // policy.schema.json 的 outcome：等人工批准，agent 只能挂起
	outcomeDeny     = "deny"     // policy.schema.json 的 outcome：转人工，不得换路
	kindRead        = "read"     // plan.schema.json 的 kind
	kindWrite       = "write"    // plan.schema.json 的 kind
	dryRunVerified  = "verified" // plan.schema.json 的 DryRunStatus：唯一允许自动执行的值
	frameTypeIntent = "tenon.intent"
	// auditStage* 与 internal/controlplane/audit 的两段写一致（dispatched / result）。
	auditStageDispatched = "dispatched"
	auditStageResult     = "result"
)

// Outcome 是一个回合的结局。三态封闭：多一态就要有人解释「它该不该转人工」。
type Outcome string

const (
	// OutcomeCompleted 表示计划全部步骤成功执行。
	OutcomeCompleted Outcome = "completed"
	// OutcomeNeedsHuman 表示需要人接手：策略/授权未放行、预演不安全、被业务拒绝到
	// 重规划上限、或能力不足。它不是错误，是「agent 正确地举手」。
	OutcomeNeedsHuman Outcome = "needs_human"
	// OutcomeFailed 表示本轮崩了：模型输出坏了、宿主/模块不可达、审计写不进去、
	// 契约违反。转人工与失败必须分开——混在一起排查方向就错了。
	OutcomeFailed Outcome = "failed"
)

// RunInput 是一次回合的输入。
//
// CorrelationID 可省略（空串时用事件自带的）：它只影响审计与 Intent 帧的业务链锚点，
// 不影响闸门判定。
//
// Request 是**人工发起**时的原话（tenon.request 帧），事件回合为 nil。它与 Event 的
// 语义不同：Event 是领域事实，Request 是**指令**，所以两者必须分块进 prompt
// （planner.userPrompt 负责分流）。塞一个非 nil 的空 UserRequest，事件回合的 prompt
// 就会凭空多出一个空指令块——判空要看 nil，不要看 Text 是否为空。
type RunInput struct {
	Event         DomainEvent
	CorrelationID string
	Request       *UserRequest
}

// RunResult 是一个回合的结局。ActionsTaken 只记**真正执行过**的动作（按执行顺序），
// 被闸门挡住、或规划出来却没轮到的步骤不在里面。
type RunResult struct {
	Outcome      Outcome
	PlanID       string
	ActionsTaken []string
	Detail       string
	// PendingPlanID 非 nil 表示有一份 pending 已落盘、可被推进（approve 命中的挂起）。
	//
	// 结局仍是 needs_human——三态封闭，不新增第四态（run.go 的 Outcome 注释写死）：
	// 「等审批」本来就是「agent 正确地举手」，只是这次举手带了一张可被批准的卡。
	PendingPlanID *string
}

// Deps 是 Run 的全部外部依赖。
//
// Trace 是调用轨迹的收集器：测试桩在收到对应请求时把闸门名写进来，于是断言的是
// **真实请求顺序**，而不是运行时对自我调用的声明。生产留空即可（nil 表示不收集）。
type Deps struct {
	Host       string // 宿主基地址，例如 http://127.0.0.1:8080（不带尾斜杠亦可）
	AgentID    string // 审计 actor.id，例如 ag_tenon
	OnBehalfOf string // 代行委托人，例如 user:alice（审计 actor.onBehalfOf 与授权评估都用它）
	Planner    *Planner
	HTTP       *http.Client
	Skills     *SkillStore
	Trace      *[]string
	// State 是 pending 的持久化口；为 nil 表示「没有挂起通道」，approve 一律转人工。
	//
	// 用 nil 而不是空串表达「没有」：NewStateStore("") 会报错（§0.1.6），
	// 装配时把空串传进来只会在第一轮挂起时炸掉，而不是在装配时炸掉。
	State *StateStore
}

// Run 跑完一个回合并返回结局。它**不返回 error**：三种结局已经涵盖了「要不要人来」
// 与「是不是崩了」，再叠一层 error 只会让调用方两处判断同一件事。
func Run(ctx context.Context, deps Deps, in RunInput) RunResult {
	if deps.Planner == nil {
		return RunResult{Outcome: OutcomeFailed, Detail: "未配置规划器：不规划就不可能安全执行"}
	}
	event := in.Event
	if strings.TrimSpace(event.EventID) == "" || strings.TrimSpace(event.Type) == "" {
		return RunResult{Outcome: OutcomeFailed, Detail: "事件缺少 eventId 或 type：没有触发源锚点，不开回合"}
	}
	host := strings.TrimSpace(deps.Host)
	if host == "" {
		return RunResult{Outcome: OutcomeFailed, Detail: "未配置宿主地址（fail-closed：不做任何写）"}
	}
	correlation := strings.TrimSpace(in.CorrelationID)
	if correlation == "" {
		correlation = strings.TrimSpace(event.CorrelationID)
	}
	r := &runner{deps: deps, host: strings.TrimRight(host, "/"), event: event,
		request: in.Request, correlationID: correlation}
	r.client = deps.HTTP
	if r.client == nil {
		r.client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return r.run(ctx)
}

// runner 是一个回合内的可变状态。它只在单次 Run 的调用栈里使用，不跨回合共享。
type runner struct {
	deps          Deps
	host          string
	event         DomainEvent
	correlationID string
	client        *http.Client
	// request  是人工发起时的原话；事件回合为 nil（挂起时决定 origin 与 requestId）。
	request *UserRequest
	// modules 是「enabled 且 manifest 可达」的模块 → 基地址；执行与预演都只认它。
	modules map[string]string
	taken   []string // 真正执行过的动作
	planID  string   // 当前（或最近一次被接受的）计划标识，结局与 Intent 帧都用它
}

// run 是外层循环：能力发现 → （规划 → 闸门 → 执行）最多 1+maxReplans 轮。
func (r *runner) run(ctx context.Context) RunResult {
	digest, err := r.discover(ctx)
	if err != nil {
		return r.finish(ctx, RunResult{Outcome: OutcomeFailed,
			Detail: "能力发现失败（fail-closed，不做任何写）：" + err.Error()})
	}
	if len(digest) == 0 {
		return r.finish(ctx, RunResult{Outcome: OutcomeNeedsHuman,
			Detail: "可达模块为 0：没有任何可执行能力，转人工"})
	}

	// skill 正文按回合类型分流（Task 5 复核后追加的三条）：
	//   - request 回合：SelectSkills 做确定性粗筛，TextForSelected 渲染候选集；
	//   - 事件回合：仍按 tenon.subscribes 取 TextFor(event.Type)，一个字都不改。
	skillText := ""
	if r.deps.Skills.Len() == 0 {
		// SelectSkills 不返回 error，区分不出「压根没加载 skill」与「加载了但没命中」，
		// 所以 0 份必须单独打出来：那是部署问题（永远没有业务语义参考），不是「这次没命中」。
		log.Printf("agent: 没有加载任何 skill（Skills 为 0 份）：本次回合没有 skill 正文，只能靠事件与动作清单")
	} else if r.request != nil {
		// viewModule 传空串：**请求帧里没有视图模块**（UserRequest 只有 requestId/text/by，
		// 本任务不为此加字段）。没有视图模块时，粗筛只看请求文本。
		selected := SelectSkills(r.deps.Skills.All(), r.request.Text, "", 2)
		skillText = r.deps.Skills.TextForSelected(selected)
	} else {
		skillText = r.deps.Skills.TextFor(r.event.Type)
	}

	feedback := ""
	for round := 0; round <= maxReplans; round++ {
		res, rejection := r.round(ctx, PlanInput{
			Event:     r.event,
			SkillText: skillText,
			Modules:   digest,
			Feedback:  feedback,
			// 事件回合这里是 nil：prompt 不得凭空多出一个空指令块（Task 6 的判据）。
			Request: r.request,
		})
		if rejection == "" {
			return r.finish(ctx, res)
		}
		feedback = rejection
	}
	return r.finish(ctx, RunResult{
		Outcome: OutcomeNeedsHuman,
		Detail: fmt.Sprintf("business_rejection 重新规划 %d 轮后仍未通过（1 次初始 + %d 次重规划），转人工：%s",
			maxReplans, maxReplans, feedback),
	})
}

// finish 补齐结局里的公共字段并发出 Intent 帧。
//
// 帧发送失败**不改结局**：没有前端接入时帧本就该丢弃，业务结果不受影响
// （frames.schema.json 的说明与设计 §3.2 决定 1），但失败原因要写进 Detail。
func (r *runner) finish(ctx context.Context, res RunResult) RunResult {
	if res.PlanID == "" {
		res.PlanID = r.planID
	}
	res.ActionsTaken = append([]string(nil), r.taken...)
	if note := r.sendIntent(ctx, res); note != "" {
		res.Detail = strings.TrimSpace(res.Detail + "；" + note)
	}
	return res
}

// round 是「规划 → 闸门 → 执行」的一轮。
//
// 返回第二值非空表示这一轮被 business_rejection 拒绝，需要带着这条原因重新规划；
// 其余结局都由第一值表达。
func (r *runner) round(ctx context.Context, in PlanInput) (RunResult, string) {
	plan, err := r.deps.Planner.Plan(ctx, in)
	if err != nil {
		var gap *capabilityGapError
		if errors.As(err, &gap) {
			// 能力不足不是崩溃：模型要求了清单里没有的动作，人需要知道缺什么。
			r.planID = gap.PlanID
			return RunResult{Outcome: OutcomeNeedsHuman, PlanID: gap.PlanID,
				Detail: "能力不足，转人工（模型要求的动作不在动态清单里）：" + err.Error()}, ""
		}
		return RunResult{Outcome: OutcomeFailed,
			Detail: "规划失败（" + planSchemaName + " 是判据）：" + err.Error()}, ""
	}

	// trigger 校正为真实触发源：它是审计与对账的锚点，模型抄错不该让整轮失败。
	plan.Trigger = PlanTrigger{EventID: r.event.EventID, CorrelationID: r.correlationID}
	r.planID = plan.PlanID

	// 闸门 2：策略。整份计划取最严（宿主实现）。
	//
	// deny 直接转人工；approve 只记下「需要审批」并继续往下走——授权与预演必须跑在
	// 「决定挂起」之前（fail-closed 要早：注定过不了授权/预演的计划不该挂上公告板
	// 等人白批一次），最后才由闸门 5 决定「挂起还是执行」。
	policy, err := r.evaluatePolicies(ctx, plan)
	if err != nil {
		return RunResult{Outcome: OutcomeFailed,
			Detail: "策略评估不可用（fail-closed，不做任何写）：" + err.Error()}, ""
	}
	if policy.Outcome == outcomeDeny {
		return RunResult{Outcome: OutcomeNeedsHuman,
			Detail: fmt.Sprintf("策略闸门拒绝：outcome=%s decidedBy=%v reasons=%v（转人工，不换路绕过）",
				policy.Outcome, policy.DecidedBy, policy.Reasons)}, ""
	}
	// policy.schema.json 是封闭枚举，但宿主的响应不受本包控制：除 auto/approve 之外的
	// 任何取值（含空串）都按「没放行」处理——不猜它是不是 approve，也不让它掉进执行。
	needApproval := policy.Outcome == outcomeApprove
	if policy.Outcome != outcomeAuto && !needApproval {
		return RunResult{Outcome: OutcomeNeedsHuman,
			Detail: fmt.Sprintf("策略闸门未放行：outcome=%s decidedBy=%v reasons=%v（转人工，不换路绕过）",
				policy.Outcome, policy.DecidedBy, policy.Reasons)}, ""
	}

	// 闸门 3：授权。逐 action 全部先过——先执行几个再撞授权拒绝，等于留下半截副作用。
	grantIDs := make(map[string]string, len(plan.Steps))
	for _, step := range plan.Steps {
		grant, err := r.evaluateGrant(ctx, step)
		if err != nil {
			return RunResult{Outcome: OutcomeFailed,
				Detail: "授权评估不可用（fail-closed，不做任何写）：" + err.Error()}, ""
		}
		if !grant.Allowed {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("授权闸门拒绝动作 %s：reason=%s（转人工，不换路绕过）",
					step.Action, grant.Reason)}, ""
		}
		grantIDs[step.StepID] = grant.MatchedGrantID
	}

	// 闸门 4：写动作的预演。**404 也算不通过**（fail-closed）——这条按设计文档 §8
	// 降级矩阵执行：「无 dry-run（unsupported）或 unsafe → 强制审批」。
	// 早期实现曾把 404 当「忽略并继续」，那等于「模块不实现预演就免检」，
	// 供应商只要删掉端点就能绕过预演；这里改回强制转人工。
	for _, step := range plan.Steps {
		if step.Kind != kindWrite {
			continue
		}
		status, found, err := r.dryRun(ctx, step)
		if err != nil {
			return RunResult{Outcome: OutcomeFailed,
				Detail: "预演失败（fail-closed，不做任何写）：" + err.Error()}, ""
		}
		if !found {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("动作 %s 的模块未实现预演（404）：按降级矩阵强制转人工，"+
					"不能把「没实现端点」当成「免检」", step.Action)}, ""
		}
		if status != dryRunVerified {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("预演结论为 %q（只有 %s 允许自动执行）：动作 %s，转人工",
					status, dryRunVerified, step.Action)}, ""
		}
	}

	// 闸门 5：需要审批 → 挂起（I7 三次落盘），一步都不执行。
	//
	// 挂起不是第四种结局：needs_human + PendingPlanID 表达「等人按一下就能继续」。
	// 挂起失败映射成 needs_human（不是 failed）：这一轮确实什么都没做、也不该重试——
	// 同一份 pending 再调一次 suspend 会复用同一个 auditId，审计链出现重复记录。
	if needApproval {
		if r.deps.State == nil {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("策略闸门要求审批（outcome=%s decidedBy=%v），但没有挂起通道（Deps.State 为空）：转人工",
					policy.Outcome, policy.DecidedBy)}, ""
		}
		pending, err := r.buildPending(plan, policy)
		if err != nil {
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: err.Error()}, ""
		}
		if err := suspend(ctx, r.deps.State, r.deps, pending); err != nil {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: "挂起失败（不留半截 pending）：" + err.Error()}, ""
		}
		id := pending.PlanID
		return RunResult{Outcome: OutcomeNeedsHuman, PlanID: id, PendingPlanID: &id,
			Detail: fmt.Sprintf("已挂起等待审批：planId=%s 需要 %v", id, pending.RequiredRoles)}, ""
	}

	// 闸门 5–7：按依赖顺序执行，每步各写两段审计。
	ordered, err := orderSteps(plan.Steps)
	if err != nil {
		return RunResult{Outcome: OutcomeFailed, Detail: "计划的 steps 无法定序：" + err.Error()}, ""
	}
	for _, step := range ordered {
		gate := gateDecision{
			outcome:  policy.Outcome,
			policyID: firstNonEmpty(policy.DecidedBy...),
			grantID:  grantIDs[step.StepID],
		}
		verdict := r.executeStep(ctx, plan, step, gate)
		switch verdict.kind {
		case verdictOK:
			r.taken = append(r.taken, step.Action)
		case verdictBusinessRejection:
			// 重新规划：不在这里判上限，上限由外层循环持有。
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: verdict.detail}, verdict.feedback
		case verdictPolicyDenied:
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: verdict.detail}, ""
		case verdictRetryableExhausted:
			return RunResult{Outcome: OutcomeFailed, Detail: verdict.detail}, ""
		default:
			return RunResult{Outcome: OutcomeFailed, Detail: verdict.detail}, ""
		}
	}
	return RunResult{Outcome: OutcomeCompleted,
		Detail: fmt.Sprintf("计划全部执行成功：%d 步", len(ordered))}, ""
}

// buildPending 把 plan 与本轮的闸门结论打包成一份 pending。
//
// labels 在这里渲染并落盘，恢复时不重算：白话来自身份化模板，模板改了两边文字就会
// 不一致（spec §3 的 pending 契约）。CreatedAt/ExpiresAt 必须在这里填——suspend 只管
// I7 的三次落盘，不碰时间；漏填 ExpiresAt 会让 Expired() 对空串 fail-closed 返回 true，
// 下一轮自检立刻把这份 pending 归档成 expired（§0.1.7）。
//
// requestId 可为 null（事件回合）：spec §6.3 要求「等审批」只有一条实现路径，不分来源，
// 所以事件回合也挂起，只是 origin=event。
func (r *runner) buildPending(plan Plan, policy policyEvaluation) (Pending, error) {
	// 挂起前必须确认「谁能批」：rolesIntersect 对空集 fail-closed，requiredRoles 为空的
	// approve 计划**谁点都是 403**。挂上去只会得到一张永远批不掉的卡——它占着公告板、
	// 占着一个审批窗口，而审批人点下去只换来一次 403。这不是可恢复状态（换个人批也一样），
	// 所以要当场转人工，而不是等人白点一次才发现。
	//
	// 空白条目同样不算审批人：["  "] 与任何角色都交不出交集，效果与空集相同。
	if !hasApproverRole(policy.Approvers) {
		return Pending{}, fmt.Errorf(
			"策略要求审批但没有给审批人角色（requiredRoles 为空，谁批都会 403）：不挂起，转人工")
	}
	labels, err := renderLabels(plan)
	if err != nil {
		return Pending{}, fmt.Errorf("渲染白话失败：%w", err)
	}
	now := time.Now().UTC()
	var reqID *string
	if r.request != nil && r.request.RequestID != "" {
		id := r.request.RequestID
		reqID = &id
	}
	origin := "event"
	if r.request != nil {
		origin = "request"
	}
	return Pending{
		PlanID:         plan.PlanID,
		State:          StateAwaitingApproval,
		RequestID:      reqID,
		TriggerEventID: plan.Trigger.EventID,
		Origin:         origin,
		By:             r.deps.OnBehalfOf,
		Plan:           plan,
		Labels:         labels,
		Summary:        strings.Join(labels, "；"),
		RequiredRoles:  policy.Approvers,
		CreatedAt:      now.Format(time.RFC3339),
		ExpiresAt:      now.Add(pendingTTL).Format(time.RFC3339),
		// subject 必须落盘：恢复时 TriggerEventID 只够锚定事件、锚不住「哪个实体」，
		// 丢了它，恢复执行的审计 effects.entities 与 Intent 帧 actions 就退化成空数组。
		Subject: r.event.Subject,
	}, nil
}

// hasApproverRole 判断策略是否真的给了审批人角色（空白条目不算）。
func hasApproverRole(roles []string) bool {
	for _, role := range roles {
		if strings.TrimSpace(role) != "" {
			return true
		}
	}
	return false
}

// ---------- 能力发现 ----------

// moduleEntry 是 GET /tenon/modules 的一行。注意它是**管理面视图**，含 enabled=false。
type moduleEntry struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Enabled bool   `json:"enabled"`
}

// discover 拉注册表并逐个模块读 manifest，只保留 enabled 的模块。
//
// enabled 过滤必须由 agent 自己做：宿主返回的是管理面视图（含已摘除条目），
// 而 D1 的 B 轮正是靠「摘掉 procurement」让同一事件的计划必须改变。
func (r *runner) discover(ctx context.Context) ([]ModuleDigest, error) {
	status, raw, err := r.getJSON(ctx, r.host+"/tenon/modules")
	if err != nil {
		return nil, fmt.Errorf("GET /tenon/modules 失败：%w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /tenon/modules 返回 %d：%s", status, snippet(raw))
	}
	var resp struct {
		Modules []moduleEntry `json:"modules"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("GET /tenon/modules 的响应不是合法 JSON：%w", err)
	}

	r.modules = make(map[string]string, len(resp.Modules))
	out := make([]ModuleDigest, 0, len(resp.Modules))
	for _, m := range resp.Modules {
		id := strings.TrimSpace(m.ID)
		base := strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
		if !m.Enabled || id == "" || base == "" {
			continue
		}
		actions, err := r.manifestActions(ctx, base)
		if err != nil {
			// 模块在场却读不到 manifest：能力清单不完整，不能假装它没有动作。
			return nil, fmt.Errorf("模块 %q 的能力发现失败：%w", id, err)
		}
		r.modules[id] = base
		out = append(out, ModuleDigest{ID: id, Actions: actions})
	}
	return out, nil
}

// manifestActions 从模块的 GET /.well-known/tenon 取动作 id 清单（摘要级，不含 schema）。
func (r *runner) manifestActions(ctx context.Context, baseURL string) ([]string, error) {
	status, raw, err := r.getJSON(ctx, baseURL+"/.well-known/tenon")
	if err != nil {
		return nil, fmt.Errorf("GET /.well-known/tenon 失败：%w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /.well-known/tenon 返回 %d：%s", status, snippet(raw))
	}
	var manifest struct {
		Actions []struct {
			ID string `json:"id"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("manifest 不是合法 JSON：%w", err)
	}
	ids := make([]string, 0, len(manifest.Actions))
	for _, a := range manifest.Actions {
		if id := strings.TrimSpace(a.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// moduleBase 从 action id（<moduleId>.<ActionName>）反查模块基地址。
func (r *runner) moduleBase(action string) (string, bool) {
	module, _, ok := strings.Cut(strings.TrimSpace(action), ".")
	if !ok {
		return "", false
	}
	base, ok := r.modules[module]
	return base, ok && base != ""
}

// ---------- 闸门 ----------

// policyEvaluation 是 POST /tenon/policies:evaluate 的响应（policy.schema.json）。
type policyEvaluation struct {
	Outcome   string   `json:"outcome"`
	DecidedBy []string `json:"decidedBy"`
	Approvers []string `json:"approvers"`
	Reasons   []string `json:"reasons"`
}

// evaluatePolicies 交整份计划去求值：宿主的粒度是「整份 plan 取最严」，
// 所以在 agent 侧逐个 step 求值再自己合并是另一种语义，不能替代它。
func (r *runner) evaluatePolicies(ctx context.Context, plan Plan) (policyEvaluation, error) {
	status, raw, err := r.postJSON(ctx, r.host+"/tenon/policies:evaluate", map[string]any{"plan": plan}, nil)
	if err != nil {
		return policyEvaluation{}, fmt.Errorf("POST /tenon/policies:evaluate 失败：%w", err)
	}
	if status != http.StatusOK {
		return policyEvaluation{}, fmt.Errorf("POST /tenon/policies:evaluate 返回 %d：%s", status, snippet(raw))
	}
	var out policyEvaluation
	if err := json.Unmarshal(raw, &out); err != nil {
		return policyEvaluation{}, fmt.Errorf("policies:evaluate 的响应不是合法 JSON：%w", err)
	}
	return out, nil
}

// grantEvaluation 是 POST /tenon/grants:evaluate 的响应（grant.schema.json 的子集）。
type grantEvaluation struct {
	Allowed        bool   `json:"allowed"`
	MatchedGrantID string `json:"matchedGrantId"`
	Reason         string `json:"reason"`
}

// evaluateGrant 对单个 action 求授权。503（引擎读不懂授权数据）按错误处理：
// 那不是一次「拒绝判定」，而是「这道闸门当前不可用」，只能 fail-closed。
func (r *runner) evaluateGrant(ctx context.Context, step PlanStep) (grantEvaluation, error) {
	body := map[string]any{
		"agent":      r.deps.AgentID,
		"onBehalfOf": r.deps.OnBehalfOf,
		"action":     step.Action,
		"args":       rawOrEmptyObject(step.Args),
	}
	status, raw, err := r.postJSON(ctx, r.host+"/tenon/grants:evaluate", body, nil)
	if err != nil {
		return grantEvaluation{}, fmt.Errorf("POST /tenon/grants:evaluate 失败：%w", err)
	}
	if status != http.StatusOK {
		return grantEvaluation{}, fmt.Errorf("POST /tenon/grants:evaluate 返回 %d：%s", status, snippet(raw))
	}
	var out grantEvaluation
	if err := json.Unmarshal(raw, &out); err != nil {
		return grantEvaluation{}, fmt.Errorf("grants:evaluate 的响应不是合法 JSON：%w", err)
	}
	return out, nil
}

// dryRun 对一个写动作做预演。返回值 found=false 表示模块没有该端点（404），
// 按任务约定忽略；found=true 时 status 是模块给的 DryRunStatus。
//
// 响应形状按 interfaces.md §3.3 是**包裹**过的：{"dryRun": {"status": ..., "diff": [...]}}，
// 不是裸的 DryRunReport——把两者当成同一种形状，就会把「预演通过」读成「没有 status」。
func (r *runner) dryRun(ctx context.Context, step PlanStep) (status string, found bool, err error) {
	base, ok := r.moduleBase(step.Action)
	if !ok {
		return "", false, fmt.Errorf("动作 %q 所属模块不在可达清单里", step.Action)
	}
	url := base + "/tenon/actions/" + step.Action + ":dry-run"
	code, raw, err := r.postJSON(ctx, url, rawOrEmptyObject(step.Args), nil)
	if err != nil {
		return "", false, fmt.Errorf("POST %s 失败：%w", url, err)
	}
	if code == http.StatusNotFound {
		return "", false, nil
	}
	if code != http.StatusOK {
		return "", false, fmt.Errorf("POST %s 返回 %d：%s", url, code, snippet(raw))
	}
	var envelope struct {
		DryRun DryRunReport `json:"dryRun"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false, fmt.Errorf("预演响应不是合法 JSON：%w", err)
	}
	if strings.TrimSpace(envelope.DryRun.Status) == "" {
		return "", false, fmt.Errorf("预演响应缺少 dryRun.status（interfaces.md §3.3 的包裹形状）")
	}
	return envelope.DryRun.Status, true, nil
}

// ---------- 执行 ----------

// verdictKind 是单步执行的判定。
type verdictKind int

const (
	// verdictOK 表示这一步成功。
	verdictOK verdictKind = iota
	// verdictBusinessRejection 表示业务拒绝：需要带着原因重新规划。
	verdictBusinessRejection
	// verdictPolicyDenied 表示治理闸门拒绝：立即转人工，不得换路。
	verdictPolicyDenied
	// verdictRetryableExhausted 表示重试上限用尽：本轮失败。
	verdictRetryableExhausted
	// verdictFailed 表示其它失败（契约违反、审计写不进、结果未定等）。
	verdictFailed
)

// stepVerdict 是单步执行的结论。feedback 只在 business_rejection 时非空，
// 它是回填给下一轮规划的原始拒绝原因。
type stepVerdict struct {
	kind     verdictKind
	detail   string
	feedback string
}

// gateDecision 是这一步在闸门上得到的结论：审计的 decision 段逐字段取自它。
//
// outcome 是处置（auto/approve/deny）；policyID 是作出该处置的策略 id（来自
// policies:evaluate 的 decidedBy），grantID 是命中的授权。三者混用会让审计
// 「看起来有策略」却答不出是哪一条，这是我在自审时修掉的一处语义错。
type gateDecision struct {
	outcome  string
	policyID string
	grantID  string
}

// executeStep 执行一步：写 dispatched 审计 → 调用 → 写 result 审计，retryable 时退避重试。
//
// 幂等键在进入重试循环之前就定下来，**重试期间不变**：这是重试安全的前提
// （semantics.md §5：同一笔业务的重试必须复用同一个幂等键）。
func (r *runner) executeStep(ctx context.Context, plan Plan, step PlanStep, gate gateDecision) stepVerdict {
	base, ok := r.moduleBase(step.Action)
	if !ok {
		return stepVerdict{kind: verdictFailed, detail: fmt.Sprintf("动作 %q 所属模块不在可达清单里", step.Action)}
	}

	// 读动作没有幂等语义（schema：read 调用的 idempotencyKey 为 null），不带头也不记账。
	var idemKey string
	if step.Kind == kindWrite {
		idemKey = protocol.IdempotencyKey(plan.PlanID, step.StepID)
	}
	inputHash := hashPayload(step.Args)
	url := base + "/tenon/actions/" + step.Action
	attempts := 1 + maxRetries
	var last stepVerdict

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if !sleepCtx(ctx, retryBaseDelay<<(attempt-1)) {
				return stepVerdict{kind: verdictFailed, detail: "上下文在重试退避期间被取消"}
			}
		}
		auditID := newID("aud_")

		// 闸门 5：dispatched 审计必须先落盘。写失败 → 立即终止，绝不发出调用。
		dispatched := r.auditFor(plan, step, auditID, auditStageDispatched, gate, inputHash, idemKey)
		if err := r.postAudit(ctx, dispatched); err != nil {
			return stepVerdict{kind: verdictFailed,
				detail: "dispatched 审计写失败，拒绝执行（本步没有发出任何调用）：" + err.Error()}
		}

		// 闸门 6：执行。写动作带幂等键，读动作不带。
		headers := map[string]string{}
		if idemKey != "" {
			headers[protocol.HeaderIdempotencyKey] = idemKey
		}
		status, raw, err := r.postJSON(ctx, url, rawOrEmptyObject(step.Args), headers)
		if err != nil {
			// 传输层失败：写调用可能已经落地，结果未定。按 unknown 记账后当 retryable 处理
			// （重试用同一个幂等键，模块会回放首次结果而不是重复副作用）。
			_ = r.postAudit(ctx, r.resultAudit(plan, step, auditID, gate, inputHash, idemKey,
				"unknown", "", ""))
			last = stepVerdict{kind: verdictRetryableExhausted,
				detail: fmt.Sprintf("动作 %s 的调用结果未定（传输层失败，已重试 %d 次）：%v",
					step.Action, attempt, err)}
			continue
		}

		if status >= 200 && status <= 299 {
			if aerr := r.postAudit(ctx, r.resultAudit(plan, step, auditID, gate, inputHash, idemKey,
				"ok", hashPayload(raw), "")); aerr != nil {
				return stepVerdict{kind: verdictFailed,
					detail: "result 审计写失败（调用已执行，但审计链不完整）：" + aerr.Error()}
			}
			return stepVerdict{kind: verdictOK}
		}

		class, code, message := classifyFailure(status, raw)
		_ = r.postAudit(ctx, r.resultAudit(plan, step, auditID, gate, inputHash, idemKey,
			"error", "", string(class)))
		switch class {
		case protocol.ClassRetryable:
			last = stepVerdict{kind: verdictRetryableExhausted,
				detail: fmt.Sprintf("动作 %s 连续返回可重试错误（已重试 %d 次）：%s %s",
					step.Action, attempt, code, message)}
			continue
		case protocol.ClassBusinessRejection:
			return stepVerdict{
				kind:     verdictBusinessRejection,
				detail:   fmt.Sprintf("动作 %s 被业务拒绝：%s %s", step.Action, code, message),
				feedback: fmt.Sprintf("动作 %s 返回 business_rejection（%s）：%s", step.Action, code, message),
			}
		case protocol.ClassPolicyDenied:
			return stepVerdict{kind: verdictPolicyDenied,
				detail: fmt.Sprintf("动作 %s 被治理闸门拒绝：%s %s（转人工，不换路绕过）", step.Action, code, message)}
		default:
			return stepVerdict{kind: verdictFailed,
				detail: fmt.Sprintf("动作 %s 返回契约违反：%s %s（属 bug，本轮失败）", step.Action, code, message)}
		}
	}
	return last
}

// classifyFailure 把一次非 2xx 响应归到四类错误之一。
//
// 没有错误信封时按 HTTP 语义兜底：5xx 与 429 当瞬时故障（重试安全：幂等键不变），
// 其余当契约违反（属 bug）。未知的 class 取值同样当契约违反——不认识的分类不能
// 悄悄落到「可重试」或「转人工」上。
func classifyFailure(status int, body []byte) (protocol.ErrorClass, string, string) {
	var env protocol.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil {
		e := env.TenonError
		switch e.Class {
		case protocol.ClassRetryable, protocol.ClassBusinessRejection,
			protocol.ClassPolicyDenied, protocol.ClassContractViolation:
			return e.Class, e.Code, e.Message
		}
	}
	code := "HTTP_" + strconv.Itoa(status)
	switch {
	case status == http.StatusTooManyRequests, status >= 500:
		return protocol.ClassRetryable, code, snippet(body)
	default:
		return protocol.ClassContractViolation, code, snippet(body)
	}
}

// ---------- 审计 ----------

// auditRecord 是 POST /tenon/audit 的请求体。
//
// 字段与 audit-event.schema.json 的 required 一一对应（trigger/inputHash/idempotencyKey/
// tokenUsage 这四个曾被漏掉，漏了会写出不是 schema 完整事件的记录）。hash 与 prevHash
// 刻意不发：链的权威只有一个，就是宿主的审计存储。
type auditRecord struct {
	AuditID        string          `json:"auditId"`
	Stage          string          `json:"stage"`
	At             string          `json:"at"`
	Actor          auditActor      `json:"actor"`
	Trigger        auditTrigger    `json:"trigger"`
	PlanID         *string         `json:"planId"`
	StepID         *string         `json:"stepId"`
	Action         string          `json:"action"`
	InputHash      string          `json:"inputHash"`
	IdempotencyKey *string         `json:"idempotencyKey"`
	Decision       auditDecision   `json:"decision"`
	Result         auditResult     `json:"result"`
	Effects        auditEffects    `json:"effects"`
	TokenUsage     auditTokenUsage `json:"tokenUsage"`
}

type auditActor struct {
	Type       string  `json:"type"`
	ID         string  `json:"id"`
	OnBehalfOf *string `json:"onBehalfOf"`
}

type auditTrigger struct {
	EventID       *string `json:"eventId"`
	CorrelationID *string `json:"correlationId"`
}

type auditDecision struct {
	Outcome    string  `json:"outcome"`
	PolicyID   *string `json:"policyId"`
	GrantID    *string `json:"grantId"`
	ApprovedBy *string `json:"approvedBy"`
}

type auditResult struct {
	Status     string  `json:"status"`
	OutputHash *string `json:"outputHash"`
	ErrorClass *string `json:"errorClass"`
}

type auditEffects struct {
	Entities []json.RawMessage `json:"entities"`
}

type auditTokenUsage struct {
	In  int `json:"in"`
	Out int `json:"out"`
}

// auditFor 构造 dispatched 段记录：结论必然未定（status=unknown），决策段带上
// 让它通过的那条策略与授权。
func (r *runner) auditFor(plan Plan, step PlanStep, auditID, stage string, gate gateDecision, inputHash, idemKey string) auditRecord {
	rec := r.auditBase(plan, step, auditID, stage, gate, inputHash, idemKey)
	rec.Result = auditResult{Status: "unknown", OutputHash: nil, ErrorClass: nil}
	return rec
}

// resultAudit 构造 result 段记录（与 dispatched 段共用同一个 auditId）。
func (r *runner) resultAudit(plan Plan, step PlanStep, auditID string, gate gateDecision, inputHash, idemKey,
	status, outputHash, errorClass string) auditRecord {
	rec := r.auditBase(plan, step, auditID, auditStageResult, gate, inputHash, idemKey)
	res := auditResult{Status: status}
	if outputHash != "" {
		hash := outputHash
		res.OutputHash = &hash
	}
	if errorClass != "" {
		class := errorClass
		res.ErrorClass = &class
	}
	rec.Result = res
	return rec
}

// auditBase 填两段共有的字段。指针字段用 nil 表示 null：schema 区分「缺失」与「null」，
// 而这两处（planId/stepId/幂等键/委托人）在哈希链里必须显式写 null。
//
// decision.policyId 是**作出该决定的策略 id**（来自 policies:evaluate 的 decidedBy），
// 不是处置值本身；两处混用会让审计看起来「有策略」，实际却答不出是哪一条。
func (r *runner) auditBase(plan Plan, step PlanStep, auditID, stage string, gate gateDecision, inputHash, idemKey string) auditRecord {
	rec := auditRecord{
		AuditID:   auditID,
		Stage:     stage,
		At:        time.Now().UTC().Format(time.RFC3339),
		Actor:     auditActor{Type: "agent", ID: r.deps.AgentID, OnBehalfOf: nilIfEmpty(r.deps.OnBehalfOf)},
		Trigger:   auditTrigger{EventID: nilIfEmpty(r.event.EventID), CorrelationID: nilIfEmpty(r.correlationID)},
		PlanID:    nilIfEmpty(plan.PlanID),
		StepID:    nilIfEmpty(step.StepID),
		Action:    step.Action,
		InputHash: inputHash,
		Decision: auditDecision{
			Outcome:    gate.outcome,
			PolicyID:   nilIfEmpty(gate.policyID),
			GrantID:    nilIfEmpty(gate.grantID),
			ApprovedBy: nil, // 走到执行的计划必是 auto：approve 在挂起路径上（buildPending），deny 直接转人工
		},
		Effects:    auditEffects{Entities: r.entities()},
		TokenUsage: auditTokenUsage{}, // internal/llm 目前不回传用量，见交付说明的已知缺口
	}
	if idemKey != "" {
		key := idemKey
		rec.IdempotencyKey = &key
	}
	return rec
}

// entities 返回本步影响的实体寻址：事件主角就是受影响的那个实体（审计与对账都要与
// Plan.expectedDiff 用同一寻址）。subject 不合法时给空数组而不是伪造一个。
func (r *runner) entities() []json.RawMessage {
	out := make([]json.RawMessage, 0, 1)
	subject := bytes.TrimSpace(r.event.Subject)
	if len(subject) > 0 && json.Valid(subject) && (subject[0] == '"' || subject[0] == '{') {
		out = append(out, json.RawMessage(subject))
	}
	return out
}

// postAudit 追加一条审计记录。非 200 一律当失败：宿主承诺「200 即已落盘」。
func (r *runner) postAudit(ctx context.Context, rec auditRecord) error {
	status, raw, err := r.postJSON(ctx, r.host+"/tenon/audit", rec, nil)
	if err != nil {
		return fmt.Errorf("POST /tenon/audit 失败：%w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("POST /tenon/audit 返回 %d：%s", status, snippet(raw))
	}
	return nil
}

// ---------- Intent 帧 ----------

// intentFrame 是 frames.schema.json#/$defs/IntentFrame 的 Go 投影。
type intentFrame struct {
	Type     string         `json:"type"`
	IntentID string         `json:"intentId"`
	PlanID   string         `json:"planId"`
	Actions  []intentAction `json:"actions"`
}

// intentAction 是一条界面动作：只表达「应进入什么状态」，不含选择器与坐标。
type intentAction struct {
	Kind   string          `json:"kind"`
	Target json.RawMessage `json:"target"`
	Badge  string          `json:"badge,omitempty"`
}

// sendIntent 把结局翻译成 Intent 帧发给宿主。返回非空字符串表示帧没发出去
// （只写进 Detail，不改结局）。
//
// 没有 planId 就不发：帧里的 planId 是必填项，前端要靠它把界面状态与可撤销计划关联；
// 编造一个 planId 比不发更糟。
func (r *runner) sendIntent(ctx context.Context, res RunResult) string {
	planID := strings.TrimSpace(res.PlanID)
	if planID == "" {
		return ""
	}
	frame := intentFrame{Type: frameTypeIntent, IntentID: newID("int_"), PlanID: planID, Actions: []intentAction{}}
	target := r.target()
	if len(target) > 0 {
		switch res.Outcome {
		case OutcomeCompleted:
			frame.Actions = append(frame.Actions,
				intentAction{Kind: "open", Target: target},
				intentAction{Kind: "highlight", Target: target,
					Badge: truncateRunes(fmt.Sprintf("%d 步已执行", len(res.ActionsTaken)), badgeLimit)})
		default:
			frame.Actions = append(frame.Actions,
				intentAction{Kind: "highlight", Target: target, Badge: truncateRunes(badgeFor(res.Outcome), badgeLimit)})
		}
	}
	status, raw, err := r.postJSON(ctx, r.host+"/tenon/intents", frame, nil)
	if err != nil {
		return "Intent 帧发送失败（不影响业务结局）：" + err.Error()
	}
	if status != http.StatusOK {
		return fmt.Sprintf("Intent 帧被宿主拒绝（%d）：%s", status, snippet(raw))
	}
	return ""
}

// badgeFor 给 highlight 一条人能读的短文案。
func badgeFor(outcome Outcome) string {
	switch outcome {
	case OutcomeNeedsHuman:
		return "需人工介入"
	case OutcomeFailed:
		return "执行失败"
	default:
		return string(outcome)
	}
}

// target 返回可用的实体寻址（事件主角）。不合法时返回 nil，此时 Intent 帧的 actions
// 是空数组——「无需界面反应」是合法的，伪造一个目标不是。
func (r *runner) target() json.RawMessage {
	subject := bytes.TrimSpace(r.event.Subject)
	if len(subject) == 0 || !json.Valid(subject) {
		return nil
	}
	if subject[0] != '"' && subject[0] != '{' {
		return nil
	}
	return json.RawMessage(subject)
}

// ---------- HTTP 与工具 ----------

// getJSON 发一个带协议版本头的 GET。
func (r *runner) getJSON(ctx context.Context, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("构造请求失败（%s）：%w", url, err)
	}
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	return r.do(req)
}

// postJSON 发一个 JSON POST。body 可以是 json.RawMessage（原样透传动作入参）。
func (r *runner) postJSON(ctx context.Context, url string, body any, headers map[string]string) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("请求体编组失败（%s）：%w", url, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, fmt.Errorf("构造请求失败（%s）：%w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return r.do(req)
}

// do 发请求并读出响应体。状态码交调用方判定（此处不把非 2xx 当传输错误）。
func (r *runner) do(req *http.Request) (int, []byte, error) {
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读取响应体失败：%w", err)
	}
	return resp.StatusCode, raw, nil
}

// orderSteps 按 dependsOn 做稳定拓扑排序（数组顺序作为同层次序）。
// 引用不存在的 stepId 或成环都是计划缺陷：schema 表达不了 DAG，只能在执行前判定。
func orderSteps(steps []PlanStep) ([]PlanStep, error) {
	index := make(map[string]int, len(steps))
	for i, step := range steps {
		if _, dup := index[step.StepID]; dup {
			return nil, fmt.Errorf("stepId %q 在计划里重复", step.StepID)
		}
		index[step.StepID] = i
	}
	done := make([]bool, len(steps))
	out := make([]PlanStep, 0, len(steps))
	for len(out) < len(steps) {
		progressed := false
		for i, step := range steps {
			if done[i] {
				continue
			}
			ready := true
			for _, dep := range step.DependsOn {
				j, ok := index[dep]
				if !ok {
					return nil, fmt.Errorf("step %q 依赖了不存在的 stepId %q", step.StepID, dep)
				}
				if !done[j] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			done[i] = true
			out = append(out, step)
			progressed = true
		}
		if !progressed {
			return nil, errors.New("dependsOn 构成环，无法定序")
		}
	}
	return out, nil
}

// idSeq 让同一纳秒内生成的两个 id 也不相同。
var idSeq atomic.Uint64

// newID 生成 <prefix><base36 纳秒><base36 序号>。前缀与字符集满足 auditId / intentId
// 的词法要求（^aud_[A-Za-z0-9_-]{4,}$ / ^int_[A-Za-z0-9_-]{4,}$）。
func newID(prefix string) string {
	return prefix + strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + strconv.FormatUint(idSeq.Add(1), 36)
}

// hashPayload 给出入参的 SHA-256 摘要（schema 的 Sha256Hash 写法）。
// 用原始字节而不是重新编组的字节：摘要要能证明「执行的就是计划里的那份参数」。
func hashPayload(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// rawOrEmptyObject 保证请求体是 JSON 对象：schema 要求 args 是 JsonObject，
// 合法计划里它总在；这里只兜住「直接使用 Run 而没有过规划器」的情形。
func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage(`{}`)
}

// nilIfEmpty 把空串映射成 nil（JSON null）。schema 区分缺失与 null，而审计链里
// 「没有委托人」必须显式为 null。
func nilIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// firstNonEmpty 返回第一个非空字符串（空列表返回空串）。
// 用于从 policies:evaluate 的 decidedBy 里取「作出该处置的策略 id」。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// snippet 截取响应体前面一段放进错误信息，按 rune 截断以免切坏汉字。
func snippet(body []byte) string {
	return truncateRunes(strings.TrimSpace(string(body)), snippetLimit)
}

// truncateRunes 按字符数截断，超出加省略号。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// sleepCtx 等待 d，期间响应上下文取消。返回 false 表示被取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
