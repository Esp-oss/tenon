package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHost 扮演 Tenon 宿主：注册中心、策略、授权、公告板、审计。
type fakeHost struct {
	*httptest.Server
	mu sync.Mutex

	ModuleURL string
	Policy    string // auto / approve / deny
	Reasons   []string
	GrantOK   bool

	DryRunCalls  int
	ModuleWrites int

	PendingItems []map[string]any
	ConfirmBody  []byte
	PendingCalls int
	AuditRaw     string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{Policy: "auto", GrantOK: true, AuditRaw: `{"records":[{"stage":"dispatched"},{"stage":"result"}]}`}

	mod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":dry-run"):
			h.mu.Lock()
			h.DryRunCalls++
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"dryRun": map[string]any{"status": "verified"}})
		case strings.HasSuffix(r.URL.Path, "inventory.GetStock"):
			writeJSON(w, http.StatusOK, map[string]any{"onHand": 108})
		default:
			// 任何写动作走到这里都算漏网：计数并让用例失败。
			h.mu.Lock()
			h.ModuleWrites++
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		}
	}))
	t.Cleanup(mod.Close)
	h.ModuleURL = mod.URL

	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tenon/modules":
			writeJSON(w, http.StatusOK, map[string]any{"modules": []any{
				map[string]any{
					"id": "inventory", "baseURL": h.ModuleURL, "enabled": true, "state": "alive",
					"lastSeen": "2026-11-01T01:00:00+08:00",
					"manifest": map[string]any{
						"module":       map[string]any{"title": "库存"},
						"capabilities": []string{"dry-run", "ticket"},
						"actions": []any{
							map[string]any{"id": "inventory.GetStock", "kind": "read"},
							map[string]any{"id": "inventory.DeductStock", "kind": "write"},
						},
					},
				},
			}})

		case "/tenon/policies:evaluate":
			writeJSON(w, http.StatusOK, map[string]any{
				"outcome": h.Policy, "decidedBy": []string{"pol-test"},
				"approvers": []string{"role:warehouse_clerk"}, "reasons": h.Reasons,
			})

		case "/tenon/grants:evaluate":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			// 预检必须 charge:false，否则每预检一次都扣配额。
			if req["charge"] != false {
				t.Errorf("授权预检必须显式 charge:false，实际 %#v", req["charge"])
			}
			writeJSON(w, http.StatusOK, map[string]any{"allowed": h.GrantOK, "reason": "QUOTA_EXCEEDED"})

		case "/tenon/pending":
			h.mu.Lock()
			h.PendingCalls++
			token := r.Header.Get("Authorization")
			items := h.PendingItems
			h.mu.Unlock()
			if token == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": items})

		case "/tenon/requests":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body := make([]byte, 0, 512)
			buf := make([]byte, 512)
			for {
				n, err := r.Body.Read(buf)
				body = append(body, buf[:n]...)
				if err != nil {
					break
				}
			}
			h.mu.Lock()
			h.ConfirmBody = body
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": 7, "durable": true, "requestId": "req_test_01", "syntheticEventId": "evt_test_01"})

		case "/tenon/audit":
			_, _ = w.Write([]byte(h.AuditRaw))

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newServer(t *testing.T, host *fakeHost) *Server {
	t.Helper()
	return &Server{
		Host:       host.URL,
		AgentID:    "ag_fulfillment",
		OnBehalfOf: "user:alice",
	}
}

// rpc 发一条请求并解析响应。
func rpc(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, _ := json.Marshal(msg)
	out, ok := s.HandleLine(raw)
	if !ok {
		t.Fatalf("%s 应当有响应", method)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("%s 响应不是 JSON：%v（%s）", method, err, out)
	}
	return resp
}

