// Package policy 是控制平面的策略引擎（G2 闸门）：把 policy.schema.json 的规则编译成
// 条件树，对「一次模块动作」求值，并按 deny > approve > auto 合并出最终处置。
//
// 规范依据（以 spec/0.1 为唯一真源）：
//   - 规则字段、算子集合、合并顺序与 fail-closed：
//     spec/0.1/schemas/policy.schema.json（description 就是语义条款）；
//   - 设计依据：docs/superpowers/specs/2026-10-27-tenon-ai-driven-system-design.md §4.7；
//   - 请求/响应形状与「无命中 = deny」：docs/interfaces.md §4.2，
//     降级矩阵见 spec/0.1/semantics.md §12（策略不可用时默认 deny，不是 auto）。
//
// 本包只依赖标准库，不含 I/O 与业务逻辑：规则从哪加载、评估结果怎么进审计由宿主决定。
// 引擎构造后不可变，可被并发求值。
//
// 规范未写死、由本实现定下的几处判断（显式记在这里，便于审计）：
//   - when 缺省（nil 或 null）视作恒真，与 schema 里 {"all": []} 等价。schema 要求 when
//     必填，但把「漏写」当成「永不命中」会让配置错误静默失效，比放宽更危险；
//   - 未知算子、未知取值前缀、非法正则、超深嵌套、非法 outcome 都是**配置错误**：
//     构造时记下第一个错误，Evaluate 一律返回它（上层据此 fail-closed 成 deny）。把
//     无法判断的条件当成「不匹配」放过，等于让 deny 规则静默消失；
//   - 取不到值（路径走不通、字段缺失）才是「不匹配」，不报错；
//   - 大小算子两侧类型不一致时不匹配；两侧都是字符串时先按 RFC 3339 比时间，解析不出
//     的按字典序比较（ISO-8601 的字符序本身就是时间序，故不引入歧义）；
//   - matches 走 Go 的 RE2：与 schema 提的 ECMA-262 在常用子集上一致，但不支持
//     lookaround 与反向引用；换来的是线性时间匹配，不会因病态正则回溯爆炸。
//
// 已知缺口（不实现、也不假装实现）：
//   - §4.7 的「自动升审批」（risk.reversible=false、dryRun.status != verified、
//     idempotency 缺失 → 强制 approve，不可被策略下调）不在这里做。这三条要的是
//     Plan 步骤级信息（该步的 compensation、预演结论、动作的幂等声明），单个动作的
//     risk 里查不到；宿主或 agent 的执行循环在握有整份 Plan 时判定，本引擎不猜；
//   - 引擎不做 schema 全量校验（如 policyId 的词法、approve 必带 approvers）：那属于
//     规则加载时的 spec/0.1 校验，引擎只保证「拿到的规则按条款求值」。
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 三种处置，取值域见 policy.schema.json#/$defs/PolicyOutcome。
const (
	// OutcomeAuto 表示可直接执行。
	OutcomeAuto = "auto"
	// OutcomeApprove 表示转人工审批。
	OutcomeApprove = "approve"
	// OutcomeDeny 表示拒绝。
	OutcomeDeny = "deny"
)

// 理由码（机器可读，大写蛇形，匹配 schema 里 reasons 的 pattern）。deny 必须能区分
// 「策略拒绝」与「无命中默认拒绝」：前者是策略本意，后者是配置缺口，运维要看得见。
const (
	// ReasonNoMatch 是没有任何策略命中时的理由码（fail-closed 的显式证据）。
	ReasonNoMatch = "NO_POLICY_MATCH_DEFAULT_DENY"
	// ReasonAuto 是策略把动作判为可直接执行时的理由码。
	ReasonAuto = "POLICY_AUTO_APPROVED"
	// ReasonApprovalRequired 是策略要求人工审批时的理由码。
	ReasonApprovalRequired = "POLICY_APPROVAL_REQUIRED"
	// ReasonDeny 是策略明确拒绝时的理由码。
	ReasonDeny = "POLICY_DENY"
)

// 条件字段路径的根前缀：只有这两类取值域，schema 也只定义这两类。
const (
	// pathRootArg 指向被评估动作的入参。
	pathRootArg = "arg"
	// pathRootRisk 指向动作声明的风险（action.schema.json#/$defs/Risk）。
	pathRootRisk = "risk"
)

