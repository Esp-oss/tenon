package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenon.dev/internal/controlplane/grant"
	"tenon.dev/internal/controlplane/policy"
	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/protocol"
)

// ---------- 脚手架 ----------

// newTestHost 构造带默认策略/授权的控制平面：规则与授权均与 config/*.dev.json 同构，
// 注册表里有 inventory 与 procurement，测试面（admin）关闭。
func newTestHost(t *testing.T) *Server {
	t.Helper()
	grants, subjects := testGrants()
	return newTestServer(t, Config{
		Policies:  testPolicies(),
		Grants:    grants,
		Subjects:  subjects,
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		Modules: []registry.ModuleEntry{
			{ID: "inventory", BaseURL: "http://127.0.0.1:8081"},
			{ID: "procurement", BaseURL: "http://127.0.0.1:8082"},
		},
	})
}

// newTestServer 装配一个 Server 并登记清理。
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("装配控制平面失败：%v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("关闭审计存储失败：%v", err)
		}
	})
	return srv
}

// serveHost 起一个 httptest 服务并登记清理。
func serveHost(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// testPolicies 与 config/policies.dev.json 同构：DeductStock 且 qty>10 → approve，
// CreatePurchaseRequest → approve，其余无命中即 deny。
func testPolicies() []policy.PolicyRule {
	return []policy.PolicyRule{
		{
			PolicyID: "pol-inventory-deduct-approve",
			Priority: 100,
			Match:    policy.PolicyMatch{Module: "inventory", Actions: []string{"inventory.DeductStock"}},
			When:     json.RawMessage(`{"all":[{"arg.qty":{">":10}}]}`),
			Outcome:  policy.OutcomeApprove,
			Approvers: []string{
				"role:warehouse_manager",
			},
		},
		{
			PolicyID:  "pol-procurement-create-approve",
			Priority:  100,
			Match:     policy.PolicyMatch{Module: "procurement", Actions: []string{"procurement.CreatePurchaseRequest"}},
			When:      json.RawMessage(`{"all":[]}`),
			Outcome:   policy.OutcomeApprove,
			Approvers: []string{"role:procurement_manager"},
		},
	}
}

// testGrants 与 config/grants.dev.json 同构：ag_fulfillment 在 1 小时窗口内有 100 units，
// 委托人 user:alice 也拥有同样的动作（交集规则的下半部分）。
func testGrants() ([]grant.Grant, map[string][]string) {
	actions := []string{
		"inventory.GetStock",
		"inventory.DeductStock",
		"inventory.ReleaseStock",
	}
	grants := []grant.Grant{{
		GrantID: "gr-dev-fulfillment",
		Agent:   "ag_fulfillment",
		Allow: []grant.GrantEntry{
			{
				Module:      "inventory",
				Actions:     actions,
				Constraints: &grant.GrantConstraints{Window: "PT1H", MaxUnitsPerWindow: 100},
			},
			{
				Module:      "procurement",
				Actions:     []string{"procurement.CreatePurchaseRequest"},
				Constraints: &grant.GrantConstraints{Window: "PT1H", MaxUnitsPerWindow: 100},
			},
		},
		ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	subjects := map[string][]string{
		"user:alice": append(append([]string{}, actions...), "procurement.CreatePurchaseRequest"),
	}
	return grants, subjects
}

// postJSON 发一个 JSON POST，返回状态码与响应原文。
func postJSON(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s 失败：%v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp.StatusCode, string(raw)
}

// getJSON 发一个 GET，返回状态码与响应原文。
func getJSON(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败：%v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp.StatusCode, string(raw)
}

// decodeInto 把响应原文解到 v 里；解不动就当成断言失败（顺带打出原文）。
func decodeInto(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("响应不是预期 JSON：%v（原文 %s）", err, raw)
	}
}

// tenonError 解出错误信封。
func tenonError(t *testing.T, raw string) protocol.Error {
	t.Helper()
	var env protocol.ErrorEnvelope
	decodeInto(t, raw, &env)
	return env.TenonError
}

// wantStrings 断言字符串切片相等（nil 与空切片视为相等，便于比较 reasons/approvers）。
func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v，期望 %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v，期望 %v", what, got, want)
		}
	}
}

// planBody 拼一个 policies:evaluate 请求体。
func planBody(steps string) string {
	return `{"plan":{"planId":"pln_test","steps":[` + steps + `]}}`
}

// ---------- 计划里的 TestHostEndpoints ----------

func TestHostEndpoints(t *testing.T) {
	h := newTestHost(t) // 构造带默认策略/授权的 controlplane server
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// 无命中策略 → deny（fail-closed）
	st, body := postJSON(t, srv.URL+"/tenon/policies:evaluate", `{"plan":{}}`)
	if st != 200 || !strings.Contains(body, `"outcome":"deny"`) {
		t.Fatalf("C5 语义不成立：%d %s", st, body)
	}

	// 审计写入 + 查询
	st, _ = postJSON(t, srv.URL+"/tenon/audit", `{"auditId":"aud_1","stage":"dispatched","planId":"pln_1"}`)
	if st != 200 {
		t.Fatalf("写审计失败：%d", st)
	}
	st, body = getJSON(t, srv.URL+"/tenon/audit?planId=pln_1")
	if !strings.Contains(body, `"aud_1"`) {
		t.Fatalf("审计查询未命中：%s", body)
	}

	// 注册表
	st, body = getJSON(t, srv.URL+"/tenon/modules")
	if st != 200 || !strings.Contains(body, "inventory") {
		t.Fatalf("注册表读取失败：%s", body)
	}

	// TEST_MODE 关闭时 admin 端点不存在
	if st, _ := postJSON(t, srv.URL+"/tenon/admin/modules", `{}`); st != 404 {
		t.Fatalf("非测试模式不应暴露 admin，得到 %d", st)
	}
}

// ---------- 策略评估 ----------

