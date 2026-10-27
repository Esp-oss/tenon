package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot 是仓库根：go test 的工作目录是包目录 tools/lint。
const repoRoot = "../.."

// TestScanSourceRuleTruePositives 覆盖三条规则各自的最小真阳性片段：
// 每条只喂一个坏例，断言命中的规则号与行号都正确。
func TestScanSourceRuleTruePositives(t *testing.T) {
	cases := []struct {
		desc string
		name string
		src  string
		rule string
		line int
	}{
		{
			desc: "规则 1：.on 的箭头回调体里 invoke 具体 action",
			name: "quote-trigger.ts",
			src:  "bus.on(\"quote.completed\", () => {\n  invoke(\"inventory.DeductStock\")\n})\n",
			rule: ruleEventAction,
			line: 2,
		},
		{
			desc: "规则 1：actions 映射表里取 action id",
			name: "quote-trigger.ts",
			src:  "events.subscribe(\"quote.completed\", () => {\n  return actions[\"inventory.DeductStock\"]\n})\n",
			rule: ruleEventAction,
			line: 2,
		},
		{
			desc: "规则 1：addListener + function 体，体内直接调 action",
			name: "quote-trigger.js",
			src:  "bus.addListener(\"stock.insufficient\", function (e) {\n  procurement.CreatePurchaseRequest(e)\n})\n",
			rule: ruleEventAction,
			line: 2,
		},
		{
			desc: "规则 1：裸 subscribe + 表达式体（无花括号）",
			name: "quote-trigger.mjs",
			src:  "subscribe(\"quote.completed\", () => invoke(\"inventory.DeductStock\"))\n",
			rule: ruleEventAction,
			line: 1,
		},
		{
			desc: "规则 2：case 全大写错误码后跟 action",
			name: "stock-error.ts",
			src:  "switch (err.code) {\ncase \"STOCK_INSUFFICIENT\":\n  return dispatch(\"inventory.ReleaseStock\")\n}\n",
			rule: ruleErrorAction,
			line: 2,
		},
		{
			desc: "规则 2：if 比较后跟 action",
			name: "stock-error.js",
			src:  "if (res.code === 'QUOTA_EXCEEDED') {\n  ask(\"inventory.GetStock\")\n}\n",
			rule: ruleErrorAction,
			line: 1,
		},
		{
			desc: "规则 3：skill 正文出现 模块.动作名",
			name: "skills/quote-to-fulfillment/SKILL.md",
			src:  "# 步骤\n\n1. 库存不足时调用 `inventory.DeductStock`。\n",
			rule: ruleSkillAction,
			line: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := ScanSource(tc.name, []byte(tc.src))
			if len(got) != 1 {
				t.Fatalf("想要 1 条命中，得到 %d 条：%+v", len(got), got)
			}
			f := got[0]
			if f.Rule != tc.rule {
				t.Errorf("Rule = %q，想要 %q", f.Rule, tc.rule)
			}
			if f.Line != tc.line {
				t.Errorf("Line = %d，想要 %d", f.Line, tc.line)
			}
			if f.File != tc.name {
				t.Errorf("File = %q，想要 %q", f.File, tc.name)
			}
			if f.Message == "" {
				t.Error("Message 为空")
			}
			if f.Exempt {
				t.Error("Exempt 不该为 true")
			}
		})
	}
}

// TestSmallMemberAccessIsNotAnAction 守住 action 字面量的判据：点号右侧必须首字母大写，
// 所以 console.log / JSON.parse / 事件名 stock.insufficient 都不算。
func TestSmallMemberAccessIsNotAnAction(t *testing.T) {
	cases := []string{
		"subscribe(\"quote.completed\", () => { console.log(x) })\n",
		"bus.on(\"quote.completed\", () => { JSON.parse(payload) })\n",
		"bus.on(\"quote.completed\", () => { const t = payload.stock.insufficient })\n",
		"bus.on(\"quote.completed\", () => { obj.actions[0].toUpperCase() })\n",
		// 整个模式躲在字符串字面量里，不是代码
		"t.Log(\"bus.on(\\\"quote.completed\\\", () => { invoke(\\\"inventory.DeductStock\\\") })\")\n",
	}
	for _, src := range cases {
		if got := ScanSource("bus.ts", []byte(src)); len(got) != 0 {
			t.Errorf("源码 %q 不该有命中，得到：%+v", src, got)
		}
	}
}

