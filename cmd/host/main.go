// Command host 是 Tenon 的控制平面进程（design §1 的 H 框）。
//
// 它只做四件事：策略求值、授权求值、审计链、模块注册表，外加共驾帧的 REST 中转。
// 它没有任何 LLM 调用、不主动调模块、也不持久化审计日志以外的状态——所以换 agent
// 或换模型都不需要动宿主（D1 只改注册表，不动 agent 一行代码）。
//
// 环境变量：
//
//	PORT               监听端口或完整地址（缺省 8080）
//	TENON_POLICY_FILE  策略规则数组文件（例 config/policies.dev.json）；未设置时规则表为空，
//	                   所有动作都会 fail-closed 成 deny
//	TENON_GRANT_FILE   授权文件 {"grants": [...], "subjects": {...}}（例 config/grants.dev.json）；
//	                   未设置时没有任何 agent 被授权，所有调用都会被拒绝
//	TENON_AUDIT_PATH   审计 JSONL 路径（缺省 ./audit-host.jsonl）
//	TENON_MODULES      启动时登记的模块，形如 id=url,id=url
//	TENON_TEST_MODE    =1 时额外挂载 POST /tenon/admin/modules（测试专用，启动时打警告）
//	TENON_USERS_FILE   dev 账号表（例 config/users.dev.json）；未设置时登录端点永远拒绝
//	TENON_SESSION_SECRET  签发/校验 token 的密钥；未设置时用 dev 缺省值并打警告，
//	                   生产必须显式设置（漏设会让启动失败，见 New 的密钥校验）
//	TENON_INBOUND_DIR  入站队列与 origin 登记表的目录（缺省 out/inbound）
//	TENON_SKILLS_DIR   业务级规则根目录 skills/*/approval.json（缺省 skills）
//	TENON_UI_DIR       同源静态页根目录；未设置时 GET / 回 503（页面没配是可诊断的部署问题）
//	TENON_REQUIRE_TRIGGER  =1 时缺 triggerEventId 即 deny（只认 "1"）
//	TENON_STRICT_RULES     =1 时业务规则有任一份未生效即启动失败（只认 "1"，CI/生产推荐）
//
// P1 过渡说明：`TENON_REQUIRE_TRIGGER` 缺省关闭，缺 `triggerEventId` 视为事件驱动。
// 现有 cmd/agent 还发不出该字段，缺省强制会让它的每份 plan 都变成 deny（传输层与业务
// 拒绝无法区分，agent 会把它报成「策略拒绝」）；P2 让 agent 带上该字段，P4 收口时把
// 缺省改成强制。见 spec §3.11 的「分阶段启用」。
//
// 启动顺序与路径三条硬约束（写在这里是因为它们都会表现成「规则已加载但行为不变」）：
//
//  1. **模块必须先于 host 启动**。加载期要拉 `GET /tenon/actions/{id}` 校验规则里的入参
//     路径；模块离线则该 skill 的规则整份不加载（只打告警），本进程内不再重试。
//  2. `TENON_SKILLS_DIR` 缺省是 `skills`，**相对 cwd**：必须在仓库根启动 host，
//     否则业务级规则一条都读不到（文件不存在不算错误，所以不会报错）。
//  3. 重置入站队列要**同时删**三个文件：`requests.jsonl`（帧）、`requests.jsonl.meta`
//     （序号水位，只删主文件会让序号续上）、`requests-index.jsonl`（origin 登记表，
//     **治理状态而非缓存**：单删它会把在途 request 静默降级成事件驱动，绕开审批）。
//
// 进程退出前必须关闭审计存储：Windows 上不关句柄会锁住审计文件，下一次启动的链校验
// 会失败。正常退出走「信号 → 优雅停机 → defer Close」；被强杀（Stop-Process -Force /
// kill -9）时没有这个机会，因此演示与 D1 脚本应当用 Ctrl+C 收尾。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tenon.dev/internal/controlplane"
	"tenon.dev/internal/controlplane/registry"
)