// TestPoliciesEvaluateStrictestAcrossSteps 固定「整份 plan 取最严」：逐 step 求值后按
// deny > approve > auto 合并，同级的 decidedBy/approvers/reasons 取并集。
func TestPoliciesEvaluateStrictestAcrossSteps(t *testing.T) {
	rules := []policy.PolicyRule{
		{
			PolicyID: "p-auto",
			Priority: 10,
			Match:    policy.PolicyMatch{Module: "inventory", Actions: []string{"inventory.GetStock"}},
			Outcome:  policy.OutcomeAuto,
		},
		{
			PolicyID:  "p-approve",
			Priority:  100,
			Match:     policy.PolicyMatch{Module: "inventory", Actions: []string{"inventory.DeductStock"}},
			When:      json.RawMessage(`{"all":[{"arg.qty":{">":10}}]}`),
			Outcome:   policy.OutcomeApprove,
			Approvers: []string{"role:warehouse_manager"},
		},
		{
			PolicyID: "p-deny",
			Priority: 1,
			Match:    policy.PolicyMatch{Module: "procurement", Actions: []string{"procurement.CreatePurchaseRequest"}},
			Outcome:  policy.OutcomeDeny,
		},
	}
	srv := newTestServer(t, Config{
		Policies:  rules,
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
	})
	ts := serveHost(t, srv)

	getStock := `{"stepId":"s1","action":"inventory.GetStock","args":{"sku":"STD-1001"}}`
	deductBig := `{"stepId":"s2","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":12}}`
	deductSmall := `{"stepId":"s3","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":3}}`
	createPR := `{"stepId":"s4","action":"procurement.CreatePurchaseRequest","args":{"sku":"STD-1002","qty":12}}`
	badAction := `{"stepId":"s5","action":"notanaction"}`

	cases := []struct {
		name          string
		steps         []string
		wantOutcome   string
		wantDecidedBy []string
		wantApprovers []string
		wantReasons   []string
	}{
		{
			name:          "单步 auto",
			steps:         []string{getStock},
			wantOutcome:   policy.OutcomeAuto,
			wantDecidedBy: []string{"p-auto"},
			wantReasons:   []string{policy.ReasonAuto},
		},
		{
			name:          "auto + approve 取 approve",
			steps:         []string{getStock, deductBig},
			wantOutcome:   policy.OutcomeApprove,
			wantDecidedBy: []string{"p-approve"},
			wantApprovers: []string{"role:warehouse_manager"},
			wantReasons:   []string{policy.ReasonApprovalRequired},
		},
		{
			name:          "auto + approve + deny 取 deny",
			steps:         []string{getStock, deductBig, createPR},
			wantOutcome:   policy.OutcomeDeny,
			wantDecidedBy: []string{"p-deny"},
			wantReasons:   []string{policy.ReasonDeny},
		},
		{
			name:          "approve + deny 取 deny",
			steps:         []string{deductBig, createPR},
			wantOutcome:   policy.OutcomeDeny,
			wantDecidedBy: []string{"p-deny"},
			wantReasons:   []string{policy.ReasonDeny},
		},
		{
			name:          "两步同一规则命中时 decidedBy 去重",
			steps:         []string{createPR, createPR},
			wantOutcome:   policy.OutcomeDeny,
			wantDecidedBy: []string{"p-deny"},
			wantReasons:   []string{policy.ReasonDeny},
		},
		{
			name:          "无规则命中的步骤把整份 plan 拉成 deny",
			steps:         []string{getStock, deductSmall},
			wantOutcome:   policy.OutcomeDeny,
			wantDecidedBy: []string{},
			wantReasons:   []string{policy.ReasonNoMatch},
		},
		{
			name:          "动作 id 不合法 → fail-closed",
			steps:         []string{badAction},
			wantOutcome:   policy.OutcomeDeny,
			wantDecidedBy: []string{},
			wantReasons:   []string{reasonEvaluationFailed},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, body := postJSON(t, ts.URL+"/tenon/policies:evaluate", planBody(strings.Join(tc.steps, ",")))
			if st != 200 {
				t.Fatalf("状态码 = %d，期望 200（原文 %s）", st, body)
			}
			var got policy.PolicyEvaluation
			decodeInto(t, body, &got)
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q，期望 %q（原文 %s）", got.Outcome, tc.wantOutcome, body)
			}
			wantStrings(t, "decidedBy", got.DecidedBy, tc.wantDecidedBy)
			wantStrings(t, "reasons", got.Reasons, tc.wantReasons)
			wantStrings(t, "approvers", got.Approvers, tc.wantApprovers)
		})
	}
}

// TestPoliciesEvaluateEmptyPlan 固定零步计划的回答：deny + NO_POLICY_MATCH_DEFAULT_DENY。
// 零步计划没有可执行语义，放它过闸门等于给「什么都不做」发通行证。
func TestPoliciesEvaluateEmptyPlan(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	bodies := map[string]string{
		"plan 是空对象":   `{"plan":{}}`,
		"steps 是空数组":  `{"plan":{"planId":"pln_x","steps":[]}}`,
		"plan 是 null": `{"plan":null}`,
		"请求体是空对象":     `{}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			st, raw := postJSON(t, ts.URL+"/tenon/policies:evaluate", body)
			if st != 200 {
				t.Fatalf("状态码 = %d，期望 200（原文 %s）", st, raw)
			}
			var got policy.PolicyEvaluation
			decodeInto(t, raw, &got)
			if got.Outcome != policy.OutcomeDeny {
				t.Fatalf("outcome = %q，期望 deny（原文 %s）", got.Outcome, raw)
			}
			wantStrings(t, "decidedBy", got.DecidedBy, []string{})
			wantStrings(t, "reasons", got.Reasons, []string{policy.ReasonNoMatch})
			// 空 decidedBy 必须是 [] 而不是 null：null 会让调用方反序列化路径分叉。
			if !strings.Contains(raw, `"decidedBy":[]`) {
				t.Fatalf("decidedBy 应是空数组而不是 null：%s", raw)
			}
		})
	}
}

// TestPoliciesEvaluateRejectsBadPlan 固定「报文不合法回 400，而不是伪装成一次 deny」。
func TestPoliciesEvaluateRejectsBadPlan(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	cases := map[string]string{
		"plan 不是对象":  `{"plan":[]}`,
		"steps 不是数组": `{"plan":{"steps":"s1"}}`,
		"请求体不是对象":    `[1,2,3]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			st, raw := postJSON(t, ts.URL+"/tenon/policies:evaluate", body)
			if st != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400（原文 %s）", st, raw)
			}
			if code := tenonError(t, raw).Code; code != "SCHEMA_MISMATCH" {
				t.Fatalf("错误码 = %q，期望 SCHEMA_MISMATCH（原文 %s）", code, raw)
			}
		})
	}
}

