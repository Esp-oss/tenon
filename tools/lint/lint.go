package lint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Finding 是一条命中记录。
type Finding struct {
	File    string // 相对传入 root 的路径，用 / 分隔
	Line    int    // 1-based
	Rule    string // "event-action" | "error-action" | "skill-action"
	Message string // 中文，说明命中了什么
	Exempt  bool   // 是否因 tenon-allow 被豁免
}

// 三条规则的标识，与 Finding.Rule 及审计备注里的取值一致（§10.2 C10）。
const (
	ruleEventAction = "event-action" // 规则 1：事件订阅回调体内出现具体 action 字面量
	ruleErrorAction = "error-action" // 规则 2：错误码到 action 的映射
	ruleSkillAction = "skill-action" // 规则 3：skill 正文出现 `模块.action` 形式的动作标识符
)

// errCodeWindow 是规则 2 的「其后 N 行内」窗口。
const errCodeWindow = 5

// allowMarker 是行内豁免标记，大小写不敏感，后面可跟 `: 理由`。
const allowMarker = "tenon-allow"

// codeExts 是本工具当作 TS/JS 源码扫描的扩展名；其他语言不在覆盖范围内（见 README「已知限制」）。
var codeExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
}

// skipDirs 逐字跳过：都是依赖树与版本库元数据，不是接入方源码。
var skipDirs = map[string]bool{"node_modules": true, ".git": true}

// ruleSet 选择一次扫描启用哪几条规则。
type ruleSet struct {
	event     bool
	errAction bool
	skill     bool
}

// hit 是规则产出的未定稿命中；File 与 Exempt 在 scanText 里统一回填。
type hit struct {
	line int
	rule string
	msg  string
}

// hitKey 用于按「规则 + 行」去重：同一行同一条规则只报一次，避免一个回调体里的多个
// action 字面量刷屏（重复命中的完整清单可以只靠行号回溯）。
type hitKey struct {
	rule string
	line int
}

// ScanTree 递归扫描 root 下的 .ts/.tsx/.js/.jsx/.mjs/.cjs/.md 文件。
// 返回全部命中（含被豁免的，Exempt=true），以及不可读文件的错误列表。
//
// 过滤规则：
//   - 路径含 `/spec/` 或以 `spec/` 开头的文件整体跳过——规范本身要用 `模块.action` 举例；
//   - 规则 3 只对 §6.4 定义的 skill 文档生效（见 isSkillDoc），普通设计文档引用能力清单是合法知识。
func ScanTree(root string) ([]Finding, error) {
	var (
		findings []Finding
		errs     []error
	)
	walk := func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			errs = append(errs, walkErr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			errs = append(errs, relErr)
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "." { // root 本身就是一个文件
			rel = filepath.ToSlash(filepath.Base(root))
		}
		if isSpecPath(rel) {
			return nil
		}
		set, ok := rulesFor(rel)
		if !ok {
			return nil
		}
		src, readErr := os.ReadFile(p)
		if readErr != nil {
			errs = append(errs, readErr)
			return nil
		}
		text := normalize(string(src))
		if set.skill && !isSkillDoc(rel, text) {
			return nil
		}
		findings = append(findings, scanText(rel, text, set)...)
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		errs = append(errs, err)
	}
	return findings, errors.Join(errs...)
}

// ScanSource 扫描单个文件的源码，name 只用于回填 Finding.File。
//
// 规则按扩展名选：.md 跑规则 3，其余跑规则 1/2——同一份源码不该被当成两种语言重复判。
// 本函数不做路径过滤：spec/ 跳过与 skill 文档判定都在 ScanTree 里，由调用方决定。
// 因此对一份普通 README 调用它不仅不会报错，还会按规则 3 报出里面的 `模块.action`——
// 这是有意的低层行为，上层要的「只约束 skill 正文」在 ScanTree 里体现。
func ScanSource(name string, src []byte) []Finding {
	set := ruleSet{event: true, errAction: true}
	if strings.EqualFold(path.Ext(name), ".md") {
		set = ruleSet{skill: true}
	}
	return scanText(name, normalize(string(src)), set)
}