// supportedOperators 是封闭的算子集合（§4.7）：只接受这九个，未知算子必须报错。
const supportedOperators = "eq/ne/>/>=/</<=/in/notIn/matches"

const (
	// maxConditionDepth 是条件嵌套上限。schema 要求「深度由实现设上限并拒绝过深输入」；
	// 32 层远超真实策略的深度，又足以挡住病态输入。
	maxConditionDepth = 32
	// maxPatternLen 是 matches 正则的长度上限（schema 的 maxLength: 1000，按字符计）。
	maxPatternLen = 1000
)

// pathSegmentPattern 是路径段的词法约束，与 schema 的 propertyNames 一致。
var pathSegmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// PolicyRule 对应 policy.schema.json 的规则对象：静态匹配（match）加条件（when）决定
// 是否命中，命中后给出处置（outcome）与（approve 时的）审批人。
type PolicyRule struct {
	// PolicyID 由控制平面分配，评估结果的 DecidedBy 与审计的 decision.policyId 都引用它，
	// 因此策略 id 不得复用（改规则即新 id 或升版本）。
	PolicyID string `json:"policyId"`
	// Priority 只在同一 outcome 内比较：合并先按 outcome 强弱（deny > approve > auto），
	// 同 outcome 再取 priority 最大者。
	Priority int `json:"priority"`
	// Match 是静态匹配范围；Actions 省略表示该模块下的全部动作。
	Match PolicyMatch `json:"match"`
	// When 是条件表达式 JSON：组合子 all/any/not 可嵌套，叶子形如 {"arg.qty": {">": 10}}。
	// 空值视作恒真（见包注释），schema 里等价写法是 {"all": []}。
	When json.RawMessage `json:"when,omitempty"`
	// Outcome 取值 auto | approve | deny。
	Outcome string `json:"outcome"`
	// Approvers 形如 role:warehouse_manager；outcome=approve 时随结果返回。
	Approvers []string `json:"approvers,omitempty"`
}

// PolicyMatch 对应 policy.schema.json#/$defs/PolicyMatch：module 必填，
// actions 省略表示该模块下全部动作（不得用通配符字符串）。
type PolicyMatch struct {
	// Module 是模块标识。
	Module string `json:"module"`
	// Actions 是动作 id 列表（完整 id，形如 inventory.DeductStock）。
	Actions []string `json:"actions,omitempty"`
}

