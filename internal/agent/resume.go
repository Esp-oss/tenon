// resume.go 实现「一条 tenon.confirm 到达之后」的恢复流程：校验审批人 → 判过期 →
// 用 pending 里的原计划重跑闸门 → 执行 → 归档。
//
// 它**刻意不复用 round()**：round() 里有规划（恢复不需要，plan 原文就存在 pending 里）
// 和二次挂起（恢复不会再挂起同一份计划）。共用的是单步执行、定序、闸门求值这些
// 「一次调用」的语义——另外还必须自己 discover()：r.modules 只在能力发现里填，
// 而 moduleBase() 靠它反查基地址，漏了它每一步都会报「动作所属模块不在可达清单里」。
//
// 固定顺序（spec §5，不可调换）：
//
//	1 匹配 pending（归档里已 done 则按幂等返回既有结局）
//	2 校验审批人（claims.roles ∩ requiredRoles ≠ ∅）
//	3 判过期（过期绝不执行，I3）
//	4 重跑 policy / grant（grantIds 不落盘，必须重跑）
//	5 执行全部步骤（**业务拒绝 = 前置条件失效**：不硬跑，归档 stale）
//	6 终态归档（done / rejected / expired / stale）
//
// 审计边界（I2）：挂起路径的计划级 dispatched 段在 suspend 里写过，那时 approvedBy
// 恒为 null（还没人批）；恢复只补**计划级** result 段，并在那里记 approvedBy——人批过的
// 事实哪怕后来被策略拒了也必须留痕。
//
// 恢复执行的**每一步**仍各自写自己的 dispatched/result 两段（与 auto 路径同一套
// executeStep）。这两段的 decision.outcome 是 approve，所以按 audit-event.schema.json
// 的要求 approvedBy 必须非空：审批人由 gateDecision 一路传进 auditBase（见 runResumed
// 的 gate 构造），不再是「硬写 null」。
//
// 公告板**不在这里推**：终态覆盖是 board.go 的 finishTerminal（spec §3.4），而它需要一个
// 带登录凭证的 HostClient——Resume 的入参里只有宿主地址（Deps.Host），没有凭证，所以
// 本文件的每个归档点都只落盘。每 tick 的 RepushBoard 也只重推 pending/ 里的活条目，
// **不会**替归档件补推终态（见 board.go 的包注释）。终态覆盖的接线要由 cmd/agent 的 tick
// 补上，否则已处理完的那张卡会一直挂在板上，直到 host 按 expiresAt 把它剔除。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"tenon.dev/internal/protocol"
)

const (
	// confirmApprove / confirmReject 是 ConfirmDecision 里本任务实现的两个取值
	// （frames.schema.json 还有第三个 amend：它要用 JSON Patch 改写未执行步骤并产生
	// 新计划，属于规划路径，不在恢复流程里）。
	confirmApprove = "approve"
	confirmReject  = "reject"

	// auditResult.status 的取值（audit-event.schema.json 的 AuditResult 四态；
	// unknown 只在 dispatched 段出现，所以这里不需要）。
	auditStatusOK              = "ok"
	auditStatusError           = "error"
	auditStatusPartiallyFailed = "partially_failed"
)

// Confirm 是 tenon.confirm 帧在 agent 侧的投影。
//
// Roles 不在帧里：它来自审批人的登录 claims（谁批的、凭什么角色批的），由调用方
// 查出来再传进来——帧本身只证明「某人按了 approve」，证明不了他有没有权限。
type Confirm struct {
	ConfirmID string
	PlanID    string
	Decision  string // approve | reject（amend 未实现，见 confirmApprove 的注释）
	By        string
	Roles     []string
	At        string
}

// resumeError 区分「可重试」与「确定性拒绝」：只有前者值得下一 tick 再来。
type resumeError struct {
	Msg       string
	Retryable bool
}

func (e resumeError) Error() string { return e.Msg }

