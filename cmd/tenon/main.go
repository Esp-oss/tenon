// Command tenon 是 Tenon 的单二进制工具入口：一致性套件、反模式扫描、schema 校验。
//
// 一次只做一件事的子命令风格，便于在 CI 与容器里组合：
//
//	tenon conformance run --target http://localhost:8081 --module inventory \
//	    --fixtures conformance/fixtures/mini-erp.json --source-dir . --report out/report.json
//	tenon lint --path .
//	tenon validate --schema action.schema.json --instance ./action.json
//
// 退出码：0 成功；1 有用例失败；2 用法或环境错误。--strict 时 SKIP 也算失败（2）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"tenon.dev/conformance"
	"tenon.dev/internal/spec"
	"tenon.dev/tools/lint"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	var code int
	switch os.Args[1] {
	case "conformance":
		code, err = runConformance(os.Args[2:])
	case "lint":
		code, err = runLint(os.Args[2:])
	case "validate":
		code, err = runValidate(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fmt.Errorf("未知子命令 %q", os.Args[1])
		code = 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `tenon —— Tenon 协议工具

用法：
  tenon conformance run --target <url> [选项]
  tenon lint --path <dir>
  tenon validate --schema <name> --instance <file|->  [--spec-dir <dir>]

子命令：
  conformance  对被测实现跑一致性用例（C1/C2/C3/C8/C9/C10；C4/C5/D1 记 SKIP 并说明原因）
  lint         C10 反模式扫描（事件/错误码 → action 的硬编码映射）
  validate     用 spec/0.1 的 schema 校验一份 JSON 实例

退出码：0 成功 / 1 有用例失败 / 2 用法或环境错误（--strict 下 SKIP 也算失败）
`)
}

// --- conformance ---

func runConformance(args []string) (int, error) {
	if len(args) == 0 || args[0] != "run" {
		return 2, fmt.Errorf("用法：tenon conformance run --target <url>")
	}
	fs := flag.NewFlagSet("conformance run", flag.ContinueOnError)
	target := fs.String("target", "", "被测模块基地址，例如 http://localhost:8081")
	host := fs.String("host", "", "宿主基地址；提供时 C4/C5 可判定，否则保持 SKIP")
	module := fs.String("module", "", "模块 id（用于挑选 fixtures）")
	fixturesPath := fs.String("fixtures", "", "场景数据 JSON 路径（可选）")
	sourceDir := fs.String("source-dir", "", "C10 扫描根目录（可选，通常传仓库根）")
	specDir := fs.String("spec-dir", spec.DefaultDir, "schema 目录")
	reportPath := fs.String("report", "", "机器可读报告输出路径（可选）")
	strict := fs.Bool("strict", false, "SKIP 也算失败")
	timeout := fs.Duration("timeout", 60*time.Second, "整体超时")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	if *target == "" {
		return 2, fmt.Errorf("--target 必填")
	}

	set, err := spec.Load(*specDir)
	if err != nil {
		return 2, fmt.Errorf("加载 schema: %w", err)
	}

	var fixtures *conformance.Fixtures
	if *fixturesPath != "" {
		raw, err := os.ReadFile(*fixturesPath)
		if err != nil {
			return 2, fmt.Errorf("读取 fixtures: %w", err)
		}
		fixtures = &conformance.Fixtures{}
		if err := json.Unmarshal(raw, fixtures); err != nil {
			return 2, fmt.Errorf("解析 fixtures: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	rep := conformance.Run(ctx, conformance.Options{
		Target:     *target,
		HostTarget: *host,
		Spec:       set,
		LintRoot:   *sourceDir,
		Fixtures:   fixtures,
		ModuleID:   *module,
	})

	printReport(rep)
	if *reportPath != "" {
		if err := writeReport(*reportPath, rep); err != nil {
			return 2, err
		}
		fmt.Printf("\n报告已写入 %s\n", *reportPath)
	}
	return rep.ExitCode(*strict), nil
}

func printReport(rep conformance.Report) {
	fmt.Printf("Tenon 一致性报告  target=%s  module=%s\n", rep.Target, orDash(rep.Module))
	fmt.Printf("起止：%s → %s（%s）\n\n",
		rep.StartedAt.Format(time.RFC3339), rep.FinishedAt.Format(time.RFC3339),
		rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond))

	for _, r := range rep.Results {
		icon := "✓"
		switch r.Status {
		case conformance.Fail:
			icon = "✗"
		case conformance.Skip:
			icon = "–"
		}
		fmt.Printf("%s %-14s %s\n", icon, r.Case, r.Title)
		if r.Detail != "" {
			fmt.Printf("    %s\n", r.Detail)
		}
		for _, e := range r.Evidence {
			fmt.Printf("    · %s\n", e)
		}
	}
	pass, fail, skip := rep.Counts()
	fmt.Printf("\n小计：pass %d / fail %d / skip %d\n", pass, fail, skip)
	if fail > 0 {
		fmt.Println("结论：未通过。")
		return
	}
	if skip > 0 {
		fmt.Println("结论：已跑用例通过，但存在 SKIP —— 这些用例本轮没有下结论，不能视为整体合规。")
		return
	}
	fmt.Println("结论：全部用例通过。")
}

func writeReport(path string, rep conformance.Report) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建报告目录: %w", err)
		}
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// --- lint ---

func runLint(args []string) (int, error) {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	path := fs.String("path", ".", "扫描根目录")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	findings, err := lint.ScanTree(*path)
	if err != nil {
		return 2, fmt.Errorf("扫描失败: %w", err)
	}
	live, exempt := 0, 0
	for _, f := range findings {
		if f.Exempt {
			exempt++
			fmt.Printf("  豁免  %s:%d %s (%s)\n", f.File, f.Line, f.Rule, f.Message)
			continue
		}
		live++
		fmt.Printf("  命中  %s:%d %s %s\n", f.File, f.Line, f.Rule, f.Message)
	}
	fmt.Printf("\n扫描 %s：命中 %d 处，豁免 %d 处\n", *path, live, exempt)
	if live > 0 {
		return 1, nil
	}
	return 0, nil
}

// --- validate ---

func runValidate(args []string) (int, error) {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	schemaName := fs.String("schema", "", "schema 文件名，如 action.schema.json")
	instance := fs.String("instance", "-", "实例 JSON 文件路径，- 表示标准输入")
	specDir := fs.String("spec-dir", spec.DefaultDir, "schema 目录")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *schemaName == "" {
		return 2, fmt.Errorf("--schema 必填")
	}
	set, err := spec.Load(*specDir)
	if err != nil {
		return 2, fmt.Errorf("加载 schema: %w", err)
	}
	var raw []byte
	if *instance == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*instance)
	}
	if err != nil {
		return 2, fmt.Errorf("读取实例: %w", err)
	}
	if err := set.Validate(*schemaName, raw); err != nil {
		fmt.Printf("✗ %s\n", err)
		return 1, nil
	}
	fmt.Printf("✓ 实例符合 %s\n", *schemaName)
	return 0, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
