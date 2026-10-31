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
//	  pending/unsupported 同样转人工——fail-closed，见设计文档 §8 降级矩阵。
//	  unsafe 例外：带裁决有界重规划，用尽仍不可行才转人工，理由见 dryRun 闸门那段注释）
//	5 需要审批 → 挂起（I7 三次落盘；结局仍是 needs_human + PendingPlanID，
//	  三态封闭不新增第四态）。否则每步执行前写 stage=dispatched 审计
//	  （非 200 立即终止，本次调用数必须为 0）
//	6 执行（write 带 Idempotency-Key = protocol.IdempotencyKey(planId, stepId)）
//	7 写 stage=result 审计
//	8 按错误分类处理：retryable 退避重试（幂等键不变）；business_rejection 重新规划；
//	  policy_denied 立即转人工（不换路）；其余当 bug 失败
//
// 除上述一次性流程外，一个回合可能发生两类「带信息再来一轮」（各有预算）：
//   - 预演判 unsafe：模块算出这一步在当前状态下不成立，裁决回填后重规划（maxReplans）；
//   - 一整轮只有只读动作：事实已到手但处置还没开始，把事实回填后重规划
//     （maxObservationRounds）；用尽仍无写动作则转人工——读结果不是完成。
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
	// maxObservationRounds 是「只有只读动作」之后的重新规划上限：1 次初始 + 2 次观察回填。
	//
	// 它不是重试次数，而是「把事实交回去再想一次」的机会。用完仍拿不出写动作，说明缺的
	// 是处置能力（能力清单里没有能完成业务的写动作），该转人工而不是继续读下去。
	maxObservationRounds = 2
	// observationLimit 是回填进 prompt 的单条只读响应字符上限。响应体可能是 KB 量级，
	// 事实块必须远小于一次 prompt 的预算。
	observationLimit = 2000

	// replanReason* 是 PlanInput.ReplanReason 的取值：它决定 prompt 里那段反馈的措辞。
	// 两种原因不能共用一段文案——「换一条路」和「给出处置」是两件不同的事。
	replanReasonRejection   = "rejection"
	replanReasonObservation = "observation"
	replanReasonBlocked     = "blocked"

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
	// messageLimit 是随 Intent 帧发给界面的一句结论的上限。它比 badge 长，因为
	// 这句话要解释「为什么」（例如哪个动作被哪条策略拒了），但也不是日志全文。
	messageLimit = 300

	// 以下取值来自 spec/0.1 的 schema（policy / plan / frames），本文件只引用字符串，
	// 不复制它们的语义定义。
	outcomeAuto     = "auto"     // policy.schema.json 的 outcome：可自动执行
	outcomeApprove  = "approve"  // policy.schema.json 的 outcome：等人工批准，agent 只能挂起
	outcomeDeny     = "deny"     // policy.schema.json 的 outcome：转人工，不得换路
	kindRead        = "read"     // plan.schema.json 的 kind
	kindWrite       = "write"    // plan.schema.json 的 kind
	dryRunVerified  = "verified" // plan.schema.json 的 DryRunStatus：唯一允许自动执行的值
	// dryRunUnsafe 是「这一步在当前状态下不成立」——事实性裁决，不是治理拒绝。
	// 它值得回填一轮：模块刚刚算出了新的信息（DAG 里没有任何别的途径能拿到它）。
	dryRunUnsafe = "unsafe"
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