// ---------- 授权评估 ----------

func TestGrantsEvaluate(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	deduct := func(qty string) string {
		return `{"agent":"ag_fulfillment","onBehalfOf":"user:alice","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":` + qty + `}}`
	}

	// 首次放行：配额余量 100-12=88。
	st, body := postJSON(t, ts.URL+"/tenon/grants:evaluate", deduct("12"))
	if st != 200 {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", st, body)
	}
	var first grant.GrantResult
	decodeInto(t, body, &first)
	if !first.Allowed || first.MatchedGrantID != "gr-dev-fulfillment" {
		t.Fatalf("应放行并命中 gr-dev-fulfillment，得到 %+v", first)
	}
	if first.Quota == nil || first.Quota.Remaining != 88 || first.Quota.Unit != grant.UnitQuantity {
		t.Fatalf("配额余量应为 88 quantity，得到 %+v", first.Quota)
	}

	// 再次放行：配额按业务量累计，余量 76（证明 amount 来自 args.qty）。
	_, body = postJSON(t, ts.URL+"/tenon/grants:evaluate", deduct("12"))
	var second grant.GrantResult
	decodeInto(t, body, &second)
	if second.Quota == nil || second.Quota.Remaining != 76 {
		t.Fatalf("第二次调用后余量应为 76，得到 %+v（原文 %s）", second.Quota, body)
	}

	// 委托人权限不足：交集为空 → SUBJECT_NOT_PERMITTED，且报文不得夹带 matchedGrantId/quota
	// （grant.schema.json 的 GrantDeniedEvaluation 是 additionalProperties=false）。
	st, body = postJSON(t, ts.URL+"/tenon/grants:evaluate",
		`{"agent":"ag_fulfillment","onBehalfOf":"user:bob","action":"inventory.DeductStock","args":{"qty":1}}`)
	if st != 200 {
		t.Fatalf("拒绝也应是 200（评估不是执行），得到 %d（原文 %s）", st, body)
	}
	var denied grant.GrantResult
	decodeInto(t, body, &denied)
	if denied.Allowed || denied.Reason != grant.ReasonSubjectNotPermitted {
		t.Fatalf("应因委托人权限不足被拒，得到 %+v", denied)
	}
	if strings.Contains(body, "matchedGrantId") || strings.Contains(body, "quota") {
		t.Fatalf("拒绝报文不得夹带领授权/配额字段：%s", body)
	}

	// 动作不在白名单 → NOT_IN_GRANT。
	_, body = postJSON(t, ts.URL+"/tenon/grants:evaluate",
		`{"agent":"ag_fulfillment","onBehalfOf":"user:alice","action":"inventory.DeleteEverything","args":{}}`)
	var notIn grant.GrantResult
	decodeInto(t, body, &notIn)
	if notIn.Allowed || notIn.Reason != grant.ReasonNotInGrant {
		t.Fatalf("应因动作不在授权内被拒，得到 %+v", notIn)
	}

	// 缺必填字段 → 400（接入方 bug，不是一次授权判定）。
	st, body = postJSON(t, ts.URL+"/tenon/grants:evaluate", `{"onBehalfOf":"user:alice","action":"inventory.DeductStock"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("缺 agent 应回 400，得到 %d（原文 %s）", st, body)
	}
	var env protocol.ErrorEnvelope
	decodeInto(t, body, &env)
	if missing, ok := env.TenonError.Details["missing"].([]any); !ok || len(missing) != 1 || missing[0] != "agent" {
		t.Fatalf("details.missing 应指出 agent：%s", body)
	}

	// args 不是对象 → 400（grant.schema.json 的 args 是 JsonObject）。
	st, body = postJSON(t, ts.URL+"/tenon/grants:evaluate",
		`{"agent":"ag_fulfillment","onBehalfOf":"user:alice","action":"inventory.DeductStock","args":"qty"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("args 不是对象应回 400，得到 %d（原文 %s）", st, body)
	}
}

// ---------- 审计 ----------

func TestAuditWriteQueryAndChain(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	records := []string{
		`{"auditId":"aud_7a","stage":"dispatched","planId":"pln_7","stepId":"s1","action":"inventory.DeductStock"}`,
		`{"auditId":"aud_7b","stage":"result","planId":"pln_7","stepId":"s1","action":"inventory.DeductStock","result":{"ok":true}}`,
		`{"auditId":"aud_8a","stage":"dispatched","planId":"pln_8","stepId":"s1","action":"procurement.CreatePurchaseRequest"}`,
	}
	for _, rec := range records {
		if st, body := postJSON(t, ts.URL+"/tenon/audit", rec); st != 200 || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("写审计失败：%d %s", st, body)
		}
	}

	// 全量查询：链首 prevHash 必须是 null，次条接上首次 hash。
	st, body := getJSON(t, ts.URL+"/tenon/audit")
	if st != 200 {
		t.Fatalf("查询审计失败：%d %s", st, body)
	}
	var all auditQueryResponse
	decodeInto(t, body, &all)
	if len(all.Records) != 3 {
		t.Fatalf("应有 3 条记录，得到 %d（原文 %s）", len(all.Records), body)
	}
	if all.Records[0].PrevHash != nil {
		t.Fatalf("链首 prevHash 必须是 null，得到 %v", *all.Records[0].PrevHash)
	}
	if all.Records[1].PrevHash == nil || *all.Records[1].PrevHash != all.Records[0].Hash {
		t.Fatalf("第二条 prevHash 未接上第一条 hash：%+v", all.Records[1])
	}
	if all.NextCursor != "3" {
		t.Fatalf("全量查询的 nextCursor 应为 3，得到 %q", all.NextCursor)
	}

	// 按 planId 过滤：命中两条，游标仍锚在写入顺序上（3 条全扫过）。
	st, body = getJSON(t, ts.URL+"/tenon/audit?planId=pln_7")
	if st != 200 {
		t.Fatalf("按 planId 查询失败：%d %s", st, body)
	}
	var byPlan auditQueryResponse
	decodeInto(t, body, &byPlan)
	if len(byPlan.Records) != 2 || byPlan.Records[0].AuditID != "aud_7a" || byPlan.Records[1].AuditID != "aud_7b" {
		t.Fatalf("planId=pln_7 应命中 aud_7a/aud_7b，得到 %+v", byPlan.Records)
	}
	if byPlan.NextCursor != "3" {
		t.Fatalf("过滤查询的 nextCursor 应为 3（扫描到末尾），得到 %q", byPlan.NextCursor)
	}

	// 分页：limit=1 → 一条 + 游标 1；since=1 → 第二条 + 游标 2。
	_, body = getJSON(t, ts.URL+"/tenon/audit?planId=pln_7&limit=1")
	var page1 auditQueryResponse
	decodeInto(t, body, &page1)
	if len(page1.Records) != 1 || page1.Records[0].AuditID != "aud_7a" || page1.NextCursor != "1" {
		t.Fatalf("第一页应只有 aud_7a 且游标 1，得到 %+v 游标 %q", page1.Records, page1.NextCursor)
	}
	_, body = getJSON(t, ts.URL+"/tenon/audit?planId=pln_7&since=1&limit=1")
	var page2 auditQueryResponse
	decodeInto(t, body, &page2)
	if len(page2.Records) != 1 || page2.Records[0].AuditID != "aud_7b" || page2.NextCursor != "2" {
		t.Fatalf("第二页应只有 aud_7b 且游标 2，得到 %+v 游标 %q", page2.Records, page2.NextCursor)
	}

	// 游标走到末尾不是错误：空数组 + 游标夹到记录总数。
	_, body = getJSON(t, ts.URL+"/tenon/audit?since=99")
	if !strings.Contains(body, `"records":[]`) {
		t.Fatalf("越界游标应回空数组，得到 %s", body)
	}
	var tail auditQueryResponse
	decodeInto(t, body, &tail)
	if tail.NextCursor != "3" {
		t.Fatalf("越界游标的 nextCursor 应夹到 3，得到 %q", tail.NextCursor)
	}

	// 非法游标 / 分页参数 → 400。
	if st, body := getJSON(t, ts.URL+"/tenon/audit?since=abc"); st != http.StatusBadRequest {
		t.Fatalf("非法 since 应回 400，得到 %d（原文 %s）", st, body)
	}
	if st, body := getJSON(t, ts.URL+"/tenon/audit?limit=0"); st != http.StatusBadRequest {
		t.Fatalf("非法 limit 应回 400，得到 %d（原文 %s）", st, body)
	}

	// 哈希链自检。
	if err := srv.VerifyChain(); err != nil {
		t.Fatalf("审计链校验失败：%v", err)
	}
}

