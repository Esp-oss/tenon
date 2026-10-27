package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// isInboundPath 判定的四条路径在测试模式下必须整条不存在（D1 的可重复性）。
//
// 用表驱动而不是逐个 case：将来加入站端点时，这里漏一条就等于给 D1 留了一个可变输入。
func TestIsInboundPath(t *testing.T) {
	inbound := []string{
		"/tenon/requests",
		"/tenon/requests/poll",
		"/tenon/requests/ack",
		"/tenon/pending",
	}
	for _, p := range inbound {
		if !isInboundPath(p) {
			t.Fatalf("%s 应判为入站路径", p)
		}
	}
	// 这几个**不是**入站：session 只发 token（关掉它 agent 起不来），
	// intents 是 agent→前端的出站队列，audit/modules 是管理面。
	notInbound := []string{
		"/tenon/session",
		"/tenon/intents",
		"/tenon/intents/poll",
		"/tenon/audit",
		"/tenon/modules",
		"/tenon/policies:evaluate",
		"/healthz",
	}
	for _, p := range notInbound {
		if isInboundPath(p) {
			t.Fatalf("%s 不该判为入站路径", p)
		}
	}
}

// 测试模式下四条入站路由必须 404，且不能是 401/405（那会让调用方以为是鉴权问题而重试）。
func TestInboundEndpointsAbsentInTestMode(t *testing.T) {
	s := newTestServer(t, Config{
		AuditPath:     t.TempDir() + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    t.TempDir() + "/inbound",
		TestMode:      true,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, p := range []string{"/tenon/requests", "/tenon/requests/poll", "/tenon/requests/ack", "/tenon/pending"} {
		resp, err := http.Post(ts.URL+p, "application/json", nil)
		if err != nil {
			t.Fatalf("%s 请求失败：%v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 在测试模式下应 404，得到 %d", p, resp.StatusCode)
		}
	}
}

// 非测试模式下入站照旧挂载：本用例只证明「门是通的」，鉴权由各端点自己管。
func TestInboundEndpointsMountedOutsideTestMode(t *testing.T) {
	s := newTestServer(t, Config{
		AuditPath:     t.TempDir() + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    t.TempDir() + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/tenon/requests", "application/json", nil)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("非测试模式下 /tenon/requests 不该 404")
	}
}
