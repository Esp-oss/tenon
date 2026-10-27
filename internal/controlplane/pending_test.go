package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