// TestAuditWriteRejectsBadRecord 固定写入口的最小校验：缺 auditId / stage 非法都是 400，
// 不能悄悄写进一条没有审计价值的记录。
func TestAuditWriteRejectsBadRecord(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	cases := map[string]struct {
		body string
		code string
	}{
		"缺 auditId":     {`{"stage":"dispatched","planId":"pln_1"}`, "MISSING_AUDIT_ID"},
		"stage 非法":      {`{"auditId":"aud_1","stage":"pending","planId":"pln_1"}`, "INVALID_AUDIT_STAGE"},
		"at 不是 RFC3339": {`{"auditId":"aud_1","stage":"dispatched","at":"昨天"}`, "SCHEMA_MISMATCH"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st, body := postJSON(t, ts.URL+"/tenon/audit", tc.body)
			if st != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400（原文 %s）", st, body)
			}
			if code := tenonError(t, body).Code; code != tc.code {
				t.Fatalf("错误码 = %q，期望 %q（原文 %s）", code, tc.code, body)
			}
		})
	}
	// 三条坏记录都没落盘：链仍然是空的。
	_, body := getJSON(t, ts.URL+"/tenon/audit")
	if !strings.Contains(body, `"records":[]`) {
		t.Fatalf("坏记录不应落盘，得到 %s", body)
	}
}

// ---------- 注册表 ----------

func TestModulesEmptyRegistryIsEmptyArray(t *testing.T) {
	srv := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	ts := serveHost(t, srv)

	st, body := getJSON(t, ts.URL+"/tenon/modules")
	if st != 200 {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", st, body)
	}
	if body != `{"modules":[]}` {
		t.Fatalf("空注册表必须序列化成 [], 得到 %s", body)
	}
}

