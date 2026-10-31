package controlplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenon.dev/internal/controlplane/audit"
	"tenon.dev/internal/controlplane/policy"
	"tenon.dev/internal/protocol"
)

// ticketPolicies 只放票据用例需要的三条：小额自动、大额需批、采购一律拒。
func ticketPolicies() []policy.PolicyRule {
	return []policy.PolicyRule{
		{
			PolicyID: "pol-deduct-auto",
			Priority: 100,
			Match:    policy.PolicyMatch{Module: "inventory", Actions: []string{"inventory.DeductStock"}},
			When:     json.RawMessage(`{"all":[{"arg.qty":{"<=":10}}]}`),
			Outcome:  policy.OutcomeAuto,
		},
		{
			PolicyID:  "pol-deduct-approve",
			Priority:  90,
			Match:     policy.PolicyMatch{Module: "inventory", Actions: []string{"inventory.DeductStock"}},
			When:      json.RawMessage(`{"all":[{"arg.qty":{">":10}}]}`),
			Outcome:   policy.OutcomeApprove,
			Approvers: []string{"role:warehouse_manager"},
		},
		{
			PolicyID:  "pol-procurement-deny",
			Priority:  80,
			Match:     policy.PolicyMatch{Module: "procurement", Actions: []string{"procurement.CreatePurchaseRequest"}},
			When:      json.RawMessage(`{"all":[]}`),
			Outcome:   policy.OutcomeDeny,
			Approvers: nil,
		},
	}
}

type ticketFixture struct {
	srv   *Server
	ts    *httptest.Server
	token string
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newTicketFixture(t *testing.T) ticketFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成签名密钥：%v", err)
	}
	grants, subjects := testGrants()
	srv := newTestServer(t, Config{
		Policies:         ticketPolicies(),
		Grants:           grants,
		Subjects:         subjects,
		AuditPath:        filepath.Join(t.TempDir(), "audit.jsonl"),
		TicketSigningKey: priv,
	})
	ts := serveHost(t, srv)
	token, err := SignToken([]byte("test-secret"), "ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("签 token：%v", err)
	}
	return ticketFixture{srv: srv, ts: ts, token: token, pub: pub, priv: priv}
}

// postAuth 发带 Bearer 的 JSON POST，返回状态码与响应原文。
func postAuth(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败：%v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应：%v", err)
	}
	return resp.StatusCode, string(raw)
}

const deductBody = `{"planId":"plan_t1","stepId":"s1","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":3},"agent":"ag_fulfillment","onBehalfOf":"user:alice"}`

type issueResponse struct {
	Ticket  string `json:"ticket"`
	AuditID string `json:"auditId"`
	Exp     int64  `json:"exp"`
}

// 1. 没有 Bearer 就签不出票——否则"运维拿着配置 curl"只是从模块挪到宿主。
func TestTicketIssueRequiresBearer(t *testing.T) {
	f := newTicketFixture(t)
	status, body := postAuth(t, f.ts.URL+"/tenon/tickets:issue", "", deductBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，得到 %d：%s", status, body)
	}
	if !strings.Contains(body, "AUTH_REQUIRED") {
		t.Fatalf("响应应说明缺凭据，得到 %s", body)
	}
	recs, err := f.srv.auditStore.Query(audit.Query{PlanID: "plan_t1"})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("被拒的签票请求不应留下审计，实际 %d 条", len(recs))
	}
}

// 2. 策略 deny 时不签票，且不留 dispatched。
func TestTicketIssuePolicyDeny(t *testing.T) {
	f := newTicketFixture(t)
	body := `{"planId":"plan_t2","stepId":"s1","action":"procurement.CreatePurchaseRequest","args":{"sku":"STD-1001","qty":3},"agent":"ag_fulfillment","onBehalfOf":"user:alice"}`
	status, resp := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token, body)
	if status != http.StatusForbidden {
		t.Fatalf("策略拒绝应 403，得到 %d：%s", status, resp)
	}
	if !strings.Contains(resp, "POLICY_DENIED") {
		t.Fatalf("响应应给出 POLICY_DENIED，得到 %s", resp)
	}
	recs, _ := f.srv.auditStore.Query(audit.Query{PlanID: "plan_t2"})
	if len(recs) != 0 {
		t.Fatalf("策略拒绝不应留下审计，实际 %d 条", len(recs))
	}
}

// 3. 需审批的动作，agent token 单独换不到票：批准证据还没落地（T6）。
func TestTicketIssueApproveNeedsApproval(t *testing.T) {
	f := newTicketFixture(t)
	body := strings.Replace(deductBody, `"qty":3`, `"qty":12`, 1)
	status, resp := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token, body)
	if status != http.StatusForbidden {
		t.Fatalf("需审批动作应 403，得到 %d：%s", status, resp)
	}
	if !strings.Contains(resp, "APPROVAL_REQUIRED") {
		t.Fatalf("响应应给出 APPROVAL_REQUIRED，得到 %s", resp)
	}
	recs, _ := f.srv.auditStore.Query(audit.Query{PlanID: "plan_t1"})
	if len(recs) != 0 {
		t.Fatalf("缺批准证据时不应留下审计，实际 %d 条", len(recs))
	}
}

