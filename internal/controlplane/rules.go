package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tenon.dev/internal/controlplane/policy"
)

// ErrArgPathMissing 表示「规则引用的入参/风险路径在该动作的声明里不存在」。
//
// 单独成哨兵值是因为它有两种处置而错误文本只有一条：加载期要整份丢弃并告警（fail-closed，
// 见 spec §3.10），补校验期要丢弃整份并写一条 rule-dropped 事件。上层按 errors.Is 区分
// 「规则本身写错了」与「拿不到动作定义」，不必去匹配错误串。
var ErrArgPathMissing = errors.New("规则引用的入参路径不存在，整份丢弃")

// ErrRuleUnverifiable 表示「这次没法判定」：模块已注册，但此刻拉不到动作定义（连接被拒、
// 超时、5xx 网关错误）。
//
// 它和「校验失败」必须分开：真实部署里模块是**先注册、后绑定端口**（注册在 goroutine 里
// 异步发出，main 紧接着才 ListenAndServe），所以注册瞬间 host 拨过去常常被拒。把这种
// 一次性的连接失败当「校验失败」丢弃，会让业务规则在每次首启时被永久丢掉——规则再也
// 不会回来，而日志里只有一条看起来很像配置错误的 reason。
//
// 因此这类错误在补校验里归入「继续挂起」（等下一次重试），只有**确定**的失败
// （路径不存在、模块级规则、模块明确回 4xx）才丢弃并记事件。
var ErrRuleUnverifiable = errors.New("模块暂不可达，无法校验规则")

// ruleLoadError 描述一条「该 skill 的规则整份不生效」的原因。
//
// 带上文件路径：告警要能直接指到该改的那份 approval.json，否则运维只能挨个猜。
type ruleLoadError struct {
	Path string
	Err  error
}

// Error 实现 error。
func (e ruleLoadError) Error() string { return e.Path + ": " + e.Err.Error() }

// PendingRuleFile 是一份「本轮还不能判定」的规则文件：不生效，也不丢弃。
//
// 两种成因：引用的模块还没注册；模块在册但补校验那一刻拉不到动作定义（模块刚注册、端口
// 还没绑）。两种情况都只意味着「现在判不了」，不意味着规则写错了。
//
// 它只在内存里活着（D6 不落盘），解析结果一并带着——补校验时按内存里的规则重新判定，
// 不再读盘。这样「规则文件在两次判定之间被改掉」不会产生一份没人校验过的规则；
// 代价是改 approval.json 仍然必须重启 host（spec §3.8 明确不做热加载，这里不改变它）。
type PendingRuleFile struct {
	// Path 是 skills/<skill>/approval.json，进 rule-dropped 事件的 ruleFile 字段。
	Path string
	// Rules 是已解析、尚未通过路径校验的规则（整份同生共死）。
	Rules []policy.PolicyRule
	// Modules 是该文件「还需要再等哪几个模块」的 id（排序、去重）：要么还没注册，要么
	// 那次拉不到它的定义。装配方按它分组重试——一个文件可能引用多个模块，最后一个能被
	// 校验通过时才会生效。
	Modules []string
}

// RuleLoadResult 是一次业务规则加载的分类结果。三类互斥：
//   - Rules 通过全部校验，可以直接进规则池；
//   - Pending 引用了尚未注册的模块，挂起等模块注册后补校验（不阻塞启动）；
//   - Errors 是真正的失败（解析不了、算子非法、路径不存在，或模块已在册但拉不到定义）——
//     只有这一类会让 TENON_STRICT_RULES=1 拒绝启动。加载期把「拉不到定义」算失败是刻意的：
//     那时注册表本该是空的，一个已登记模块连不上是部署异常，不该被静默挂起。
type RuleLoadResult struct {
	Rules   []policy.PolicyRule
	Pending map[string][]PendingRuleFile
	Errors  []error
}

// RuleRejection 是一份「引用模块都注册了、但校验不过」的规则文件：整份丢弃。
type RuleRejection struct {
	// File 是 approval.json 的路径（事件里的 ruleFile）。
	File string
	// PolicyID 是导致丢弃的规则 id；判不出具体某条时是该文件 policyId 的逗号列表。
	PolicyID string
	// Err 是丢弃原因（事件里的 reason）。
	Err error
}

