package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadConfig 是纯装配：给它环境变量与一个假模块，它必须把规则、身份、目录都接上。
func TestLoadConfigWiresInboundSurface(t *testing.T) {
	mod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 加载期会 GET /tenon/actions/{id} 校验规则里的入参路径
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "procurement.CreatePurchaseRequest",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"qty": map[string]any{"type": "integer"}}},
			"risk":        map[string]any{"level": "medium", "reversible": true},
		})
	}))
	defer mod.Close()

	dir := t.TempDir()
	skills := filepath.Join(dir, "skills", "s1")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	rules := `[{"policyId":"biz-1","priority":1,
	  "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	  "when":{"all":[{"arg.qty":{"<=":5}}]},"outcome":"auto"}]`
	if err := os.WriteFile(filepath.Join(skills, "approval.json"), []byte(rules), 0o600); err != nil {
		t.Fatalf("写规则失败：%v", err)
	}
	users := filepath.Join(dir, "users.json")
	if err := os.WriteFile(users, []byte(`[{"user":"alice","password":"pw","roles":["role:clerk"]}]`), 0o600); err != nil {
		t.Fatalf("写账号失败：%v", err)
	}

	// TENON_MODULES 的线上格式是 id=url（registry.ParseModules），不是 JSON 数组。
	t.Setenv("TENON_MODULES", "procurement="+mod.URL)
	t.Setenv("TENON_SKILLS_DIR", filepath.Join(dir, "skills"))
	t.Setenv("TENON_USERS_FILE", users)
	t.Setenv("TENON_SESSION_SECRET", "test-secret")
	t.Setenv("TENON_UI_DIR", filepath.Join(dir, "ui"))
	t.Setenv("TENON_INBOUND_DIR", filepath.Join(dir, "inbound"))
	t.Setenv("TENON_REQUIRE_TRIGGER", "1")
	t.Setenv("TENON_STRICT_RULES", "1")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if len(cfg.Policies) != 1 || cfg.Policies[0].PolicyID != "biz-1" {
		t.Fatalf("业务规则未接上：%+v", cfg.Policies)
	}
	if len(cfg.Users) != 1 || cfg.Users[0].User != "alice" {
		t.Fatalf("账号未接上：%+v", cfg.Users)
	}
	if string(cfg.SessionSecret) != "test-secret" {
		t.Fatalf("会话密钥未接上")
	}
	if cfg.UIDir == "" || cfg.InboundDir == "" || !cfg.RequireTrigger || !cfg.StrictRules {
		t.Fatalf("目录或开关未接上：%+v", cfg)
	}
}

// 开启 TENON_REQUIRE_TRIGGER 时要有一条显式告警（现有 agent 还发不出 triggerEventId）。
func TestLoadConfigWarnsOnRequireTrigger(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	t.Setenv("TENON_REQUIRE_TRIGGER", "1")
	t.Setenv("TENON_SESSION_SECRET", "s")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if !cfg.RequireTrigger {
		t.Fatalf("开关未接上")
	}
	if !strings.Contains(buf.String(), "TENON_REQUIRE_TRIGGER") {
		t.Fatalf("开启时必须打显式告警，实际日志：%s", buf.String())
	}
}