// IsRetryable 回答「这条 confirm 该留到下一 tick 重放，还是当场 ack 掉」。
//
// 确定性拒绝重放一万次也是同一个结论——角色不匹配（换个人批是另一帧的事）、已过期、
// 未知 planId、已归档为非 done、不认识的处置、缺审批人/缺宿主地址这类装配问题都该
// 立刻 ack：留着只会让同一个坏帧每 tick 重来一次，把日志刷满而永远不成功。
//
// 可重试的是「这一次没问成」：审计/策略/授权/能力发现/归档写失败，以及执行结果未定。
// 那类错误的 pending 与审计都还完整，下一 tick 重放是正确动作。
//
// 不是 resumeError 的错误按**可重试**处理：Resume 的返回值恒为 resumeError（本文件的
// 每个出口都包了它），真收到别的类型说明有人在包装链上丢了 Retryable 这个事实。此时
// 宁可多来一次，也不要把一条可能成功的 confirm 当成确定性拒绝丢掉。
func IsRetryable(err error) bool {
	if err == nil {
		// 已经处理完（含幂等重放）：没有可重试的事。
		return false
	}
	var re resumeError
	if errors.As(err, &re) {
		return re.Retryable
	}
	return true
}

// resumeOutcome 是 runResumed 的结论：要么执行完毕，要么给出归档原因。
//
// archiveAs 为**空**有确切含义：这是可重试的瞬时故障，pending 必须留着。把它当成
// 「归档成空状态」会写出一份 state="" 的归档件，等于把待批件弄丢了。
type resumeOutcome struct {
	err       error
	archiveAs PendingState
	retryable bool
	// steps 是**真正落地**的步数（只有全步成功才会走到归档 done，所以它等于 ordered 的
	// 长度）。日志行要报「这一帧到底做了几步」，不能只有一句「归档了」。
	steps int
}

