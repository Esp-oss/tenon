package controlplane

import (
	"encoding/json"
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

// ruleLoadError 描述一条「该 skill 的规则整份不生效」的原因。
//
// 带上文件路径：告警要能直接指到该改的那份 approval.json，否则运维只能挨个猜。
type ruleLoadError struct {
	Path string
	Err  error
}

// Error 实现 error。
func (e ruleLoadError) Error() string { return e.Path + ": " + e.Err.Error() }

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
//   - 模块离线 / 动作定义拉不下来：整份丢弃（模块的动作本来也调不到）；
//   - 引用的 arg.* 不在该动作的 inputSchema 里：整份丢弃（spec §3.10）。
//
// baseURLFor 返回模块 baseURL，第二个返回值为 false 表示模块不在线。
//
// 返回的 errs 只用于告警，**不中断加载**：一个 skill 写坏不该拖垮整个宿主，
// 平台级规则仍要生效（两边都没命中才 deny，不是 auto）。
//
// 明确不做：热加载（改 approval.json 需重启 host，见 spec §3.8）、规则缓存、
// 加载期校验审批人是否合法（那是审批侧权威判定，加载期只回答「规则能不能被求值」）。
func LoadApprovalRules(skillsDir string, baseURLFor func(module string) (string, bool)) ([]policy.PolicyRule, []error) {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("读取 skills 目录失败：%w", err)}
	}
	var (
		rules []policy.PolicyRule
		errs  []error
		names []string
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
			errs = append(errs, ruleLoadError{Path: path, Err: err})
			continue
		}
		var fileRules []policy.PolicyRule
		if err := json.Unmarshal(raw, &fileRules); err != nil {
			errs = append(errs, ruleLoadError{Path: path, Err: fmt.Errorf("不是合法规则数组：%w", err)})
			continue
		}
		bad, verr := validateRulePaths(fileRules, baseURLFor)
		if verr != nil {
			errs = append(errs, ruleLoadError{Path: path, Err: verr})
			continue
		}
		if bad {
			errs = append(errs, ruleLoadError{Path: path, Err: fmt.Errorf("规则引用的入参路径不存在，整份丢弃")})
			continue
		}
		rules = append(rules, fileRules...)
	}
	return rules, errs
}

// validateRulePaths 校验每条规则引用的 arg.* 路径确实是该动作的入参。
//
// 只校验首段：`not` 取反下的笔误（arg.amout）正落在首段，这一层就够挡住；
// 嵌套段需要完整 schema 递归，收益不足以引入一个解析器。
//
// **只校验 arg. 前缀**：origin 是宿主判定的单段来源、risk.* 是动作声明的风险
// （spec §3.11 与 action.schema.json#/$defs/Risk），两者都不是动作入参。
// 把它们拿去 inputSchema.properties 里找，合法规则会被整份丢弃
// （见实施计划 §0 勘误 10）。
//
// 路径**语法**（前缀、段名字符集、多段形态）不在这里判：那是 policy.parsePath 的
// 职责，本函数只回答「这个入参字段存不存在」。另写一套文法，两处迟早分叉。
//
// 第一个返回值为 true 表示「有引用的入参路径不存在，整份丢弃」；第二个返回值是
// 「没法校验」的原因（模块离线、动作定义拉不下来）。两种都让该 skill 的规则
// 整份不生效，区别只在告警文案。
//
// 注意：模块是否在线、定义是否拉得到，与「这条规则有没有 arg.* 路径」无关——
// 连定义都拿不到的规则不予加载（fail-closed），空 when 也一样。
func validateRulePaths(rules []policy.PolicyRule, baseURLFor func(string) (string, bool)) (bool, error) {
	cache := map[string]map[string]bool{} // actionID -> 允许的 arg 首段
	for _, rule := range rules {
		for _, action := range rule.Match.Actions {
			allowed, ok := cache[action]
			if !ok {
				module := strings.SplitN(action, ".", 2)[0]
				base, online := baseURLFor(module)
				if !online {
					return false, fmt.Errorf("模块 %q 离线，无法校验 %q 的入参路径", module, action)
				}
				schema, err := fetchInputSchema(base, action)
				if err != nil {
					return false, err
				}
				allowed = firstLevelProps(schema)
				cache[action] = allowed
			}
			for _, path := range collectArgPaths(rule.When) {
				first := strings.SplitN(strings.TrimPrefix(path, "arg."), ".", 2)[0]
				if !allowed[first] {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// fetchInputSchema 拉一次动作定义并取出 inputSchema。
//
// manifest 摘要不含 inputSchema 是按需拉取的设计（D15 的上下文成本），所以校验只能
// 逐个 action id 追一次 GET /tenon/actions/{id}；同一份规则文件里重复出现的 action
// 由 validateRulePaths 的 cache 收敛成一次。
func fetchInputSchema(baseURL, actionID string) (json.RawMessage, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/tenon/actions/" + actionID)
	if err != nil {
		return nil, fmt.Errorf("拉取 %q 的定义失败：%w", actionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取 %q 的定义返回 %d", actionID, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var wrap struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	return wrap.InputSchema, nil
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
// 把「规则里写了哪些入参字段」找出来，交给 inputSchema 比对。
//
// origin 与 risk.* 的叶子刻意不收集：它们不是动作入参（origin 由宿主判定、
// risk 是动作声明的风险），拿它们去 inputSchema 里找会把合法规则整份丢掉。
func collectArgPaths(when json.RawMessage) []string {
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
				if strings.HasPrefix(key, "arg.") {
					out = append(out, key)
				}
			}
		}
	}
	walk(node)
	sort.Strings(out)
	return out
}
