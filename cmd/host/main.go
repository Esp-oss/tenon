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
//  1. **业务规则分两段校验**（裁决 ①a）。加载期要拉 `GET /tenon/actions/{id}` 校验规则
//     里的入参路径，而模块改成自注册之后，加载期注册表必定还是空的（host 先起、模块后
//     注册）。所以引用未注册模块的规则**不丢弃、不阻塞启动**，按 moduleId 挂起；模块
//     注册成功时（registry.Upsert 之后、锁外）同步触发一次该模块的补校验：
//     通过 → 并入运行期规则集（controlplane.Server.ReplacePolicies），
//     失败 → 整份丢弃并写一条 `rule-dropped` 注册事件（registry-events.jsonl）。
//     `TENON_STRICT_RULES=1` 仍然「校验失败就拒绝启动」——但挂起（模块还没注册）不算
//     失败，只有「模块已注册且校验失败」才是失败。
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"tenon.dev/internal/controlplane"
	"tenon.dev/internal/controlplane/policy"
	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/controlplane/registrylog"
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
	// envRegistryEvents 是注册事件链的落盘路径（append-only + prevHash）。
	// 未设置则注册表纯内存：链是运维产物，不是业务正确性的前提。
	envRegistryEvents = "TENON_REGISTRY_EVENTS"
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
	// ruleRetryTimeout 是「模块注册后补校验挂起的业务规则」的整轮上限（裁决 ①a）。
	//
	// 补校验在注册请求的 handler 里同步跑（要拉 GET /tenon/actions/{id}），所以必须有
	// 硬上限：一个卡住的模块不能把注册响应也卡住。5s 与单次拉取的 client 超时同量级——
	// 规则文件引用多个动作时，整轮最多也就这个数量级。
	ruleRetryTimeout = 5 * time.Second
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
	// 注册事件链：设了 TENON_REGISTRY_EVENTS 就落盘（append-only + prevHash），
	// 否则纯内存。链是运维产物，写失败不改变注册结果——所以构造失败时降级而不是拒绝启动，
	// 但要把原因打出来（静默降级会让人以为事件在记）。
	modules, err := openRegistry()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := modules.Close(); cerr != nil {
			log.Printf("host: 关闭注册事件链失败：%v", cerr)
		}
	}()
	cfg, suspended, err := loadConfig(modules)
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
	// 挂起的业务规则必须在开始服务**之前**接上注册钩子：否则模块的第一次注册可能
	// 落在钩子装好之前，那批规则要等一次重申报才会生效。
	suspended.watch(srv.ReplacePolicies)

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
	// 挂起规则的周期重试：模块注册的那一刻往往还没绑定端口，补校验注定拿不到动作定义；
	// 这类「暂不可达」不丢弃，由这个循环在模块活过来之后补上（否则挂起=永久不生效）。
	go retrySuspendedRules(ctx, suspended, registry.HeartbeatInterval)

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
// openRegistry 按 TENON_REGISTRY_EVENTS 决定是否给注册表挂事件链。
//
// 配了路径却打不开时**返回错误让进程退出**，而不是降级成纯内存：运维会以为事件在记，
// 实际什么都没落盘——那是比「没有事件链」更坏的状态。
func openRegistry() (*registry.Registry, error) {
	path := strings.TrimSpace(os.Getenv(envRegistryEvents))
	if path == "" {
		return registry.NewRegistry(), nil
	}
	reg, err := registry.NewRegistryWithLog(path)
	if err != nil {
		return nil, fmt.Errorf("打开注册事件链 %s 失败：%w", path, err)
	}
	log.Printf("host: 注册事件链 → %s", path)
	return reg, nil
}

