package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tenon.dev/internal/controlplane/audit"
	"tenon.dev/internal/controlplane/policy"
	"tenon.dev/internal/protocol"
)

// defaultTicketTTL 是票据缺省有效期。票只能"当场用"：有效期越长，
// 一张被截获的票的可用窗口就越大。
const defaultTicketTTL = 30 * time.Second

func ticketTTLOrDefault(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultTicketTTL
	}
	if ttl > protocol.TicketMaxTTL {
		return protocol.TicketMaxTTL
	}
	return ttl
}

// ticketIssueRequest 是 POST /tenon/tickets:issue 的请求体。
//
// Args 是**原始字节**：票据里的 argsHash 必须绑定调用方即将发出去的那串字节，
// 而不是宿主解析后重新序列化的结果。解析只用于取 qty 做配额，不参与摘要。
type ticketIssueRequest struct {
	PlanID     string          `json:"planId"`
	StepID     string          `json:"stepId"`
	Action     string          `json:"action"`
	Args       json.RawMessage `json:"args"`
	Agent      string          `json:"agent"`
	OnBehalfOf string          `json:"onBehalfOf"`
}

type ticketIssueResponse struct {
	Ticket  string `json:"ticket"`
	AuditID string `json:"auditId"`
	Exp     int64  `json:"exp"`
}

