package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPendingBoardUpsertAndRoleFilter(t *testing.T) {
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
	defer ts.Close()

	agentTok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)
	body := `{"planId":"pln_1","requestId":"req_1","by":"user:alice",
	          "summary":"扣 12 件、开 1 张采购单","items":[{"label":"从标准件仓扣掉 12 件"}],
	          "requiredRoles":["role:warehouse_manager"],"expiresAt":"2099-01-01T00:00:00Z"}`
	if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", agentTok, body); status != http.StatusOK {
		t.Fatalf("推摘要失败 %d：%s", status, resp)
	}
	// 写公告板只允许 agent：普通用户 token 若能写，就能给审批人塞伪造卡片。
	clerkTok, _ := SignToken([]byte("secret"), "user:alice", []string{"role:warehouse_clerk"}, time.Now().UTC(), time.Hour)
	if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", clerkTok, body); status != http.StatusForbidden {
		t.Fatalf("用户 token 写公告板应 403，得到 %d：%s", status, resp)
	}
	// 幂等 upsert：同 planId 再推一次不应变成两条。
	postJSONAuth(t, ts.URL+"/tenon/pending", agentTok, body)

	bobTok, _ := SignToken([]byte("secret"), "user:bob", []string{"role:warehouse_manager"}, time.Now().UTC(), time.Hour)
	_, raw := getJSONAuth(t, ts.URL+"/tenon/pending", bobTok)
	var got struct {
		Items []struct {
			PlanID string `json:"planId"`
			By     string `json:"by"`
			Mine   bool   `json:"mine"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("响应坏：%v", err)
	}
	if len(got.Items) != 1 || !got.Items[0].Mine {
		t.Fatalf("仓管应看到 1 条且 mine=true，得到 %+v", got.Items)
	}
	// by 必须被 token 的 sub 覆写：请求体里自报的 user:alice 不构成来源凭证。
	if got.Items[0].By != "agent:ag_fulfillment" {
		t.Fatalf("by 必须被 token 覆写为 agent:ag_fulfillment，得到 %q", got.Items[0].By)
	}
	if status, _ := getJSONAuth(t, ts.URL+"/tenon/pending", ""); status != http.StatusUnauthorized {
		t.Fatalf("GET 无 token 应 401，得到 %d", status)
	}

	aliceTok, _ := SignToken([]byte("secret"), "user:alice", []string{"role:warehouse_clerk"}, time.Now().UTC(), time.Hour)
	_, raw = getJSONAuth(t, ts.URL+"/tenon/pending", aliceTok)
	json.Unmarshal([]byte(raw), &got)
	if len(got.Items) != 1 || got.Items[0].Mine {
		t.Fatalf("业务员应看到同一条但 mine=false，得到 %+v", got.Items)
	}
}

// 过期条目要在 GET 时顺手剔除（不是只过滤掉）：公告板不落盘，过期残留会一直占着页面。
func TestPendingBoardPrunesExpiredItems(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	tok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)

	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, item := range []string{
		`{"planId":"pln_expired","summary":"过期","requiredRoles":[],"expiresAt":"` + past + `"}`,
		`{"planId":"pln_fresh","summary":"未过期","requiredRoles":[],"expiresAt":"` + future + `"}`,
	} {
		if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", tok, item); status != http.StatusOK {
			t.Fatalf("推摘要失败 %d：%s", status, resp)
		}
	}

	_, raw := getJSONAuth(t, ts.URL+"/tenon/pending", tok)
	var got struct {
		Items []struct {
			PlanID string `json:"planId"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("响应坏：%v", err)
	}
	if len(got.Items) != 1 || got.Items[0].PlanID != "pln_fresh" {
		t.Fatalf("过期条目不该出现在列表里，得到 %+v", got.Items)
	}

	// 「剔除」而不是「过滤」：过期条目必须真的离开公告板，否则它会永远躺在内存里。
	s.pending.mu.RLock()
	_, stillThere := s.pending.items["pln_expired"]
	left := len(s.pending.items)
	s.pending.mu.RUnlock()
	if stillThere || left != 1 {
		t.Fatalf("过期条目应被删除：stillThere=%v 剩余=%d", stillThere, left)
	}
}