// modules 是运行期注册表的**同一个实例**：加载期校验业务规则要按模块取 baseURL，
// 运行期可见性看 Enabled()，两处必须是同一张表。它启动时为空——模块自注册是唯一
// 入口（D5），没有 TENON_MODULES 之类的静态登记路径。
//
// 传 nil 等价于「自建一张空表」，与 controlplane.Config.Registry 的缺省语义一致。
//
// 第三个返回值是「引用了未注册模块、等模块注册后补校验」的业务规则（裁决 ①a）。
// 没有挂起的规则时返回 nil（watch 是空操作）。它必须交给 controlplane.New **之后**
// 的 watch 装配——加载期还没有 Server，也就没有可替换的策略引擎。
func loadConfig(modules *registry.Registry) (controlplane.Config, *suspendedRules, error) {
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
			return cfg, nil, err
		}
		cfg.Policies = rules
	} else {
		log.Printf("host: 未设置 %s，策略表为空 —— 所有动作都会 fail-closed 成 deny", envPolicyFile)
	}
	if path := strings.TrimSpace(os.Getenv(envGrantFile)); path != "" {
		grants, subjects, err := controlplane.LoadGrants(path)
		if err != nil {
			return cfg, nil, err
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
		return cfg, nil, err
	}
	cfg.HMACSecret = secretBytes
	if path := strings.TrimSpace(os.Getenv(envUsersFile)); path != "" {
		users, err := controlplane.LoadUsers(path)
		if err != nil {
			return cfg, nil, err
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

	suspended, err := loadApprovalRules(&cfg, modules)
	if err != nil {
		return cfg, nil, err
	}
	if cfg.TestMode {
		log.Printf("host: %s=1 —— POST /tenon/admin/modules 已挂载，仅限测试环境使用", envTestMode)
	}
	return cfg, suspended, nil
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
// 反过来，**挂起**的规则只能在新之后接（见 suspendedRules.watch）。
//
// 规则与平台级规则同 schema、同规则池（spec §3.8），合并仍按 deny > approve > auto。
//
// 分类见 controlplane.LoadApprovalRulesDeferred：真正的失败（解析不了、算子非法、路径
// 不存在、模块已在册但拉不到定义）照旧告警，`TENON_STRICT_RULES=1` 时拒绝启动；引用
// **未注册**模块的整份规则进挂起集合，不丢弃、不阻塞启动（裁决 ①a）。
func loadApprovalRules(cfg *controlplane.Config, modules *registry.Registry) (*suspendedRules, error) {
	dir := strings.TrimSpace(os.Getenv(envSkillsDir))
	if dir == "" {
		dir = defaultSkillsDir
	}
	res := controlplane.LoadApprovalRulesDeferred(dir, registeredBaseURL(modules))
	for _, e := range res.Errors {
		log.Printf("host: 业务规则未生效：%v", e)
	}
	if cfg.StrictRules && len(res.Errors) > 0 {
		return nil, fmt.Errorf("host: %d 份业务规则未生效，%s=1 要求启动失败", len(res.Errors), envStrictRules)
	}
	if len(res.Rules) > 0 {
		log.Printf("host: 业务级规则已加载 %d 条（来自 %s）", len(res.Rules), dir)
	}
	cfg.Policies = append(cfg.Policies, res.Rules...)
	if len(res.Pending) == 0 {
		return nil, nil
	}
	for _, moduleID := range sortedModuleIDs(res.Pending) {
		paths := make([]string, 0, len(res.Pending[moduleID]))
		seen := map[string]bool{}
		for _, f := range res.Pending[moduleID] {
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			paths = append(paths, f.Path)
		}
		log.Printf("host: %d 份业务规则引用未注册模块 %q，已挂起等它注册后补校验：%s",
			len(paths), moduleID, strings.Join(paths, "、"))
	}
	return newSuspendedRules(modules, res.Pending, cfg.Policies), nil
}

// registeredBaseURL 返回「按已登记状态取模块地址」的查询函数：只认 registry.List()。
//
// 为什么不用 Enabled()（`enabled && alive`）：加载期要判的是「这个模块的声明到齐了没有」，
// 而可达性由随后的 GET /tenon/actions/{id} 本身判定——拉不到就整份失败/丢弃。用 Enabled()
// 会把「已注册但人工停用」的模块也算成未注册，让它的规则永远挂起（SetEnabled 不触发补校验）。
func registeredBaseURL(modules *registry.Registry) func(string) (string, bool) {
	return func(module string) (string, bool) {
		if modules == nil {
			return "", false
		}
		for _, m := range modules.List() {
			if m.ID == module {
				return m.BaseURL, true
			}
		}
		return "", false
	}
}

// sortedModuleIDs 返回按字典序排序的模块 id：日志与分组顺序必须稳定，否则同样的
// 仓库两次启动会打印出顺序不同的清单。
func sortedModuleIDs(m map[string][]controlplane.PendingRuleFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// suspendedRules 是「引用了未注册模块的业务规则」的挂起集合，以及它们在补校验通过后
// 并入的生效规则集（裁决 ①a）。
//
// 它是装配侧的胶水：注册表不认识规则引擎、策略引擎不认识注册表，两边都由这里连起来
// （registry.OnRegistered 钩子 → controlplane.RetryPendingRules → Server.ReplacePolicies）。
//
// 并发：钩子由注册请求的 handler 同步调用，多个模块同时注册就会并发进来。整段（含补校验
// 的网络请求）都在 mu 之下串行——补校验是「模块注册」这一步的一部分，串行化换来的是
// 「同一批规则不会被两次注册同时判定」；代价是并发注册要排队 ≤5s（ruleRetryTimeout），
// 相对于「规则半生效」这点延迟是值得的。
type suspendedRules struct {
	reg *registry.Registry
	// mu 保护以下全部字段。
	mu sync.Mutex
	// pending 按 moduleId 分组：模块注册时只补校验挂在它名下的那批文件。
	// 一份引用 N 个未注册模块的文件会同时出现在 N 个分组里——所有模块到齐才可能通过。
	pending map[string][]controlplane.PendingRuleFile
	// effective 是当前**全部**生效规则（平台级 + 已生效的业务级），顺序即 cfg.Policies
	// 的顺序再加后续补校验通过的批。
	effective []policy.PolicyRule
	// apply 把一份完整规则集装进运行期策略引擎；watch 时装上，装之前收到的注册只记
	// pending（服务还没开始监听，实际不会发生）。
	apply func([]policy.PolicyRule) error
}

// newSuspendedRules 用加载期的挂起分组与已生效规则集建状态机。
func newSuspendedRules(reg *registry.Registry, pending map[string][]controlplane.PendingRuleFile, effective []policy.PolicyRule) *suspendedRules {
	return &suspendedRules{
		reg:       reg,
		pending:   pending,
		effective: append([]policy.PolicyRule{}, effective...),
	}
}

// watch 接上运行期策略引擎并注册「模块登记成功」钩子。
//
// 必须在 http 服务开始监听之前调用：晚一步就可能漏掉某个模块的首次注册（那批挂起规则
// 要等一次重申报才生效）。nil 接收者是空操作——没有挂起规则时宿主不该装一个什么都不做的钩子。
func (p *suspendedRules) watch(apply func([]policy.PolicyRule) error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.apply = apply
	p.mu.Unlock()
	p.reg.OnRegistered(p.onRegistered)
}

// onRegistered 是 registry.OnRegistered 的钩子：模块 e 注册成功 → 补校验挂在它名下的规则。
//
// 同步：钩子返回时要么规则已经进了策略引擎，要么已经被丢弃并记了事件。注册响应因此
// 慢一点（最多 ruleRetryTimeout），换来的是「模块看到 200 时规则已经生效」。
//
// 已经在锁外（registry.Upsert 释放写锁后才回调），所以这里可以放心发 HTTP。
//
// 注意有一种情况**注定要等下一轮**：模块侧是「先异步注册、后绑定端口」——注册请求发出
// 的那一刻它还没 listen，host 这时拨过去必然被拒。这类失败归入「暂不可达、继续挂起」
// （controlplane.ErrRuleUnverifiable），由 retryAll 的周期重试在模块活过来之后补上；
// 只有确定的校验失败才丢弃并记事件。
func (p *suspendedRules) onRegistered(e registry.ModuleEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retryGroupLocked(context.Background(), e.ID)
}

// retryGroupLocked 补校验一个模块名下的全部挂起规则。调用方必须持有 p.mu。
//
// ctx 只用来限住网络 I/O；每个模块一个独立预算（注册路径与周期重试各自给 ≤ruleRetryTimeout）。
func (p *suspendedRules) retryGroupLocked(parent context.Context, moduleID string) {
	files := p.pending[moduleID]
	if len(files) == 0 || p.apply == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, ruleRetryTimeout)
	defer cancel()
	res := controlplane.RetryPendingRules(ctx, files, registeredBaseURL(p.reg))

	// 该模块名下的分组整个重算：通过的移出、被拒的丢弃、还剩别的模块缺的换到那些
	// 模块名下。先把旧分组删掉，避免「同一个文件既在 pending 又已生效」。
	delete(p.pending, moduleID)

	for _, f := range res.StillPending {
		for _, id := range f.Modules {
			p.pending[id] = append(p.pending[id], f)
		}
		log.Printf("host: 模块 %q 的规则 %s 仍无法判定（相关模块未注册或暂不可达：%s），继续挂起",
			moduleID, f.Path, strings.Join(f.Modules, "、"))
	}

	for _, rej := range res.Rejected {
		log.Printf("host: 模块 %q 注册后补校验失败，规则整份丢弃（%s，policyId=%s）：%v",
			moduleID, rej.File, rej.PolicyID, rej.Err)
		// 「记事件」落在这里：注册事件链（与 registered/lost 同一条 append-only 哈希链），
		// 不是业务审计——规则生命周期与 plan/step 无关（spec D4）。
		p.reg.LogEvent(registrylog.Event{
			Event:    registrylog.EventRuleDropped,
			ModuleID: moduleID,
			RuleFile: rej.File,
			PolicyID: rej.PolicyID,
			Reason:   rej.Err.Error(),
		})
	}

	if len(res.Accepted) == 0 {
		return
	}
	next := append([]policy.PolicyRule{}, p.effective...)
	for _, f := range res.Accepted {
		next = append(next, f.Rules...)
	}
	if err := p.apply(next); err != nil {
		// 规则本身已经逐份编译过，走到这里说明是装配侧的问题。**不能**假装生效：
		// 记下错误、把这几份文件重新挂起（下一次重试再补校验一次）。
		log.Printf("host: 模块 %q 补校验通过，但装载规则集失败（%d 份规则未生效，仍挂起）：%v",
			moduleID, len(res.Accepted), err)
		for _, f := range res.Accepted {
			retry := controlplane.PendingRuleFile{Path: f.Path, Rules: f.Rules, Modules: []string{moduleID}}
			p.pending[moduleID] = append(p.pending[moduleID], retry)
		}
		return
	}
	p.effective = next
	added := 0
	for _, f := range res.Accepted {
		added += len(f.Rules)
		log.Printf("host: 模块 %q 补校验通过，%d 条业务规则生效（%s）", moduleID, len(f.Rules), f.Path)
	}
	log.Printf("host: 生效规则集共 %d 条（本次新增 %d 条）", len(p.effective), added)
}

// retryAll 对**当前活着**的模块名下的挂起规则各补校验一次（周期重试，见 retrySuspendedRules）。
//
// 只扫 Enabled()（enabled && alive）：模块失联/被人工停用时它的动作本来也调不到，扫它只会
// 白打一串必然失败的请求；而它一旦恢复 alive，下一次 tick 就会把它扫到——所以这里的
// 「跳过」是自愈的，不是放弃。
//
// 空转代价接近零：没有挂起规则时（生产稳态）整个函数只读一次注册表。
func (p *suspendedRules) retryAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 || p.apply == nil {
		return
	}
	live := make(map[string]bool, len(p.pending))
	for _, m := range p.reg.Enabled() {
		live[m.ID] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), ruleRetryTimeout)
	defer cancel()
	for _, moduleID := range sortedModuleIDs(p.pending) {
		if !live[moduleID] {
			continue
		}
		if ctx.Err() != nil {
			// 本轮预算用完：剩下的留给下一个 tick（挂起状态不变，不会丢）。
			return
		}
		p.retryGroupLocked(ctx, moduleID)
	}
}

// retrySuspendedRules 每 interval 调一次 retryAll，直到 ctx 取消。
//
// 为什么需要它：模块注册的那一瞬间它往往还没绑定端口（注册在 goroutine 里、listen 在
// main 里），补校验拿到的是一次注定失败的连接——这类「暂不可达」按裁决不丢弃，但必须
// 有人再来一次，否则挂起就变成了永久不生效。周期与心跳同拍（5s）：与 spec §6 的
// 「重启窗口 ≤1 个心跳周期」是同一个量级。
//
// suspended 为 nil（没有任何挂起规则）时直接返回，不留悬挂 goroutine。
func retrySuspendedRules(ctx context.Context, suspended *suspendedRules, interval time.Duration) {
	if suspended == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			suspended.retryAll()
		}
	}
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
