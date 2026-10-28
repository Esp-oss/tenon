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

// setHMACSecretFile 给测试注入一份临时共享密钥文件。
//
// 注册/心跳端点的密钥是**必填**的（D7：缺了宿主拒绝启动），所以每个走 loadConfig 的
// 用例都得先把它准备好——否则测的就不是「装配接对了没」，而是「密钥校验有没有生效」。
func setHMACSecretFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hmac-secret")
	// 带行尾换行是真实文件的样子（Set-Content / echo 都会留一个）。
	if err := os.WriteFile(path, []byte("test-hmac-secret\n"), 0o600); err != nil {
		t.Fatalf("写密钥文件失败：%v", err)
	}
	t.Setenv("TENON_HMAC_SECRET_FILE", path)
}

// loadConfig 是纯装配：给它环境变量与一个假模块，它必须把规则、身份、目录都接上。
func TestLoadConfigWiresInboundSurface(t *testing.T) {
	setHMACSecretFile(t)
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
	if string(cfg.HMACSecret) != "test-hmac-secret" {
		t.Fatalf("注册/心跳的共享密钥未接上：%q", cfg.HMACSecret)
	}
}

// 开启 TENON_REQUIRE_TRIGGER 时要有一条显式告警（现有 agent 还发不出 triggerEventId）。
func TestLoadConfigWarnsOnRequireTrigger(t *testing.T) {
	setHMACSecretFile(t)
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

// D7：拿不到可用的共享密钥就启动失败（未设置 / 文件不存在 / 空文件 / 多行），
// 且错误信息必须点名是哪个环境变量——运维只凭日志修问题。
func TestLoadConfigFailsWithoutUsableHMACSecret(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n\r\n"), 0o600); err != nil {
		t.Fatalf("写空密钥文件失败：%v", err)
	}
	multi := filepath.Join(dir, "multi")
	if err := os.WriteFile(multi, []byte("line1\nline2\n"), 0o600); err != nil {
		t.Fatalf("写多行密钥文件失败：%v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"未设置", ""},
		{"文件不存在", filepath.Join(dir, "nope")},
		{"空文件", empty},
		{"多行", multi},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TENON_HMAC_SECRET_FILE", c.value)
			cfg, err := loadConfig()
			if err == nil {
				t.Fatalf("缺可用密钥时必须启动失败，实际装配出了 cfg=%+v", cfg)
			}
			if !strings.Contains(err.Error(), "TENON_HMAC_SECRET_FILE") {
				t.Fatalf("错误信息必须点名 TENON_HMAC_SECRET_FILE，实际：%v", err)
			}
		})
	}
}

// 密钥文件带 UTF-8 BOM 与 CRLF（PowerShell 5.1 的 UTF8 就写 BOM）：取到的密钥不能带上
// BOM——带上之后症状是「两边明明同一个值却验签失败」，极难排查。
func TestLoadHMACSecretStripsBOMAndLineEnding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hmac-secret")
	if err := os.WriteFile(path, []byte("\ufeffs3cr3t\r\n"), 0o600); err != nil {
		t.Fatalf("写密钥文件失败：%v", err)
	}
	t.Setenv("TENON_HMAC_SECRET_FILE", path)
	got, err := loadHMACSecret()
	if err != nil {
		t.Fatalf("读密钥失败：%v", err)
	}
	if string(got) != "s3cr3t" {
		t.Fatalf("密钥应是去 BOM、去行尾的 s3cr3t，得到 %q", got)
	}
}
