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

	// defaultPort 是 interfaces.md §1 给宿主约定的端口。
	defaultPort = "8080"
	// defaultAuditPath 与模块侧 ./audit-<module>.jsonl 的命名习惯对齐。
	defaultAuditPath = "./audit-host.jsonl"
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
	if cfg.TestMode {
		log.Printf("host: %s=1 —— POST /tenon/admin/modules 已挂载，仅限测试环境使用", envTestMode)
	}
	return cfg, nil
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