// TestAdminModulesRequiresTestMode 固定测试面的边界：TEST_MODE 关闭时端点「不存在」。
func TestAdminModulesRequiresTestMode(t *testing.T) {
	srv := newTestHost(t) // TestMode 默认 false
	ts := serveHost(t, srv)

	st, body := postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"inventory","enabled":true}`)
	if st != http.StatusNotFound {
		t.Fatalf("非测试模式应回 404，得到 %d（原文 %s）", st, body)
	}
	if code := tenonError(t, body).Code; code != "UNKNOWN_ENDPOINT" {
		t.Fatalf("错误码 = %q，期望 UNKNOWN_ENDPOINT（原文 %s）", code, body)
	}
}

// TestAdminModulesEnabledSemantics 固定 enabled 的 *bool 语义：省略=不改开关，显式
// false 才摘除；同时覆盖「登记即禁用」与未登记模块开关报 404。
func TestAdminModulesEnabledSemantics(t *testing.T) {
	srv := newTestServer(t, Config{
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		TestMode:  true,
		Modules:   []registry.ModuleEntry{{ID: "inventory", BaseURL: "http://127.0.0.1:8081"}},
	})
	ts := serveHost(t, srv)
	enabledIDs := func() []string {
		entries := srv.Registry().Enabled()
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.ID)
		}
		return out
	}

	// 只更新地址（省略 enabled）→ 开关不变，仍是启用的。
	st, body := postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"inventory","baseURL":"http://127.0.0.1:9081/"}`)
	if st != 200 {
		t.Fatalf("登记模块失败：%d %s", st, body)
	}
	list := srv.Registry().List()
	if len(list) != 1 || !list[0].Enabled || list[0].BaseURL != "http://127.0.0.1:9081" {
		t.Fatalf("省略 enabled 不该改动开关，且地址应去掉结尾斜杠：%+v", list)
	}
	wantStrings(t, "Enabled()", enabledIDs(), []string{"inventory"})

	// 只给 id（既无 baseURL 也无 enabled）→ 400。
	st, body = postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"inventory"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("只给 id 应回 400，得到 %d（原文 %s）", st, body)
	}
	if code := tenonError(t, body).Code; code != "EMPTY_MODULE_PATCH" {
		t.Fatalf("错误码 = %q，期望 EMPTY_MODULE_PATCH（原文 %s）", code, body)
	}

	// 显式 false → 摘除：条目留在 List 里，但 Enabled() 不再返回它（D1 的 B 轮）。
	st, body = postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"inventory","enabled":false}`)
	if st != 200 {
		t.Fatalf("摘除失败：%d %s", st, body)
	}
	removed := srv.Registry().List()
	if len(removed) != 1 || removed[0].Enabled {
		t.Fatalf("摘除后 List 应保留条目并标记 enabled=false：%+v", removed)
	}
	wantStrings(t, "Enabled()", enabledIDs(), []string{})

	// 再显式 true → 恢复。
	if st, body := postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"inventory","enabled":true}`); st != 200 {
		t.Fatalf("恢复失败：%d %s", st, body)
	}
	wantStrings(t, "Enabled()", enabledIDs(), []string{"inventory"})

	// 「登记即禁用」：先 Upsert 再 SetEnabled(false)，两个语义一次请求表达完。
	st, body = postJSON(t, ts.URL+"/tenon/admin/modules",
		`{"id":"quote","baseURL":"http://127.0.0.1:8083","enabled":false}`)
	if st != 200 {
		t.Fatalf("登记即禁用失败：%d %s", st, body)
	}
	wantStrings(t, "Enabled()", enabledIDs(), []string{"inventory"})
	found := false
	for _, e := range srv.Registry().List() {
		if e.ID == "quote" {
			found = true
			if e.Enabled {
				t.Fatalf("quote 应被登记为禁用：%+v", e)
			}
		}
	}
	if !found {
		t.Fatal("quote 应已登记进注册表（摘除不等于未登记）")
	}

	// 未登记模块的开关 → 404（开关只能作用于已登记模块，注册只走 baseURL）。
	st, body = postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"procurement","enabled":false}`)
	if st != http.StatusNotFound {
		t.Fatalf("未登记模块应回 404，得到 %d（原文 %s）", st, body)
	}
	if code := tenonError(t, body).Code; code != "MODULE_NOT_FOUND" {
		t.Fatalf("错误码 = %q，期望 MODULE_NOT_FOUND（原文 %s）", code, body)
	}

	// 非法地址 → 400。
	st, body = postJSON(t, ts.URL+"/tenon/admin/modules", `{"id":"bad","baseURL":"ftp://127.0.0.1:9000"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("非法地址应回 400，得到 %d（原文 %s）", st, body)
	}
	if code := tenonError(t, body).Code; code != "INVALID_MODULE_BASE_URL" {
		t.Fatalf("错误码 = %q，期望 INVALID_MODULE_BASE_URL（原文 %s）", code, body)
	}
}

// ---------- 共驾帧 ----------