// RuleRetryResult 是一次补校验的分类结果，与 RuleLoadResult 同构（少了 Errors）：
// 通过的文件并入生效集，还判不了的继续挂起，确定的校验失败整份丢弃。
//
// 三个列表都按入参顺序排列，且 Accepted/StillPending 返回的是**整份文件**而不是扁平的
// 规则切片：装配方需要文件路径来记日志与事件，也需要在装载失败时把整份文件重新挂起。
type RuleRetryResult struct {
	Accepted     []PendingRuleFile
	StillPending []PendingRuleFile
	Rejected     []RuleRejection
}

// LoadApprovalRules 遍历 skills/*/approval.json，读成策略规则并做加载期路径校验。
//
// 业务级规则与平台级规则同 schema、同规则池（spec §3.8）：加载结果由调用方
// append 进 Config.Policies，合并仍按 deny > approve > auto，不设「谁覆盖谁」。
//
// 失败一律按 skill 粒度 fail-closed——**整份丢弃 + 返回告警**，而不是丢掉出问题的那条：
// 半份规则最容易骗人，「这条已经生效了」与「它整份没生效」看起来一样。
//
//   - 文件不存在：不是错误（该 skill 只是没有业务级规则）；
//   - 解析失败：整份丢弃；
//   - 模块未登记 / 动作定义拉不下来：整份丢弃（模块的动作本来也调不到）；
//   - 引用的 arg.* 不在该动作的 inputSchema 里：整份丢弃（spec §3.10）。
//
// 本函数是**严格视图**：把「模块还没注册」也当失败。它适合没有「注册后补校验」通道的
// 调用方与单测；宿主自己的启动路径用 LoadApprovalRulesDeferred（裁决 ①a：模块自注册
// 之后加载期注册表必为空，把引用模块的规则整份丢掉会让 TENON_STRICT_RULES=1 起不来）。
func LoadApprovalRules(skillsDir string, baseURLFor func(module string) (string, bool)) ([]policy.PolicyRule, []error) {
	res := LoadApprovalRulesDeferred(skillsDir, baseURLFor)
	errs := append([]error(nil), res.Errors...)
	// pending 在严格视图里是错误：一个文件可能引用多个未注册模块，但只该报一条。
	for _, file := range uniquePendingFiles(res.Pending) {
		errs = append(errs, ruleLoadError{Path: file.Path,
			Err: fmt.Errorf("模块 %s 未登记，无法校验入参路径", strings.Join(file.Modules, "、"))})
	}
	return res.Rules, errs
}

// LoadApprovalRulesDeferred 与 LoadApprovalRules 同一套校验，但把「引用的模块还没注册」
// 的规则文件交给 Pending 而不是当失败（裁决 ①a）。
//
// 分类顺序是刻意的：先做**与本机无关**的检查（JSON 能不能解析、算子能不能编译），再做
// 与模块有关的判定。否则一个算子写错的规则文件只要引用了还没注册的模块，就会被挂起等到
// 模块注册才暴露——那是一个配置错误被延迟成了运行时故障。
//
// 模块是否「在注册表里」由 baseURLFor 回答。宿主的实现读 registry.List()（含 disabled/lost
// 条目）：可达性由随后的 GET /tenon/actions/{id} 本身判定——拉不到就整份失败/丢弃，
// 所以「已登记但连不上」不会被误判成有效。
func LoadApprovalRulesDeferred(skillsDir string, baseURLFor func(module string) (string, bool)) RuleLoadResult {
	res := RuleLoadResult{}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return res
		}
		res.Errors = []error{fmt.Errorf("读取 skills 目录失败：%w", err)}
		return res
	}
	var (
		names []string
		files []PendingRuleFile
	)
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 顺序稳定：同一份目录必得同一规则顺序

	for _, name := range names {
		path := filepath.Join(skillsDir, name, "approval.json")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			res.Errors = append(res.Errors, ruleLoadError{Path: path, Err: err})
			continue
		}
		var fileRules []policy.PolicyRule
		if err := json.Unmarshal(raw, &fileRules); err != nil {
			res.Errors = append(res.Errors, ruleLoadError{Path: path, Err: fmt.Errorf("不是合法规则数组：%w", err)})
			continue
		}
		// 用引擎试编译一遍：未知算子、非法路径语法（arg 不带点、foo.bar）在这里就会暴露。
		// 不做这一步，坏规则会进规则池，然后在 New() 的策略自检里把**整个宿主**拖垮——
		// 而 spec §3.8 承诺的是「只丢这个 skill 的规则」。
		if err := compileFileRules(fileRules); err != nil {
			res.Errors = append(res.Errors, ruleLoadError{Path: path,
				Err: fmt.Errorf("规则无法编译（未知算子或非法路径语法）：%w", err)})
			continue
		}
		missing, _, verr := checkRulePaths(context.Background(), fileRules, baseURLFor)
		if len(missing) > 0 {
			files = append(files, PendingRuleFile{Path: path, Rules: fileRules, Modules: missing})
			continue
		}
		if verr != nil {
			res.Errors = append(res.Errors, ruleLoadError{Path: path, Err: verr})
			continue
		}
		res.Rules = append(res.Rules, fileRules...)
	}
	for _, f := range files {
		if res.Pending == nil {
			res.Pending = make(map[string][]PendingRuleFile)
		}
		// 一个文件引用 N 个未注册模块就进 N 个分组：最后一个到齐时才补校验通过，
		// 此前每次有相关的模块注册都会重判一遍（不会因为「另一个先到」而误丢）。
		for _, moduleID := range f.Modules {
			res.Pending[moduleID] = append(res.Pending[moduleID], f)
		}
	}
	return res
}