// 4. 自动档：签票成功，且 dispatched 已在签票时由宿主写好。
func TestTicketIssueWritesDispatchedAndSigns(t *testing.T) {
	f := newTicketFixture(t)
	status, body := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token, deductBody)
	if status != http.StatusOK {
		t.Fatalf("自动档应签票成功，得到 %d：%s", status, body)
	}
	var got issueResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 {ticket,auditId,exp}：%v (%s)", err, body)
	}
	if got.Ticket == "" || got.AuditID == "" || got.Exp == 0 {
		t.Fatalf("响应字段不全：%+v", got)
	}

	ticket, err := protocol.VerifyTicket(protocol.KeySet{protocol.KeyID(f.pub): f.pub}, got.Ticket, time.Now().UTC(), protocol.SignatureTolerance)
	if err != nil {
		t.Fatalf("签出的票验不过：%v", err)
	}
	if ticket.Module != "inventory" || ticket.Action != "inventory.DeductStock" {
		t.Fatalf("票绑定错误：%+v", ticket)
	}
	if ticket.AuditID != got.AuditID {
		t.Fatalf("票里的 aud %q 与响应 auditId %q 不一致", ticket.AuditID, got.AuditID)
	}
	if want := protocol.ArgsHash([]byte(`{"sku":"STD-1001","qty":3}`)); ticket.ArgsHash != want {
		t.Fatalf("argsHash = %q，期望 %q", ticket.ArgsHash, want)
	}
	if ticket.IdempotencyKey != protocol.IdempotencyKey("plan_t1", "s1") {
		t.Fatalf("幂等键 = %q", ticket.IdempotencyKey)
	}
	if ttl := ticket.ExpiresAt - ticket.IssuedAt; ttl <= 0 || ttl > 60 {
		t.Fatalf("有效期 %ds 超出 (0,60]", ttl)
	}

	recs, err := f.srv.auditStore.Query(audit.Query{PlanID: "plan_t1"})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(recs) != 1 || recs[0].Stage != audit.StageDispatched || recs[0].AuditID != got.AuditID {
		t.Fatalf("签票时必须已写一条 dispatched，实际 %+v", recs)
	}
	if recs[0].StepID != "s1" || recs[0].IdempotencyKey != protocol.IdempotencyKey("plan_t1", "s1") {
		t.Fatalf("dispatched 审计字段不对：%+v", recs[0])
	}
}

// 5. 配额只在签发时扣：预检（charge:false）调多少次都不动余量。
func TestTicketIssueChargesQuotaOnce(t *testing.T) {
	f := newTicketFixture(t)
	evalBody := `{"agent":"ag_fulfillment","onBehalfOf":"user:alice","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":5},"charge":false}`

	remaining := func() float64 {
		t.Helper()
		status, body := postAuth(t, f.ts.URL+"/tenon/grants:evaluate", f.token, evalBody)
		if status != http.StatusOK {
			t.Fatalf("预检失败 %d：%s", status, body)
		}
		var res struct {
			Quota *struct {
				Remaining float64 `json:"remaining"`
			} `json:"quota"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil || res.Quota == nil {
			t.Fatalf("预检响应缺 quota：%s", body)
		}
		return res.Quota.Remaining
	}

	before := remaining()
	if again := remaining(); again != before {
		t.Fatalf("charge:false 的预检不应扣配额：%v → %v", before, again)
	}

	status, body := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token, deductBody)
	if status != http.StatusOK {
		t.Fatalf("签票失败 %d：%s", status, body)
	}
	after := remaining()
	if want := before - 3; after != want {
		t.Fatalf("签票应扣一次 3：%v → %v，期望 %v", before, after, want)
	}
}

// 6. 步骤级 dispatched 只能由签发流程产生，agent 自己写必须被拒。
func TestAuditAppendRejectsStepDispatched(t *testing.T) {
	f := newTicketFixture(t)
	body := `{"auditId":"aud_forged","stage":"dispatched","at":"2026-10-31T00:00:00Z","actor":{"type":"agent","id":"ag_fulfillment"},"planId":"plan_t6","stepId":"s1","action":"inventory.DeductStock"}`
	status, resp := postAuth(t, f.ts.URL+"/tenon/audit", f.token, body)
	if status != http.StatusForbidden {
		t.Fatalf("步骤级 dispatched 应 403，得到 %d：%s", status, resp)
	}
	if !strings.Contains(resp, "AUDIT_STAGE_FORBIDDEN") {
		t.Fatalf("响应应给出 AUDIT_STAGE_FORBIDDEN，得到 %s", resp)
	}
}

// 7. 计划级 dispatched 是挂起/续跑链路的一环，不能被误伤。
func TestAuditAppendAllowsPlanDispatched(t *testing.T) {
	f := newTicketFixture(t)
	body := `{"auditId":"aud_plan","stage":"dispatched","at":"2026-10-31T00:00:00Z","actor":{"type":"agent","id":"ag_fulfillment"},"planId":"plan_t7","stepId":"","action":"plan"}`
	status, resp := postAuth(t, f.ts.URL+"/tenon/audit", f.token, body)
	if status != http.StatusOK {
		t.Fatalf("计划级 dispatched 应放行，得到 %d：%s", status, resp)
	}
	recs, err := f.srv.auditStore.Query(audit.Query{PlanID: "plan_t7"})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(recs) != 1 || recs[0].Stage != audit.StageDispatched {
		t.Fatalf("计划级 dispatched 没落盘：%+v", recs)
	}
}
