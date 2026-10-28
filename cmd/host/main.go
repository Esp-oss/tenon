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
//	TENON_HMAC_SECRET_FILE  模块注册/心跳端点的共享密钥文件（D7：**未设置或读不到即拒绝启动**）。
//	                   宿主不再有「不挂注册端点」的档位——静默不可用会让运维以为注册在工作，
//	                   实际所有模块永远上不了线
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
// 模块注册（R1 / D5）：**只有模块自注册一条路径**。没有 `TENON_MODULES`、没有启动时
// 静态登记、没有 static 例外——模块 `POST /tenon/registry/register` 申报自己，之后每
// `registry.HeartbeatInterval`（5s）心跳一次；host 用一个后台清扫循环按
// `registry.DefaultTTL`（3 个心跳）把失联的判成 lost。`GET /tenon/modules` 只返回
// `enabled && alive` 的条目，所以「不心跳 = 不可见」，重启后的空表靠模块收到 409
// 重申报自行恢复（D6 不落盘）。
//
// P1 过渡说明：`TENON_REQUIRE_TRIGGER` 缺省关闭，缺 `triggerEventId` 视为事件驱动。
// 现有 cmd/agent 还发不出该字段，缺省强制会让它的每份 plan 都变成 deny（传输层与业务
// 拒绝无法区分，agent 会把它报成「策略拒绝」）；P2 让 agent 带上该字段，P4 收口时把
// 缺省改成强制。见 spec §3.11 的「分阶段启用」。
//
// 启动顺序与路径三条硬约束（写在这里是因为它们都会表现成「规则已加载但行为不变」）：
//
//  1. **业务规则必须在模块在场时校验**。加载期要拉 `GET /tenon/actions/{id}` 校验规则里
//     的入参路径；拿不到定义就整份丢弃（fail-closed）。模块改成自注册之后，加载期注册表
//     必为空（host 先起、模块后注册），于是 `skills/*/approval.json` 里引用模块的规则都会
//     在这一步被丢弃，`TENON_STRICT_RULES=1` 时更会直接启动失败——**这是本轮 plan 未覆盖
//     的已知缺口**：「模块注册后重试校验」还没有实现，方向留待后续任务决定。
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
	// envHMACSecretFile 是注册/心跳端点的共享密钥文件。名字的唯一定义在 controlplane
	// （EnvHMACSecretFile），那里还要用它拼 503 的报文，两处写死迟早分叉。
	envHMACSecretFile = controlplane.EnvHMACSecretFile
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
	// 注册表在装配前建好：加载期的业务规则校验要按模块取 baseURL，运行期的可见性
	// （GET /tenon/modules → Enabled）也必须看同一张表——两份真相会让「规则判定在线」
	// 与「agent 看见在线」互相矛盾。注意它**启动时是空的**（D5：只有模块自注册）。
	modules := registry.NewRegistry()
	cfg, err := loadConfig(modules)
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
	log.Printf("host: 控制平面监听 %s（策略 %d 条、授权 %d 条、审计 %s；模块注册表启动为空，等模块自注册）",
		httpSrv.Addr, len(cfg.Policies), len(cfg.Grants), cfg.AuditPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 失联判定是 **push 模型的必要另一半**：没人清扫，注册表里 5 秒前就死掉的模块会
	// 永远保持 alive，agent 的能力发现就会把回合打到死地址上。
	go sweepRegistry(ctx, srv.Registry(), registry.HeartbeatInterval)

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

// sweepRegistry 每 interval 调一次 Sweep，把超过 TTL 没心跳的模块判成 lost。
//
// 这是「失联」的**唯一判定点**：模块侧只管 push 心跳，host 侧只管按 TTL 扫表。两端没有
// 共享的存活概念，于是谁先重启都不会留下永久脏状态——host 重启 → 空表 → 模块下一个心跳
// 收到 409 并重申报；模块重启 → 心跳断了 → TTL 到点变 lost。
//
// 为什么必须有这个循环：注册表本身不会自己变旧。没有它，5 秒前就死掉的模块会永远保持
// alive，agent 的能力发现就会把回合打到死地址上（fail-closed 只保证「错在宿主这一侧
// 被拒绝」，不保证「宿主知道对面死了」）。
//
// 返回的 id 本轮只打日志；写进 registry-events.jsonl 是下一轮的事（Task 5），届时那条
// 链写失败也不得改变失联判定——观测不决定业务处置。
//
// ctx 由调用方随进程退出取消（信号 → 优雅停机），ticker 一并停止，不留悬挂 goroutine。
func sweepRegistry(ctx context.Context, reg *registry.Registry, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, id := range reg.Sweep(time.Now()) {
				log.Printf("host: 模块 %q 超过 %s 没有心跳，已标记 lost（不再出现在 GET /tenon/modules）",
					id, registry.DefaultTTL)
			}
		}
	}
}