// matches 判断本策略的静态范围是否覆盖给定动作。action 是完整 action id。
func (m PolicyMatch) matches(module, action string) bool {
	if m.Module != module {
		return false
	}
	if len(m.Actions) == 0 {
		return true
	}
	for _, a := range m.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// PolicyRequest 是一次策略求值的输入：待评估的单个模块动作，外加它本次的入参与风险声明。
//
// 为什么是「单个动作」而不是整份 Plan：§4.7 的 match 以 module/action 为键，合并规则
// 也在这个粒度上成立；调用方（宿主）逐步骤求值后再按步骤取最严，本引擎因此不必理解
// plan.schema.json 的结构。
type PolicyRequest struct {
	// Action 是完整 action id（<module>.<Action>），模块前缀用于 match.module 比对。
	Action string `json:"action"`
	// Args 是动作入参，供条件里的 arg.* 路径取值；省略或 null 视为空对象。
	Args json.RawMessage `json:"args,omitempty"`
	// Risk 是动作声明的风险（action.schema.json#/$defs/Risk），供 risk.* 路径取值。
	Risk json.RawMessage `json:"risk,omitempty"`
}

// PolicyEvaluation 是 policies:evaluate 的响应体
// （policy.schema.json#/$defs/PolicyEvaluationResponse）。
type PolicyEvaluation struct {
	// Outcome 是合并后的最终处置；无任何策略命中时必须是 deny（fail-closed）。
	Outcome string `json:"outcome"`
	// DecidedBy 是最终裁决者：无命中时为空数组（不是 null），命中时是胜出规则的 policyId。
	DecidedBy []string `json:"decidedBy"`
	// Approvers 只在 outcome=approve 时给出，来自胜出规则。
	Approvers []string `json:"approvers,omitempty"`
	// Reasons 是机器可读的理由码，供审计与排障使用。
	Reasons []string `json:"reasons"`
}

// PolicyEngine 是编译好的规则集合。构造后不可变，可并发求值。
type PolicyEngine struct {
	// rules 是规则的副本，调用方事后改自己的切片不影响引擎。
	rules []PolicyRule
	// conds 与 rules 一一对应，是编译后的条件树。
	conds []condition
	// err 是构造期的第一个配置错误；非 nil 时 Evaluate 一律返回它。
	err error
}

// NewPolicyEngine 编译一批规则。返回的引擎不可变，可被并发求值。
//
// 签名不返回错误（见实施计划的接口约定），编译失败的错误记在引擎里，由 Evaluate 返回：
// 上层拿到错误就 fail-closed 成 deny，而不是拿着半套规则继续放行一两个动作。
func NewPolicyEngine(rules []PolicyRule) *PolicyEngine {
	e := &PolicyEngine{
		rules: append([]PolicyRule(nil), rules...),
		conds: make([]condition, 0, len(rules)),
	}
	for _, r := range e.rules {
		if _, ok := outcomeRank(r.Outcome); !ok {
			e.err = fmt.Errorf("policy: 规则 %q 的 outcome %q 非法，只能是 %s/%s/%s",
				r.PolicyID, r.Outcome, OutcomeAuto, OutcomeApprove, OutcomeDeny)
			return e
		}
		c, err := compileWhen(r.When)
		if err != nil {
			e.err = fmt.Errorf("policy: 规则 %q 的 when：%w", r.PolicyID, err)
			return e
		}
		e.conds = append(e.conds, c)
	}
	return e
}

// Evaluate 对一次动作求值：先按 match 过滤，再求条件，最后按 deny > approve > auto 合并
// （同 outcome 取 priority 最大者）。无命中返回 deny 与 NO_POLICY_MATCH_DEFAULT_DENY。
//
// 返回错误表示「引擎或请求本身不合法」（规则配置坏、动作 id 不成立、入参不是合法 JSON）；
// 调用方拿到错误必须 deny——semantics.md §12：策略服务不可用默认 deny，不是 auto。
func (e *PolicyEngine) Evaluate(ctx context.Context, req PolicyRequest) (PolicyEvaluation, error) {
	if err := ctx.Err(); err != nil {
		return PolicyEvaluation{}, err
	}
	if e == nil {
		return PolicyEvaluation{}, fmt.Errorf("policy: 引擎为 nil")
	}
	if e.err != nil {
		return PolicyEvaluation{}, e.err
	}
	module, action, err := splitActionID(req.Action)
	if err != nil {
		return PolicyEvaluation{}, err
	}
	scope, err := newEvalScope(req)
	if err != nil {
		return PolicyEvaluation{}, err
	}

	winner := -1
	for i := range e.rules {
		if !e.rules[i].Match.matches(module, action) {
			continue
		}
		if !e.conds[i].eval(scope) {
			continue
		}
		if winner < 0 || stronger(e.rules[winner], e.rules[i]) {
			winner = i
		}
	}

	if winner < 0 {
		return PolicyEvaluation{
			Outcome:   OutcomeDeny,
			DecidedBy: []string{},
			Reasons:   []string{ReasonNoMatch},
		}, nil
	}
	best := e.rules[winner]
	out := PolicyEvaluation{
		Outcome:   best.Outcome,
		DecidedBy: []string{best.PolicyID},
		Reasons:   []string{reasonFor(best.Outcome)},
	}
	if best.Outcome == OutcomeApprove && len(best.Approvers) > 0 {
		out.Approvers = append([]string(nil), best.Approvers...)
	}
	return out, nil
}

// splitActionID 把 <module>.<Action> 拆成模块前缀与完整动作 id。
func splitActionID(id string) (module, action string, err error) {
	i := strings.Index(id, ".")
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("policy: 动作 id %q 不是 <module>.<Action> 形式", id)
	}
	return id[:i], id, nil
}

// outcomeRank 给出合并时的强弱序：deny > approve > auto。第二个返回值是「该 outcome 合法吗」。
func outcomeRank(outcome string) (int, bool) {
	switch outcome {
	case OutcomeAuto:
		return 0, true
	case OutcomeApprove:
		return 1, true
	case OutcomeDeny:
		return 2, true
	}
	return 0, false
}

// stronger 判断 challenger 是否压过 incumbent：先比 outcome 强弱，再比 priority。
// 同优先级保留先声明者，使结果只依赖规则顺序，与遍历细节无关。
func stronger(incumbent, challenger PolicyRule) bool {
	ir, _ := outcomeRank(incumbent.Outcome)
	cr, _ := outcomeRank(challenger.Outcome)
	if cr != ir {
		return cr > ir
	}
	return challenger.Priority > incumbent.Priority
}

