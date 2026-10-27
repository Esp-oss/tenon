// Command tenon 是 Tenon 的单二进制工具入口：一致性套件、反模式扫描、schema 校验。
//
// 一次只做一件事的子命令风格，便于在 CI 与容器里组合：
//
//	tenon conformance run --target http://localhost:8081 --module inventory \
//	    --fixtures conformance/fixtures/mini-erp.json --source-dir . --report out/report.json
//	tenon d1 run --host http://127.0.0.1:8080 --quote http://127.0.0.1:8083 \
//	    --inventory http://127.0.0.1:8081 --procurement http://127.0.0.1:8082 --report out/d1.json
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
	"strings"
	"time"

	"tenon.dev/conformance"
	"tenon.dev/internal/d1"
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
	case "d1":
		code, err = runD1(os.Args[2:])
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
  tenon d1 run --host <url> --quote <url> --inventory <url> --procurement <url> [选项]
  tenon lint --path <dir>
  tenon validate --schema <name> --instance <file|->  [--spec-dir <dir>]

子命令：
  conformance  对被测实现跑一致性用例（C1/C2/C3/C8/C9/C10；C4/C5/D1 记 SKIP 并说明原因）
  d1           D1 差异测试：摘掉一个模块后，agent 的规划必须自适应改变并转人工
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

// --- d1 ---

// runD1 是 D1 差异测试的入口：A/B 两轮只差「被摘除的模块在不在场」。
//
// 退出码与 conformance 同一套语义：pass→0，fail→1（结论本身是不通过），
// 用法/参数错误→2。D1 的 fail 不是工具坏了，是**被测实现没通过命题**，
// 所以它必须和「命令用错了」分开。
func runD1(args []string) (int, error) {
	if len(args) == 0 || args[0] != "run" {
		return 2, fmt.Errorf("用法：tenon d1 run --host <url> --quote <url> " +
			"--inventory <url> --procurement <url> [--agent-result <path>] " +
			"[--module-a <id>=<url> ...] [--report <path>] [--round-timeout 120s]")
	}
	fs := flag.NewFlagSet("d1 run", flag.ContinueOnError)
	host := fs.String("host", "", "宿主基地址，例如 http://127.0.0.1:8080（须以 TENON_TEST_MODE=1 启动）")
	quote := fs.String("quote", "", "quote 模块基地址，例如 http://127.0.0.1:8083（须以测试态启动）")
	inventory := fs.String("inventory", "", "inventory 模块基地址，例如 http://127.0.0.1:8081")
	procurement := fs.String("procurement", "", "procurement 模块基地址，例如 http://127.0.0.1:8082（B 轮摘除它）")
	agentResult := fs.String("agent-result", "",
		"agent 的 TENON_RESULT_FILE 路径；省略时读同名环境变量")
	reportPath := fs.String("report", "", "报告输出路径（可选，缺省只打印）")
	roundTimeout := fs.Duration("round-timeout", d1.DefaultRoundTimeout, "每轮等 agent 结果行的上限")
	var extra moduleFlags
	fs.Var(&extra, "module-a", "只在 A 轮出现的额外模块 id=url（可重复；B 轮与 procurement 一起摘除）")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{"--host", *host},
		{"--quote", *quote},
		{"--inventory", *inventory},
		{"--procurement", *procurement},
	} {
		if strings.TrimSpace(required.value) == "" {
			return 2, fmt.Errorf("%s 必填", required.name)
		}
	}

	// 结果文件路径：命令行优先，其次继承 TENON_RESULT_FILE（在这台机器上两边
	// 通常由同一个脚本导出，少写一次就少一次对不上）。
	resultPath := strings.TrimSpace(*agentResult)
	if resultPath == "" {
		resultPath = strings.TrimSpace(os.Getenv("TENON_RESULT_FILE"))
	}
	if resultPath == "" {
		return 2, fmt.Errorf("--agent-result 必填（或设置 TENON_RESULT_FILE）：" +
			"D1 从它取 A/B 两轮规划出来的动作序列")
	}

	rep, err := d1.Run(context.Background(), d1.Config{
		Host:         *host,
		Quote:        *quote,
		Inventory:    *inventory,
		Procurement:  *procurement,
		AgentResult:  resultPath,
		ExtraModules: extra,
		RoundTimeout: *roundTimeout,
	})
	if err != nil {
		return 2, err
	}

	printD1Report(rep)
	if *reportPath != "" {
		if err := writeD1Report(*reportPath, rep); err != nil {
			return 2, err
		}
		fmt.Printf("\n报告已写入 %s\n", *reportPath)
	}
	if rep.Verdict == d1.VerdictPass {
		return 0, nil
	}
	return 1, nil
}

// moduleFlags 收集可重复的 --module-a id=url。
//
// 解析放在命令行而不是 runner 里：runner 只认结构化输入，把「id=url 这种
// 命令行语法」留在命令行边界上。
type moduleFlags []d1.Module

func (f *moduleFlags) String() string {
	parts := make([]string, 0, len(*f))
	for _, m := range *f {
		parts = append(parts, m.ID+"="+m.BaseURL)
	}
	return strings.Join(parts, ",")
}

func (f *moduleFlags) Set(raw string) error {
	id, base, ok := strings.Cut(strings.TrimSpace(raw), "=")
	if !ok {
		return fmt.Errorf("--module-a 需要 id=url 形式，收到 %q", raw)
	}
	*f = append(*f, d1.Module{ID: strings.TrimSpace(id), BaseURL: strings.TrimSpace(base)})
	return nil
}

// printD1Report 把结论与两轮的动作序列打到终端：报告可以归档，但人第一眼
// 就要看到「A 轮做了什么、摘掉模块后 B 轮做了什么」。
func printD1Report(rep d1.Report) {
	fmt.Printf("Tenon D1 差异测试  结论：%s\n", rep.Verdict)
	fmt.Printf("理由：%s\n", rep.Reason)
	fmt.Printf("每轮等结果行上限：%gs\n\n", rep.RoundTimeout)

	for _, view := range []struct {
		label string
		round d1.Round
	}{
		{"A 轮（全部模块在场）", rep.Rounds.A},
		{"B 轮（被摘除模块不在场）", rep.Rounds.B},
	} {
		fmt.Printf("%s：结局=%s planId=%s\n", view.label, orDash(view.round.Outcome), orDash(view.round.PlanID))
		if len(view.round.Actions) == 0 {
			fmt.Println("    （空：没有规划出任何动作）")
			continue
		}
		for i, action := range view.round.Actions {
			fmt.Printf("    %d. %s\n", i+1, action)
		}
	}
}

// writeD1Report 把报告落盘（目录不存在就建）。
func writeD1Report(path string, rep d1.Report) error {
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