// TestErrorCodeAloneIsNotAMapping 守住规则 2：只有全大写错误码、后面没有 action，不报。
func TestErrorCodeAloneIsNotAMapping(t *testing.T) {
	cases := []string{
		"switch (code) { case \"STOCK_INSUFFICIENT\": throw new Error(\"x\") }\n",
		"if (code === \"POLICY_DENIED\") { report(\"denied\") }\n",
		"const CODES = ['STOCK_INSUFFICIENT', 'QUOTA_EXCEEDED']\n",
	}
	for _, src := range cases {
		if got := ScanSource("stock-error.ts", []byte(src)); len(got) != 0 {
			t.Errorf("源码 %q 不该有命中，得到：%+v", src, got)
		}
	}
}

// TestErrorCodeWindowIsFiveLines 固定规则 2 的窗口边界：其后第 5 行内算命中，第 6 行起不算。
func TestErrorCodeWindowIsFiveLines(t *testing.T) {
	cases := []struct {
		desc string
		src  string
		want int
	}{
		{
			desc: "action 落在窗口最后一行（第 1+5 行）",
			src:  "case \"STOCK_INSUFFICIENT\":\n\t// 1\n\t// 2\n\t// 3\n\t// 4\n\tinvoke(\"inventory.DeductStock\")\n",
			want: 1,
		},
		{
			desc: "action 落在窗口之外（第 1+6 行）",
			src:  "case \"STOCK_INSUFFICIENT\":\n\t// 1\n\t// 2\n\t// 3\n\t// 4\n\t// 5\n\tinvoke(\"inventory.DeductStock\")\n",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := ScanSource("stock-error.ts", []byte(tc.src))
			if len(got) != tc.want {
				t.Fatalf("想要 %d 条命中，得到 %d 条：%+v", tc.want, len(got), got)
			}
		})
	}
}

// TestEventActionNeedsSameCallbackBody 守住规则 1 的「同一回调体」：事件名 + 别处的 action 不报。
func TestEventActionNeedsSameCallbackBody(t *testing.T) {
	cases := []string{
		// action 在订阅调用之外
		"bus.on(\"quote.completed\", () => {\n  log(\"ok\")\n})\ninvoke(\"inventory.DeductStock\")\n",
		// 回调只传函数名，定义在别处（跨函数引用属已知漏报）
		"bus.on(\"quote.completed\", handler)\nconst handler = () => { invoke(\"inventory.DeductStock\") }\n",
		// 回调体里的注释不算代码
		"bus.on(\"quote.completed\", () => {\n  // invoke(\"inventory.DeductStock\")\n})\n",
	}
	for _, src := range cases {
		if got := ScanSource("bus.ts", []byte(src)); len(got) != 0 {
			t.Errorf("源码 %q 不该有命中，得到：%+v", src, got)
		}
	}
}

// TestSkillActionIgnoresEventNames 守住规则 3 的判据：事件名与错误码（点号右侧小写）不算。
func TestSkillActionIgnoresEventNames(t *testing.T) {
	src := "# 知识\n\n- 事件 `quote.completed` 只决定是否开启回合。\n" +
		"- 错误码 `stock.insufficient` 对应哪个 action 由 skill 自行判断。\n" +
		"- `console.log(x)` / `JSON.parse(s)` 也不是动作标识符。\n"
	if got := ScanSource("skills/x/SKILL.md", []byte(src)); len(got) != 0 {
		t.Fatalf("不该有命中，得到：%+v", got)
	}
}

