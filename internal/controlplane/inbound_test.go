package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newInboundServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		Users:         []User{{User: "alice", Password: "pw", Roles: []string{"role:warehouse_clerk"}}},
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	return s, httptest.NewServer(s.Handler())
}

func login(t *testing.T, base string) string {
	t.Helper()
	return loginAs(t, base, "alice", "pw")
}

// loginAs 用指定账号换 token：审批人角色的用例必须换人（不同人握有不同角色）。
func loginAs(t *testing.T, base, user, password string) string {
	t.Helper()
	status, body := postJSON(t, base+"/tenon/session",
		`{"user":"`+user+`","password":"`+password+`"}`)
	if status != http.StatusOK {
		t.Fatalf("登录 %s 失败 %d：%s", user, status, body)
	}
	var got struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("登录响应坏：%v", err)
	}
	return got.Token
}

// agentToken 签一枚 agent 服务账号的 token：入站队列的消费侧（poll/ack）与公告板的
// 写入侧都要求 role:agent，普通用户 token 会被 403。
func agentToken(t *testing.T) string {
	t.Helper()
	tok, err := SignToken([]byte("secret"), "agent:test", []string{roleAgent}, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("签发 agent token 失败：%v", err)
	}
	return tok
}

func postJSONAuth(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

func TestInboundRequiresToken(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	status, _ := postJSON(t, ts.URL+"/tenon/requests", `{"type":"tenon.request","text":"hi"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，得到 %d", status)
	}
}

func TestInboundOverwritesByAndMintsSyntheticID(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	tok := login(t, ts.URL)

	status, body := postJSONAuth(t, ts.URL+"/tenon/requests", tok,
		`{"type":"tenon.request","requestId":"req_FAKE","text":"扣 12 件","by":"user:mallory","at":"2026-10-27T09:00:00Z"}`)
	if status != http.StatusOK {
		t.Fatalf("入队失败 %d：%s", status, body)
	}

	_, pollBody := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", agentToken(t))
	var got struct {
		Frames []struct {
			Seq   int64           `json:"seq"`
			Frame json.RawMessage `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(pollBody), &got); err != nil {
		t.Fatalf("poll 响应坏：%v", err)
	}
	if len(got.Frames) != 1 {
		t.Fatalf("应有 1 帧，得到 %d", len(got.Frames))
	}
	var frame struct {
		Type             string `json:"type"`
		RequestID        string `json:"requestId"`
		By               string `json:"by"`
		SyntheticEventID string `json:"syntheticEventId"`
	}
	if err := json.Unmarshal(got.Frames[0].Frame, &frame); err != nil {
		t.Fatalf("帧不是 JSON：%v", err)
	}
	if frame.By != "user:alice" {
		t.Fatalf("by 必须被 token 覆写，得到 %q", frame.By)
	}
	if frame.RequestID == "req_FAKE" || frame.RequestID == "" {
		t.Fatalf("requestId 必须由宿主签发，得到 %q", frame.RequestID)
	}
	if !strings.HasPrefix(frame.SyntheticEventID, "evt_") {
		t.Fatalf("syntheticEventId 必须以 evt_ 开头，得到 %q", frame.SyntheticEventID)
	}
}

// I8：登记表不能跟着内存队列消失，否则重启后未完成的 request 会被当成事件而自动通过。
func TestInboundOriginRegistrySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AuditPath:     dir + "/audit.jsonl",
		Users:         []User{{User: "alice", Password: "pw"}},
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	}
	s1, err := New(cfg)
	if err != nil {
		t.Fatalf("首次装配失败：%v", err)
	}
	ts1 := httptest.NewServer(s1.Handler())
	tok, _ := SignToken(cfg.SessionSecret, "user:alice", nil, time.Now().UTC(), time.Hour)
	if status, body := postJSONAuth(t, ts1.URL+"/tenon/requests", tok,
		`{"type":"tenon.request","text":"x"}`); status != http.StatusOK {
		t.Fatalf("入队失败 %d：%s", status, body)
	}
	var evtID string
	_, pollBody := getJSONAuth(t, ts1.URL+"/tenon/requests/poll?after=0", agentToken(t))
	var poll struct {
		Frames []struct {
			Frame json.RawMessage `json:"frame"`
		} `json:"frames"`
	}
	json.Unmarshal([]byte(pollBody), &poll)
	var frame struct {
		SyntheticEventID string `json:"syntheticEventId"`
	}
	json.Unmarshal(poll.Frames[0].Frame, &frame)
	evtID = frame.SyntheticEventID
	ts1.Close()
	s1.Close()

	s2, err := New(cfg)
	if err != nil {
		t.Fatalf("二次装配失败：%v", err)
	}
	defer s2.Close()
	if !s2.originRegistry().IsRequest(evtID) {
		t.Fatalf("重启后合成事件 id 仍须判定为 request：%q", evtID)
	}
}