// reasonFor 把处置映射成理由码。
func reasonFor(outcome string) string {
	switch outcome {
	case OutcomeApprove:
		return ReasonApprovalRequired
	case OutcomeAuto:
		return ReasonAuto
	default:
		return ReasonDeny
	}
}

// evalScope 是一次求值的取值上下文：条件里的 arg.* 与 risk.* 分别落到这两棵树上。
type evalScope struct {
	args any
	risk any
}

// newEvalScope 把请求里的入参与风险解码成可走路径的 JSON 树。
// 解码统一用 UseNumber（见 decodeJSON），因此数字在引擎内只有一种表示：json.Number。
func newEvalScope(req PolicyRequest) (*evalScope, error) {
	args, err := decodeOptional(req.Args)
	if err != nil {
		return nil, fmt.Errorf("policy: 解析 args 失败：%w", err)
	}
	risk, err := decodeOptional(req.Risk)
	if err != nil {
		return nil, fmt.Errorf("policy: 解析 risk 失败：%w", err)
	}
	return &evalScope{args: args, risk: risk}, nil
}

// decodeOptional 允许字段缺省（空字节或 null 都当作「没有这棵树」）。
func decodeOptional(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	return decodeJSON(raw)
}

// decodeJSON 解码一段 JSON 值。数字统一解码成 json.Number：这样 12 与 12.0 的判等
// 不会因为「谁先被 float64 化」而不一致，也不需要两条比较路径。
func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// lookup 沿路径段找值。任一段走不通（中间层不是对象、键不存在）都算「取不到值」，
// 由调用方按「不匹配」处理——这不是错误，条件本就是在描述「有没有这个值」。
func (s *evalScope) lookup(root string, path []string) (any, bool) {
	cur := s.args
	if root == pathRootRisk {
		cur = s.risk
	}
	for _, seg := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// condition 是编译后的条件树。求值只返回真假、不返回错误：所有「无法判断」的输入都在
// 构造期被拦下，避免把配置错误伪装成一次安静的不匹配。
type condition interface {
	eval(*evalScope) bool
}

// alwaysCond 是恒真条件：空 when 与 {"all": []} 都编译成它。
type alwaysCond struct{}

// eval 恒为真。
func (alwaysCond) eval(*evalScope) bool { return true }

// allCond 是「全部子条件成立」；空数组恒真（schema 唯一认可的 always 写法）。
type allCond []condition

// eval 短路求值，遇假即假。
func (c allCond) eval(s *evalScope) bool {
	for _, k := range c {
		if !k.eval(s) {
			return false
		}
	}
	return true
}

// anyCond 是「任一子条件成立」；schema 禁止空数组，构造期已挡。
type anyCond []condition

// eval 短路求值，遇真即真。
func (c anyCond) eval(s *evalScope) bool {
	for _, k := range c {
		if k.eval(s) {
			return true
		}
	}
	return false
}

// notCond 是子条件取反。
type notCond struct {
	child condition
}

// eval 返回子条件的否定。
func (c notCond) eval(s *evalScope) bool { return !c.child.eval(s) }

// leafCond 是算子叶：一条取值路径配一个算子。
type leafCond struct {
	// root 是取值前缀（arg 或 risk）。
	root string
	// path 是前缀之后的路径段。
	path []string
	// op 是算子，构造期已验证属于封闭集合。
	op string
	// operand 是算子右值；in/notIn 时是 []any。
	operand any
	// re 只对 matches 有效，构造期已编译。
	re *regexp.Regexp
}

// eval 取值后按算子判定；取不到值一律「不匹配」，连 ne 也不因缺值而成立。
func (c leafCond) eval(s *evalScope) bool {
	v, ok := s.lookup(c.root, c.path)
	if !ok {
		return false
	}
	switch c.op {
	case "eq":
		return jsonEqual(v, c.operand)
	case "ne":
		return !jsonEqual(v, c.operand)
	case ">", ">=", "<", "<=":
		return compareOrdered(v, c.operand, c.op)
	case "in":
		return containsValue(c.operand.([]any), v)
	case "notIn":
		return !containsValue(c.operand.([]any), v)
	case "matches":
		str, ok := v.(string)
		return ok && c.re.MatchString(str)
	}
	// 构造期已排除未知算子；这里只是让编译器看得见返回路径。
	return false
}

// compileWhen 编译一条规则的 when；缺省与 null 视作恒真（等价于 {"all": []}）。
func compileWhen(raw json.RawMessage) (condition, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return alwaysCond{}, nil
	}
	return compileCondition(trimmed, 0)
}

