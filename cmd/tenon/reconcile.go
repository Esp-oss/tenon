package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"tenon.dev/internal/reconcile"
)

type moduleFlag []reconcile.ModuleSource

func (m *moduleFlag) String() string { return fmt.Sprint(*m) }

func (m *moduleFlag) Set(v string) error {
	id, url, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(url) == "" {
		return fmt.Errorf("--module 需要 id=url，收到 %q", v)
	}
	*m = append(*m, reconcile.ModuleSource{ID: strings.TrimSpace(id), BaseURL: strings.TrimSpace(url)})
	return nil
}

// runReconcile 把宿主审计与各模块的审计链对起来。
//
// 退出码按严重度分开，是为了让 cron/CI 能区分"差账"和"有人绕路"：
// 0 干净，1 只差账，2 有绕过或改账，3 链断了。
func runReconcile(args []string) (int, error) {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	hostAudit := fs.String("host-audit", "", "宿主审计 JSONL 路径（例如 state/audit-host.jsonl）")
	var modules moduleFlag
	fs.Var(&modules, "module", "待对账模块，可重复：--module inventory=http://127.0.0.1:8081")
	jsonOut := fs.String("json", "", "把完整报告写到这个路径（可选）")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if strings.TrimSpace(*hostAudit) == "" {
		return 2, fmt.Errorf("缺少 --host-audit")
	}
	if len(modules) == 0 {
		return 2, fmt.Errorf("至少给一个 --module id=url")
	}

	rep, err := reconcile.Run(context.Background(), reconcile.Options{
		HostAuditPath: *hostAudit,
		Modules:       modules,
	})
	if err != nil {
		return 2, err
	}

	classes := make([]string, 0, len(rep.Counts))
	for k := range rep.Counts {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	fmt.Printf("对账时间 %s\n", rep.GeneratedAt.Format("2006-01-02 15:04:05"))
	for _, h := range rep.Heads {
		if h.Error != "" {
			fmt.Printf("  %-12s 链不可读：%s\n", h.Module, h.Error)
			continue
		}
		fmt.Printf("  %-12s head=%s seq=%d legacy=%d\n", h.Module, short(h.HeadHash), h.HeadSeq, h.Legacy)
	}
	if len(rep.Findings) == 0 {
		fmt.Println("无差异。")
	} else {
		for _, f := range rep.Findings {
			fmt.Printf("  [%s] %s %s %s —— %s\n", f.Class, f.Module, f.AuditID, f.Action, f.Detail)
		}
		for _, c := range classes {
			fmt.Printf("  %-20s %d\n", c, rep.Counts[c])
		}
	}

	if *jsonOut != "" {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return 2, err
		}
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			return 2, err
		}
		fmt.Printf("报告已写入 %s\n", *jsonOut)
	}
	return rep.ExitCode(), nil
}

func short(hash string) string {
	if len(hash) <= 20 {
		return hash
	}
	return hash[:20] + "…"
}