// loadConfig 从环境变量组装控制平面配置。治理数据文件用 controlplane 的加载器读，
// 这里只负责「用哪个文件、缺失时打什么警告」。
//
// modules 是运行期注册表的**同一个实例**：加载期校验业务规则要按模块取 baseURL，
// 运行期可见性看 Enabled()，两处必须是同一张表。它启动时为空——模块自注册是唯一
// 入口（D5），没有 TENON_MODULES 之类的静态登记路径。
//
// 传 nil 等价于「自建一张空表」，与 controlplane.Config.Registry 的缺省语义一致。
func loadConfig(modules *registry.Registry) (controlplane.Config, error) {
	if modules == nil {
		modules = registry.NewRegistry()
	}
	cfg := controlplane.Config{
		AuditPath: defaultAuditPath,
		TestMode:  testMode(),
		Registry:  modules,
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
	// 注册/心跳的共享密钥：**读不到就返回错误**，由 main 以 log.Fatalf 收尾（D7）。
	// 这里绝不退回缺省值：一个猜出来的密钥等于让注册端点对任何知道缺省值的人开放。
	secretBytes, err := loadHMACSecret()
	if err != nil {
		return cfg, err
	}
	cfg.HMACSecret = secretBytes
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

// loadHMACSecret 读模块注册/心跳端点的共享密钥文件。
//
// D7：缺密钥**拒绝启动**，不是「不挂端点」。区别在运维看到什么——「起不来，缺
// TENON_HMAC_SECRET_FILE」当场可修；「宿主正常启动、模块永远上不了线」要等验收才发现。
// 所以这里一律返回错误，由 main 的 log.Fatalf 收尾（非 0 退出、信息点名环境变量）。
//
// 取内容的规则刻意做得宽容又明确：允许 UTF-8 BOM（PowerShell 5.1 的 UTF8 编码会写 BOM，
// 留着它会让密钥开头多三个字节，症状是「两边明明同一个值却验签失败」），允许行尾换行与
// 两端空白；**不允许**多行内容——那说明文件里混进了别的东西（注释、说明、第二个密钥），
// 替调用方猜哪一行是密钥比直接报错危险。
func loadHMACSecret() ([]byte, error) {
	path := strings.TrimSpace(os.Getenv(envHMACSecretFile))
	if path == "" {
		return nil, fmt.Errorf("未设置 %s：模块注册/心跳端点（POST /tenon/registry/register 与 "+
			"/tenon/registry/heartbeat）靠它验签，缺密钥时宿主拒绝启动（D7）", envHMACSecretFile)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s=%s 失败：%w；注册/心跳端点没有密钥可用，宿主拒绝启动（D7）",
			envHMACSecretFile, path, err)
	}
	secret := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
	if secret == "" {
		return nil, fmt.Errorf("%s=%s 是空文件：注册/心跳端点需要非空共享密钥，宿主拒绝启动（D7）",
			envHMACSecretFile, path)
	}
	if strings.ContainsAny(secret, "\r\n") {
		return nil, fmt.Errorf("%s=%s 含多行内容：密钥文件应当只有一行，"+
			"多行时无法判断哪一行是密钥，宿主拒绝启动（D7）", envHMACSecretFile, path)
	}
	return []byte(secret), nil
}

// loadApprovalRules 把 skills/*/approval.json 读成业务级规则并 append 进 cfg.Policies。
//
// 必须在 controlplane.New() **之前**调用：New 用 cfg.Policies 建引擎并做一次自检，
// 晚于它的规则一条都不会生效（日志照样会打印「已加载 N 条」，最坏的一种静默失败）。
//
// 规则与平台级规则同 schema、同规则池（spec §3.8），合并仍按 deny > approve > auto。
// baseURLFor 只认 Enabled()（`enabled && alive`）：加载期要拉 GET /tenon/actions/{id}
// 校验入参路径，把停用的、或已经失联的模块当成在线，会让它的规则在模块其实调不到时
// 仍被判为「有效」。
//
// **本轮已知缺口**：模块改成自注册之后，加载期注册表必定还是空的（模块在 host 起来
// 之后才注册），所以引用模块的规则都会在这一步被判「模块未登记/离线」而整份丢弃；
// `TENON_STRICT_RULES=1` 会因此拒绝启动。方向仍是 fail-closed（没有 schema 就不放行
// 未校验的规则），但「注册后重试校验」还没做——见包注释的第 1 条硬约束。
func loadApprovalRules(cfg *controlplane.Config) error {
	dir := strings.TrimSpace(os.Getenv(envSkillsDir))
	if dir == "" {
		dir = defaultSkillsDir
	}
	rules, errs := controlplane.LoadApprovalRules(dir, func(module string) (string, bool) {
		for _, m := range cfg.Registry.Enabled() {
			if m.ID == module {
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