// TestIntentsRESTRoundTrip 覆盖 REST 三端点：入队 → 按 after 回放 → ack 后不再回放。
func TestIntentsRESTRoundTrip(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	intentFrame := `{"type":"tenon.intent","intentId":"int_01JBTEST","planId":"pln_01JBTEST","actions":[]}`
	st, body := postJSON(t, ts.URL+"/tenon/intents", intentFrame)
	if st != 200 {
		t.Fatalf("入队失败：%d %s", st, body)
	}
	var enq enqueueResponse
	decodeInto(t, body, &enq)
	if enq.Seq != 1 {
		t.Fatalf("第一帧的序号应为 1，得到 %d", enq.Seq)
	}

	// poll?after=0 读回这一帧，nextCursor 指向它。
	_, body = getJSON(t, ts.URL+"/tenon/intents/poll?after=0")
	var first pollResponse
	decodeInto(t, body, &first)
	if len(first.Frames) != 1 || first.Frames[0].Seq != 1 || first.NextCursor != 1 {
		t.Fatalf("首帧回放不对：%+v", first)
	}
	if !strings.Contains(string(first.Frames[0].Frame), `"type":"tenon.intent"`) {
		t.Fatalf("回放的帧内容被改动了：%s", first.Frames[0].Frame)
	}

	// 第二帧：after=1 只读回它。
	contextFrame := `{"type":"tenon.context","view":{"module":"quote","type":"Quote","id":"Q-1"},"edits":[],"at":"2026-10-27T09:15:00Z"}`
	if st, body := postJSON(t, ts.URL+"/tenon/intents", contextFrame); st != 200 {
		t.Fatalf("第二帧入队失败：%d %s", st, body)
	}
	_, body = getJSON(t, ts.URL+"/tenon/intents/poll?after=1")
	var second pollResponse
	decodeInto(t, body, &second)
	if len(second.Frames) != 1 || second.Frames[0].Seq != 2 || second.NextCursor != 2 {
		t.Fatalf("第二帧回放不对：%+v", second)
	}

	// ack 到 2 → 之后无论从哪个游标 poll 都不再返回前两帧。
	st, body = postJSON(t, ts.URL+"/tenon/intents/ack", `{"upto":2}`)
	if st != 200 {
		t.Fatalf("ack 失败：%d %s", st, body)
	}
	var acked ackResponse
	decodeInto(t, body, &acked)
	if !acked.OK || acked.Acked != 2 {
		t.Fatalf("ack 应确认到 2，得到 %+v", acked)
	}
	for _, after := range []string{"0", "1", "2"} {
		_, body := getJSON(t, ts.URL+"/tenon/intents/poll?after="+after)
		var polled pollResponse
		decodeInto(t, body, &polled)
		if len(polled.Frames) != 0 {
			t.Fatalf("ack 之后 after=%s 仍回放了 %d 帧（原文 %s）", after, len(polled.Frames), body)
		}
		if !strings.Contains(body, `"frames":[]`) {
			t.Fatalf("无帧时应回空数组：%s", body)
		}
	}

	// ack 省略 upto → 确认全部已入队的帧。
	if st, body := postJSON(t, ts.URL+"/tenon/intents", intentFrame); st != 200 {
		t.Fatalf("第三帧入队失败：%d %s", st, body)
	}
	_, body = postJSON(t, ts.URL+"/tenon/intents/ack", `{}`)
	decodeInto(t, body, &acked)
	if acked.Acked != 3 {
		t.Fatalf("省略 upto 应确认到 3，得到 %+v", acked)
	}
	_, body = getJSON(t, ts.URL+"/tenon/intents/poll?after=0")
	var empty pollResponse
	decodeInto(t, body, &empty)
	if len(empty.Frames) != 0 {
		t.Fatalf("确认全部后不应回放任何帧：%s", body)
	}
}