// 负游标与出站 serveIntentAck 保持同一校验：回 400，而不是回一个负的 acked。
func TestInboundAckRejectsNegativeUpto(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	// ack 要求 role:agent：先用用户 token 会被 403 挡在参数校验之前。
	status, body := postJSONAuth(t, ts.URL+"/tenon/requests/ack", agentToken(t), `{"upto":-1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("负 upto 应 400，得到 %d：%s", status, body)
	}
}

// 落盘失败时不能假装持久化：200 但 durable=false，且帧仍能在内存队列里回放。
func TestInboundReportsNonDurableOnPersistFailure(t *testing.T) {
	s, ts := newInboundServer(t)
	defer ts.Close()
	tok := login(t, ts.URL)

	q, ok := s.inbound.(*jsonlQueue)
	if !ok {
		t.Fatalf("入站队列应为 jsonlQueue，得到 %T", s.inbound)
	}
	q.mu.Lock()
	q.failNext = errors.New("注入：磁盘写满")
	q.mu.Unlock()

	status, body := postJSONAuth(t, ts.URL+"/tenon/requests", tok, `{"type":"tenon.request","text":"x"}`)
	if status != http.StatusOK {
		t.Fatalf("落盘失败仍应 200（帧已在内存，由客户端据 durable 决定是否提示），得到 %d：%s", status, body)
	}
	if !strings.Contains(body, `"durable":false`) {
		t.Fatalf("必须明确告知未落盘，得到 %s", body)
	}
	if _, pollBody := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", agentToken(t)); !strings.Contains(pollBody, `"seq":1`) {
		t.Fatalf("落盘失败时帧仍应在内存队列里可回放，得到 %s", pollBody)
	}
	// Broken 是这条失败的唯一出口（Push 只回序号）：必须能读到。
	if err := q.Broken(); err == nil {
		t.Fatalf("Broken 必须暴露落盘失败")
	}
	// 清掉注入错误，避免 Cleanup 里的 Close 把同一个注入错误再报一次
	// （Close 会返回 broken，那条行为另有用例意义，不在这里重复计分）。
	q.mu.Lock()
	q.broken = nil
	q.mu.Unlock()
}

func TestInboundRejectsUnknownFrameType(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	tok := login(t, ts.URL)
	status, _ := postJSONAuth(t, ts.URL+"/tenon/requests", tok, `{"type":"tenon.unknown"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知帧类型应 400，得到 %d", status)
	}
}

// 入站三个端点都必须鉴权：poll 会把别人说过的话摊开，ack 能把未被处理的请求直接吞掉。
func TestInboundPollAndAckRequireToken(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()

	if status, _ := getJSON(t, ts.URL+"/tenon/requests/poll?after=0"); status != http.StatusUnauthorized {
		t.Fatalf("poll 无 token 应 401，得到 %d", status)
	}
	if status, _ := postJSON(t, ts.URL+"/tenon/requests/ack", `{"upto":1}`); status != http.StatusUnauthorized {
		t.Fatalf("ack 无 token 应 401，得到 %d", status)
	}

	// 鉴权先于 preflight：Content-Type 不对且没有 token 时回 401，而不是 400。
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tenon/requests", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 时应先回 401（不该泄露 Content-Type 要求），得到 %d", resp.StatusCode)
	}
}

func getJSONAuth(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp.StatusCode, string(raw)
}

// 普通用户 token 不能消费入站队列：poll 读得到别人说过的话，ack 能吞掉未处理的请求。
func TestInboundRejectsUserTokenOnConsumerEndpoints(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	userTok := login(t, ts.URL)

	if status, _ := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", userTok); status != http.StatusForbidden {
		t.Fatalf("用户 token poll 应 403，得到 %d", status)
	}
	if status, _ := postJSONAuth(t, ts.URL+"/tenon/requests/ack", userTok, `{"upto":1}`); status != http.StatusForbidden {
		t.Fatalf("用户 token ack 应 403，得到 %d", status)
	}
	if status, _ := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", agentToken(t)); status != http.StatusOK {
		t.Fatalf("agent token poll 应 200，得到 %d", status)
	}
}

