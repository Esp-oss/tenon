package controlplane

import (
	"encoding/json"
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

	_, pollBody := getJSON(t, ts.URL+"/tenon/requests/poll?after=0")
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
	_, pollBody := getJSON(t, ts1.URL+"/tenon/requests/poll?after=0")
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

func TestInboundRejectsUnknownFrameType(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	tok := login(t, ts.URL)
	status, _ := postJSONAuth(t, ts.URL+"/tenon/requests", tok, `{"type":"tenon.unknown"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知帧类型应 400，得到 %d", status)
	}
}
