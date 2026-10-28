package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenon.dev/internal/controlplane"
	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/controlplane/registrylog"
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

	cfg, suspended, err := loadConfig(modules)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if suspended != nil {
		t.Fatal("模块已登记时不该有挂起的业务规则（补校验应当当场通过）")
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
	cfg, _, err := loadConfig(registry.NewRegistry())
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
			cfg, _, err := loadConfig(registry.NewRegistry())
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

// ---------- 业务规则延迟校验（裁决 ①a） ----------

// actionModule 起一个只提供 GET /tenon/actions/{id} 的假模块。
// schemas 里没有的 action 回 404（模拟「定义拉不到」）。
func actionModule(t *testing.T, schemas map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/tenon/actions/")
		schema, ok := schemas[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          id,
			"inputSchema": json.RawMessage(schema),
			"risk":        map[string]any{"level": "medium", "reversible": true},
		})
	}))
}

// writeApproval 在临时目录下写 skills/s1/approval.json，返回 skills 根目录。
func writeApproval(t *testing.T, rules string) string {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "s1")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(rules), 0o600); err != nil {
		t.Fatalf("写规则失败：%v", err)
	}
	return filepath.Join(dir, "skills")
}

// setupRulesEnv 把 loadConfig 需要但与本组用例无关的环境变量压成「测试态缺省」：
// 策略/授权/账号都留空（于是唯一的规则来源就是 skills），审计与入站写到临时目录
// （否则会在仓库根写出 audit-host.jsonl）。
func setupRulesEnv(t *testing.T, skills string) {
	t.Helper()
	setHMACSecretFile(t)
	dir := t.TempDir()
	t.Setenv("TENON_SKILLS_DIR", skills)
	t.Setenv("TENON_POLICY_FILE", "")
	t.Setenv("TENON_GRANT_FILE", "")
	t.Setenv("TENON_USERS_FILE", "")
	t.Setenv("TENON_SESSION_SECRET", "test-secret")
	t.Setenv("TENON_AUDIT_PATH", filepath.Join(dir, "audit.jsonl"))
	t.Setenv("TENON_INBOUND_DIR", filepath.Join(dir, "inbound"))
	t.Setenv("TENON_UI_DIR", "")
	t.Setenv("TENON_REQUIRE_TRIGGER", "")
	t.Setenv("TENON_STRICT_RULES", "")
}

// pendingRuleJSON 是一条引用 procurement 的业务规则：qty<=5 自动过，其余不命中。
const pendingRuleJSON = `[
  {"policyId":"biz-small","priority":10,
   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
   "when":{"all":[{"arg.qty":{"<=":5}}]},"outcome":"auto"}
]`