// serveTicketIssue 是「系统裁决」的落地点：策略与授权都通过之后，
// 宿主先写 dispatched 审计，再签一张票。三步任一失败都不签票。
//
// 为什么顺序不能换：审计前置的意义是"没有记录就没有执行"。如果先签票再补审计，
// 审计写失败时票已经在 agent 手上，宿主唯一的补救手段只剩"希望它别用"。
func (s *Server) serveTicketIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	claims, ok := s.bearerClaims(w, r)
	if !ok {
		return // bearerClaims 已写好 401
	}
	if !contains(claims.Roles, roleAgent) {
		writeTenonError(w, *newError(protocol.ClassPolicyDenied, "AGENT_ROLE_REQUIRED",
			fmt.Sprintf("签发票据需要 %s 角色，当前角色 %v", roleAgent, claims.Roles)))
		return
	}
	if len(s.ticketKey) != 64 {
		writeTenonError(w, *newError(protocol.ClassRetryable, "TICKET_SIGNING_DISABLED",
			"宿主未配置签名私钥，拒绝签发（不降级为无票执行）"))
		return
	}

	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req ticketIssueRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {planId, stepId, action, args, agent, onBehalfOf}："+err.Error()))
		return
	}
	if perr := requireTicketFields(req); perr != nil {
		writeTenonError(w, *perr)
		return
	}

	args := req.Args
	if len(strings.TrimSpace(string(args))) == 0 {
		args = json.RawMessage("{}")
	}

	// 闸门 1：策略。求值出错按 fail-closed 处理，不签票。
	eval, err := s.policyEngine.Evaluate(r.Context(), policy.PolicyRequest{
		Action: req.Action,
		Args:   args,
		Origin: "request",
	})
	if err != nil {
		writeTenonError(w, *newError(protocol.ClassRetryable, "POLICY_EVALUATION_FAILED",
			"策略求值失败（不构成放行）："+err.Error()))
		return
	}
	switch eval.Outcome {
	case policy.OutcomeDeny:
		e := newError(protocol.ClassPolicyDenied, "POLICY_DENIED",
			fmt.Sprintf("策略拒绝动作 %s（%v）", req.Action, eval.Reasons))
		e.Details = map[string]any{"decidedBy": eval.DecidedBy, "reasons": eval.Reasons}
		writeTenonError(w, *e)
		return
	case policy.OutcomeApprove:
		// 批准依据 = 该计划公布时冻结的步骤参数摘要。没公布过就不签票：
		// "没人批过"与"批过但参数被换掉"都必须是明确的拒绝理由，不能合流成一句"需要批准"。
		want, ok := s.pending.approvedArgs(req.PlanID, req.StepID)
		if !ok {
			e := newError(protocol.ClassPolicyDenied, "APPROVAL_REQUIRED",
				fmt.Sprintf("动作 %s 需要人工批准，但该步骤没有公布过的参数快照", req.Action))
			e.Details = map[string]any{"approvers": eval.Approvers, "planId": req.PlanID, "stepId": req.StepID}
			writeTenonError(w, *e)
			return
		}
		got := protocol.ArgsHash(req.Args)
		if want != got {
			e := newError(protocol.ClassPolicyDenied, protocol.CodeTicketNotApproved,
				"签票参数与人批准的那一版不一致：驳回本次执行，重新挂起后再批")
			e.Details = map[string]any{
				"approvedArgsHash": want, "requestedArgsHash": got,
				"planId": req.PlanID, "stepId": req.StepID,
			}
			writeTenonError(w, *e)
			return
		}
	}

	// 闸门 2：授权。这里扣配额（agent 侧的预检应传 charge:false，不再重复扣）。
	amount, perr := requestAmount(args)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	grantResult, err := s.grantEngine.Evaluate(req.Agent, req.OnBehalfOf, req.Action, amount)
	if err != nil {
		writeTenonError(w, *newError(protocol.ClassRetryable, "GRANT_EVALUATION_FAILED",
			"授权评估失败（不构成放行）："+err.Error()))
		return
	}
	if !grantResult.Allowed {
		e := newError(protocol.ClassPolicyDenied, "GRANT_DENIED",
			fmt.Sprintf("授权不足：%s", grantResult.Reason))
		e.Details = map[string]any{"reason": grantResult.Reason}
		writeTenonError(w, *e)
		return
	}

	// 闸门 3：审计前置。写失败即拒绝签票，绝不"先签了再说"。
	now := time.Now().UTC()
	auditID := newID("aud_")
	actor, _ := json.Marshal(map[string]string{"type": "agent", "id": claims.Subject})
	decision, _ := json.Marshal(map[string]any{
		"outcome":   eval.Outcome,
		"decidedBy": eval.DecidedBy,
		"grantId":   grantResult.MatchedGrantID,
	})
	rec := audit.AuditRecord{
		AuditID:        auditID,
		Stage:          audit.StageDispatched,
		At:             now,
		Actor:          actor,
		PlanID:         req.PlanID,
		StepID:         req.StepID,
		Action:         req.Action,
		Decision:       decision,
		InputHash:      protocol.ArgsHash(args),
		IdempotencyKey: protocol.IdempotencyKey(req.PlanID, req.StepID),
	}
	if err := s.auditStore.Append(rec); err != nil {
		writeTenonError(w, *newError(protocol.ClassRetryable, "AUDIT_WRITE_FAILED",
			"dispatched 审计写失败，拒绝签票："+err.Error()))
		return
	}

	var grantID *string
	if grantResult.MatchedGrantID != "" {
		id := grantResult.MatchedGrantID
		grantID = &id
	}
	ticket := protocol.Ticket{
		TicketID:       newID("tkt_"),
		Module:         moduleOf(req.Action),
		Action:         req.Action,
		ArgsHash:       protocol.ArgsHash(args),
		IdempotencyKey: protocol.IdempotencyKey(req.PlanID, req.StepID),
		PlanID:         req.PlanID,
		StepID:         req.StepID,
		AuditID:        auditID,
		GrantID:        grantID,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(s.ticketTTL).Unix(),
	}
	token, err := protocol.SignTicket(s.ticketKey, ticket)
	if err != nil {
		// 审计已写但票没签出来：这条 dispatched 会以 result 缺失的形式留在链上，
		// 对账时属于"宿主有、模块无"，正好暴露"这一笔没执行成功"。
		writeTenonError(w, *newError(protocol.ClassRetryable, "TICKET_SIGN_FAILED",
			"票签发失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, ticketIssueResponse{Ticket: token, AuditID: auditID, Exp: ticket.ExpiresAt})
}

func requireTicketFields(req ticketIssueRequest) *protocol.Error {
	missing := make([]string, 0, 5)
	for name, v := range map[string]string{
		"planId": req.PlanID, "stepId": req.StepID, "action": req.Action,
		"agent": req.Agent, "onBehalfOf": req.OnBehalfOf,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	e := newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
		fmt.Sprintf("签票请求缺少必填字段 %v", missing))
	e.Details = map[string]any{"missing": missing}
	return e
}

// moduleOf 取动作 id 的模块前缀（<module>.<Action>）。取不到时原样返回：
// 票据里的 mod 与请求里的 action 由模块各自比对，宿主不在这里替它猜。
func moduleOf(action string) string {
	if i := strings.Index(action, "."); i > 0 {
		return action[:i]
	}
	return action
}