// rulesFor 按扩展名决定一个文件跑哪几条规则；第二个返回值为 false 表示不扫描。
func rulesFor(rel string) (ruleSet, bool) {
	switch ext := strings.ToLower(path.Ext(rel)); {
	case ext == ".md":
		return ruleSet{skill: true}, true
	case codeExts[ext]:
		return ruleSet{event: true, errAction: true}, true
	default:
		return ruleSet{}, false
	}
}

// isSpecPath 判断相对路径是否落在 spec/ 下。
func isSpecPath(rel string) bool {
	return rel == "spec" || strings.HasPrefix(rel, "spec/") || strings.Contains(rel, "/spec/")
}

// isSkillDoc 判断一份 Markdown 是否是 §6.4 定义的 skill 文档：
// 文件名为 SKILL.md、路径里有 skills/ 段、或带含 `tenon:` 的 YAML frontmatter。
//
// 依据 §6.4 的措辞「skill 只能声明我关心哪些事实；正文中不得出现 `模块.action` 形式的动作标识符」：
// 硬约束的对象是 skill 正文。把范围放到全部 Markdown 会让设计文档（例如 demo 的 README 里
// 「先 `inventory.GetStock`，再 `inventory.DeductStock`」这种对 D1 期望值的描述）必然误报，
// 而误报率是 M0 的验收指标之一。
func isSkillDoc(rel, text string) bool {
	if strings.EqualFold(path.Base(rel), "skill.md") {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.EqualFold(seg, "skills") {
			return true
		}
	}
	return hasTenonFrontmatter(text)
}

// hasTenonFrontmatter 判断开头的 YAML frontmatter 里是否声明了 tenon 业务元数据（§6.4 示例）。
func hasTenonFrontmatter(text string) bool {
	s := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return false
	}
	s = s[len("---"):]
	end := strings.Index(s, "\n---")
	if end < 0 {
		return false
	}
	for _, line := range strings.Split(s[:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "tenon:") {
			return true
		}
	}
	return false
}

// scanText 对一份已归一化（BOM 去掉、行尾统一为 \n）的源码执行选定的规则，回填
// File/Exempt，按行号排序后返回。
func scanText(name, text string, set ruleSet) []Finding {
	lines := strings.Split(text, "\n")
	allow := allowIndex(lines)

	var hits []hit
	if set.event || set.errAction {
		hits = append(hits, scanCode(text, set)...)
	}
	if set.skill {
		hits = append(hits, scanMarkdown(text)...)
	}

	seen := make(map[hitKey]bool, len(hits))
	findings := make([]Finding, 0, len(hits))
	for _, h := range hits {
		k := hitKey{rule: h.rule, line: h.line}
		if seen[k] {
			continue
		}
		seen[k] = true
		f := Finding{File: name, Line: h.line, Rule: h.rule, Message: h.msg}
		if h.line >= 1 && h.line <= len(allow) {
			f.Exempt = allow[h.line-1]
		}
		findings = append(findings, f)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Rule < findings[j].Rule
	})
	return findings
}

// allowIndex 计算每行是否可豁免：命中行或它上一行出现 tenon-allow（大小写不敏感）即可。
func allowIndex(lines []string) []bool {
	allow := make([]bool, len(lines))
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), allowMarker) {
			continue
		}
		allow[i] = true
		if i+1 < len(lines) {
			allow[i+1] = true
		}
	}
	return allow
}

// scanCode 跑规则 1 与规则 2。先剥注释（等长空格，保留行号），这样注释里的举例不算命中。
func scanCode(text string, set ruleSet) []hit {
	code := stripComments(text)
	starts := lineStarts(code)
	var hits []hit
	if set.event {
		hits = append(hits, scanEventRule(code, starts)...)
	}
	if set.errAction {
		hits = append(hits, scanErrorRule(code, starts)...)
	}
	return hits
}