// callTool 走完整协议路径，返回正文与 isError。
func callTool(t *testing.T, s *Server, name string, args any) (string, bool) {
	t.Helper()
	resp := rpc(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if e, ok := resp["error"]; ok {
		t.Fatalf("%s 不该是协议错误：%v", name, e)
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s 没有 content", name)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	isErr, _ := result["isError"].(bool)
	return text, isErr
}

func TestInitializeAndToolsList(t *testing.T) {
	s := newServer(t, newFakeHost(t))

	resp := rpc(t, s, "initialize", map[string]any{"protocolVersion": protocolVersion})
	result, _ := resp["result"].(map[string]any)
	if result["protocolVersion"] != protocolVersion {
		t.Fatalf("协议版本不对：%v", result)
	}

	list := rpc(t, s, "tools/list", nil)
	res, _ := list["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	// 7 个：比设计 §6 的六条多出 request_submit（把人的原话交给 agent）。
	// 它不执行、不造卡，只投递入站帧——没有它，对话层就没有任何东西可批准。
	if len(tools) != 7 {
		t.Fatalf("应有 7 个工具，实际 %d", len(tools))
	}
	names := map[string]bool{}
	for _, item := range tools {
		m, _ := item.(map[string]any)
		if n, _ := m["name"].(string); n != "" {
			names[n] = true
		}
	}
	for _, want := range []string{"registry_list", "action_invoke", "plan_preview", "request_submit", "pending_list", "pending_decide", "audit_query"} {
		if !names[want] {
			t.Fatalf("缺工具 %s，实际 %v", want, names)
		}
	}
}

// 通知没有 id，不能有响应：写回去会打乱客户端的请求队列。
func TestNotificationGetsNoResponse(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if _, ok := s.HandleLine(raw); ok {
		t.Fatal("通知不应产生响应")
	}
}

func TestUnknownMethodIsProtocolError(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	resp := rpc(t, s, "resources/list", nil)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(codeMethodNotFound) {
		t.Fatalf("未知方法应回 -32601，实际 %v", resp)
	}
}

func TestRegistryListShowsKinds(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	text, isErr := callTool(t, s, "registry_list", map[string]any{})
	if isErr {
		t.Fatalf("不该失败：%s", text)
	}
	for _, want := range []string{"inventory", "inventory.GetStock [read]", "inventory.DeductStock [write]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("注册中心输出缺 %q：\n%s", want, text)
		}
	}
}

func TestActionInvokeReadOnlyWorks(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	text, isErr := callTool(t, s, "action_invoke", map[string]any{
		"actionId": "inventory.GetStock", "args": map[string]any{"sku": "STD-1001"},
	})
	if isErr {
		t.Fatalf("只读调用不该失败：%s", text)
	}
	if !strings.Contains(text, "108") {
		t.Fatalf("应返回真实库存：%s", text)
	}
}

// 这条是设计的核心负向：对话层不能调写动作，且模块一次请求都收不到。
func TestActionInvokeRejectsWriteWithoutTouchingModule(t *testing.T) {
	host := newFakeHost(t)
	s := newServer(t, host)

	text, isErr := callTool(t, s, "action_invoke", map[string]any{
		"actionId": "inventory.DeductStock", "args": map[string]any{"sku": "STD-1001", "qty": 12},
	})
	if !isErr {
		t.Fatalf("写动作必须被拒：%s", text)
	}
	if !strings.Contains(text, "写") {
		t.Fatalf("拒绝理由要说清是写动作：%s", text)
	}
	host.mu.Lock()
	writes := host.ModuleWrites
	host.mu.Unlock()
	if writes != 0 {
		t.Fatalf("拒绝后模块不能收到任何请求，实际 %d 次", writes)
	}
}

func TestPlanPreviewReportsVerdictsWithoutSideEffects(t *testing.T) {
	host := newFakeHost(t)
	s := newServer(t, host)

	text, isErr := callTool(t, s, "plan_preview", map[string]any{
		"triggerEventId": "evt_test01",
		"steps": []any{map[string]any{
			"action": "inventory.DeductStock",
			"args":   map[string]any{"sku": "STD-1001", "qty": 12, "referenceNo": "Q-1"},
		}},
	})
	if isErr {
		t.Fatalf("预检不该失败：%s", text)
	}
	if !strings.Contains(text, "策略：auto") || !strings.Contains(text, "verified") {
		t.Fatalf("预检结论缺失：\n%s", text)
	}
	host.mu.Lock()
	pending := host.PendingCalls
	dry := host.DryRunCalls
	writes := host.ModuleWrites
	host.mu.Unlock()
	if pending != 0 {
		t.Fatalf("预检不许碰公告板，实际 %d 次", pending)
	}
	if dry != 1 {
		t.Fatalf("写动作应预演 1 次，实际 %d", dry)
	}
	if writes != 0 {
		t.Fatalf("预检不许执行，模块写调用 %d 次", writes)
	}
}

func TestPlanPreviewPassesDenyThrough(t *testing.T) {
	host := newFakeHost(t)
	host.Policy = "deny"
	host.Reasons = []string{"NO_POLICY_MATCH_DEFAULT_DENY"}
	s := newServer(t, host)

	text, _ := callTool(t, s, "plan_preview", map[string]any{
		"steps": []any{map[string]any{"action": "inventory.DeductStock", "args": map[string]any{"qty": 5}}},
	})
	if !strings.Contains(text, "NO_POLICY_MATCH_DEFAULT_DENY") {
		t.Fatalf("拒绝理由要原样透出：\n%s", text)
	}
	if !strings.Contains(text, "预检未通过") {
		t.Fatalf("结论要明确未通过：\n%s", text)
	}
}

// 没有 token 就一步都不能走：这条分支必须早于任何副作用。
func TestPendingDecideWithoutTokenRefuses(t *testing.T) {
	host := newFakeHost(t)
	s := newServer(t, host)

	text, isErr := callTool(t, s, "pending_decide", map[string]any{
		"planId": "pln_x", "decision": "approve",
	})
	if !isErr {
		t.Fatalf("无 token 必须拒绝：%s", text)
	}
	host.mu.Lock()
	confirm := host.ConfirmBody
	host.mu.Unlock()
	if len(confirm) != 0 {
		t.Fatalf("无 token 时不能提交任何确认帧，实际：%s", confirm)
	}
}

func TestPendingDecideWithTokenSendsConfirm(t *testing.T) {
	host := newFakeHost(t)
	s := newServer(t, host)
	s.HumanToken = "tok-alice"

	text, isErr := callTool(t, s, "pending_decide", map[string]any{
		"planId": "pln_appr01", "decision": "approve",
	})
	if isErr {
		t.Fatalf("有 token 应成功：%s", text)
	}
	host.mu.Lock()
	raw := host.ConfirmBody
	host.mu.Unlock()
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("确认帧不是 JSON：%v（%s）", err, raw)
	}
	if frame["type"] != "tenon.confirm" || frame["planId"] != "pln_appr01" || frame["decision"] != "approve" {
		t.Fatalf("确认帧字段不对：%v", frame)
	}
	if id, _ := frame["confirmId"].(string); !strings.HasPrefix(id, "cfm_") {
		t.Fatalf("confirmId 不合法：%v", frame["confirmId"])
	}
}

// request_submit 只搬运原话：帧里必须有原文，且**不能**带 by（宿主用 token 覆写）。
// 这条防的是「模型把用户的话改写成它以为的意思」——改写过的请求与审计里的
// 那条 request 对不上，事后没人能还原人到底说过什么。
func TestRequestSubmitCarriesVerbatimText(t *testing.T) {
	host := newFakeHost(t)
	s := newServer(t, host)
	s.HumanToken = "tok-alice"

	const spoken = "把 STD-1001 的 12 件扣掉，给 Q-LOCAL 发货"
	text, isErr := callTool(t, s, "request_submit", map[string]any{"text": "  " + spoken + "  "})
	if isErr {
		t.Fatalf("有 token 应成功：%s", text)
	}
	host.mu.Lock()
	raw := host.ConfirmBody
	host.mu.Unlock()

	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("帧不是 JSON：%v（%s）", err, raw)
	}
	if frame["type"] != "tenon.request" {
		t.Fatalf("帧类型应为 tenon.request，实际 %v", frame["type"])
	}
	if frame["text"] != spoken {
		t.Fatalf("原话被改写了：%q（期望 %q）", frame["text"], spoken)
	}
	if _, ok := frame["by"]; ok {
		t.Fatalf("帧里不该带 by：宿主会用 token 的 sub 覆写，写了只会制造不一致")
	}
	for _, want := range []string{"pending_list", "pending_decide"} {
		if !strings.Contains(text, want) {
			t.Fatalf("回执里应说明后续怎么批（缺 %q）：%s", want, text)
		}
	}
}