// TENON_REQUIRE_TRIGGER=1 时缺 triggerEventId 必须 200 + deny + MISSING_TRIGGER：
// 处置与其它策略结论同形，消费方不该为它加 4xx 重试逻辑。
func TestPoliciesEvaluateMissingTriggerDenies(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:      dir + "/audit.jsonl",
		SessionSecret:  []byte("secret"),
		InboundDir:     dir + "/inbound",
		RequireTrigger: true,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	status, raw := postJSON(t, ts.URL+"/tenon/policies:evaluate",
		`{"plan":{"planId":"pln_1","steps":[{"stepId":"s1","action":"inventory.GetStock","args":{"sku":"X"}}]}}`)
	if status != http.StatusOK {
		t.Fatalf("缺 triggerEventId 应 200（处置走 body），得到 %d：%s", status, raw)
	}
	var got struct {
		Outcome   string   `json:"outcome"`
		Reasons   []string `json:"reasons"`
		DecidedBy []string `json:"decidedBy"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("响应坏：%v", err)
	}
	if got.Outcome != "deny" {
		t.Fatalf("应为 deny，得到 %q（%s）", got.Outcome, raw)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "MISSING_TRIGGER" {
		t.Fatalf("reasons 应为 [MISSING_TRIGGER]，得到 %v", got.Reasons)
	}
	if len(got.DecidedBy) != 0 {
		t.Fatalf("decidedBy 应为空数组，得到 %v", got.DecidedBy)
	}
}

// ---------- tenon.confirm 的审批人角色通道 ----------

// newRolesServer 起一台带两个审批人的控制平面：alice=role:warehouse_clerk、
// bob=role:warehouse_manager。角色通道的用例都要求「握有不同角色的人各提一次」。
func newRolesServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath: dir + "/audit.jsonl",
		Users: []User{
			{User: "alice", Password: "pw", Roles: []string{"role:warehouse_clerk"}},
			{User: "bob", Password: "pw", Roles: []string{"role:warehouse_manager"}},
		},
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// lastInboundFrame 用 agent token 轮询入站队列，返回**最后一帧**的字段表。
//
// 断言写在「agent 从队列里真会读到什么」上，而不是写在 normalizeInboundFrame 的
// 返回值上：这一层要保证的是入队结果，函数返回什么只是手段。
func lastInboundFrame(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	status, body := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", agentToken(t))
	if status != http.StatusOK {
		t.Fatalf("poll 失败 %d：%s", status, body)
	}
	var got struct {
		Frames []struct {
			Frame json.RawMessage `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("poll 响应坏：%v", err)
	}
	if len(got.Frames) == 0 {
		t.Fatalf("入站队列里没有帧：%s", body)
	}
	var frame map[string]any
	if err := json.Unmarshal(got.Frames[len(got.Frames)-1].Frame, &frame); err != nil {
		t.Fatalf("帧不是 JSON 对象：%v", err)
	}
	return frame
}

// frameRoles 从帧字段表里取 roles。第二个返回值区分「没有这个键」与「空集合」：
// 两者是不同的事实——前者是通道没接上，后者才是 fail-closed 的空角色集。
func frameRoles(t *testing.T, frame map[string]any) ([]string, bool) {
	t.Helper()
	raw, ok := frame["roles"]
	if !ok {
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("roles 必须是数组，得到 %T（%v）", raw, raw)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("roles 里出现了非字符串：%v", v)
		}
		out = append(out, s)
	}
	return out, true
}