// RetryPendingRules 对一批挂起的规则文件重新判定一次（模块注册后由装配方调用）。
//
// baseURLFor 必须是**当前**的注册表视图；调用方在一个 context 里传入 ≤5s 的超时
// （拉动作定义是网络 I/O，补校验不能无限期挂着注册请求）。
//
// 三种结局，其中两种都是「继续挂起」：
//   - 还有模块没注册（missing）→ 挂回剩下的模块名下；
//   - 模块在册但此刻拉不到定义（ErrRuleUnverifiable，典型是「刚注册、端口还没绑」）→
//     挂回它引用到的模块名下，等下一次重试。**不丢弃**：一次连接失败不等于规则写错；
//   - 确定的校验失败（路径不存在 / 模块级规则 / 模块回 4xx）→ Rejected，整份丢弃并记事件。
//
// 返回顺序保持入参顺序：生效规则的追加顺序因此只取决于 approval.json 的路径序，而不
// 取决于哪次注册先到——同一份仓库重放两次得到同一个规则顺序。
func RetryPendingRules(ctx context.Context, files []PendingRuleFile, baseURLFor func(module string) (string, bool)) RuleRetryResult {
	var out RuleRetryResult
	for _, f := range files {
		missing, policyID, err := checkRulePaths(ctx, f.Rules, baseURLFor)
		switch {
		case len(missing) > 0:
			// 还有别的模块没注册：换到剩下的模块名下继续等（触发本次补校验的模块已经到齐）。
			out.StillPending = append(out.StillPending, PendingRuleFile{
				Path: f.Path, Rules: f.Rules, Modules: missing,
			})
		case errors.Is(err, ErrRuleUnverifiable):
			// 模块在册但此刻拉不到定义（多半是「刚注册、还没绑定端口」）：不判定、不丢弃，
			// 挂回它引用到的模块名下等下一次重试。
			out.StillPending = append(out.StillPending, PendingRuleFile{
				Path: f.Path, Rules: f.Rules, Modules: referencedModules(f.Rules),
			})
		case err != nil:
			if policyID == "" {
				policyID = policyIDsOf(f.Rules)
			}
			out.Rejected = append(out.Rejected, RuleRejection{File: f.Path, PolicyID: policyID, Err: err})
		default:
			out.Accepted = append(out.Accepted, f)
		}
	}
	return out
}

// referencedModules 列出规则引用到的模块 id（排序、去重）。
//
// 用于「这次拉不到、下次再试」的分组：那一轮 not 缺少模块，而是模块暂不可达，
// 所以挂回它引用到的模块名下，由下一次重试再判定。
func referencedModules(rules []policy.PolicyRule) []string {
	set := map[string]bool{}
	for _, rule := range rules {
		for _, action := range rule.Match.Actions {
			module := strings.SplitN(action, ".", 2)[0]
			set[module] = true
		}
	}
	return sortedKeys(set)
}