// actionIDRE 判定 `模块.动作名`：点号右侧首字母大写，以区别于 console.log、JSON.parse、
// stock.insufficient（事件名）这类普通成员访问。
var actionIDRE = regexp.MustCompile(`\b[a-z][a-z0-9_]*\.[A-Z][A-Za-z0-9_]*\b`)

// subCallRE 匹配事件订阅调用：`.on(`、`.subscribe(`、`.addListener(`、`subscribe(`、`onEvent(` 等。
// 裸 `on(` 只接受带点的形式，避免把普通函数调用误认成订阅。
var subCallRE = regexp.MustCompile(
	`\.\s*(?:on|once|subscribe|subscribeTo|addListener|addEventListener|onEvent)\s*\(` +
		`|\b(?:subscribe|subscribeTo|addListener|addEventListener|onEvent)\s*\(`)

// scanEventRule 实现规则 1：订阅调用参数里有事件名字面量，且同一回调体内出现 action 字面量。
// 只在订阅调用自己的参数区间里找回调体——「事件名 + 别处的 action」不报（见 callbackBody）。
func scanEventRule(code string, starts []int) []hit {
	lits := stringLiterals(code)
	seenOpen := make(map[int]bool, 8)
	var hits []hit
	for _, m := range subCallRE.FindAllStringIndex(code, -1) {
		if offsetInsideLiteral(lits, m[0]) {
			continue
		}
		open := m[0] + strings.IndexByte(code[m[0]:m[1]], '(')
		if seenOpen[open] {
			continue // 带点与裸调用的正则都命中了同一处
		}
		seenOpen[open] = true
		closing := matchPair(code, open, '(', ')')
		if closing < 0 {
			continue
		}
		event, ok := firstLiteralBetween(lits, open+1, closing)
		if !ok {
			continue
		}
		bodyStart, bodyEnd, ok := callbackBody(code, open+1, closing)
		if !ok {
			continue
		}
		loc := actionIDRE.FindStringIndex(code[bodyStart:bodyEnd])
		if loc == nil {
			continue
		}
		at := bodyStart + loc[0]
		hits = append(hits, hit{
			line: lineOf(starts, at),
			rule: ruleEventAction,
			msg: fmt.Sprintf("事件订阅 %q 的回调体内出现 action 字面量 %q：订阅只能决定「是否思考」，不能决定「做什么」",
				event, code[at:bodyStart+loc[1]]),
		})
	}
	return hits
}

// callbackBody 在订阅调用的参数区间 [lo,hi) 里定位回调体，返回体的字节区间。
// 支持 `=> { ... }`、`function (...) { ... }` 与 `=>` 后的表达式体；
// 回调定义在别处（传函数名）时不猜，属已知漏报。
func callbackBody(code string, lo, hi int) (int, int, bool) {
	arrow := -1
	depth := 0
	for i := lo; i < hi; i++ {
		switch c := code[i]; {
		case isQuote(c):
			i = skipLiteral(code, i) - 1
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == '=' && depth == 0 && i+1 < hi && code[i+1] == '>':
			arrow = i
			i++
		}
		if arrow >= 0 {
			break
		}
	}

	if arrow >= 0 {
		if brace := findByte(code, arrow+2, hi, '{'); brace >= 0 {
			if end := matchPair(code, brace, '{', '}'); end > brace && end <= hi {
				return brace, end, true
			}
		}
		// 表达式体：`=> invoke("inventory.DeductStock")`，到深度 0 的逗号或参数结束为止。
		end := hi
		d := 0
	scan:
		for i := arrow + 2; i < hi; i++ {
			switch c := code[i]; {
			case isQuote(c):
				i = skipLiteral(code, i) - 1
			case c == '(' || c == '[' || c == '{':
				d++
			case c == ')' || c == ']' || c == '}':
				d--
			case c == ',' && d == 0:
				end = i
				break scan
			}
		}
		if end <= arrow+2 {
			return 0, 0, false
		}
		return arrow + 2, end, true
	}

	if brace := findByte(code, lo, hi, '{'); brace >= 0 {
		if end := matchPair(code, brace, '{', '}'); end > brace && end <= hi {
			return brace, end, true
		}
	}
	return 0, 0, false
}