// TestExemption 覆盖豁免：命中行或它上一行带 tenon-allow 即 Exempt，且不计入非豁免命中。
func TestExemption(t *testing.T) {
	const sub = "bus.on(\"quote.completed\", () => {\n  invoke(\"inventory.DeductStock\")\n})\n"
	cases := []struct {
		desc       string
		name       string
		src        string
		wantExempt bool
	}{
		{
			desc:       "没有豁免标记",
			name:       "bus.ts",
			src:        sub,
			wantExempt: false,
		},
		{
			desc:       "命中行行尾带理由",
			name:       "bus.ts",
			src:        "bus.on(\"quote.completed\", () => {\n  invoke(\"inventory.DeductStock\") // tenon-allow: 迁移期\n})\n",
			wantExempt: true,
		},
		{
			desc:       "标记在命中行的上一行",
			name:       "bus.ts",
			src:        "bus.on(\"quote.completed\", () => {\n  // tenon-allow: 迁移期\n  invoke(\"inventory.DeductStock\")\n})\n",
			wantExempt: true,
		},
		{
			desc:       "大小写不敏感",
			name:       "bus.ts",
			src:        "bus.on(\"quote.completed\", () => {\n  invoke(\"inventory.DeductStock\") // TENON-ALLOW 迁移期\n})\n",
			wantExempt: true,
		},
		{
			desc:       "Markdown 里的 HTML 注释豁免",
			name:       "skills/x/SKILL.md",
			src:        "# 步骤\n\n<!-- tenon-allow: 迁移期 -->\n1. 调用 `inventory.DeductStock`。\n",
			wantExempt: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := ScanSource(tc.name, []byte(tc.src))
			if len(got) != 1 {
				t.Fatalf("想要 1 条命中，得到 %d 条：%+v", len(got), got)
			}
			if got[0].Exempt != tc.wantExempt {
				t.Fatalf("Exempt = %v，想要 %v（%+v）", got[0].Exempt, tc.wantExempt, got[0])
			}
			nonExempt := 0
			for _, f := range got {
				if !f.Exempt {
					nonExempt++
				}
			}
			wantNonExempt := 1
			if tc.wantExempt {
				wantNonExempt = 0
			}
			if nonExempt != wantNonExempt {
				t.Errorf("非豁免命中数 = %d，想要 %d", nonExempt, wantNonExempt)
			}
		})
	}
}

// TestScanTreeRealRepoNoFalsePositives 是 M0 的验收项：对本仓库的 demo/ 与 packages/ 全量扫描，
// 非豁免命中数必须是 0。demo/mini-erp/README.md 讨论事件名、错误码与 D1 期望的 action 序列，
// 那是合法知识，不是「事件 → action」的代码映射。
func TestScanTreeRealRepoNoFalsePositives(t *testing.T) {
	for _, root := range []string{"demo", "packages"} {
		t.Run(root, func(t *testing.T) {
			findings, err := ScanTree(filepath.Join(repoRoot, root))
			if err != nil {
				t.Fatalf("ScanTree(%s): %v", root, err)
			}
			nonExempt := 0
			for _, f := range findings {
				if f.Exempt {
					continue
				}
				nonExempt++
				t.Errorf("非豁免命中：%s:%d [%s] %s", f.File, f.Line, f.Rule, f.Message)
			}
			if nonExempt != 0 {
				t.Fatalf("%s/ 的非豁免命中数 = %d，要求 0", root, nonExempt)
			}
			t.Logf("%s/ 扫描完成：命中 %d 条（全部已豁免）", root, len(findings))
		})
	}
}