func TestRequestSubmitWithoutTokenRefuses(t *testing.T) {
	host := newFakeHost(t)
	s := newServer(t, host)

	text, isErr := callTool(t, s, "request_submit", map[string]any{"text": "扣 12 件"})
	if !isErr {
		t.Fatalf("无 token 必须拒绝：%s", text)
	}
	host.mu.Lock()
	raw := host.ConfirmBody
	host.mu.Unlock()
	if len(raw) != 0 {
		t.Fatalf("无 token 时不能投递任何帧，实际：%s", raw)
	}
}

func TestPendingListRequiresToken(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	text, isErr := callTool(t, s, "pending_list", map[string]any{})
	if !isErr {
		t.Fatalf("无 token 应拒绝：%s", text)
	}
}

// pending_list 的输出必须先让人选，再让人批。
//
// 这条断言防的是一个真实的失败模式：模型拿到待批条目后直接调 pending_decide
// （它手里的服务账号口令真能通过），于是「有人看一眼」这个环节被静默跳过，
// 而界面上看起来一切正常。指令必须出现在**工具输出里**——系统提示管不住这种局部决策。
func TestPendingListTellsModelToAskBeforeDeciding(t *testing.T) {
	host := newFakeHost(t)
	host.PendingItems = []map[string]any{
		{"planId": "pln_wait01", "state": "awaiting_approval", "summary": "扣减 STD-1001 十二件",
			"requiredRoles": []string{"role:warehouse_clerk"}},
		{"planId": "pln_done01", "state": "done", "summary": "已经执行完的卡"},
	}
	s := newServer(t, host)
	s.HumanToken = "tok-alice"

	text, isErr := callTool(t, s, "pending_list", map[string]any{})
	if isErr {
		t.Fatalf("有 token 应成功：%s", text)
	}
	for _, want := range []string{
		"ask_user_question",
		"允许执行",
		"驳回",
		"pending_decide",
		"planId=pln_wait01",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("输出里缺 %q（模型不会稳定走选项卡片）：\n%s", want, text)
		}
	}
	// 终态卡不该被要求再批一次：多问一句就是把已完成的事重新摆到人面前。
	if strings.Contains(text, "planId=pln_done01") {
		t.Fatalf("state=done 的卡不该再要求人选择：\n%s", text)
	}
}

func TestAuditQueryReturnsRecords(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	text, isErr := callTool(t, s, "audit_query", map[string]any{"planId": "pln_appr01"})
	if isErr {
		t.Fatalf("查审计不该失败：%s", text)
	}
	if !strings.Contains(text, "dispatched") {
		t.Fatalf("应返回审计记录：%s", text)
	}
}

func TestMalformedJSONGetsParseError(t *testing.T) {
	s := newServer(t, newFakeHost(t))
	out, ok := s.HandleLine([]byte("{not json"))
	if !ok {
		t.Fatal("坏 JSON 也要回错误，否则客户端会一直等")
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("错误响应本身不是 JSON：%s", out)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(codeParseError) {
		t.Fatalf("应回 -32700，实际 %s", out)
	}
}

func TestToolSpecsHaveSchemas(t *testing.T) {
	for _, spec := range toolSpecs() {
		if spec.Name == "" || spec.Description == "" {
			t.Fatalf("工具必须有名字与描述：%+v", spec)
		}
		if spec.InputSchema == nil {
			t.Fatalf("%s 缺 inputSchema", spec.Name)
		}
	}
	_ = fmt.Sprintf
}