// startHostForRules 用一份装配好的 Config 起一个控制平面服务，并把挂起规则接上
// 「补校验通过 → 替换引擎」的通路。返回的 registry 就是加载期用的那个实例。
func startHostForRules(t *testing.T, cfg controlplane.Config, suspended *suspendedRules) *httptest.Server {
	t.Helper()
	srv, err := controlplane.New(cfg)
	if err != nil {
		t.Fatalf("装配控制平面失败：%v", err)
	}
	t.Cleanup(func() {
		if cerr := srv.Close(); cerr != nil {
			t.Errorf("关闭控制平面失败：%v", cerr)
		}
	})
	suspended.watch(srv.ReplacePolicies)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// evaluateOutcome 用真实端点求值一次「采购申请 qty 件」，返回合并后的处置。
//
// 走 HTTP 而不是直接调引擎：本组用例要证明的是「规则真的进了运行期的策略引擎」，
// 内部字段替换成功但没接上求值路径的话，只有这条断言抓得住。
func evaluateOutcome(t *testing.T, host string, qty int) string {
	t.Helper()
	body := fmt.Sprintf(`{"plan":{"planId":"pln_pending","steps":[`+
		`{"stepId":"s1","action":"procurement.CreatePurchaseRequest","args":{"qty":%d}}]}}`, qty)
	resp, err := http.Post(host+"/tenon/policies:evaluate", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("求值请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Outcome string `json:"outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("求值响应不是合法 JSON：%v", err)
	}
	return out.Outcome
}

// readRegistryEvents 读出注册事件链的每一行（map 便于按字段断言）。
func readRegistryEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读注册事件链失败：%v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("事件行不是合法 JSON（%s）：%v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// ① 挂起 → 模块注册 → 生效：加载期注册表为空时规则挂起（不阻塞启动），模块注册成功时
// 同步补校验并把规则真的装进运行期策略引擎。
func TestPendingApprovalRulesTakeEffectAfterModuleRegisters(t *testing.T) {
	mod := actionModule(t, map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()
	setupRulesEnv(t, writeApproval(t, pendingRuleJSON))

	reg := registry.NewRegistry() // 生产里 host 启动时就是这张空表（D5）
	cfg, suspended, err := loadConfig(reg)
	if err != nil {
		t.Fatalf("装配失败（挂起规则不该阻塞启动）：%v", err)
	}
	if len(cfg.Policies) != 0 {
		t.Fatalf("引用未注册模块的规则不该在加载期生效：%+v", cfg.Policies)
	}
	if suspended == nil {
		t.Fatalf("引用未注册模块的规则应被挂起，而不是丢弃（D5 之后加载期注册表必为空）")
	}
	ts := startHostForRules(t, cfg, suspended)

	// 注册之前：没有任何规则命中 → fail-closed 成 deny。
	if got := evaluateOutcome(t, ts.URL, 3); got != "deny" {
		t.Fatalf("模块未注册时的处置 = %q，期望 deny", got)
	}

	// 注册成功 → 同步补校验通过 → 规则生效（Upsert 返回时已经装好）。
	reg.Upsert(registry.ModuleEntry{ID: "procurement", BaseURL: mod.URL})
	if got := evaluateOutcome(t, ts.URL, 3); got != "auto" {
		t.Fatalf("模块注册后小额的处置 = %q，期望 auto（补校验通过后规则应当已生效）", got)
	}
	// 生效的是这条规则的条件本身：不满足条件的请求仍无命中 → deny。
	if got := evaluateOutcome(t, ts.URL, 12); got != "deny" {
		t.Fatalf("不满足规则的请求处置 = %q，期望 deny（说明生效的是规则的条件，不是无条件放行）", got)
	}
}

// ② 补校验失败 → 整份丢弃 + 记 rule-dropped 事件（落在注册事件链上，不是业务审计）。
func TestPendingApprovalRuleDroppedWhenRetryValidationFails(t *testing.T) {
	// 模块只活着，但动作定义拉不到（404）——补校验必然失败。
	mod := actionModule(t, nil)
	defer mod.Close()
	setupRulesEnv(t, writeApproval(t, pendingRuleJSON))

	eventsPath := filepath.Join(t.TempDir(), "registry-events.jsonl")
	reg, err := registry.NewRegistryWithLog(eventsPath)
	if err != nil {
		t.Fatalf("打开注册事件链失败：%v", err)
	}
	t.Cleanup(func() {
		if cerr := reg.Close(); cerr != nil {
			t.Errorf("关闭注册事件链失败：%v", cerr)
		}
	})

	cfg, suspended, err := loadConfig(reg)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if suspended == nil {
		t.Fatal("规则应被挂起")
	}
	ts := startHostForRules(t, cfg, suspended)

	reg.Upsert(registry.ModuleEntry{ID: "procurement", BaseURL: mod.URL})

	// 规则没有生效：补校验失败 → 整份丢弃（而不是进规则池）。
	if got := evaluateOutcome(t, ts.URL, 3); got != "deny" {
		t.Fatalf("补校验失败后规则不该生效，处置 = %q，期望 deny", got)
	}

	// 事件：一条 registered（模块） + 一条 rule-dropped（规则）。
	events := readRegistryEvents(t, eventsPath)
	var dropped []map[string]any
	for _, ev := range events {
		if ev["event"] == "rule-dropped" {
			dropped = append(dropped, ev)
		}
	}
	if len(dropped) != 1 {
		t.Fatalf("rule-dropped 事件数 = %d，期望 1（全部事件：%v）", len(dropped), events)
	}
	ev := dropped[0]
	wantFields := map[string]string{
		"moduleId": "procurement",
		"policyId": "biz-small",
	}
	for field, want := range wantFields {
		if got, _ := ev[field].(string); got != want {
			t.Fatalf("rule-dropped 的 %s = %q，期望 %q（事件：%v）", field, got, want, ev)
		}
	}
	if file, _ := ev["ruleFile"].(string); !strings.HasSuffix(file, "approval.json") {
		t.Fatalf("rule-dropped 的 ruleFile = %q，期望指向 skills/s1/approval.json", file)
	}
	if reason, _ := ev["reason"].(string); !strings.Contains(reason, "404") {
		t.Fatalf("rule-dropped 的 reason = %q，期望点名拉取动作定义返回 404", reason)
	}
	// 链本身仍然自洽（新增字段不破坏哈希链）。
	if err := registrylog.Verify(eventsPath); err != nil {
		t.Fatalf("写入 rule-dropped 之后链应仍然完整：%v", err)
	}
}

// ③ 严格模式下「pending（模块还没注册）」不算失败：宿主照常启动。
func TestStrictRulesDoNotFailOnUnregisteredModule(t *testing.T) {
	setupRulesEnv(t, writeApproval(t, pendingRuleJSON))
	t.Setenv("TENON_STRICT_RULES", "1")

	cfg, suspended, err := loadConfig(registry.NewRegistry())
	if err != nil {
		t.Fatalf("%s=1 时「模块还没注册」不该导致启动失败：%v", envStrictRules, err)
	}
	if len(cfg.Policies) != 0 || suspended == nil {
		t.Fatalf("规则应挂起而不是生效：policies=%d suspended=%v", len(cfg.Policies), suspended != nil)
	}
}

// ③ 的另一半：严格模式没有被放宽——模块**已注册**且校验失败仍然拒绝启动。
func TestStrictRulesStillFailWhenRegisteredModuleFailsValidation(t *testing.T) {
	mod := actionModule(t, nil) // 动作定义 404
	defer mod.Close()
	setupRulesEnv(t, writeApproval(t, pendingRuleJSON))
	t.Setenv("TENON_STRICT_RULES", "1")

	reg := registry.NewRegistry()
	reg.Upsert(registry.ModuleEntry{ID: "procurement", BaseURL: mod.URL})

	_, _, err := loadConfig(reg)
	if err == nil {
		t.Fatalf("%s=1 时「模块已注册且校验失败」必须拒绝启动", envStrictRules)
	}
	if !strings.Contains(err.Error(), envStrictRules) {
		t.Fatalf("错误信息必须点名 %s，实际：%v", envStrictRules, err)
	}
}

// flakyModule 是一个「暂时不可达」的模块桩：up=false 时动作定义回 503（模拟真实部署里
// 「刚注册、端口还没绑」的窗口，连接被拒同理），up=true 后才正常返回定义。
type flakyModule struct {
	*httptest.Server
	up atomic.Bool
}

func newFlakyModule(t *testing.T, schema string) *flakyModule {
	t.Helper()
	m := &flakyModule{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.up.Load() {
			http.Error(w, "还没起好", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          strings.TrimPrefix(r.URL.Path, "/tenon/actions/"),
			"inputSchema": json.RawMessage(schema),
			"risk":        map[string]any{"level": "medium", "reversible": true},
		})
	}))
	t.Cleanup(m.Close)
	return m
}

// ④ 模块暂时不可达（503 / 连接被拒）**不是**校验失败：规则继续挂起、不记 rule-dropped，
// 等模块活过来由周期重试补上并生效。
//
// 这条用例来自一次真实的首启观测：模块是「先异步注册、后 ListenAndServe」，所以注册那一
// 刻 host 拨过去必然失败。若把它当校验失败丢弃，业务规则会每次首启都被永久丢掉。
func TestPendingApprovalRulesSurviveTransientlyUnreachableModule(t *testing.T) {
	mod := newFlakyModule(t, `{"type":"object","properties":{"qty":{"type":"integer"}}}`)
	setupRulesEnv(t, writeApproval(t, pendingRuleJSON))

	eventsPath := filepath.Join(t.TempDir(), "registry-events.jsonl")
	reg, err := registry.NewRegistryWithLog(eventsPath)
	if err != nil {
		t.Fatalf("打开注册事件链失败：%v", err)
	}
	t.Cleanup(func() {
		if cerr := reg.Close(); cerr != nil {
			t.Errorf("关闭注册事件链失败：%v", cerr)
		}
	})

	cfg, suspended, err := loadConfig(reg)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	ts := startHostForRules(t, cfg, suspended)

	// 注册瞬间模块还没「起好」：补校验拿不到定义。
	reg.Upsert(registry.ModuleEntry{ID: "procurement", BaseURL: mod.URL})
	if got := evaluateOutcome(t, ts.URL, 3); got != "deny" {
		t.Fatalf("模块暂不可达时规则不该生效，处置 = %q，期望 deny", got)
	}
	for _, ev := range readRegistryEvents(t, eventsPath) {
		if ev["event"] == "rule-dropped" {
			t.Fatalf("「暂不可达」不该记 rule-dropped（一次连接失败被当成规则写错），事件：%v", ev)
		}
	}

	// 模块起好了 → 周期重试补上 → 规则生效。
	mod.up.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go retrySuspendedRules(ctx, suspended, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if evaluateOutcome(t, ts.URL, 3) == "auto" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("模块恢复后 3s 内周期重试没有把挂起的规则补上（规则仍未生效）")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, ev := range readRegistryEvents(t, eventsPath) {
		if ev["event"] == "rule-dropped" {
			t.Fatalf("恢复路径上也不该丢弃规则，事件：%v", ev)
		}
	}
}