// 审批人角色只能来自宿主验证过的 token：tenon.confirm 帧里的 roles 由 claims.Roles
// 覆写，客户端自报的一律丢弃（自报等于让人自己给自己发审批权限）。
func TestConfirmFrameRoles(t *testing.T) {
	t.Run("客户端自报的角色被丢弃", func(t *testing.T) {
		ts := newRolesServer(t)
		// alice 只有 role:warehouse_clerk，却在帧里自报 role:warehouse_manager 与 user:bob。
		tok := loginAs(t, ts.URL, "alice", "pw")
		status, body := postJSONAuth(t, ts.URL+"/tenon/requests", tok,
			`{"type":"tenon.confirm","confirmId":"cfm_roles1","planId":"pln_roles1",
			  "decision":"approve","patch":null,"roles":["role:warehouse_manager"],
			  "by":"user:bob","at":"2026-10-27T09:00:00Z"}`)
		if status != http.StatusOK {
			t.Fatalf("入队失败 %d：%s", status, body)
		}
		frame := lastInboundFrame(t, ts)
		roles, ok := frameRoles(t, frame)
		if !ok {
			t.Fatal("confirm 帧必须带 roles 字段（通道没接上）：客户端自报的角色被删掉后要写入 token 的角色")
		}
		if len(roles) != 1 || roles[0] != "role:warehouse_clerk" {
			t.Fatalf("roles 必须被 token 的 claims.Roles 覆写，得到 %v", roles)
		}
		if frame["by"] != "user:alice" {
			t.Fatalf("by 也必须被 token 覆写（客户端自称 user:bob 无效），得到 %v", frame["by"])
		}
	})

	t.Run("token 角色写进帧", func(t *testing.T) {
		ts := newRolesServer(t)
		// bob 是 role:warehouse_manager，帧里一个 roles 字段都没带：token 角色仍须写进去。
		tok := loginAs(t, ts.URL, "bob", "pw")
		status, body := postJSONAuth(t, ts.URL+"/tenon/requests", tok,
			`{"type":"tenon.confirm","confirmId":"cfm_roles2","planId":"pln_roles2",
			  "decision":"approve","patch":null,"by":"user:alice","at":"2026-10-27T09:00:00Z"}`)
		if status != http.StatusOK {
			t.Fatalf("入队失败 %d：%s", status, body)
		}
		roles, ok := frameRoles(t, lastInboundFrame(t, ts))
		if !ok {
			t.Fatal("confirm 帧必须带 roles 字段（通道没接上）")
		}
		if len(roles) != 1 || roles[0] != "role:warehouse_manager" {
			t.Fatalf("帧里 roles 必须是 token 的 claims.Roles，得到 %v", roles)
		}
	})

	t.Run("无角色 token 得到空角色集", func(t *testing.T) {
		ts := newRolesServer(t)
		// 没有角色的 token：帧里必须是**空集**，不能凭空补一个角色（fail-closed 由 agent 侧
		// 的 rolesIntersect 兑现：空集 ∩ requiredRoles = ∅ ⇒ approve 403）。
		tok, err := SignToken([]byte("secret"), "user:carol", nil, time.Now().UTC(), time.Hour)
		if err != nil {
			t.Fatalf("签发无角色 token 失败：%v", err)
		}
		status, body := postJSONAuth(t, ts.URL+"/tenon/requests", tok,
			`{"type":"tenon.confirm","confirmId":"cfm_roles3","planId":"pln_roles3",
			  "decision":"approve","patch":null,"roles":["role:warehouse_manager"],
			  "by":"user:carol","at":"2026-10-27T09:00:00Z"}`)
		if status != http.StatusOK {
			t.Fatalf("入队失败 %d：%s", status, body)
		}
		roles, ok := frameRoles(t, lastInboundFrame(t, ts))
		if !ok {
			t.Fatal("即便 token 没有角色，帧里的 roles 字段也必须存在（空集而不是缺键）")
		}
		if len(roles) != 0 {
			t.Fatalf("token 没有任何角色时帧里不能出现角色（不给默认角色），得到 %v", roles)
		}
	})

	t.Run("无 token 仍 401 且不入队", func(t *testing.T) {
		ts := newRolesServer(t)
		status, _ := postJSON(t, ts.URL+"/tenon/requests",
			`{"type":"tenon.confirm","confirmId":"cfm_roles4","planId":"pln_roles4",
			  "decision":"approve","patch":null,"roles":["role:warehouse_manager"],
			  "by":"user:bob","at":"2026-10-27T09:00:00Z"}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("未鉴权的 confirm 应 401，得到 %d", status)
		}
		_, pollBody := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", agentToken(t))
		var poll struct {
			Frames []json.RawMessage `json:"frames"`
		}
		if err := json.Unmarshal([]byte(pollBody), &poll); err != nil {
			t.Fatalf("poll 响应坏：%v", err)
		}
		if len(poll.Frames) != 0 {
			t.Fatalf("401 之后不能留下任何帧（客户端自报的角色更不该活着进队列），得到 %d 帧", len(poll.Frames))
		}
	})
}