// compileCondition 把条件 JSON 编译成条件树。depth 从 0 起，超过上限即报错。
func compileCondition(raw json.RawMessage, depth int) (condition, error) {
	if depth > maxConditionDepth {
		return nil, fmt.Errorf("条件嵌套超过 %d 层", maxConditionDepth)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("条件必须是 JSON 对象：%w", err)
	}
	switch {
	case hasKey(obj, "all"):
		kids, err := compileChildren(obj, "all", depth)
		if err != nil {
			return nil, err
		}
		return allCond(kids), nil
	case hasKey(obj, "any"):
		kids, err := compileChildren(obj, "any", depth)
		if err != nil {
			return nil, err
		}
		if len(kids) == 0 {
			return nil, fmt.Errorf("any 至少要有一项子条件（空数组恒假，schema 禁止）")
		}
		return anyCond(kids), nil
	case hasKey(obj, "not"):
		if len(obj) != 1 {
			return nil, fmt.Errorf("组合子 not 的对象只能有 not 一个键")
		}
		child, err := compileCondition(obj["not"], depth+1)
		if err != nil {
			return nil, fmt.Errorf("not：%w", err)
		}
		return notCond{child: child}, nil
	default:
		return compileLeaf(obj)
	}
}

// compileChildren 展开 all/any 的子条件数组，并锁死「组合子对象只能有一个键」。
func compileChildren(obj map[string]json.RawMessage, name string, depth int) ([]condition, error) {
	if len(obj) != 1 {
		return nil, fmt.Errorf("组合子 %s 的对象只能有 %s 一个键，多个条件请放进数组", name, name)
	}
	raw := bytes.TrimSpace(obj[name])
	if len(raw) == 0 || raw[0] != '[' {
		return nil, fmt.Errorf("%s 的取值必须是条件数组", name)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s 的取值必须是条件数组：%w", name, err)
	}
	out := make([]condition, 0, len(items))
	for i, item := range items {
		c, err := compileCondition(item, depth+1)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]：%w", name, i, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// compileLeaf 编译算子叶：恰好一个字段路径映射到一个算子对象（多个条件必须用 all 组合，
// 否则求值顺序无歧义可谈）。
func compileLeaf(obj map[string]json.RawMessage) (condition, error) {
	if len(obj) != 1 {
		return nil, fmt.Errorf("条件叶必须恰好一个字段路径（实际 %d 个），多个条件请用 all 组合", len(obj))
	}
	var path string
	var opRaw json.RawMessage
	for p, r := range obj {
		path, opRaw = p, r
	}
	root, segs, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	c := leafCond{root: root, path: segs}
	if err := c.compileOperator(opRaw); err != nil {
		return nil, fmt.Errorf("字段 %s：%w", path, err)
	}
	return c, nil
}

// parsePath 拆出取值前缀与路径段。前缀只有 arg（动作入参）与 risk（动作风险声明）；
// 未知前缀是配置错误而不是「取不到值」——前缀写错会让规则静默永不命中。
func parsePath(path string) (string, []string, error) {
	root, rest, found := strings.Cut(path, ".")
	if !found {
		return "", nil, fmt.Errorf("取值路径 %q 必须形如 arg.qty 或 risk.reversible", path)
	}
	if root != pathRootArg && root != pathRootRisk {
		return "", nil, fmt.Errorf("取值路径 %q 的前缀只能是 arg 或 risk", path)
	}
	segs := strings.Split(rest, ".")
	for _, seg := range segs {
		if !pathSegmentPattern.MatchString(seg) {
			return "", nil, fmt.Errorf("取值路径 %q 的段 %q 不合法", path, seg)
		}
	}
	return root, segs, nil
}

// compileOperator 解析算子对象：恰好一个算子，右值按该算子的取值域校验。
// 未知算子必须报错：把它当成「不匹配」会让 deny 规则静默失效。
func (c *leafCond) compileOperator(raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("算子对象必须是 JSON 对象：%w", err)
	}
	if len(obj) != 1 {
		return fmt.Errorf("算子对象必须恰好一个算子（实际 %d 个）", len(obj))
	}
	var op string
	var operandRaw json.RawMessage
	for k, v := range obj {
		op, operandRaw = k, v
	}

	switch op {
	case "eq", "ne":
		v, err := decodeJSON(operandRaw)
		if err != nil {
			return fmt.Errorf("算子 %s 的取值不是合法 JSON：%w", op, err)
		}
		c.op, c.operand = op, v
		return nil
	case ">", ">=", "<", "<=":
		v, err := decodeJSON(operandRaw)
		if err != nil {
			return fmt.Errorf("算子 %s 的取值不是合法 JSON：%w", op, err)
		}
		switch v.(type) {
		case json.Number, string:
		default:
			return fmt.Errorf("算子 %s 的取值必须是数字或 ISO-8601 时间字符串", op)
		}
		c.op, c.operand = op, v
		return nil
	case "in", "notIn":
		v, err := decodeJSON(operandRaw)
		if err != nil {
			return fmt.Errorf("算子 %s 的取值不是合法 JSON：%w", op, err)
		}
		set, ok := v.([]any)
		if !ok || len(set) == 0 {
			return fmt.Errorf("算子 %s 的取值必须是非空数组", op)
		}
		c.op, c.operand = op, set
		return nil
	case "matches":
		v, err := decodeJSON(operandRaw)
		if err != nil {
			return fmt.Errorf("算子 matches 的取值不是合法 JSON：%w", err)
		}
		pattern, ok := v.(string)
		if !ok || pattern == "" {
			return fmt.Errorf("算子 matches 的取值必须是非空字符串")
		}
		if utf8.RuneCountInString(pattern) > maxPatternLen {
			return fmt.Errorf("算子 matches 的正则超过 %d 字符", maxPatternLen)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("算子 matches 的正则非法：%w", err)
		}
		c.op, c.re = op, re
		return nil
	}
	return fmt.Errorf("未知算子 %q（支持 %s）", op, supportedOperators)
}