const (
	// envPort 是监听端口的覆盖值，与 internal/module 用同一个名字（interfaces.md §7）。
	envPort = "PORT"
	// envPolicyFile / envGrantFile 是两份治理数据的入口，缺省都不设置。
	envPolicyFile = "TENON_POLICY_FILE"
	envGrantFile  = "TENON_GRANT_FILE"
	// envAuditPath 覆盖审计路径；TENON_AUDIT_PATH 与模块侧同名同义。
	envAuditPath = "TENON_AUDIT_PATH"
	// envModules 是启动时的模块登记表（id=url,id=url）。
	envModules = "TENON_MODULES"
	// envTestMode 打开测试面（admin 端点），仅限测试环境。
	envTestMode = "TENON_TEST_MODE"
	// envUsersFile 是 dev 账号表（例 config/users.dev.json）；未设置时登录端点永远拒绝。
	envUsersFile = "TENON_USERS_FILE"
	// envSessionSecret 是签发/校验 token 的密钥。空则退回 dev 缺省值并告警：
	// New() 要求非空密钥，留空会让 host 根本起不来。
	envSessionSecret = "TENON_SESSION_SECRET"
	// envInboundDir 是入站队列与 origin 登记表的目录（缺省 out/inbound）。
	envInboundDir = "TENON_INBOUND_DIR"
	// envSkillsDir 是 skills/*/approval.json 的根目录（缺省 skills，相对 cwd）。
	envSkillsDir = "TENON_SKILLS_DIR"
	// envUIDir 是同源静态页的根目录；空表示不提供页面。
	envUIDir = "TENON_UI_DIR"
	// envRequireTrig 决定缺 triggerEventId 是否即 deny。**只认 "1"**，缺省 false。
	envRequireTrig = "TENON_REQUIRE_TRIGGER"
	// envStrictRules 打开「业务规则有任一份未生效即启动失败」。只认 "1"。
	envStrictRules = "TENON_STRICT_RULES"

	// defaultPort 是 interfaces.md §1 给宿主约定的端口。
	defaultPort = "8080"
	// defaultAuditPath 与模块侧 ./audit-<module>.jsonl 的命名习惯对齐。
	defaultAuditPath = "./audit-host.jsonl"
	// defaultSkillsDir 是业务级规则的缺省根目录：相对 cwd，必须在仓库根启动。
	defaultSkillsDir = "skills"
	// devSessionSecret 是本地演示用的缺省密钥；生产必须显式设置。
	devSessionSecret = "tenon-dev-session-secret"
	// shutdownTimeout 是优雅停机的上限：宿主无长连接任务，5 秒足够收尾。
	shutdownTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("host: %v", err)
	}
}

// run 装配并运行宿主，返回错误即进程应当以非 0 退出。
func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	srv, err := controlplane.New(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := srv.Close(); cerr != nil {
			log.Printf("host: 关闭审计存储失败：%v", cerr)
		}
	}()

	httpSrv := &http.Server{
		Addr:              listenAddr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("host: 控制平面监听 %s（策略 %d 条、授权 %d 条、模块 %d 个，审计 %s）",
		httpSrv.Addr, len(cfg.Policies), len(cfg.Grants), len(cfg.Modules), cfg.AuditPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// 恢复默认信号行为：第二次 Ctrl+C 让进程立刻退出，不再等优雅停机。
		stop()
		log.Printf("host: 收到退出信号，开始优雅停机")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅停机失败：%w", err)
		}
		return nil
	}
}