// 公告板条目必须带回 state：它是「待批」与「刚刚处理完」的唯一区分。
//
// 没有这个字段时 json.Unmarshal 会静默丢掉 agent 推的 state，host 原样重写同一条目，
// 页面永远显示「待批」——agent 侧推的终态覆盖在线上等于零。
func TestPendingBoardKeepsState(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	tok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)

	// 同一 planId 先待批后 done：第二次就是线上的终态覆盖，顺序不能反。
	// expiresAt 用远期常量：会到期的夹具过几个小时就自己变红。
	for _, body := range []string{
		`{"planId":"pln_state","summary":"扣 12 件","requiredRoles":["role:warehouse_manager"],
		  "expiresAt":"2099-01-01T00:00:00Z","state":"awaiting_approval"}`,
		`{"planId":"pln_state","summary":"扣 12 件","requiredRoles":["role:warehouse_manager"],
		  "expiresAt":"2099-01-01T00:00:00Z","state":"done"}`,
	} {
		if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", tok, body); status != http.StatusOK {
			t.Fatalf("推摘要失败 %d：%s", status, resp)
		}
	}

	_, raw := getJSONAuth(t, ts.URL+"/tenon/pending", tok)
	var got struct {
		Items []struct {
			PlanID string `json:"planId"`
			State  string `json:"state"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("响应坏：%v", err)
	}
	// 终态条目**仍然出现在列表里**：协议层不替页面过滤（过滤是页面的事），
	// 但 state 必须原样带回 "done"，否则页面无从判断该怎么显示。
	if len(got.Items) != 1 || got.Items[0].PlanID != "pln_state" {
		t.Fatalf("终态条目不该被协议层过滤掉，得到 %+v", got.Items)
	}
	if got.Items[0].State != "done" {
		t.Fatalf("state 必须被保留为 done，得到 %q（完整响应：%s）", got.Items[0].State, raw)
	}
	// 板上存的那一份也必须已经是 done：state 若只是 GET 时临时算出来的，
	// 下一次 upsert 又会退回待批。
	s.pending.mu.RLock()
	stored := s.pending.items["pln_state"].State
	s.pending.mu.RUnlock()
	if stored != "done" {
		t.Fatalf("公告板上存的 state 应是 done，得到 %q", stored)
	}
}

// 缺省值不是「空串」：老客户端（不推 state）的条目仍要显示成待批，不能被当成终态。
func TestPendingBoardDefaultsStateToAwaiting(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	tok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)

	// 请求体里**没有** state 字段：这是 Task 9 之前的客户端。
	body := `{"planId":"pln_legacy","summary":"老客户端推的","requiredRoles":[],
	          "expiresAt":"2099-01-01T00:00:00Z"}`
	if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", tok, body); status != http.StatusOK {
		t.Fatalf("推摘要失败 %d：%s", status, resp)
	}

	_, raw := getJSONAuth(t, ts.URL+"/tenon/pending", tok)
	var got struct {
		Items []struct {
			PlanID string `json:"planId"`
			State  string `json:"state"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("响应坏：%v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("应看到 1 条，得到 %+v", got.Items)
	}
	if got.Items[0].State != "awaiting_approval" {
		t.Fatalf("缺省 state 必须是 awaiting_approval（不是空串），得到 %q（完整响应：%s）",
			got.Items[0].State, raw)
	}
	// 缺省值要落在板上，而不是只在响应时现算：Upsert 是唯一的写入口。
	s.pending.mu.RLock()
	stored := s.pending.items["pln_legacy"].State
	s.pending.mu.RUnlock()
	if stored != "awaiting_approval" {
		t.Fatalf("公告板上存的缺省 state 应是 awaiting_approval，得到 %q", stored)
	}
}

// host 不解释 items 的内容：带 detail 进来就要原样带回去（页面靠它渲染折叠区）。
func TestPendingBoardKeepsItemDetail(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	agentTok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)
	body := `{"planId":"pln_1","summary":"扣减 STD-1002 库存 3 件","state":"awaiting_approval",
	          "requiredRoles":["role:warehouse_manager"],"expiresAt":"2099-01-01T00:00:00Z",
	          "items":[{"label":"扣减 STD-1002 库存 3 件","detail":"inventory.DeductStock（{\"qty\":3}）"}]}`
	if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", agentTok, body); status != http.StatusOK {
		t.Fatalf("推公告板失败 %d：%s", status, resp)
	}
	code, raw := getJSONAuth(t, ts.URL+"/tenon/pending", agentTok)
	if code != http.StatusOK || !strings.Contains(raw, `"detail":"inventory.DeductStock`) {
		t.Fatalf("detail 没有被透传：%d %s", code, raw)
	}
}
