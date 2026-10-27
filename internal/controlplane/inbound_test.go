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
	status, body := postJSON(t, base+"/tenon/session", `{"user":"alice","password":"pw"}`)
	if status != http.StatusOK {
		t.Fatalf("登录失败 %d：%s", status, body)
	}
	var got struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("登录响应坏：%v", err)
	}
	return got.Token
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

	_, pollBody := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", tok)
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
	_, pollBody := getJSONAuth(t, ts1.URL+"/tenon/requests/poll?after=0", tok)
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
	tok := login(t, ts.URL)
	status, body := postJSONAuth(t, ts.URL+"/tenon/requests/ack", tok, `{"upto":-1}`)
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
	if _, pollBody := getJSONAuth(t, ts.URL+"/tenon/requests/poll?after=0", tok); !strings.Contains(pollBody, `"seq":1`) {
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