// TestIntentsRejectsBadFrame 固定帧的最小校验与「不做 WS 升级」的边界。
func TestIntentsRejectsBadFrame(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	// 未知 type → 400。
	st, body := postJSON(t, ts.URL+"/tenon/intents", `{"type":"tenon.unknown"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("未知帧类型应回 400，得到 %d（原文 %s）", st, body)
	}
	if code := tenonError(t, body).Code; code != "UNKNOWN_FRAME_TYPE" {
		t.Fatalf("错误码 = %q，期望 UNKNOWN_FRAME_TYPE（原文 %s）", code, body)
	}

	// 缺 type → 400（空 type 不在三种判别值里）。
	if st, body := postJSON(t, ts.URL+"/tenon/intents", `{"intentId":"int_1"}`); st != http.StatusBadRequest {
		t.Fatalf("缺 type 应回 400，得到 %d（原文 %s）", st, body)
	}

	// GET /tenon/intents 不是 WS 升级（留 M1）：405 + Allow。
	resp, err := http.Get(ts.URL + "/tenon/intents")
	if err != nil {
		t.Fatalf("GET /tenon/intents 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /tenon/intents 应回 405（WS 升级留 M1），得到 %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q，期望 POST", allow)
	}
}

// ---------- 健康探针 ----------

func TestHealthAndReady(t *testing.T) {
	grants, subjects := testGrants()
	srv := newTestServer(t, Config{
		Policies:  testPolicies(),
		Grants:    grants,
		Subjects:  subjects,
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
	})
	ts := serveHost(t, srv)

	st, body := getJSON(t, ts.URL+"/healthz")
	if st != 200 || body != "ok" {
		t.Fatalf("/healthz 应回 200 ok，得到 %d %q", st, body)
	}

	st, body = getJSON(t, ts.URL+"/readyz")
	if st != 200 {
		t.Fatalf("/readyz 应回 200，得到 %d（原文 %s）", st, body)
	}
	for _, want := range []string{`"ready":true`, `"policies":2`, `"grants":1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("/readyz 缺少 %s：%s", want, body)
		}
	}

	// 审计不可写 → 503。先把句柄关掉（Windows 上不关就删不掉文件），再让路径变成一个目录。
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	broken := newTestServer(t, Config{AuditPath: auditPath})
	brokenTS := serveHost(t, broken)
	if err := broken.Close(); err != nil {
		t.Fatalf("关闭审计存储失败：%v", err)
	}
	if err := os.Remove(auditPath); err != nil {
		t.Fatalf("删除审计文件失败：%v", err)
	}
	if err := os.Mkdir(auditPath, 0o755); err != nil {
		t.Fatalf("占位目录创建失败：%v", err)
	}
	st, body = getJSON(t, brokenTS.URL+"/readyz")
	if st != http.StatusServiceUnavailable {
		t.Fatalf("审计不可写时 /readyz 应回 503，得到 %d（原文 %s）", st, body)
	}
	for _, want := range []string{`"ready":false`, `"audit_file"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("/readyz 503 响应缺少 %s：%s", want, body)
		}
	}
}

// ---------- 装配期自检与配置加载 ----------

// TestNewRejectsBrokenPolicyConfig 固定启动期自检：规则配置坏必须在启动时报错，
// 而不是让第一次闸门以 deny 的形式把配置错误伪装成业务拒绝。
func TestNewRejectsBrokenPolicyConfig(t *testing.T) {
	_, err := New(Config{
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		Policies: []policy.PolicyRule{{
			PolicyID: "p-bad",
			Match:    policy.PolicyMatch{Module: "inventory"},
			Outcome:  "maybe",
		}},
	})
	if err == nil {
		t.Fatal("outcome 非法的规则必须在启动期被拒")
	}
	if !strings.Contains(err.Error(), "自检未通过") {
		t.Fatalf("错误应说明自检未通过，得到 %v", err)
	}

	// 审计路径为空同样不能装配：审计链是宿主唯一不可省的持久状态。
	if _, err := New(Config{}); err == nil {
		t.Fatal("空审计路径必须被拒")
	}
}

// TestLoadDevConfigFiles 校验仓库里那两份开发配置真的能被装配并给出预期语义：
// Task 15 真跑 D1 用的就是它们，文件写错必须在这里就炸。
func TestLoadDevConfigFiles(t *testing.T) {
	dir := filepath.Join("..", "..", "config")
	rules, err := LoadPolicies(filepath.Join(dir, "policies.dev.json"))
	if err != nil {
		t.Fatalf("加载 config/policies.dev.json 失败：%v", err)
	}
	grants, subjects, err := LoadGrants(filepath.Join(dir, "grants.dev.json"))
	if err != nil {
		t.Fatalf("加载 config/grants.dev.json 失败：%v", err)
	}
	srv := newTestServer(t, Config{
		Policies:  rules,
		Grants:    grants,
		Subjects:  subjects,
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
	})
	ts := serveHost(t, srv)

	// 策略：写动作靠 approve 规则；读动作在 dev 配置里显式登记成 auto（见 http.go 包注释
	// 「读动作必须逐个登记」）；未登记的动作仍然 deny —— 这是 fail-closed 的不变量。
	cases := []struct {
		action        string
		args          string
		wantOutcome   string
		wantApprovers []string
		wantReasons   []string
	}{
		{"inventory.DeductStock", `{"sku":"STD-1001","qty":12}`, policy.OutcomeApprove,
			[]string{"role:warehouse_manager"}, []string{policy.ReasonApprovalRequired}},
		{"inventory.DeductStock", `{"sku":"STD-1001","qty":3}`, policy.OutcomeDeny,
			[]string{}, []string{policy.ReasonNoMatch}},
		{"procurement.CreatePurchaseRequest", `{"sku":"STD-1002","qty":12}`, policy.OutcomeApprove,
			[]string{"role:procurement_manager"}, []string{policy.ReasonApprovalRequired}},
		// 读动作逐个登记后的效果：读 get 放行（auto）。
		{"inventory.GetStock", `{"sku":"STD-1001"}`, policy.OutcomeAuto,
			[]string{}, []string{policy.ReasonAuto}},
		// Task 9 会加的 quote 读动作：规则已在 dev 配置里登记，这里锁住它不被误删。
		{"quote.GetQuote", `{"quoteId":"Q-2026-0417"}`, policy.OutcomeAuto,
			[]string{}, []string{policy.ReasonAuto}},
		// fail-closed 不变量：没登记过的动作照样 deny，不能因为「看起来是读」就放行。
		{"inventory.Unknown", `{"sku":"STD-1001"}`, policy.OutcomeDeny,
			[]string{}, []string{policy.ReasonNoMatch}},
	}
	for _, tc := range cases {
		body := planBody(`{"stepId":"s1","action":"` + tc.action + `","args":` + tc.args + `}`)
		st, raw := postJSON(t, ts.URL+"/tenon/policies:evaluate", body)
		if st != 200 {
			t.Fatalf("%s 求值失败：%d %s", tc.action, st, raw)
		}
		var got policy.PolicyEvaluation
		decodeInto(t, raw, &got)
		if got.Outcome != tc.wantOutcome {
			t.Fatalf("%s args=%s 的 outcome = %q，期望 %q（原文 %s）",
				tc.action, tc.args, got.Outcome, tc.wantOutcome, raw)
		}
		wantStrings(t, "approvers", got.Approvers, tc.wantApprovers)
		wantStrings(t, "reasons", got.Reasons, tc.wantReasons)
	}

	// 读 + 写的多步 plan：读步骤（auto）不得把整份 plan 拉到 deny，最终仍取写步骤的 approve。
	st, raw := postJSON(t, ts.URL+"/tenon/policies:evaluate", planBody(
		`{"stepId":"s1","action":"inventory.GetStock","args":{"sku":"STD-1001"}},`+
			`{"stepId":"s2","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":12}}`))
	if st != 200 {
		t.Fatalf("读+写多步 plan 求值失败：%d %s", st, raw)
	}
	var mixed policy.PolicyEvaluation
	decodeInto(t, raw, &mixed)
	if mixed.Outcome != policy.OutcomeApprove {
		t.Fatalf("读+写多步 plan 应取 approve（读 auto 不拉低写 approve），得到 %s", raw)
	}
	wantStrings(t, "decidedBy", mixed.DecidedBy, []string{"pol-inventory-deduct-approve"})

	// 写步骤未登记（qty 3 无规则）时，读步骤的 auto 也救不回来：整份 plan 仍是 deny。
	st, raw = postJSON(t, ts.URL+"/tenon/policies:evaluate", planBody(
		`{"stepId":"s1","action":"inventory.GetStock","args":{"sku":"STD-1001"}},`+
			`{"stepId":"s2","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":3}}`))
	if st != 200 {
		t.Fatalf("读+未登记写多步 plan 求值失败：%d %s", st, raw)
	}
	var mixedDeny policy.PolicyEvaluation
	decodeInto(t, raw, &mixedDeny)
	if mixedDeny.Outcome != policy.OutcomeDeny {
		t.Fatalf("未登记的写步骤必须把整份 plan 拉成 deny，得到 %s", raw)
	}
	wantStrings(t, "decidedBy", mixedDeny.DecidedBy, []string{})
	wantStrings(t, "reasons", mixedDeny.Reasons, []string{policy.ReasonNoMatch})

	// 授权：ag_fulfillment 代 user:alice 扣 12 → 放行且余量 88；委托人换人 → 权限不足。
	st, raw = postJSON(t, ts.URL+"/tenon/grants:evaluate",
		`{"agent":"ag_fulfillment","onBehalfOf":"user:alice","action":"inventory.DeductStock","args":{"sku":"STD-1001","qty":12}}`)
	if st != 200 {
		t.Fatalf("授权评估失败：%d %s", st, raw)
	}
	var allowed grant.GrantResult
	decodeInto(t, raw, &allowed)
	if !allowed.Allowed || allowed.Quota == nil || allowed.Quota.Remaining != 88 {
		t.Fatalf("应放行且余量 88，得到 %s", raw)
	}
	st, raw = postJSON(t, ts.URL+"/tenon/grants:evaluate",
		`{"agent":"ag_fulfillment","onBehalfOf":"user:mallory","action":"inventory.DeductStock","args":{"qty":1}}`)
	if st != 200 {
		t.Fatalf("授权评估失败：%d %s", st, raw)
	}
	var denied grant.GrantResult
	decodeInto(t, raw, &denied)
	if denied.Allowed || denied.Reason != grant.ReasonSubjectNotPermitted {
		t.Fatalf("未登记的委托人必须 SUBJECT_NOT_PERMITTED，得到 %s", raw)
	}

	// 授权表里配额窗口是 PT1H（grant 引擎只支持 PnD/PnW/PTnH）：真跑一次就能证实。
	if err := srv.VerifyChain(); err != nil {
		t.Fatalf("审计链校验失败：%v", err)
	}
}

// TestLoadPoliciesAndGrantsRejectBadFiles 固定配置加载的容错边界：宁可启动失败，
// 也不要让一份读不懂的治理数据静默变成「空表 + 全部 deny」。
func TestLoadPoliciesAndGrantsRejectBadFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写临时配置失败：%v", err)
		}
		return path
	}

	policyCases := map[string]struct {
		path string
		want string
	}{
		"空文件":    {write("p-empty.json", "  \n"), "是空的"},
		"不是数组":   {write("p-object.json", `{"rules":[]}`), "规则数组"},
		"未知字段":   {write("p-unknown.json", `[{"policyId":"p1","outcome":"deny","match":{"module":"inventory"},"typo":1}]`), "unknown field"},
		"JSON 坏": {write("p-broken.json", `[{]`), "解析失败"},
		"多余内容":   {write("p-extra.json", `[] []`), "多余内容"},
		"文件不存在":  {filepath.Join(dir, "p-missing.json"), "读取策略文件"},
	}
	for name, tc := range policyCases {
		t.Run("policies/"+name, func(t *testing.T) {
			if _, err := LoadPolicies(tc.path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应包含 %q，得到 %v", tc.want, err)
			}
		})
	}

	grantCases := map[string]struct {
		path string
		want string
	}{
		"空文件":    {write("g-empty.json", ""), "是空的"},
		"不是对象":   {write("g-array.json", `[]`), "JSON 对象"},
		"未知字段":   {write("g-unknown.json", `{"grants":[],"subject":{}}`), "unknown field"},
		"JSON 坏": {write("g-broken.json", `{"grants":`), "解析失败"},
	}
	for name, tc := range grantCases {
		t.Run("grants/"+name, func(t *testing.T) {
			if _, _, err := LoadGrants(tc.path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应包含 %q，得到 %v", tc.want, err)
			}
		})
	}

	// 结构合法但窗口写法不受支持（PnM/PTnM 之类）：加载期不猜，求值时报
	// ErrUnsupportedWindow，宿主据此 fail-closed。这里固定这条边界。
	windowPath := write("g-window.json", `{"grants":[{"grantId":"gr-x0001","agent":"ag_fulfillment",`+
		`"allow":[{"module":"inventory","actions":["inventory.GetStock"],`+
		`"constraints":{"window":"PT30M","maxUnitsPerWindow":10}}],`+
		`"expiresAt":"2030-01-01T00:00:00Z"}]}`)
	grants, _, err := LoadGrants(windowPath)
	if err != nil {
		t.Fatalf("结构合法的授权文件不该在加载期报错：%v", err)
	}
	eng := grant.NewGrantEngine(grants, grant.WithSubjects(map[string][]string{
		"user:alice": {"inventory.GetStock"},
	}))
	if _, err := eng.Evaluate("ag_fulfillment", "user:alice", "inventory.GetStock", 1); !errors.Is(err, grant.ErrUnsupportedWindow) {
		t.Fatalf("PT30M 窗口应在求值时被拒（ErrUnsupportedWindow），得到 %v", err)
	}
}

// TestUnchangedErrorsAreTenonEnvelopes 固定「无论哪条路径出错，响应都是合法错误信封」。
func TestUnchangedErrorsAreTenonEnvelopes(t *testing.T) {
	srv := newTestHost(t)
	ts := serveHost(t, srv)

	// 未知路径 404。
	st, body := getJSON(t, ts.URL+"/tenon/nope")
	if st != http.StatusNotFound {
		t.Fatalf("未知路径应回 404，得到 %d（原文 %s）", st, body)
	}
	if code := tenonError(t, body).Code; code != "UNKNOWN_ENDPOINT" {
		t.Fatalf("错误码 = %q，期望 UNKNOWN_ENDPOINT（原文 %s）", code, body)
	}

	// 已知路径、错误方法 → 405 + Allow。
	resp, err := http.Post(ts.URL+"/healthz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /healthz 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz 应回 405，得到 %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q，期望 GET", allow)
	}

	// 版本头存在且不是 0.1 → 400（本轮不做版本协商）。
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tenon/policies:evaluate",
		strings.NewReader(`{"plan":{}}`))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderVersion, "0.2")
	versioned, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = versioned.Body.Close() }()
	if versioned.StatusCode != http.StatusBadRequest {
		t.Fatalf("不兼容版本应回 400，得到 %d", versioned.StatusCode)
	}

	// 成功响应也带版本头。
	resp2, err := http.Get(ts.URL + "/tenon/modules")
	if err != nil {
		t.Fatalf("GET /tenon/modules 失败：%v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if got := resp2.Header.Get(protocol.HeaderVersion); got != protocol.Version {
		t.Fatalf("响应版本头 = %q，期望 %q", got, protocol.Version)
	}
}