var (
	// caseCodeRE 匹配 `case "STOCK_INSUFFICIENT":` / `case 'X':`。
	caseCodeRE = regexp.MustCompile(`\bcase\s+(?:"([A-Z][A-Z0-9_]{2,})"|'([A-Z][A-Z0-9_]{2,})')\s*:`)
	// cmpCodeRE 匹配 `x === "STOCK_INSUFFICIENT"` 一侧。
	cmpCodeRE = regexp.MustCompile(`(?:===|!==|==|!=)\s*(?:"([A-Z][A-Z0-9_]{2,})"|'([A-Z][A-Z0-9_]{2,})')`)
	// cmpCodeRevRE 匹配 `"STOCK_INSUFFICIENT" === x` 一侧。
	cmpCodeRevRE = regexp.MustCompile(`(?:"([A-Z][A-Z0-9_]{2,})"|'([A-Z][A-Z0-9_]{2,})')\s*(?:===|!==|==|!=)`)
)

// scanErrorRule 实现规则 2：全大写下划线的错误码字面量之后 N 行内出现 action 字面量。
// 只有错误码不算命中——必须有 action 跟上。
func scanErrorRule(code string, starts []int) []hit {
	var hits []hit
	for _, re := range []*regexp.Regexp{caseCodeRE, cmpCodeRE, cmpCodeRevRE} {
		for _, m := range re.FindAllStringSubmatchIndex(code, -1) {
			var errCode string
			switch {
			case m[2] >= 0:
				errCode = code[m[2]:m[3]]
			case m[4] >= 0:
				errCode = code[m[4]:m[5]]
			default:
				continue
			}
			from, to := m[1], windowEnd(starts, lineOf(starts, m[0]), len(code))
			if to <= from {
				continue
			}
			loc := actionIDRE.FindStringIndex(code[from:to])
			if loc == nil {
				continue
			}
			hits = append(hits, hit{
				line: lineOf(starts, m[0]),
				rule: ruleErrorAction,
				msg: fmt.Sprintf("错误码 %q 之后 %d 行内出现 action 字面量 %q：错误码是重新规划的信号，不能映射到具体 action",
					errCode, errCodeWindow, code[from+loc[0]:from+loc[1]]),
			})
		}
	}
	return hits
}

// windowEnd 返回第 line 行之后 window 行的行末偏移；越界时返回 size。
func windowEnd(starts []int, line, size int) int {
	if last := line + errCodeWindow; last < len(starts) {
		return starts[last]
	}
	return size
}

// scanMarkdown 实现规则 3：skill 正文（含代码块）里出现 `模块.action` 标识符。
func scanMarkdown(text string) []hit {
	body := stripHTMLComments(text)
	starts := lineStarts(body)
	var hits []hit
	for _, m := range actionIDRE.FindAllStringIndex(body, -1) {
		hits = append(hits, hit{
			line: lineOf(starts, m[0]),
			rule: ruleSkillAction,
			msg: fmt.Sprintf("skill 正文出现动作标识符 %q：正文只能描述业务语义与关心的事实，不能指定 action",
				body[m[0]:m[1]]),
		})
	}
	return hits
}

