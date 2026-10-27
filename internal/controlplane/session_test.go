package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionTokenRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	tok, err := SignToken(secret, "user:alice", []string{"role:warehouse_clerk"}, now, time.Hour)
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}
	claims, err := VerifyToken(secret, tok, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if claims.Subject != "user:alice" || len(claims.Roles) != 1 {
		t.Fatalf("claims 不符：%+v", claims)
	}
}

func TestVerifyTokenRejectsTamperedAndExpired(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	tok, _ := SignToken(secret, "user:alice", nil, now, time.Hour)

	if _, err := VerifyToken(secret, tok+"x", now); err == nil {
		t.Fatalf("篡改后必须失败")
	}
	if _, err := VerifyToken([]byte("other"), tok, now); err == nil {
		t.Fatalf("换密钥必须失败")
	}
	if _, err := VerifyToken(secret, tok, now.Add(2*time.Hour)); err == nil {
		t.Fatalf("过期必须失败")
	}
}

func TestSessionEndpointIssuesTokenForKnownUser(t *testing.T) {
	s := newTestServer(t, Config{
		AuditPath: t.TempDir() + "/audit.jsonl",
		Users: []User{
			{User: "alice", Password: "pw-a", Roles: []string{"role:warehouse_clerk"}},
		},
		SessionSecret: []byte("secret"),
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	status, body := postJSON(t, ts.URL+"/tenon/session", `{"user":"alice","password":"pw-a"}`)
	if status != http.StatusOK {
		t.Fatalf("登录应成功，得到 %d：%s", status, body)
	}
	var got struct {
		Token string   `json:"token"`
		User  string   `json:"user"`
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if got.User != "user:alice" || len(got.Roles) != 1 || got.Token == "" {
		t.Fatalf("响应不符：%+v", got)
	}

	if status, _ := postJSON(t, ts.URL+"/tenon/session", `{"user":"alice","password":"bad"}`); status != http.StatusUnauthorized {
		t.Fatalf("错口令应 401，得到 %d", status)
	}
}