// RequestCorrelationID 给人工请求回合一个**确定性且合规**的业务链锚点。
//
// common.schema.json#/$defs/CorrelationId 的模式是 ^cor:\S+$，所以「谁问的」不能当
// 锚点：把 by（user:alice）写进 correlationId，落进审计 trigger.correlationId 就是一条
// 不合规记录（真冒烟暴露的 F2）；而「谁问的」已由 actor.onBehalfOf 承载，两者不是一回事。
//
// 用 requestId 而不是 newID()：同一句请求重放后必须仍落在同一条业务链上，否则审计对账
// 与差异测试都对不上。请求回合是**合成事件**，没有自带锚点，所以由这里派生；事件回合
// 不经过这里——它沿用事件自带的 correlationId。
func RequestCorrelationID(requestID string) string {
	return "cor:request/" + requestID
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
	// Token 取宿主签发的 agent token，供 /tenon/tickets:issue 鉴权。
	//
	// 为 nil 表示「没有凭据」：写动作会 fail-closed（不签票、不执行），
	// 读动作与规划不受影响。
	Token func(ctx context.Context) (string, error)
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
	// observations 是本回合只读步骤实际取回的事实，逐轮累积后回填进下一轮 prompt。
	observations []Observation
	// observationRounds 是已经用掉的「观察回填」次数（上限 maxObservationRounds）。
	observationRounds int
	// planRounds 是本回合已经发生的规划次数（含重规划），只用于日志：D1 失败时
	// 「模型看过事实没有」与「重规划了几轮」是两条不同的结论，日志里必须能分开看。
	planRounds int
}

// replanKind 说明「为什么还要再规划一轮」。
type replanKind int

const (
	// replanNone 表示这一轮已经给出结局，不再规划。
	replanNone replanKind = iota
	// replanRejection 表示业务拒绝：换一条路。
	replanRejection
	// replanObservation 表示这一轮只有只读动作：事实到手了，处置还没开始。
	replanObservation
	// replanBlocked 表示预演判 unsafe：某个写动作在当前状态下不成立，整轮一步没执行。
	replanBlocked
)

// replanFeedback 是「进入下一轮」的理由。
//
// 为什么不是一段字符串：两种理由要写进 prompt 的是不同的话（换路 vs 给处置），
// 而且预算也不同（业务拒绝数的是 maxReplans，只读数的是 maxObservationRounds）。
type replanFeedback struct {
	kind replanKind
	text string
}