// checkRulePaths 判定一批规则能否生效。三态用返回值天然分开：
//   - missing 非空：引用的这些模块不在注册表里，本轮无法判定（不发任何网络请求）；
//   - err 非 nil：模块都在，但校验失败（拉不到定义 / 路径不存在 / 模块级规则），整份丢弃；
//   - 两者都空：通过。
//
// policyID 是能定位到的「肇事规则」：路径不存在时是写错路径的那条，拉取失败时是第一条
// 引用该动作的规则；判断不出时为空（由调用方用整份文件的 id 兜底）。
//
// 先收集模块再发请求：一份引用未注册模块的规则文件在模块到齐之前一个字节都不该发出去，
// 否则每次启动都会向「还没起来的模块」打一串必然失败的请求。
func checkRulePaths(ctx context.Context, rules []policy.PolicyRule, baseURLFor func(module string) (string, bool)) ([]string, string, error) {
	for _, rule := range rules {
		if len(rule.Match.Actions) == 0 {
			// 模块级规则（省略 actions）覆盖该模块全部动作，我们无法枚举它们去校验入参，
			// 只能要求业务级规则写明 actions —— 否则 arg.* 的笔误会静默通过加载期。
			return nil, rule.PolicyID, fmt.Errorf(
				"规则 %q 省略了 match.actions：业务级文件必须写明要覆盖的动作，否则无法校验入参路径", rule.PolicyID)
		}
	}

	missingSet := map[string]bool{}
	known := map[string]string{} // module → baseURL（只缓存已登记的）
	var missing []string
	for _, rule := range rules {
		for _, action := range rule.Match.Actions {
			module := strings.SplitN(action, ".", 2)[0]
			if _, ok := known[module]; ok {
				continue
			}
			base, online := baseURLFor(module)
			if !online {
				if !missingSet[module] {
					missingSet[module] = true
					missing = append(missing, module)
				}
				continue
			}
			known[module] = base
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return missing, "", nil
	}

	cache := map[string]actionSchema{}
	for _, rule := range rules {
		for _, action := range rule.Match.Actions {
			schema, ok := cache[action]
			if !ok {
				module := strings.SplitN(action, ".", 2)[0]
				var err error
				schema, err = fetchActionSchema(ctx, known[module], action)
				if err != nil {
					return nil, rule.PolicyID, err
				}
				cache[action] = schema
			}
			for _, path := range collectArgPaths(rule.When) {
				first := strings.SplitN(strings.TrimPrefix(path, "arg."), ".", 2)[0]
				if !schema.inputProps[first] {
					return nil, rule.PolicyID, ErrArgPathMissing
				}
			}
			for _, path := range collectRiskPaths(rule.When) {
				first := strings.SplitN(strings.TrimPrefix(path, "risk."), ".", 2)[0]
				if !schema.riskProps[first] {
					return nil, rule.PolicyID, ErrArgPathMissing
				}
			}
		}
	}
	return nil, "", nil
}

// compileFileRules 用引擎试编译一份规则文件，返回第一个配置错误。
//
// 引擎把编译错误留在对象里、等第一次求值才返回，所以这里必须真的求值一次（动作 id 用
// 一个必定不命中任何规则的哨兵，不产生副作用）。
func compileFileRules(rules []policy.PolicyRule) error {
	_, err := policy.NewPolicyEngine(rules).Evaluate(context.Background(),
		policy.PolicyRequest{Action: "tenon.selfcheck.Noop"})
	return err
}

// policyIDsOf 把一份规则的 policyId 串起来，供「定位不到具体哪条」的事件字段兜底。
func policyIDsOf(rules []policy.PolicyRule) string {
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.PolicyID)
	}
	return strings.Join(ids, ",")
}