// hasKey 判断对象里有没有某个键。
func hasKey(obj map[string]json.RawMessage, key string) bool {
	_, ok := obj[key]
	return ok
}

// numberValue 取出数值。引擎内的数字只有一种表示：json.Number（decodeJSON 用 UseNumber）。
func numberValue(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

// jsonEqual 按 JSON 语义判等：数字比数值（12 == 12.0），其余比结构。
func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(normalizeJSON(a), normalizeJSON(b))
}

// normalizeJSON 递归把 json.Number 换成 float64，让 12 与 12.0 判等不依赖解码路径。
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeJSON(e)
		}
		return out
	}
	return v
}

// containsValue 是 in/notIn 的相等语义判定；集合项可以是任意 JSON 值。
func containsValue(set []any, v any) bool {
	for _, e := range set {
		if jsonEqual(e, v) {
			return true
		}
	}
	return false
}

// compareOrdered 实现四个大小算子：两侧类型必须一致（都是数字，或都是字符串）。
func compareOrdered(a, b any, op string) bool {
	cmp, ok := compareValues(a, b)
	if !ok {
		return false
	}
	return orderedMatch(op, cmp)
}

// compareValues 给出 -1/0/1。数字按数值；字符串先试 RFC 3339（两侧都必须是时间，
// 一侧是时间另一侧不是即视为类型不一致），否则按字典序——ISO-8601 的字符序即时间序。
func compareValues(a, b any) (int, bool) {
	if af, ok := numberValue(a); ok {
		bf, ok := numberValue(b)
		if !ok {
			return 0, false
		}
		return compareFloat(af, bf), true
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if !aok || !bok {
		return 0, false
	}
	at, aerr := time.Parse(time.RFC3339, as)
	bt, berr := time.Parse(time.RFC3339, bs)
	switch {
	case aerr == nil && berr == nil:
		return compareTime(at, bt), true
	case aerr == nil || berr == nil:
		// 一侧是时间、另一侧不是：类型不一致，按不匹配处理。
		return 0, false
	}
	return strings.Compare(as, bs), true
}

// compareFloat 比较两个数值。
func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareTime 比较两个时刻。
func compareTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}
	return 0
}

// orderedMatch 把比较结果套到算子上。
func orderedMatch(op string, cmp int) bool {
	switch op {
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	}
	return false
}