// TestDemoReadmeMentionsAreOutsideSkillScope 记录一个必须说清的事实：
// demo/mini-erp/README.md 里确实有 `模块.动作名`（能力清单与 D1 期望值），
// 规则 3 若对全部 Markdown 生效就必然误报。所以规则 3 的作用域是 §6.4 的 skill 正文，
// 这一条用低层 ScanSource（不判作用域）证明「不是正则太窄」，用 ScanTree 证明整树干净。
func TestDemoReadmeMentionsAreOutsideSkillScope(t *testing.T) {
	readme := filepath.Join(repoRoot, "demo", "mini-erp", "README.md")
	src, err := os.ReadFile(readme)
	if err != nil {
		t.Skipf("读不到 demo README，跳过这条作用域证据：%v", err)
	}
	mentions := actionIDRE.FindAllString(string(src), -1)
	if len(mentions) == 0 {
		t.Skip("demo README 已不再引用动作标识符，作用域证据失效")
	}

	raw := ScanSource("demo/mini-erp/README.md", src)
	rawLines := map[int]bool{}
	for _, f := range raw {
		if f.Rule == ruleSkillAction {
			rawLines[f.Line] = true
		}
	}
	if len(rawLines) == 0 {
		t.Fatalf("规则 3 在对全部 Markdown 生效时没能命中这些动作标识符，说明正则太窄：%v", mentions)
	}

	findings, err := ScanTree(filepath.Join(repoRoot, "demo"))
	if err != nil {
		t.Fatalf("ScanTree(demo): %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("demo/ 不该有命中，得到：%+v", findings)
	}
	t.Logf("demo README 里有 %d 处动作标识符（规则 3 若扫全部 Markdown 会命中 %d 行：%v）；"+
		"因作用域限定在 skill 正文，ScanTree 报 0 条", len(mentions), len(rawLines), rawLines)
}

// TestScanTreeSkipsSpec 覆盖 spec/ 跳过：规范本身要用 `inventory.DeductStock` 举例。
// 用临时目录把同一份 skill 文档放在 spec/ 与普通目录下对拍，证明零命中来自路径跳过。
func TestScanTreeSkipsSpec(t *testing.T) {
	findings, err := ScanTree(filepath.Join(repoRoot, "spec"))
	if err != nil {
		t.Fatalf("ScanTree(spec): %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("spec/ 应被整体跳过，却报了 %d 条：%+v", len(findings), findings)
	}

	dir := t.TempDir()
	const body = "# 步骤\n\n1. 库存不足时调用 `inventory.DeductStock`。\n"
	writeTemp(t, dir, "spec/SKILL.md", body)
	writeTemp(t, dir, "normal/SKILL.md", body)

	findings, err = ScanTree(dir)
	if err != nil {
		t.Fatalf("ScanTree(%s): %v", dir, err)
	}
	if len(findings) != 1 {
		t.Fatalf("想要 1 条命中（spec/ 下的那份被跳过），得到 %d 条：%+v", len(findings), findings)
	}
	f := findings[0]
	if f.File != "normal/SKILL.md" {
		t.Errorf("File = %q，想要 %q（路径要用 / 分隔）", f.File, "normal/SKILL.md")
	}
	if f.Rule != ruleSkillAction {
		t.Errorf("Rule = %q，想要 %q", f.Rule, ruleSkillAction)
	}
	if f.Line != 3 {
		t.Errorf("Line = %d，想要 3", f.Line)
	}
}

// TestScanTreeScope 覆盖作用域判定：同名内容，skill 文档报、普通文档不报；
// 扩展名不认识的文件不扫；root 不存在返回错误。
func TestScanTreeScope(t *testing.T) {
	dir := t.TempDir()
	const body = "# 文档\n\n1. 调用 `inventory.DeductStock`。\n"
	writeTemp(t, dir, "docs/README.md", body)
	writeTemp(t, dir, "note.txt", body)
	writeTemp(t, dir, "plain/skillbox.md", body)

	findings, err := ScanTree(dir)
	if err != nil {
		t.Fatalf("ScanTree(%s): %v", dir, err)
	}
	if len(findings) != 0 {
		t.Fatalf("非 skill 文档与未知扩展名都不该命中，得到：%+v", findings)
	}

	writeTemp(t, dir, "SKILL.md", body)
	writeTemp(t, dir, "with-frontmatter.md", "---\nname: quote-to-fulfillment\ntenon:\n  subscribes: [\"quote.completed\"]\n---\n"+body)
	findings, err = ScanTree(dir)
	if err != nil {
		t.Fatalf("ScanTree(%s): %v", dir, err)
	}
	got := map[string]bool{}
	for _, f := range findings {
		if f.Rule != ruleSkillAction {
			t.Errorf("只应有 %s，得到 %s（%s:%d）", ruleSkillAction, f.Rule, f.File, f.Line)
		}
		got[f.File] = true
	}
	for _, want := range []string{"SKILL.md", "with-frontmatter.md"} {
		if !got[want] {
			t.Errorf("%s 是 skill 文档，应被扫出规则 3；实际命中文件：%v", want, got)
		}
	}
}

// TestScanSourceIgnoresPath 固定低层接口的契约：ScanSource 不做路径过滤（spec/ 与 skill
// 文档判定都在 ScanTree），name 只用于回填 Finding.File。
func TestScanSourceIgnoresPath(t *testing.T) {
	src := []byte("# 文档\n\n1. 调用 `inventory.DeductStock`。\n")
	got := ScanSource("spec/0.1/SKILL.md", src)
	if len(got) != 1 {
		t.Fatalf("想要 1 条命中，得到 %d 条：%+v", len(got), got)
	}
	if got[0].File != "spec/0.1/SKILL.md" {
		t.Errorf("File = %q，想要 %q", got[0].File, "spec/0.1/SKILL.md")
	}
	if got[0].Line != 3 {
		t.Errorf("Line = %d，想要 3", got[0].Line)
	}
}

// TestSkillActionCoversCodeBlocksAndSkipsComments 覆盖规则 3 的两个边界：
// 代码块里的动作标识符要报（正文含代码块），HTML 注释里的不报。
func TestSkillActionCoversCodeBlocksAndSkipsComments(t *testing.T) {
	inBlock := "# 步骤\n\n```ts\nconst a = \"inventory.DeductStock\"\n```\n"
	got := ScanSource("skills/x/SKILL.md", []byte(inBlock))
	if len(got) != 1 || got[0].Rule != ruleSkillAction || got[0].Line != 4 {
		t.Fatalf("代码块内的动作标识符应命中第 4 行，得到：%+v", got)
	}

	inComment := "# 步骤\n\n<!-- 反例：inventory.DeductStock 不该出现 -->\n1. 描述业务语义。\n"
	if got := ScanSource("skills/x/SKILL.md", []byte(inComment)); len(got) != 0 {
		t.Fatalf("HTML 注释里的标识符不该命中，得到：%+v", got)
	}
}

// TestScanTreeScansCodeExtensions 覆盖扩展名清单：6 种 TS/JS 扩展名都要扫，.md 不跑规则 1/2。
func TestScanTreeScansCodeExtensions(t *testing.T) {
	dir := t.TempDir()
	const body = "case \"STOCK_INSUFFICIENT\":\n  return call(\"inventory.ReleaseStock\")\n"
	exts := []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"}
	for _, ext := range exts {
		writeTemp(t, dir, "src/mod."+ext, body)
	}
	writeTemp(t, dir, "src/mod.md", body)

	findings, err := ScanTree(dir)
	if err != nil {
		t.Fatalf("ScanTree(%s): %v", dir, err)
	}
	if len(findings) != len(exts) {
		t.Fatalf("想要 %d 条命中（每种代码扩展名一条），得到 %d 条：%+v", len(exts), len(findings), findings)
	}
	for _, f := range findings {
		if f.Rule != ruleErrorAction {
			t.Errorf("%s 的 Rule = %q，想要 %q", f.File, f.Rule, ruleErrorAction)
		}
		if f.Line != 1 {
			t.Errorf("%s 的 Line = %d，想要 1", f.File, f.Line)
		}
		if !strings.HasPrefix(f.File, "src/mod.") || !strings.Contains(f.File, "/") {
			t.Errorf("File = %q，应为相对路径且用 / 分隔", f.File)
		}
	}
}

// TestScanTreeMissingRoot 覆盖错误返回：不可读的 root 不能静默返回空结果。
func TestScanTreeMissingRoot(t *testing.T) {
	findings, err := ScanTree(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("不存在的 root 应返回错误，却返回 nil")
	}
	if len(findings) != 0 {
		t.Fatalf("不该有命中，得到：%+v", findings)
	}
}

// writeTemp 在测试临时目录里写一个文件（rel 用 / 分隔）。
func writeTemp(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestMessageIsChinese 确认 Message 用中文描述（评审要求），且带上具体标识符。
func TestMessageIsChinese(t *testing.T) {
	got := ScanSource("bus.ts", []byte("bus.on(\"quote.completed\", () => {\n  invoke(\"inventory.DeductStock\")\n})\n"))
	if len(got) != 1 {
		t.Fatalf("想要 1 条命中，得到 %d 条", len(got))
	}
	msg := got[0].Message
	if !strings.ContainsAny(msg, "订阅动作事件错误码") {
		t.Errorf("Message 应为中文说明，得到 %q", msg)
	}
	if !strings.Contains(msg, "inventory.DeductStock") {
		t.Errorf("Message 应指明命中的标识符，得到 %q", msg)
	}
}