// uniquePendingFiles 把按模块分组的 pending 拍平成「一份文件一条」，按路径排序。
//
// 分组视图对「按模块重试」是必须的，对告警是错的：一份引用两个未注册模块的文件在告警里
// 出现两次，会让人以为有两份坏规则。
func uniquePendingFiles(pending map[string][]PendingRuleFile) []PendingRuleFile {
	if len(pending) == 0 {
		return nil
	}
	seen := map[string]PendingRuleFile{}
	for _, files := range pending {
		for _, f := range files {
			prev, ok := seen[f.Path]
			if !ok {
				seen[f.Path] = f
				continue
			}
			// 合并 Modules：同一份文件在不同分组里记的是它当时缺的模块。
			merged := map[string]bool{}
			for _, m := range prev.Modules {
				merged[m] = true
			}
			for _, m := range f.Modules {
				merged[m] = true
			}
			prev.Modules = sortedKeys(merged)
			seen[f.Path] = prev
		}
	}
	out := make([]PendingRuleFile, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// sortedKeys 返回按字典序排序的键，保证日志与分组顺序稳定。
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// actionSchema 是校验需要的两份要点：入参属性键与风险字段键。
//
// riskProps 为空表示动作定义里没有 risk 字段：此时任何 risk.* 路径都判「不存在」
// （fail-closed）。action.schema.json 里 risk 是必填，正常不会走到这里。
type actionSchema struct {
	inputProps map[string]bool
	riskProps  map[string]bool
}

// fetchActionSchema 拉一次动作定义，取出校验需要的两份要点。
//
// manifest 摘要不含 inputSchema 是按需拉取的设计（D15 的上下文成本），所以校验只能
// 逐个 action id 追一次 GET /tenon/actions/{id}；同一份规则文件里重复出现的 action
// 由 checkRulePaths 的 cache 收敛成一次。
//
// ctx 是**补校验路径**的硬约束：模块注册的 HTTP 响应要等这次补校验跑完，所以整轮拉取
// 必须有超时（宿主传 ≤5s）。加载期传 context.Background()——那时还没开始服务，
// 一次慢请求不至于拖坏谁。
//
// 失败分两种（见 ErrRuleUnverifiable）：连接不上 / 超时 / 5xx 是「此刻没法判定」，
// 4xx 是「模块明确说没有这个动作」——后者是配置错误，前者多半只是模块刚注册还没绑定
// 端口。两种都不允许规则生效（fail-closed），但只有后者该被丢弃。
func fetchActionSchema(ctx context.Context, baseURL, actionID string) (actionSchema, error) {
	var out actionSchema
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/tenon/actions/"+actionID, nil)
	if err != nil {
		return out, fmt.Errorf("%w：构造拉取 %q 定义的请求失败：%v",
			ErrRuleUnverifiable, actionID, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("%w：拉取 %q 的定义失败：%v", ErrRuleUnverifiable, actionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return out, fmt.Errorf("拉取 %q 的定义返回 %d（模块没有这个动作）", actionID, resp.StatusCode)
		}
		return out, fmt.Errorf("%w：拉取 %q 的定义返回 %d", ErrRuleUnverifiable, actionID, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, fmt.Errorf("%w：读取 %q 的定义失败：%v", ErrRuleUnverifiable, actionID, err)
	}
	var wrap struct {
		InputSchema json.RawMessage            `json:"inputSchema"`
		Risk        map[string]json.RawMessage `json:"risk"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return out, fmt.Errorf("拉取 %q 的定义无法解析：%w", actionID, err)
	}
	out.inputProps = firstLevelProps(wrap.InputSchema)
	out.riskProps = map[string]bool{}
	for k := range wrap.Risk {
		out.riskProps[k] = true
	}
	return out, nil
}

// firstLevelProps 取 inputSchema.properties 的键集合。
//
// schema 解析不出来时返回空集合而不是报错：空集合会让所有 arg.* 路径都判「不存在」，
// 于是整份规则被丢弃——这是正确的方向（读不懂的 schema 不能用来放行）。
func firstLevelProps(schema json.RawMessage) map[string]bool {
	out := map[string]bool{}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return out
	}
	for k := range s.Properties {
		out[k] = true
	}
	return out
}

// collectArgPaths 从条件树里收集所有 arg.* 叶子路径。
//
// 它只按 JSON 结构走 all/any/not，**不做路径文法校验**（不复制 policy.parsePath 的
// 前缀与段名规则）：路径是否合法由 policy.NewPolicyEngine 编译时决定，这里只负责
// 把「规则里写了哪些字段」找出来，交给对应的声明去比对。
//
// origin 的叶子刻意不收集：它由宿主判定，没有「声明」可比对。arg.* 与 risk.* 分两路
// 收集，各自比对不同的声明（入参 schema / 动作 risk）。
func collectArgPaths(when json.RawMessage) []string { return collectLeafPaths(when, "arg.") }

// collectRiskPaths 从条件树里收集所有 risk.* 叶子路径。
func collectRiskPaths(when json.RawMessage) []string { return collectLeafPaths(when, "risk.") }

func collectLeafPaths(when json.RawMessage, prefix string) []string {
	var out []string
	if len(when) == 0 {
		return out
	}
	var node map[string]json.RawMessage
	if err := json.Unmarshal(when, &node); err != nil {
		return out
	}
	var walk func(map[string]json.RawMessage)
	walk = func(m map[string]json.RawMessage) {
		for key, val := range m {
			switch key {
			case "all", "any":
				var list []map[string]json.RawMessage
				if err := json.Unmarshal(val, &list); err != nil {
					continue
				}
				for _, child := range list {
					walk(child)
				}
			case "not":
				var child map[string]json.RawMessage
				if err := json.Unmarshal(val, &child); err == nil {
					walk(child)
				}
			default:
				if strings.HasPrefix(key, prefix) {
					out = append(out, key)
				}
			}
		}
	}
	walk(node)
	sort.Strings(out)
	return out
}