// loadConfig 从环境变量组装控制平面配置。治理数据文件用 controlplane 的加载器读，
// 这里只负责「用哪个文件、缺失时打什么警告」。
func loadConfig() (controlplane.Config, error) {
	cfg := controlplane.Config{
		AuditPath: defaultAuditPath,
		TestMode:  testMode(),
	}
	if path := strings.TrimSpace(os.Getenv(envPolicyFile)); path != "" {
		rules, err := controlplane.LoadPolicies(path)
		if err != nil {
			return cfg, err
		}
		cfg.Policies = rules
	} else {
		log.Printf("host: 未设置 %s，策略表为空 —— 所有动作都会 fail-closed 成 deny", envPolicyFile)
	}
	if path := strings.TrimSpace(os.Getenv(envGrantFile)); path != "" {
		grants, subjects, err := controlplane.LoadGrants(path)
		if err != nil {
			return cfg, err
		}
		cfg.Grants, cfg.Subjects = grants, subjects
	} else {
		log.Printf("host: 未设置 %s，授权表为空 —— 所有调用都会被拒绝", envGrantFile)
	}
	if path := strings.TrimSpace(os.Getenv(envAuditPath)); path != "" {
		cfg.AuditPath = path
	}
	if raw := strings.TrimSpace(os.Getenv(envModules)); raw != "" {
		// 解析失败即退出：带着半份注册表上线，D1 的 A/B 轮就不再是同一个前提。
		modules, err := registry.ParseModules(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Modules = modules
	}
	if path := strings.TrimSpace(os.Getenv(envUsersFile)); path != "" {
		users, err := controlplane.LoadUsers(path)
		if err != nil {
			return cfg, err
		}
		cfg.Users = users
	} else {
		log.Printf("host: 未设置 %s，登录端点将永远拒绝", envUsersFile)
	}

	secret := strings.TrimSpace(os.Getenv(envSessionSecret))
	if secret == "" {
		// New() 要求非空密钥（空密钥签出的 token 谁都能伪造），所以这里必须给缺省值，
		// 否则一次「忘了设环境变量」会让 host 直接起不来。生产必须显式设置。
		secret = devSessionSecret
		log.Printf("host: 未设置 %s，使用 dev 缺省密钥 —— 仅限本地演示", envSessionSecret)
	}
	cfg.SessionSecret = []byte(secret)

	cfg.InboundDir = strings.TrimSpace(os.Getenv(envInboundDir))
	cfg.UIDir = strings.TrimSpace(os.Getenv(envUIDir))
	cfg.StrictRules = strings.TrimSpace(os.Getenv(envStrictRules)) == "1"
	// 只认 "1"：一个拼错的值（true/yes/on）不该悄悄改变治理行为。缺省 false ——
	// 现有 cmd/agent 还发不出 triggerEventId，缺省强制会让每份 plan 都变成 deny。
	cfg.RequireTrigger = strings.TrimSpace(os.Getenv(envRequireTrig)) == "1"
	if cfg.RequireTrigger {
		log.Printf("host: %s=1 —— 缺少 triggerEventId 的 policies:evaluate 一律 deny；"+
			"现有 agent 还发不出该字段，开启后它的每份 plan 都会被拒。P4 收口前只在测试里开",
			envRequireTrig)
	}

	if err := loadApprovalRules(&cfg); err != nil {
		return cfg, err
	}
	if cfg.TestMode {
		log.Printf("host: %s=1 —— POST /tenon/admin/modules 已挂载，仅限测试环境使用", envTestMode)
	}
	return cfg, nil
}

// loadApprovalRules 把 skills/*/approval.json 读成业务级规则并 append 进 cfg.Policies。
//
// 必须在 controlplane.New() **之前**调用：New 用 cfg.Policies 建引擎并做一次自检，
// 晚于它的规则一条都不会生效（日志照样会打印「已加载 N 条」，最坏的一种静默失败）。
//
// 规则与平台级规则同 schema、同规则池（spec §3.8），合并仍按 deny > approve > auto。
// baseURLFor 只认 Enabled 的模块：加载期要拉 GET /tenon/actions/{id} 校验入参路径，
// 把停用的模块当成在线，会让它的规则在模块其实调不到时仍被判为「有效」。
func loadApprovalRules(cfg *controlplane.Config) error {
	dir := strings.TrimSpace(os.Getenv(envSkillsDir))
	if dir == "" {
		dir = defaultSkillsDir
	}
	rules, errs := controlplane.LoadApprovalRules(dir, func(module string) (string, bool) {
		for _, m := range cfg.Modules {
			if m.ID == module && m.Enabled {
				return m.BaseURL, true
			}
		}
		return "", false
	})
	for _, e := range errs {
		log.Printf("host: 业务规则未生效：%v", e)
	}
	if cfg.StrictRules && len(errs) > 0 {
		return fmt.Errorf("host: %d 份业务规则未生效，%s=1 要求启动失败", len(errs), envStrictRules)
	}
	if len(rules) > 0 {
		log.Printf("host: 业务级规则已加载 %d 条（来自 %s）", len(rules), dir)
	}
	cfg.Policies = append(cfg.Policies, rules...)
	return nil
}

// testMode 判断是否打开测试面：只认 "1"，其它写法（true/yes/on）一律不算——
// 一个拼错的值不该悄悄把测试端点暴露到生产。
func testMode() bool {
	return strings.TrimSpace(os.Getenv(envTestMode)) == "1"
}

// listenAddr 解析监听地址：PORT 可给纯端口或完整地址，缺省 defaultPort。
func listenAddr() string {
	port := strings.TrimSpace(os.Getenv(envPort))
	if port == "" {
		port = defaultPort
	}
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}