// Resume 处理一条 confirm。返回 nil 表示已处理完毕（含幂等重放）；
// 返回 retryable 错误表示下一 tick 该帧会被重放。
//
// store 为 nil 表示「没有挂起通道」（Deps.State 的约定），不是空目录：空串在
// NewStateStore 那里就已经被拒绝过一次了（§0.1.6）。
func Resume(ctx context.Context, deps Deps, store *StateStore, c Confirm) error {
	if store == nil {
		return resumeError{Msg: "没有挂起通道（StateStore 为 nil）：无法恢复"}
	}
	if strings.TrimSpace(deps.Host) == "" {
		// 配置问题不是瞬时故障：重试一万次也还是没有宿主地址。
		return resumeError{Msg: "未配置宿主地址（fail-closed：不执行）"}
	}
	planID := strings.TrimSpace(c.PlanID)
	if planID == "" {
		return resumeError{Msg: "confirm 缺少 planId"}
	}
	// planId 会直接拼进状态目录的文件路径（state.go 的 pendingPath/archivedPath），
	// 而它是从入站帧里来的：先挡掉路径分隔符与 "."/".."，别让一条构造过的 confirm
	// 把读写引到状态目录之外（完整词法校验归 schema，这里只挡路径）。
	if planID == "." || planID == ".." || strings.ContainsAny(planID, `/\`) {
		return resumeError{Msg: fmt.Sprintf("planId %q 含路径分隔符，拒绝", planID)}
	}
	by := strings.TrimSpace(c.By)
	if by == "" {
		// 没有审批人就没法填 approvedBy，而「谁批的」是恢复路径唯一新增的审计事实。
		return resumeError{Msg: "confirm 缺少审批人（by）：人批过的事实必须有名有姓"}
	}

	// 先查归档（§0.1.3）：Archive 是「写归档件 → 删 pending 件」两步，中间失败会留下
	// 同名双份；这时 Load(pending) 仍然成功，照着它再跑一遍就是把同一份计划执行第二次
	// （幂等键挡得住重复副作用，挡不住重复审计与状态漂移）。
	//
	// 用 readPending 而不是 LoadArchived：后者的「没有」与「坏文件」都是 error，
	// 而这两种情形在这里的处理正相反（前者是正常路径，后者要报出去）。
	archived, found, err := readPending(store.archivedPath(planID))
	if err != nil {
		return resumeError{Msg: "读归档失败：" + err.Error(), Retryable: true}
	}
	if found {
		if archived.State == StateDone {
			return nil // I1 幂等：已经执行过，重放返回既有结局
		}
		return resumeError{Msg: fmt.Sprintf("planId %s 已归档为 %s，不能再次确认", planID, archived.State)}
	}

	p, ok, err := store.Load(planID)
	if err != nil {
		return resumeError{Msg: "读 pending 失败：" + err.Error(), Retryable: true}
	}
	if !ok {
		// 不在 pending/ 也不在 archived/：要么早被清理，要么 confirm 指错了地方。
		return resumeError{Msg: fmt.Sprintf("confirm 指向未知 planId：%s", planID)}
	}
	if p.State != StateAwaitingApproval && p.State != StateResuming {
		// 既不是待批也不是恢复中断：不猜它是什么意思，也不执行。
		return resumeError{Msg: fmt.Sprintf("planId %s 处于 %s，不能确认", planID, p.State)}
	}
	// plan 原文是审计锚点与幂等键的一半：缺失就补上，不一致就拒绝——
	// 用不一致的 planId 执行会让幂等键与当初挂起的那份对不上。
	if p.Plan.PlanID == "" {
		p.Plan.PlanID = p.PlanID
	} else if p.Plan.PlanID != p.PlanID {
		return resumeError{Msg: fmt.Sprintf("pending 与 plan 的 planId 不一致（%s / %s），不执行",
			p.PlanID, p.Plan.PlanID)}
	}

	// 这一帧被处理的耗时（F5：成功路径原来一行日志都没有，排障时看不出「处理过」）。
	// 从校验之后开始算：入站 ack 与重放的耗时不属于恢复本身。
	frameStarted := time.Now()

	switch strings.TrimSpace(c.Decision) {
	case confirmReject:
		if err := archivePending(store, p.PlanID, StateRejected); err != nil {
			return resumeError{Msg: err.Error(), Retryable: true}
		}
		// 归档即可：终态覆盖见 board.go 的 finishTerminal（它需要带凭证的 HostClient，
		// 接线在 cmd/agent，不在恢复路径里）。
		logResumed(p, c, confirmReject, 0, time.Since(frameStarted), StateRejected)
		return nil
	case confirmApprove:
		// 继续走下面的固定顺序
	default:
		// 不认识的处置绝不按 approve 处理：多一个 if，少一次误执行。
		return resumeError{Msg: fmt.Sprintf(
			"无法识别的 confirm 处置 %q（本任务只实现 %s/%s；amend 属规划路径）",
			c.Decision, confirmApprove, confirmReject)}
	}

	// 顺序 2：审批人。角色不匹配就不执行、也不动 pending——换个有权限的人还能批。
	if !rolesIntersect(c.Roles, p.RequiredRoles) {
		return resumeError{Msg: fmt.Sprintf(
			"审批人 %s 的角色 %v 与 requiredRoles %v 不相交（403），不执行",
			by, c.Roles, p.RequiredRoles)}
	}

	// 顺序 3：过期。过期绝不执行（I3）。
	if p.Expired(time.Now().UTC()) {
		r := newResumeRunner(deps, p)
		// spec §5.3：过期也要把计划级 result 段收尾，否则 suspend 留下的那条
		// dispatched 会永远停在 unknown。审计写不进去就不落终态：审计链才是凭据。
		if werr := r.writePlanResult(ctx, p, by, auditStatusError, string(protocol.ClassPolicyDenied)); werr != nil {
			return resumeError{Msg: "过期件写 result 审计失败（不归档，下一 tick 重来）：" + werr.Error(), Retryable: true}
		}
		if aerr := archivePending(store, p.PlanID, StateExpired); aerr != nil {
			return resumeError{Msg: "归档 expired 失败：" + aerr.Error(), Retryable: true}
		}
		return resumeError{Msg: fmt.Sprintf("planId %s 已过期（expiresAt=%s），未执行，请重新发起",
			p.PlanID, p.ExpiresAt)}
	}

	// spec §5.5：先把 state 改成 resuming 再执行。崩在执行中途时，盘上要能看出这条
	// confirm 已经被接受过，而不是仍然冒充「等人批」（那会让人以为没人按过）。
	p.State = StateResuming
	if err := store.Save(p); err != nil {
		return resumeError{Msg: "写 resuming 标记失败（不执行）：" + err.Error(), Retryable: true}
	}

	out := runResumed(ctx, deps, p, c)
	if out.err != nil {
		if out.archiveAs != "" {
			if aerr := archivePending(store, p.PlanID, out.archiveAs); aerr != nil {
				return resumeError{Msg: out.err.Error() + "；归档失败：" + aerr.Error(), Retryable: true}
			}
		}
		return resumeError{Msg: out.err.Error(), Retryable: out.retryable}
	}
	if err := archivePending(store, p.PlanID, StateDone); err != nil {
		return resumeError{Msg: "执行已完成但归档 done 失败（重放会被幂等键挡住）：" + err.Error(), Retryable: true}
	}
	logResumed(p, c, confirmApprove, out.steps, time.Since(frameStarted), StateDone)
	return nil
}

// logResumed 在恢复**成功**时打一行（planId、处置、审批人、执行了几步、耗时、终态）。
//
// 失败路径各自已经带了上下文（错误会经 IsRetryable 决定 ack 还是重放，并打进调用方的
// 日志），所以这里只管成功：原来成功的 resume 完全静默——审计落了、库存改了、归档成
// done 了，日志里一个字都没有，排障时看不出「这一帧被处理过」。
func logResumed(p Pending, c Confirm, decision string, steps int, elapsed time.Duration, state PendingState) {
	log.Printf("agent: confirm %s planId=%s decision=%s 审批人=%s 执行 %d/%d 步 耗时=%s → 归档 %s",
		firstNonEmpty(c.ConfirmID, "-"), p.PlanID, decision, firstNonEmpty(c.By, "-"),
		steps, len(p.Plan.Steps), elapsed.Round(time.Millisecond), state)
}

// runResumed 用原计划重跑闸门并执行全部步骤。
//
// 返回的 archiveAs 非空表示该归档成什么；为空表示可重试的瞬时故障，pending 必须留着。
func runResumed(ctx context.Context, deps Deps, p Pending, c Confirm) resumeOutcome {
	r := newResumeRunner(deps, p)
	// 审批人在这条路径上**只有一个取值**：宿主已用 token 的 sub 覆写过 c.By，这里去掉
	// 空白后同时喂给 step 级审计（gate.approvedBy）与计划级 result 段——同一次批准在两条
	// 记录里写两个样（一个带空格、一个不带）会让对账侧读成两个人。
	by := strings.TrimSpace(c.By)

	// Step 0b：必须自己装模块清单。discover 填 r.modules，moduleBase 靠它反查基地址；
	// 漏了这一步，每一步都会报「动作所属模块不在可达清单里」。
	if _, err := r.discover(ctx); err != nil {
		return resumeOutcome{
			err:       fmt.Errorf("恢复时能力发现失败（不执行，下一 tick 重来）：%w", err),
			retryable: true,
		}
	}

	// 顺序 4a：重跑策略。人在审批期间改了策略，就该按新策略办（收紧即生效）。
	policy, err := r.evaluatePolicies(ctx, p.Plan)
	if err != nil {
		return resumeOutcome{
			err:       fmt.Errorf("恢复时策略评估不可用（不执行，下一 tick 重来）：%w", err),
			retryable: true,
		}
	}
	if policy.Outcome != outcomeApprove && policy.Outcome != outcomeAuto {
		// deny 与任何未知取值都不放行（与 Run 的策略闸门同一口径）：
		// 不执行，但人批过的事实必须留痕——result 段仍带 approvedBy。
		return r.closeWithResult(ctx, p, by, auditStatusError, string(protocol.ClassPolicyDenied),
			fmt.Errorf("恢复时策略已收紧为 %s（decidedBy=%v reasons=%v），不执行；旧计划归档 stale，请重新发起",
				policy.Outcome, policy.DecidedBy, policy.Reasons),
			StateStale)
	}

	// 顺序 4b：重跑授权。grantIds 不落盘（Step 0e）：从 pending 里翻「当初命中的授权」
	// 等于让一次授权替之后所有的调用背书，恢复必须重新问一遍。
	grantIDs := make(map[string]string, len(p.Plan.Steps))
	for _, step := range p.Plan.Steps {
		grant, gerr := r.evaluateGrant(ctx, step)
		if gerr != nil {
			return resumeOutcome{
				err:       fmt.Errorf("恢复时授权评估不可用（不执行，下一 tick 重来）：%w", gerr),
				retryable: true,
			}
		}
		if !grant.Allowed {
			return r.closeWithResult(ctx, p, by, auditStatusError, string(protocol.ClassPolicyDenied),
				fmt.Errorf("恢复时授权已拒绝动作 %s（reason=%s），不执行；旧计划归档 stale",
					step.Action, grant.Reason),
				StateStale)
		}
		grantIDs[step.StepID] = grant.MatchedGrantID
	}

	ordered, err := orderSteps(p.Plan.Steps)
	if err != nil {
		// 计划缺陷：重放也不会变。用 contract_violation 收尾不是为了归罪，而是要把
		// suspend 那条 dispatched 的 unknown 关掉——一个调用都没发出去，结果是确定的。
		return r.closeWithResult(ctx, p, by, auditStatusError, string(protocol.ClassContractViolation),
			fmt.Errorf("计划的 steps 无法定序（不执行）：%w", err),
			StateStale)
	}

	// 审批人：整份计划的 step 级两段审计都要带上它。audit-event.schema.json 规定
	// outcome=approve 时 approvedBy 非空，而恢复执行的每一步都是 approve 处置——
	// 不填就写出一串「批准了却不知道谁批的」记录（真冒烟暴露的 F1）。
	// 与计划级 result 段同一来源（by，见函数开头）。
	approver := nilIfEmpty(by)

	for _, step := range ordered {
		// 能力清单的变化是可判定的终局：模块被摘除/禁用之后，这份计划再也执行不了。
		// **这不是 G4**（G4 由执行时的业务拒绝暴露，见下面的 verdictBusinessRejection）：
		// 它只是「计划作废」，无限重试没有意义。注册表/manifest 读不到另走上面的可重试分支，
		// 所以走到这里的「不在清单里」就是真的被摘掉了。
		if _, ok := r.moduleBase(step.Action); !ok {
			return r.closeWithResult(ctx, p, by, auditStatusError, string(protocol.ClassContractViolation),
				fmt.Errorf("恢复时能力已变化：%s 所属模块不在可达清单里（被摘除或禁用），旧计划作废",
					step.Action),
				StateStale)
		}
		gate := gateDecision{
			outcome:  outcomeApprove, // 人批的：计划级与步骤级的处置都是 approve，不改写成 auto
			policyID: firstNonEmpty(policy.DecidedBy...),
			grantID:  grantIDs[step.StepID],
			// 步骤级审计的 approvedBy 就来自这里（auditBase 不再硬写 null）。
			approvedBy: approver,
		}
		verdict := r.executeStep(ctx, p.Plan, step, gate)
		switch verdict.kind {
		case verdictOK:
			r.taken = append(r.taken, step.Action)
		case verdictBusinessRejection:
			// G4 失效的**唯一可靠来源**就是模块在执行时给的业务拒绝（例如 422 库存不足）：
			// 前置条件成不成立只有模块自己知道，agent 探测一次再执行只是自欺。
			// 不硬跑、也不重新规划（spec §5）：把旧 plan 归档 stale，由人重新发起——
			// 审批期间世界已经变了，原来那份计划的入参多半不该照抄。
			status := auditStatusError
			if len(r.taken) > 0 {
				// 前面几步已经落地：这是「部分成功」，写成整体失败会让对账少算已完成的部分。
				status = auditStatusPartiallyFailed
			}
			return r.closeWithResult(ctx, p, by, status, string(protocol.ClassBusinessRejection),
				fmt.Errorf("恢复时前置条件失效（如库存已被他人改变），动作 %s 被业务拒绝：%s；"+
					"旧计划归档 stale，请重新发起（已执行 %d/%d 步）",
					step.Action, verdict.detail, len(r.taken), len(ordered)),
				StateStale)
		case verdictPolicyDenied:
			// 模块侧的治理闸门拒绝：不换路、不重试。
			return r.closeWithResult(ctx, p, by, auditStatusError, string(protocol.ClassPolicyDenied),
				fmt.Errorf("恢复执行被模块的治理闸门拒绝：%s；旧计划归档 stale", verdict.detail),
				StateStale)
		default:
			// retryable 用尽（结果未定）与契约违反都**不归档**：重放用的是同一个幂等键
			// （planId+stepId 没变），模块会回放首次结果而不是重复副作用。
			return resumeOutcome{
				err:       fmt.Errorf("恢复执行失败（不归档，下一 tick 重来）：%s", verdict.detail),
				retryable: true,
			}
		}
	}

	// 收尾：计划级 result 段（stepId 为空，auditId 复用挂起时那条）带 approvedBy。
	// archiveAs 为空 + cause 为 nil，调用方据此归档 done。
	out := r.closeWithResult(ctx, p, by, auditStatusOK, "", nil, "")
	out.steps = len(r.taken) // 全步成功才走到这里，所以它就是 ordered 的长度
	return out
}

// closeWithResult 写计划级 result 段，并把「归档到哪」一起给出。
//
// 审计写失败时返回**可重试且不归档**：凭据没落地就不该把 pending 推进终态，否则下一
// 个 tick 连「这条 confirm 被接受过」都查不到了。所以这里刻意不让调用处写成
// `_ = r.writePlanResult(...)`——静默失败会把「人批过」这件事整个丢掉。
func (r *runner) closeWithResult(ctx context.Context, p Pending, approvedBy, status, class string,
	cause error, archiveAs PendingState) resumeOutcome {
	if err := r.writePlanResult(ctx, p, approvedBy, status, class); err != nil {
		if cause != nil {
			return resumeOutcome{
				err:       fmt.Errorf("%v；result 审计写失败（不归档，下一 tick 重来）：%w", cause, err),
				retryable: true,
			}
		}
		return resumeOutcome{
			err:       fmt.Errorf("result 审计写失败（不归档，下一 tick 重来）：%w", err),
			retryable: true,
		}
	}
	return resumeOutcome{err: cause, archiveAs: archiveAs}
}

// newResumeRunner 组装一个**只用于恢复**的 runner：没有规划器、没有 request、没有二次挂起。
func newResumeRunner(deps Deps, p Pending) *runner {
	rdeps := deps
	// 尾斜杠在这里统一次：postAuditRecord 直接用 deps.Host 拼 URL，
	// 而 runner 自己的请求走 r.host——两处必须一致，否则一半请求会拼出 //tenon/audit。
	rdeps.Host = strings.TrimRight(strings.TrimSpace(deps.Host), "/")
	// 授权必须重跑在当初的委托人身上：p.By 是挂起时记下的请求人。丢了它，
	// grants:evaluate 会以 agent 自己的身份去问，答的不是同一个问题。
	if by := strings.TrimSpace(p.By); by != "" {
		rdeps.OnBehalfOf = by
	}
	client := deps.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &runner{
		deps: rdeps,
		host: rdeps.Host,
		event: DomainEvent{
			EventID:       p.TriggerEventID,
			CorrelationID: p.Plan.Trigger.CorrelationID,
			// 事件的 type 没落盘，恢复路径也不用它（审计只锚 eventId/correlationId、
			// 界面反应用 subject）：与其编一个 "tenon.request"，不如留空。
			Subject: p.Subject,
		},
		correlationID: p.Plan.Trigger.CorrelationID,
		planID:        p.PlanID,
		client:        client,
	}
}

// writePlanResult 写计划级 result 段（stepId 为空，auditId 复用挂起时那条 dispatched）。
//
// 不复用 auditBase：那里把 decision.approvedBy 硬写成 null（auto 路径的正确语义），
// 而恢复路径的全部意义就是「有人批过」。所以 approvedBy 必须在这里显式填——哪怕
// 策略在恢复时把计划拒了：他确实批了，是策略后来收紧的。
func (r *runner) writePlanResult(ctx context.Context, p Pending, approvedBy, status, errClass string) error {
	by := approvedBy
	rec := auditRecord{
		AuditID: p.DispatchedAuditID,
		Stage:   auditStageResult,
		At:      time.Now().UTC().Format(time.RFC3339),
		Actor:   auditActor{Type: "agent", ID: r.deps.AgentID, OnBehalfOf: nilIfEmpty(p.By)},
		Trigger: auditTrigger{EventID: nilIfEmpty(p.TriggerEventID), CorrelationID: nilIfEmpty(p.Plan.Trigger.CorrelationID)},
		PlanID:  nilIfEmpty(p.PlanID),
		Decision: auditDecision{
			// outcome 是**闸门处置**（人批的还是自动放行的），不是策略在恢复时的结论：
			// 这份计划是人批的，所以恒为 approve，绝不改写成 auto。
			Outcome:    outcomeApprove,
			ApprovedBy: &by,
		},
		Result: auditResult{Status: status, ErrorClass: nilIfEmpty(errClass)},
		// subject 从 pending 里回来（Step 0c），恢复后的审计才不会丢实体。
		Effects: auditEffects{Entities: r.entities()},
		// action/inputHash 刻意留空：计划级记录的已知偏离，见 spec §4。
	}
	return postAuditRecord(ctx, r.deps, rec)
}

// archivePending 归档，并容忍「已经被别人归档」：Archive 不幂等，pending 不在盘上时
// 返回「归档时找不到 <id>」（§0.1.2）。那种情况按已归档处理，只记一行日志。
//
// **只归档、不推公告板**：终态覆盖见 board.go 的 finishTerminal。注意 RepushBoard 每 tick
// 只重推 pending/ 里的活条目、看不见归档件——所以「下一个 tick 会自动补推终态」是假的，
// 终态必须由归档方自己推（需要一个带凭证的 HostClient，接线在 cmd/agent，尚未落地）。
func archivePending(store *StateStore, planID string, state PendingState) error {
	err := store.Archive(planID, state)
	if err == nil {
		return nil
	}
	if _, ok, lerr := store.Load(planID); lerr == nil && !ok {
		log.Printf("agent: planId %s 归档为 %s 时 pending 已不在盘上（按已归档处理）：%v", planID, state, err)
		return nil
	}
	return fmt.Errorf("归档 %s 为 %s 失败：%w", planID, state, err)
}

// rolesIntersect 判断审批人角色与计划要求的角色是否有交集。
//
// requiredRoles 为空时交集必然为空 —— 这是**故意**的 fail-closed：policy 没给出
// approvers 的 approve 计划不该由「谁都行」来兜底（那等于把审批降级成点击）。
func rolesIntersect(have, need []string) bool {
	for _, n := range need {
		for _, h := range have {
			if h == n {
				return true
			}
		}
	}
	return false
}