// normalize 去掉开头的 BOM 并把行尾统一成 \n，使偏移与行号的计算只有一套口径。
func normalize(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// stripComments 把 JS/TS 的行注释与块注释替换成等长空格（保留换行与字符串字面量），
// 于是正则命中的偏移仍能原样映射回行号，注释里的举例也不会被当成代码。
func stripComments(s string) string {
	out := []byte(s)
	for i := 0; i < len(out); i++ {
		switch {
		case isQuote(out[i]):
			// out 是 s 的逐字节副本，下标通用；跳字面量时读 s，免得被已抹掉的注释字节干扰。
			i = skipLiteral(s, i) - 1
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '/':
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i += 2
			for i < len(out) {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		}
	}
	return string(out)
}

// stripHTMLComments 把 Markdown 的 <!-- --> 注释替换成等长空格（保留换行），
// 使注释里的例子不参与规则 3，同时不破坏行号。
func stripHTMLComments(s string) string {
	out := []byte(s)
	for i := 0; i+3 < len(out); i++ {
		if out[i] != '<' || out[i+1] != '!' || out[i+2] != '-' || out[i+3] != '-' {
			continue
		}
		end := len(out)
		for j := i + 4; j+2 < len(out); j++ {
			if out[j] == '-' && out[j+1] == '-' && out[j+2] == '>' {
				end = j + 3
				break
			}
		}
		for k := i; k < end; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
		i = end - 1
	}
	return string(out)
}

// strLit 是一处字符串字面量：start/end 是含引号的字节区间，body 是去掉引号的原文。
type strLit struct {
	start, end int
	body       string
}

// stringLiterals 收集源码里的字符串字面量（单引号、双引号与模板串），供规则 1/2 取事件名与错误码。
func stringLiterals(s string) []strLit {
	var lits []strLit
	for i := 0; i < len(s); i++ {
		if !isQuote(s[i]) {
			continue
		}
		end := skipLiteral(s, i)
		var body string
		if end-i >= 2 && s[end-1] == s[i] {
			body = s[i+1 : end-1]
		}
		lits = append(lits, strLit{start: i, end: end, body: body})
		i = end - 1
	}
	return lits
}

// firstLiteralBetween 返回完全落在 [lo,hi) 内的第一个非空字符串字面量。
func firstLiteralBetween(lits []strLit, lo, hi int) (string, bool) {
	for _, l := range lits {
		if l.start >= lo && l.end <= hi && l.body != "" {
			return l.body, true
		}
	}
	return "", false
}

// offsetInsideLiteral 判断偏移是否落在某个字符串字面量内部。
func offsetInsideLiteral(lits []strLit, off int) bool {
	i := sort.Search(len(lits), func(i int) bool { return lits[i].end > off })
	return i < len(lits) && lits[i].start <= off
}

// matchPair 返回与 open 处的 openCh 配对的 closeCh 偏移；不配对返回 -1。
// 字符串字面量里的括号不参与计数。
func matchPair(s string, open int, openCh, closeCh byte) int {
	if open < 0 || open >= len(s) || s[open] != openCh {
		return -1
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch c := s[i]; {
		case isQuote(c):
			i = skipLiteral(s, i) - 1
		case c == openCh:
			depth++
		case c == closeCh:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// findByte 在 [lo,hi) 里找第一个 ch，跳过字符串字面量里的同名字节；找不到返回 -1。
func findByte(s string, lo, hi int, ch byte) int {
	for i := lo; i < hi; i++ {
		if isQuote(s[i]) {
			i = skipLiteral(s, i) - 1
			continue
		}
		if s[i] == ch {
			return i
		}
	}
	return -1
}

// lineStarts 返回每行首字节的偏移：下标 i 对应第 i+1 行。
func lineStarts(s string) []int {
	starts := make([]int, 1, 1+strings.Count(s, "\n"))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineOf 把字节偏移映射成 1-based 行号。
func lineOf(starts []int, off int) int {
	return sort.SearchInts(starts, off+1)
}

// isQuote 判断字节是否是字符串引号（单引号、双引号、模板串反引号）。
func isQuote(b byte) bool { return b == '\'' || b == '"' || b == '`' }

// skipLiteral 从 s[i]（引号）开始跳过一个字符串字面量，返回其后一位；未闭合返回 len(s)。
func skipLiteral(s string, i int) int {
	if i < 0 || i >= len(s) || !isQuote(s[i]) {
		return len(s)
	}
	quote := s[i]
	for j := i + 1; j < len(s); {
		if s[j] == '\\' {
			j += 2
			continue
		}
		if s[j] == quote {
			return j + 1
		}
		j++
	}
	return len(s)
}