// reason 映射成 PlanInput.ReplanReason（replanNone 返回空串）。
func (f replanFeedback) reason() string {
	switch f.kind {
	case replanRejection:
		return replanReasonRejection
	case replanObservation:
		return replanReasonObservation
	case replanBlocked:
		return replanReasonBlocked
	default:
		return ""
	}
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

	// 两种「再来一轮」共用这一个循环，但各有各的预算：业务拒绝数 maxReplans，
	// 只读回填数 maxObservationRounds（后者在 round 内部判定，到顶就当场转人工）。
	// 每轮要么给出结局、要么消耗掉一次预算，所以循环必然收敛。
	feedback := replanFeedback{}
	replans := 0
	for {
		res, next := r.round(ctx, PlanInput{
			Event:        r.event,
			SkillText:    skillText,
			Modules:      digest,
			Feedback:     feedback.text,
			ReplanReason: feedback.reason(),
			Observations: r.observations,
			// 事件回合这里是 nil：prompt 不得凭空多出一个空指令块（Task 6 的判据）。
			Request: r.request,
		})
		if next.kind == replanNone {
			return r.finish(ctx, res)
		}
		// 业务拒绝与预演 unsafe 共用同一份「换计划」预算：都是「这一份计划不可行」，
		// 分开计两套上限只会让最坏轮次变成 1+2+2，而收益是同一种东西。
		if next.kind == replanRejection || next.kind == replanBlocked {
			replans++
			if replans > maxReplans {
				cause := "business_rejection"
				if next.kind == replanBlocked {
					cause = "预演 unsafe"
				}
				return r.finish(ctx, RunResult{
					Outcome: OutcomeNeedsHuman,
					Detail: fmt.Sprintf("%s 重新规划 %d 轮后仍未通过（1 次初始 + %d 次重规划），转人工：%s",
						cause, maxReplans, maxReplans, next.text),
				})
			}
		}
		feedback = next
	}
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
// 返回的第二值 kind 非 replanNone 表示这一轮要求再来一轮规划（业务拒绝、或只读轮次）；
// 其余结局都由第一值表达。
func (r *runner) round(ctx context.Context, in PlanInput) (RunResult, replanFeedback) {
	plan, err := r.deps.Planner.Plan(ctx, in)
	if err != nil {
		var gap *capabilityGapError
		if errors.As(err, &gap) {
			// 能力不足不是崩溃：模型要求了清单里没有的动作，人需要知道缺什么。
			r.planID = gap.PlanID
			return RunResult{Outcome: OutcomeNeedsHuman, PlanID: gap.PlanID,
				Detail: "能力不足，转人工（模型要求的动作不在动态清单里）：" + err.Error()}, replanFeedback{}
		}
		return RunResult{Outcome: OutcomeFailed,
			Detail: "规划失败（" + planSchemaName + " 是判据）：" + err.Error()}, replanFeedback{}
	}

	// trigger 校正为真实触发源：它是审计与对账的锚点，模型抄错不该让整轮失败。
	plan.Trigger = PlanTrigger{EventID: r.event.EventID, CorrelationID: r.correlationID}
	r.planID = plan.PlanID
	// 一行说清这一轮规划的形状：D1 排查时，「模型没看到事实」与「看到了仍然这么规划」
	// 是两条完全不同的结论，而它们在结果行里长得一样（都只有一串 action id）。
	r.planRounds++
	log.Printf("agent: 第 %d 轮规划 planId=%s 只读=%v 事实=%d 条 动作=%v",
		r.planRounds, plan.PlanID, allReadOnly(plan.Steps), len(in.Observations), actionIDs(plan.Steps))

	// 闸门 2：策略。整份计划取最严（宿主实现）。
	//
	// deny 直接转人工；approve 只记下「需要审批」并继续往下走——授权与预演必须跑在
	// 「决定挂起」之前（fail-closed 要早：注定过不了授权/预演的计划不该挂上公告板
	// 等人白批一次），最后才由闸门 5 决定「挂起还是执行」。
	policy, err := r.evaluatePolicies(ctx, plan)
	if err != nil {
		return RunResult{Outcome: OutcomeFailed,
			Detail: "策略评估不可用（fail-closed，不做任何写）：" + err.Error()}, replanFeedback{}
	}
	if policy.Outcome == outcomeDeny {
		// 把计划动作写进 detail：只写「被拒绝」而不写「谁被拒」，排障只能靠猜
		// （2026-10-27 的真实教训：结果行里 动作=[]，不知道是哪个动作触发了 deny）。
		detail := fmt.Sprintf("策略闸门拒绝：outcome=%s decidedBy=%v reasons=%v",
			policy.Outcome, policy.DecidedBy, policy.Reasons)
		if note := planActionsNote(plan); note != "" {
			detail += " " + note
		}
		detail += "（转人工，不换路绕过）"
		return RunResult{Outcome: OutcomeNeedsHuman, Detail: detail}, replanFeedback{}
	}
	// policy.schema.json 是封闭枚举，但宿主的响应不受本包控制：除 auto/approve 之外的
	// 任何取值（含空串）都按「没放行」处理——不猜它是不是 approve，也不让它掉进执行。
	needApproval := policy.Outcome == outcomeApprove
	if policy.Outcome != outcomeAuto && !needApproval {
		return RunResult{Outcome: OutcomeNeedsHuman,
			Detail: fmt.Sprintf("策略闸门未放行：outcome=%s decidedBy=%v reasons=%v（转人工，不换路绕过）",
				policy.Outcome, policy.DecidedBy, policy.Reasons)}, replanFeedback{}
	}

	// 闸门 3：授权。逐 action 全部先过——先执行几个再撞授权拒绝，等于留下半截副作用。
	grantIDs := make(map[string]string, len(plan.Steps))
	for _, step := range plan.Steps {
		grant, err := r.evaluateGrant(ctx, step)
		if err != nil {
			return RunResult{Outcome: OutcomeFailed,
				Detail: "授权评估不可用（fail-closed，不做任何写）：" + err.Error()}, replanFeedback{}
		}
		if !grant.Allowed {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("授权闸门拒绝动作 %s：reason=%s（转人工，不换路绕过）",
					step.Action, grant.Reason)}, replanFeedback{}
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
				Detail: "预演失败（fail-closed，不做任何写）：" + err.Error()}, replanFeedback{}
		}
		if !found {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("动作 %s 的模块未实现预演（404）：按降级矩阵强制转人工，"+
					"不能把「没实现端点」当成「免检」", step.Action)}, replanFeedback{}
		}
		if status == dryRunUnsafe {
			// unsafe 是**事实性**裁决：模块按当前状态算出来这一步不成立（例如可用量不足）。
			// 直接转人工等于把它刚算出的事实丢掉——D1 实测里模型在还没读库存时就规了扣减，
			// 预演判 unsafe 后整轮放弃，A/B 两轮序列因此完全相同，看上去像「没用能力清单」。
			//
			// 所以给一次带裁决的重新规划（预算与业务拒绝共用）。为什么这不是「绕路」：
			// 换出来的写动作要自己再过一遍预演，没有哪一步能跳过它。
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("预演结论为 unsafe：动作 %s 在当前状态下不成立，本轮一步未执行，重新规划",
					step.Action)},
				replanFeedback{kind: replanBlocked,
					text: fmt.Sprintf("动作 %s 的预演结论是 unsafe：模块判定它在当前状态下不成立"+
						"（这一步没有被执行过，也没有留下任何副作用）。", step.Action)}
		}
		if status != dryRunVerified {
			// pending / unsupported 等其余非 verified 结论保持 fail-closed：那不是
			// 「这一步不成立」，而是「说不清」——说不清就该由人来判，不是再赌一轮。
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("预演结论为 %q（只有 %s 允许自动执行）：动作 %s，转人工",
					status, dryRunVerified, step.Action)}, replanFeedback{}
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
					policy.Outcome, policy.DecidedBy)}, replanFeedback{}
		}
		// 元信息取本轮规划用的同一份发现结果（PlanInput.Modules 已带 ActionMeta）：
		// 渲染与规划看的是同一张能力清单，不会出现「计划里的动作渲染不出模板」。
		pending, err := r.buildPending(plan, policy, actorMeta(in.Modules))
		if err != nil {
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: err.Error()}, replanFeedback{}
		}
		if err := suspend(ctx, r.deps.State, r.deps, pending); err != nil {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: "挂起失败（不留半截 pending）：" + err.Error()}, replanFeedback{}
		}
		id := pending.PlanID
		return RunResult{Outcome: OutcomeNeedsHuman, PlanID: id, PendingPlanID: &id,
			Detail: fmt.Sprintf("已挂起等待审批：planId=%s 需要 %v", id, pending.RequiredRoles)}, replanFeedback{}
	}

	// 闸门 5–7：按依赖顺序执行，每步各写两段审计。
	ordered, err := orderSteps(plan.Steps)
	if err != nil {
		return RunResult{Outcome: OutcomeFailed, Detail: "计划的 steps 无法定序：" + err.Error()}, replanFeedback{}
	}
	for _, step := range ordered {
		gate := gateDecision{
			outcome:  policy.Outcome,
			policyID: firstNonEmpty(policy.DecidedBy...),
			grantID:  grantIDs[step.StepID],
			// approvedBy 留 nil：走到这里的计划必是 auto（approve 在挂起路径上，
			// deny 直接转人工），这条路径确实没有人按过批准。
		}
		verdict := r.executeStep(ctx, plan, step, gate)
		switch verdict.kind {
		case verdictOK:
			r.taken = append(r.taken, step.Action)
		case verdictBusinessRejection:
			// 重新规划：不在这里判上限，上限由外层循环持有。
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: verdict.detail},
				replanFeedback{kind: replanRejection, text: verdict.feedback}
		case verdictPolicyDenied:
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: verdict.detail}, replanFeedback{}
		case verdictRetryableExhausted:
			return RunResult{Outcome: OutcomeFailed, Detail: verdict.detail}, replanFeedback{}
		default:
			return RunResult{Outcome: OutcomeFailed, Detail: verdict.detail}, replanFeedback{}
		}
	}

	// 只读不是完成（D1）：事件是「事情已经发生，需要一个处置」，而一整轮只有读动作时
	// 事实才刚到手、处置还没开始。把它当成 completed 回报，就是「什么都没做却宣布做完了」。
	//
	// 这不是为了迁就测试：`quote.completed` 的载荷里没有库存快照，「够就扣、不够就采购」
	// 是只有运行期才看得见的分支。模型一次要给出一份完整动作序列，所以它必然先读；不给
	// 它第二轮，它永远走不到第二步。带回来的事实进下一轮 prompt，第二轮才有据可依。
	//
	// 请求回合同样套这条：人说「把货扣了」，只读一轮就报 completed 等于「我查了库存，办完了」
	// ——2026-10-31 的本地演示里就是这样，一次静默无动作看起来跟办成了一模一样。
	// 代价是纯查询会多花一次规划；但那个方向的错很轻（多问一次模型），另一个方向的错很重
	// （业务没发生却显示完成）。两害相权，宁可多问一轮。
	if allReadOnly(ordered) {
		if r.observationRounds >= maxObservationRounds {
			if r.request != nil {
				// 请求回合到此为止：指令的范围是人给的，「只问一句」是合法指令。但结局必须
				// 把「没有产生业务动作」写出来，别让静默无动作冒充办完。
				return RunResult{Outcome: OutcomeCompleted,
					Detail: fmt.Sprintf("只读完成：连续 %d 轮只有只读动作、没有产生任何业务动作"+
						"（指令若是要办的事，这就等于没办）。最后一份计划：%s",
						r.observationRounds, planActionsNote(plan))}, replanFeedback{}
			}
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("连续 %d 轮只读、没有产生任何业务动作（事实已回填仍规划不出写动作）："+
					"能力清单里没有能完成这笔业务的写动作，转人工。最后一份计划：%s",
					r.observationRounds, planActionsNote(plan))}, replanFeedback{}
		}
		r.observationRounds++
		return RunResult{Outcome: OutcomeNeedsHuman,
			Detail: fmt.Sprintf("第 %d 轮只有只读动作（%d 步），进入下一轮规划", r.observationRounds, len(ordered))},
			replanFeedback{kind: replanObservation,
				text: fmt.Sprintf("上一轮（planId=%s）执行的全部是只读动作，清单里的写动作一个都没用到。",
					plan.PlanID)}
	}

	return RunResult{Outcome: OutcomeCompleted,
		Detail: fmt.Sprintf("计划全部执行成功：%d 步", len(ordered))}, replanFeedback{}
}

