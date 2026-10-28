package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tenon.dev/internal/controlplane/registry"
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

	// 模块不再是环境变量：注册表实例从小起，先 Upsert 再 loadConfig。
	// 加载期校验业务规则要按模块取 baseURL（GET /tenon/actions/{id}），这正是注册表
	// 与装配共享同一个实例的原因——生产里这张表在 host 起来时是空的，模块自注册后才满。
	modules := registry.NewRegistry()
	modules.Upsert(registry.ModuleEntry{ID: "procurement", BaseURL: mod.URL})
	t.Setenv("TENON_SKILLS_DIR", filepath.Join(dir, "skills"))
	t.Setenv("TENON_USERS_FILE", users)
	t.Setenv("TENON_SESSION_SECRET", "test-secret")
	t.Setenv("TENON_UI_DIR", filepath.Join(dir, "ui"))
	t.Setenv("TENON_INBOUND_DIR", filepath.Join(dir, "inbound"))
	t.Setenv("TENON_REQUIRE_TRIGGER", "1")
	t.Setenv("TENON_STRICT_RULES", "1")

	cfg, err := loadConfig(modules)
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
	cfg, err := loadConfig(registry.NewRegistry())
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
			cfg, err := loadConfig(registry.NewRegistry())
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

// 失联判定必须有唯一的后台判定点：注册表自己不会变旧，没人扫表的话，5 秒前就死掉的
// 模块会永远保持 alive，agent 的能力发现就会把回合打到死地址上。
//
// 用例覆盖三件事：①过期条目被扫成 lost（并留一行日志，下一轮要进事件链）；②lost 不等于
// 删除——List() 仍看得见它、只是 Enabled() 不再返回；③ctx 取消后循环真的停了，不留一个
// 继续改状态的悬挂 goroutine。TTL 是包常量（15s），所以用 DebugSetLastSeen 把时间拨回
// 过去，而不是真的等三个心跳。
//
// 注意日志是**另一个 goroutine** 写的：所以缓冲区必须带锁，并且要等到「状态变了 + 日志
// 出现了」两个条件同时成立——只看 Enabled() 会在日志落盘之前就断言，跑 -count=5 就会闪红。
func TestSweepRegistryMarksLostAndStopsOnCancel(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	reg := registry.NewRegistry()
	reg.Upsert(registry.ModuleEntry{ID: "inventory", BaseURL: "http://127.0.0.1:8081"})
	reg.DebugSetLastSeen("inventory", time.Now().Add(-time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sweepRegistry(ctx, reg, 5*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for !(len(reg.Enabled()) == 0 && strings.Contains(buf.String(), "inventory")) {
		if time.Now().After(deadline) {
			t.Fatalf("清扫循环没把过期模块判成 lost 并留日志：Enabled() 剩 %d 条，日志：%q",
				len(reg.Enabled()), buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if list := reg.List(); len(list) != 1 || list[0].State != registry.StateLost {
		t.Fatalf("List 应保留条目并标记 lost（失联不是删除）：%+v", list)
	}

	// 取消后先给它足够时间跑到 ctx.Done 分支，再投放一个同样过期的条目：
	// 顺序是刻意的——先停干净再投放，才不会把「刚好扫了一次」误判成「循环没停」。
	cancel()
	time.Sleep(100 * time.Millisecond)
	buf.Reset()
	reg.Upsert(registry.ModuleEntry{ID: "procurement", BaseURL: "http://127.0.0.1:8082"})
	reg.DebugSetLastSeen("procurement", time.Now().Add(-time.Minute))
	time.Sleep(50 * time.Millisecond)
	if got := len(reg.Enabled()); got != 1 {
		t.Fatalf("ctx 取消后清扫循环仍在跑：procurement 也被判成 lost（Enabled()=%d）", got)
	}
	if log := buf.String(); strings.Contains(log, "procurement") {
		t.Fatalf("ctx 取消后仍在写日志：%s", log)
	}
}

// syncBuffer 是带锁的 bytes.Buffer：清扫循环在另一个 goroutine 里写日志，测试这边同时
// 读，裸的 bytes.Buffer 既是数据竞争，也会让「日志还没落盘」看起来像「没有日志」。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