// actionIDs 按计划声明顺序取出动作 id，供日志用（不要用集合：顺序本身就是信息）。
func actionIDs(steps []PlanStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Action)
	}
	return out
}

// allReadOnly 判断一份已定序的计划是不是「一步写动作都没有」。
// 空计划返回 false：零步计划没有可执行语义（闸门 2 已经把它判 deny），不该被读成「只读」。
func allReadOnly(steps []PlanStep) bool {
	if len(steps) == 0 {
		return false
	}
	for _, step := range steps {
		if step.Kind != kindRead {
			return false
		}
	}
	return true
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
func (r *runner) buildPending(plan Plan, policy policyEvaluation, meta map[string]ActionMeta) (Pending, error) {
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
	labels, details := renderSteps(plan, meta)
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
		Details:        details,
		Summary:        strings.Join(labels, "；"),
		RequiredRoles:  policy.Approvers,
		CreatedAt:      now.Format(time.RFC3339),
		ExpiresAt:      now.Add(pendingTTL).Format(time.RFC3339),
		// subject 必须落盘：恢复时 TriggerEventID 只够锚定事件、锚不住「哪个实体」，
		// 丢了它，恢复执行的审计 effects.entities 与 Intent 帧 actions 就退化成空数组。
		Subject: r.event.Subject,
	}, nil
}

// actorMeta 把本轮发现结果压平成 action id → 元信息，供渲染器按 step 查。
// 模块间 action id 全局唯一（module.action 的格式），所以不需要再带模块维度；
// 同一 id 重复出现只可能来自注册表脏数据，后者覆盖前者不影响渲染的可读性。
func actorMeta(modules []ModuleDigest) map[string]ActionMeta {
	out := make(map[string]ActionMeta)
	for _, m := range modules {
		for id, meta := range m.ActionMeta {
			out[id] = meta
		}
	}
	return out
}

// planActionsNote 把计划里的动作按步骤顺序列出来，供拒绝/失败时写进 detail。
// 用 step 顺序而不是排序：排障时要能对上计划原文。
// 空计划返回空串——「计划为空」与「计划里全是未覆盖动作」是两种不同的失败，
// 不要在 detail 里留一句「计划动作=[]」把它们混成一种。
func planActionsNote(plan Plan) string {
	if len(plan.Steps) == 0 {
		return ""
	}
	actions := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		actions = append(actions, step.Action)
	}
	return "计划动作=[" + strings.Join(actions, ", ") + "]"
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

// moduleEntry 是 GET /tenon/modules 的一行。宿主只返回**可见**模块（enabled && alive）；enabled 字段保留是为了兼容旧宿主，新宿主不会再吐 false。
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
		actions, meta, err := r.manifestActions(ctx, base)
		if err != nil {
			// 模块在场却读不到 manifest：能力清单不完整，不能假装它没有动作。
			return nil, fmt.Errorf("模块 %q 的能力发现失败：%w", id, err)
		}
		r.modules[id] = base
		out = append(out, ModuleDigest{ID: id, Actions: actions, ActionMeta: meta})
	}
	return out, nil
}

// manifestActions 从模块的 GET /.well-known/tenon 取动作清单（摘要级，不含 schema），
// 顺带把渲染白话要用的 title 与 summaryTemplate 一起带走。
//
// 模板**不进 prompt**：它在挂起时用于渲染审批卡，属于给人看的数据。
func (r *runner) manifestActions(ctx context.Context, baseURL string) ([]string, map[string]ActionMeta, error) {
	status, raw, err := r.getJSON(ctx, baseURL+"/.well-known/tenon")
	if err != nil {
		return nil, nil, fmt.Errorf("GET /.well-known/tenon 失败：%w", err)
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("GET /.well-known/tenon 返回 %d：%s", status, snippet(raw))
	}
	var manifest struct {
		Actions []struct {
			ID              string   `json:"id"`
			Title           string   `json:"title"`
			SummaryTemplate string   `json:"summaryTemplate"`
			RequiredArgs    []string `json:"requiredArgs"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, nil, fmt.Errorf("manifest 不是合法 JSON：%w", err)
	}
	ids := make([]string, 0, len(manifest.Actions))
	meta := make(map[string]ActionMeta, len(manifest.Actions))
	for _, a := range manifest.Actions {
		id := strings.TrimSpace(a.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
		meta[id] = ActionMeta{Title: a.Title, SummaryTemplate: a.SummaryTemplate, RequiredArgs: a.RequiredArgs}
	}
	return ids, meta, nil
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
	// triggerEventId 必须带上：宿主拿它查来源登记表，命中 → origin=request（对话发起），
	// 未命中 → origin=event。漏了它，宿主一律按事件驱动判，**所有「对话发起」类策略
	// 永远不命中**——症状是「人在对话里要的事」被判 NO_POLICY_MATCH_DEFAULT_DENY，
	// 而同一份计划由事件触发就没问题（2026-10-31 本地演示：带采购的计划就这样被拒）。
	// 来源只能由宿主判定、agent 不得自报（spec §3.11），所以这里只递交触发源 id。
	status, raw, err := r.postJSON(ctx, r.host+"/tenon/policies:evaluate",
		map[string]any{"plan": plan, "triggerEventId": r.event.EventID}, nil)
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
		// 预检不记账（interfaces.md §4.2.1：配额在签票时扣一次）。
		//
		// 这条不是优化而是正确性：宿主的 grants:evaluate 缺省「预检即扣」以兼容 0.1，
		// 不显式传 false 的话，一次写动作会被扣两次（预检一次、签票一次），重试还会
		// 继续扣——2026-10-31 本地演示实测：一张待批卡批准失败后按秒重试 90 秒，
		// 窗口配额 100 被 12/次扣穿，最终以 QUOTA_EXCEEDED 归档。
		"charge": false,
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
//
// approvedBy 是**谁按的批准**，与 outcome 是两个维度：outcome 说「这道闸门怎么处置的」，
// approvedBy 说「哪个主体批的」。auto 路径没人批，必须为 nil；人工批准过的路径（恢复
// 执行）必须填审批人——audit-event.schema.json 规定 outcome=approve 时 approvedBy 非空，
// 留 nil 就是一条不合规记录。
type gateDecision struct {
	outcome    string
	policyID   string
	grantID    string
	approvedBy *string
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
		// 闸门 5：写动作必须先拿到宿主签发的票据。签票这一步同时完成三件事：
		// 策略+授权判定、写 dispatched 审计、给出本次执行的 auditId。
		//
		// 顺序不能再由 agent 自己拼：0.2.0 起步骤级 dispatched 只由宿主在签票时写，
		// agent 自己写会被 403 拒掉——这正是不再"先写审计再调用"的原因。
		headers := map[string]string{}
		auditID := ""
		if step.Kind == kindWrite {
			ticket, gotAuditID, err := r.issueTicket(ctx, plan, step)
			if err != nil {
				return stepVerdict{kind: verdictFailed,
					detail: "取不到执行票据，拒绝执行（本步没有发出任何调用）：" + err.Error()}
			}
			headers[protocol.HeaderTicket] = ticket
			auditID = gotAuditID
		} else {
			// 读动作没有副作用，不需要票据；它也不进审计链。
			auditID = newID("aud_")
		}

		// 闸门 6：执行。写动作带幂等键与票据，读动作都不带。
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
			// 只读步骤的返回值就是这一轮的全部收获：把它原样留下来，供下一轮规划使用。
			// 写动作不留：它的响应是执行结果（单号、结余），不是「做决策所需的事实」。
			if step.Kind == kindRead {
				r.observeRead(step.Action, raw)
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

// observeRead 记下一条只读步骤的实际返回，供下一轮规划使用。
//
// 截断到 observationLimit：事实块是 prompt 的一段，不是日志；一个返回整张库存表的
// 模块不应该把整轮预算吃掉。截断按 rune 走，不会切坏汉字。
func (r *runner) observeRead(action string, body []byte) {
	excerpt := truncateRunes(strings.TrimSpace(string(body)), observationLimit)
	r.observations = append(r.observations, Observation{Action: action, Body: excerpt})
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
			Outcome:  gate.outcome,
			PolicyID: nilIfEmpty(gate.policyID),
			GrantID:  nilIfEmpty(gate.grantID),
			// 审批人只能由闸门结论带进来：auto 路径的 gateDecision 不填它（那条路径确实
			// 没人批），恢复路径填 c.By（人批过的事实必须留痕）。
			ApprovedBy: gate.approvedBy,
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
// ticketIssueRequest 是 POST /tenon/tickets:issue 的请求体。
// Args 用 RawMessage：签发端对**原始字节**求摘要，重新序列化会让票在模块侧验不过。
type ticketIssueRequest struct {
	PlanID     string          `json:"planId"`
	StepID     string          `json:"stepId"`
	Action     string          `json:"action"`
	Args       json.RawMessage `json:"args"`
	Agent      string          `json:"agent"`
	OnBehalfOf string          `json:"onBehalfOf"`
}

type ticketIssueReply struct {
	Ticket  string `json:"ticket"`
	AuditID string `json:"auditId"`
	Exp     int64  `json:"exp"`
}

// issueTicket 向宿主要一张执行票据。返回票与本次执行的 auditId（审计已由宿主落盘）。
//
// 没有凭据就不签、也不降级为无票执行：签票端要求 agent 角色 token，
// 这条路径如果能在缺 token 时"放过去"，票据机制就等于没上。
func (r *runner) issueTicket(ctx context.Context, plan Plan, step PlanStep) (string, string, error) {
	if r.deps.Token == nil {
		return "", "", errors.New("未装配宿主凭据（Deps.Token）：签票需要 agent token")
	}
	token, err := r.deps.Token(ctx)
	if err != nil {
		return "", "", fmt.Errorf("获取宿主 token 失败：%w", err)
	}
	req := ticketIssueRequest{
		PlanID:     plan.PlanID,
		StepID:     step.StepID,
		Action:     step.Action,
		Args:       rawOrEmptyObject(step.Args),
		Agent:      r.deps.AgentID,
		OnBehalfOf: r.deps.OnBehalfOf,
	}
	status, raw, err := r.postJSON(ctx, r.host+"/tenon/tickets:issue", req,
		map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return "", "", fmt.Errorf("POST /tenon/tickets:issue 失败：%w", err)
	}
	if status != http.StatusOK {
		return "", "", fmt.Errorf("签票返回 %d：%s", status, snippet(raw))
	}
	var reply ticketIssueReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", "", fmt.Errorf("签票响应不是 {ticket,auditId,exp}：%w", err)
	}
	if reply.Ticket == "" || reply.AuditID == "" {
		return "", "", errors.New("签票响应缺 ticket 或 auditId")
	}
	return reply.Ticket, reply.AuditID, nil
}

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
	// Outcome 与 Message 是本回合的**结论**，给人看的那一句。
	//
	// 光有 actions 的话，界面只能表达「该高亮哪个对象」，表达不了「为什么」。而一回合
	// 常常没有公告板条目（策略/授权拒绝、预演拦截、能力不足、或 auto 直接执行完了），
	// 于是对话页上人只看到自己那句话、agent 一声不响（2026-11-01 实测反馈）。
	// 结论走 Intent 帧是最短的路径：这条通道本来就是 agent → 界面，宿主原样转发。
	Outcome string `json:"outcome,omitempty"`
	Message string `json:"message,omitempty"`
	// RequestID 是这一回合由哪句人话发起（事件驱动回合为空）。
	//
	// 界面上「我说了什么」与「agent 回了什么」必须能配对。只靠 planId 配不上：
	// 没有待批卡的回合（被闸门拒、auto 直接执行完）根本不产生卡片，planId 无处可查，
	// 于是那句话下面只能显示兜底文案，真正的结论却飘在列表末尾，同一回合看着像两次回复。
	RequestID string `json:"requestId,omitempty"`
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
	// 结论必须随帧走：截断到 messageLimit，因为这是一句给人读的摘要，不是日志搬运。
	frame.Outcome = string(res.Outcome)
	frame.Message = truncateRunes(strings.TrimSpace(res.Detail), messageLimit)
	if r.request != nil {
		frame.RequestID = r.request.RequestID
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
