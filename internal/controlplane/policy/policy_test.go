package policy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// defaultAction 是表驱动用例里的被评估动作，与 match 里的动作一致。
const defaultAction = "inventory.DeductStock"

// match 构造 PolicyMatch；actions 省略时表示匹配该模块的全部动作。
func match(module string, actions ...string) PolicyMatch {
	return PolicyMatch{Module: module, Actions: actions}
}

// mustCond 把条件 JSON 字面量交给引擎；字面量自己写错时要立刻炸在测试里，
// 否则「非法 JSON」会被误当成「条件不成立」而掩盖用例意图。
func mustCond(t *testing.T, cond string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(cond)) {
		t.Fatalf("条件不是合法 JSON：%s", cond)
	}
	return json.RawMessage(cond)
}

// evalReq 构造一次评估请求：动作 id 加本次入参。
func evalReq(action, args string) PolicyRequest {
	return PolicyRequest{Action: action, Args: json.RawMessage(args)}
}

// TestPolicyMergeOrderDenyWins 锁定合并顺序：deny 压过 approve 与 auto，与 priority 无关。
func TestPolicyMergeOrderDenyWins(t *testing.T) {
	rules := []PolicyRule{
		{PolicyID: "p-auto", Priority: 10, Match: match("inventory", "inventory.DeductStock"),
			When: nil, Outcome: "auto"},
		{PolicyID: "p-approve", Priority: 100, Match: match("inventory", "inventory.DeductStock"),
			When: mustCond(t, `{"all":[{"arg.qty":{">":10}}]}`), Outcome: "approve"},
		{PolicyID: "p-deny", Priority: 1, Match: match("inventory", "inventory.DeductStock"),
			When: mustCond(t, `{"all":[{"arg.qty":{"in":[999]}}]}`), Outcome: "deny"},
	}
	eng := NewPolicyEngine(rules)

	got, err := eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{"qty":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "approve" || len(got.DecidedBy) != 1 || got.DecidedBy[0] != "p-approve" {
		t.Fatalf("期望 approve/p-approve，得到 %+v", got)
	}

	got, err = eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{"qty":999}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "deny" {
		t.Fatalf("deny 必须压过 approve，得到 %+v", got)
	}
}

// TestPolicyNoMatchDefaultsToDeny 锁定 fail-closed：规则为零时也是 deny，且理由码可读。
func TestPolicyNoMatchDefaultsToDeny(t *testing.T) {
	eng := NewPolicyEngine(nil)
	got, err := eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "deny" {
		t.Fatalf("无命中必须 deny（fail-closed），得到 %q", got.Outcome)
	}
	if len(got.Reasons) == 0 || got.Reasons[0] != "NO_POLICY_MATCH_DEFAULT_DENY" {
		t.Fatalf("reasons 应说明默认拒绝：%v", got.Reasons)
	}
	if len(got.DecidedBy) != 0 {
		t.Fatalf("无命中时 decidedBy 必须为空：%v", got.DecidedBy)
	}
}

// TestPolicyNoMatchSerializesEmptyDecidedBy 锁定响应形状：schema 要求 decidedBy 是数组，
// 序列化成 null 会让跨语言的 C5 断言（「无命中即 deny」）读不出空集合。
func TestPolicyNoMatchSerializesEmptyDecidedBy(t *testing.T) {
	eng := NewPolicyEngine(nil)
	got, err := eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("评估结果不能序列化：%v", err)
	}
	if !strings.Contains(string(raw), `"decidedBy":[]`) {
		t.Fatalf("空 decidedBy 必须序列化成 []，得到 %s", raw)
	}
}

// TestPolicyConditionEval 是算子与组合子的表驱动主体。「命中」用 auto 表达，
// 「未命中」回落默认 deny——这样一张表就同时覆盖了条件求值与 fail-closed。
func TestPolicyConditionEval(t *testing.T) {
	cases := []struct {
		name string
		when string
		args string
		risk string
		want string // auto = 条件成立，deny = 条件不成立
	}{
		{name: "eq 数字", when: `{"arg.qty":{"eq":12}}`, args: `{"qty":12}`, want: OutcomeAuto},
		{name: "eq 按下数值判等（12 与 12.0）", when: `{"arg.qty":{"eq":12}}`, args: `{"qty":12.0}`, want: OutcomeAuto},
		{name: "eq 字符串不等", when: `{"arg.sku":{"eq":"STD-1"}}`, args: `{"sku":"STD-2"}`, want: OutcomeDeny},
		{name: "eq 布尔与 null", when: `{"arg.note":{"eq":null}}`, args: `{"note":null}`, want: OutcomeAuto},
		{name: "ne", when: `{"arg.sku":{"ne":"STD-1"}}`, args: `{"sku":"STD-2"}`, want: OutcomeAuto},
		{name: "ne 相等则不成立", when: `{"arg.sku":{"ne":"STD-2"}}`, args: `{"sku":"STD-2"}`, want: OutcomeDeny},
		{name: "> 数字", when: `{"arg.qty":{">":10}}`, args: `{"qty":10.5}`, want: OutcomeAuto},
		{name: "> 边界相等不成立", when: `{"arg.qty":{">":10}}`, args: `{"qty":10}`, want: OutcomeDeny},
		{name: ">= 边界相等成立", when: `{"arg.qty":{">=":10}}`, args: `{"qty":10}`, want: OutcomeAuto},
		{name: "< 不成立", when: `{"arg.qty":{"<":10}}`, args: `{"qty":10}`, want: OutcomeDeny},
		{name: "<= 边界相等成立", when: `{"arg.qty":{"<=":10}}`, args: `{"qty":10}`, want: OutcomeAuto},
		{name: "in 命中", when: `{"arg.sku":{"in":["STD-1","STD-2"]}}`, args: `{"sku":"STD-2"}`, want: OutcomeAuto},
		{name: "in 未命中", when: `{"arg.sku":{"in":["STD-1"]}}`, args: `{"sku":"STD-2"}`, want: OutcomeDeny},
		{name: "notIn 命中", when: `{"arg.sku":{"notIn":["STD-9"]}}`, args: `{"sku":"STD-2"}`, want: OutcomeAuto},
		{name: "notIn 未命中", when: `{"arg.sku":{"notIn":["STD-2"]}}`, args: `{"sku":"STD-2"}`, want: OutcomeDeny},
		{name: "matches 整串锚定", when: `{"arg.sku":{"matches":"^STD-[0-9]+$"}}`, args: `{"sku":"STD-1002"}`, want: OutcomeAuto},
		{name: "matches 不锚定则子串即命中", when: `{"arg.sku":{"matches":"[0-9]{4}"}}`, args: `{"sku":"STD-1002"}`, want: OutcomeAuto},
		{name: "matches 非字符串值不成立", when: `{"arg.qty":{"matches":"^1"}}`, args: `{"qty":12}`, want: OutcomeDeny},
		{name: "all 全部成立", when: `{"all":[{"arg.qty":{">":10}},{"arg.sku":{"eq":"STD-1002"}}]}`, args: `{"qty":12,"sku":"STD-1002"}`, want: OutcomeAuto},
		{name: "all 有一项不成立", when: `{"all":[{"arg.qty":{">":10}},{"arg.sku":{"eq":"STD-1"}}]}`, args: `{"qty":12,"sku":"STD-1002"}`, want: OutcomeDeny},
		{name: "all 空数组恒真", when: `{"all":[]}`, args: `{}`, want: OutcomeAuto},
		{name: "any 任一成立", when: `{"any":[{"arg.qty":{">":100}},{"arg.sku":{"eq":"STD-1002"}}]}`, args: `{"qty":1,"sku":"STD-1002"}`, want: OutcomeAuto},
		{name: "any 全不成立", when: `{"any":[{"arg.qty":{">":100}},{"arg.sku":{"eq":"STD-1"}}]}`, args: `{"qty":1,"sku":"STD-1002"}`, want: OutcomeDeny},
		{name: "not 取反成立", when: `{"not":{"arg.qty":{">":10}}}`, args: `{"qty":3}`, want: OutcomeAuto},
		{name: "not 取反不成立", when: `{"not":{"arg.qty":{">":10}}}`, args: `{"qty":12}`, want: OutcomeDeny},
		{name: "not 套 all", when: `{"not":{"all":[{"arg.qty":{">":10}},{"arg.sku":{"eq":"STD-1"}}]}}`, args: `{"qty":12,"sku":"STD-2"}`, want: OutcomeAuto},
		{name: "any 套 not", when: `{"any":[{"not":{"arg.qty":{">":10}}},{"arg.sku":{"eq":"STD-1002"}}]}`, args: `{"qty":12,"sku":"STD-1002"}`, want: OutcomeAuto},
		{name: "risk 路径", when: `{"risk.reversible":{"eq":false}}`, args: `{}`, risk: `{"level":"high","reversible":false}`, want: OutcomeAuto},
		{name: "risk 路径与 arg 混用", when: `{"all":[{"risk.level":{"eq":"high"}},{"arg.qty":{">":10}}]}`, args: `{"qty":12}`, risk: `{"level":"high","reversible":true}`, want: OutcomeAuto},
		{name: "取不到值即不成立（字段缺失）", when: `{"arg.qty":{">":0}}`, args: `{"sku":"STD-1"}`, want: OutcomeDeny},
		{name: "取不到值即不成立（中间层不是对象）", when: `{"arg.line.qty":{">":0}}`, args: `{"line":[1]}`, want: OutcomeDeny},
		{name: "完全没给 args 也不报错", when: `{"arg.qty":{">":0}}`, args: ``, want: OutcomeDeny},
		{name: "ne 也不能因缺值而通过", when: `{"arg.qty":{"ne":999}}`, args: `{}`, want: OutcomeDeny},
		{name: "大小算子两侧类型不一致即不成立", when: `{"arg.qty":{">":10}}`, args: `{"qty":"12"}`, want: OutcomeDeny},
		{name: "ISO-8601 时间比较", when: `{"arg.dueAt":{">":"2026-10-27T00:00:00Z"}}`, args: `{"dueAt":"2026-10-27T09:15:00Z"}`, want: OutcomeAuto},
		{name: "ISO-8601 时间比较（带偏移）", when: `{"arg.dueAt":{"<":"2026-10-27T00:00:00Z"}}`, args: `{"dueAt":"2026-10-26T21:00:00-02:00"}`, want: OutcomeAuto},
		{name: "一侧是时间另一侧不是即不成立", when: `{"arg.dueAt":{">":"2026-10-27T00:00:00Z"}}`, args: `{"dueAt":"2026-10-27"}`, want: OutcomeDeny},
		{name: "非时间字符串按字典序比较", when: `{"arg.sku":{">":"STD-1000"}}`, args: `{"sku":"STD-1002"}`, want: OutcomeAuto},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewPolicyEngine([]PolicyRule{{
				PolicyID: "p-1",
				Match:    match("inventory", defaultAction),
				When:     mustCond(t, tc.when),
				Outcome:  OutcomeAuto,
			}})
			req := evalReq(defaultAction, tc.args)
			if tc.risk != "" {
				req.Risk = json.RawMessage(tc.risk)
			}
			got, err := eng.Evaluate(context.Background(), req)
			if err != nil {
				t.Fatalf("求值不应报错：%v", err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("条件 %s 求值不符：期望 %s，得到 %+v", tc.when, tc.want, got)
			}
			if tc.want == OutcomeDeny && (len(got.Reasons) != 1 || got.Reasons[0] != ReasonNoMatch) {
				t.Fatalf("未命中应给出 %s，得到 %v", ReasonNoMatch, got.Reasons)
			}
			if tc.want == OutcomeAuto && (len(got.Reasons) != 1 || got.Reasons[0] != ReasonAuto) {
				t.Fatalf("命中应给出 %s，得到 %v", ReasonAuto, got.Reasons)
			}
		})
	}
}

// nestedNot 构造 depth 层嵌套的 not，用于测嵌套上限。
func nestedNot(depth int) string {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"not":`)
	}
	b.WriteString(`{"all":[]}`)
	for i := 0; i < depth; i++ {
		b.WriteString(`}`)
	}
	return b.String()
}

// TestPolicyConditionErrors 锁定「无法判断即报错，绝不静默放行」这条底线。
func TestPolicyConditionErrors(t *testing.T) {
	cases := []struct {
		name string
		when string
		want string // 期望错误信息里出现的关键字
	}{
		{name: "未知算子", when: `{"arg.qty":{"between":[1,2]}}`, want: "未知算子"},
		{name: "非法正则", when: `{"arg.sku":{"matches":"([a-z"}}`, want: "正则"},
		{name: "未知取值前缀", when: `{"args.qty":{"eq":1}}`, want: "前缀只能是 arg 或 risk"},
		{name: "路径缺前缀段", when: `{"qty":{"eq":1}}`, want: "必须形如"},
		{name: "路径段非法", when: `{"arg.q-t":{"eq":1}}`, want: "不合法"},
		{name: "条件叶有两个字段路径", when: `{"arg.qty":{"eq":1},"arg.sku":{"eq":"x"}}`, want: "恰好一个字段路径"},
		{name: "组合子混入其他键", when: `{"all":[],"arg.qty":{"eq":1}}`, want: "只能有 all 一个键"},
		{name: "any 空数组", when: `{"any":[]}`, want: "any 至少要有一项"},
		{name: "not 的对象为空", when: `{"not":{}}`, want: "恰好一个字段路径"},
		{name: "算子对象有两个算子", when: `{"arg.qty":{">":1,"<":9}}`, want: "恰好一个算子"},
		{name: "大小算子取值不是数字或时间串", when: `{"arg.meta":{">":true}}`, want: "必须是数字或 ISO-8601"},
		{name: "in 空数组", when: `{"arg.qty":{"in":[]}}`, want: "非空数组"},
		{name: "matches 空串", when: `{"arg.sku":{"matches":""}}`, want: "非空字符串"},
		{name: "顶层不是对象", when: `[{"arg.qty":{"eq":1}}]`, want: "条件必须是 JSON 对象"},
		{name: "嵌套过深", when: nestedNot(40), want: "嵌套超过"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewPolicyEngine([]PolicyRule{{
				PolicyID: "p-1",
				Match:    match("inventory", defaultAction),
				When:     mustCond(t, tc.when),
				Outcome:  OutcomeAuto,
			}})
			_, err := eng.Evaluate(context.Background(), evalReq(defaultAction, `{"qty":12}`))
			if err == nil {
				t.Fatalf("条件 %s 必须报错，不能当成不匹配放过", tc.when)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q，得到：%v", tc.want, err)
			}
		})
	}
}

// TestPolicyInvalidRuleOutcomeIsError 锁定：outcome 不合法是配置错误，不是「不命中」。
func TestPolicyInvalidRuleOutcomeIsError(t *testing.T) {
	eng := NewPolicyEngine([]PolicyRule{{
		PolicyID: "p-yolo",
		Match:    match("inventory", defaultAction),
		When:     mustCond(t, `{"all":[]}`),
		Outcome:  "yolo",
	}})
	if _, err := eng.Evaluate(context.Background(), evalReq(defaultAction, `{}`)); err == nil {
		t.Fatal("非法 outcome 必须报错")
	}
}

// TestPolicyBadRequestIsError 锁定请求侧的输入错误：动作 id 不成立、入参不是合法 JSON。
func TestPolicyBadRequestIsError(t *testing.T) {
	eng := NewPolicyEngine([]PolicyRule{{
		PolicyID: "p-1",
		Match:    match("inventory"),
		When:     mustCond(t, `{"all":[]}`),
		Outcome:  OutcomeAuto,
	}})
	cases := []struct {
		name string
		req  PolicyRequest
		want string
	}{
		{name: "动作 id 为空", req: PolicyRequest{Action: ""}, want: "动作 id"},
		{name: "动作 id 没有模块前缀", req: PolicyRequest{Action: "DeductStock"}, want: "动作 id"},
		{name: "动作 id 以点结尾", req: PolicyRequest{Action: "inventory."}, want: "动作 id"},
		{name: "args 不是合法 JSON", req: PolicyRequest{Action: defaultAction, Args: json.RawMessage(`{"qty":`)}, want: "args"},
		{name: "risk 不是合法 JSON", req: PolicyRequest{Action: defaultAction, Risk: json.RawMessage(`{`)}, want: "risk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := eng.Evaluate(context.Background(), tc.req); err == nil {
				t.Fatalf("期望报错，得到 nil")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q，得到：%v", tc.want, err)
			}
		})
	}
}

// TestPolicyMerge 是合并规则的表驱动主体：deny > approve > auto，同 outcome 取
// priority 最大者并记入 decidedBy，审批人只随 approve 的胜出规则返回。
func TestPolicyMerge(t *testing.T) {
	cases := []struct {
		name          string
		action        string // 缺省用 defaultAction
		args          string
		rules         []PolicyRule
		wantOutcome   string
		wantDecidedBy []string
		wantApprovers []string
		wantReason    string
	}{
		{
			name: "deny 压过更高优先级的 approve 与 auto",
			args: `{"qty":999}`,
			rules: []PolicyRule{
				{PolicyID: "p-auto", Priority: 900, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeAuto},
				{PolicyID: "p-approve", Priority: 500, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"arg.qty":{">":10}}`), Outcome: OutcomeApprove},
				{PolicyID: "p-deny", Priority: 1, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"arg.qty":{">":100}}`), Outcome: OutcomeDeny},
			},
			wantOutcome:   OutcomeDeny,
			wantDecidedBy: []string{"p-deny"},
			wantReason:    ReasonDeny,
		},
		{
			name: "同 outcome 取 priority 最大者",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-low", Priority: 10, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeApprove},
				{PolicyID: "p-high", Priority: 100, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeApprove},
			},
			wantOutcome:   OutcomeApprove,
			wantDecidedBy: []string{"p-high"},
			wantReason:    ReasonApprovalRequired,
		},
		{
			name: "全部命中时 decidedBy 只记最终裁决者",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-1", Priority: 1, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeAuto},
				{PolicyID: "p-2", Priority: 50, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"arg.qty":{">":0}}`), Outcome: OutcomeApprove},
				{PolicyID: "p-3", Priority: 999, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"arg.qty":{">":0}}`), Outcome: OutcomeApprove},
			},
			wantOutcome:   OutcomeApprove,
			wantDecidedBy: []string{"p-3"},
			wantReason:    ReasonApprovalRequired,
		},
		{
			name: "全部命中且都是 auto 时也是 auto",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-1", Priority: 1, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeAuto},
				{PolicyID: "p-2", Priority: 9, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"arg.qty":{">":0}}`), Outcome: OutcomeAuto},
			},
			wantOutcome:   OutcomeAuto,
			wantDecidedBy: []string{"p-2"},
			wantReason:    ReasonAuto,
		},
		{
			name: "审批人随 approve 的胜出规则返回",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-low", Priority: 10, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeApprove,
					Approvers: []string{"role:warehouse_clerk"}},
				{PolicyID: "p-high", Priority: 99, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeApprove,
					Approvers: []string{"role:warehouse_manager"}},
			},
			wantOutcome:   OutcomeApprove,
			wantDecidedBy: []string{"p-high"},
			wantApprovers: []string{"role:warehouse_manager"},
			wantReason:    ReasonApprovalRequired,
		},
		{
			name: "deny 即使声明了审批人也不返回",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-deny", Priority: 1, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeDeny,
					Approvers: []string{"role:warehouse_manager"}},
			},
			wantOutcome:   OutcomeDeny,
			wantDecidedBy: []string{"p-deny"},
			wantReason:    ReasonDeny,
		},
		{
			name: "同 priority 同 outcome 先声明者胜",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-first", Priority: 50, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeApprove},
				{PolicyID: "p-second", Priority: 50, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeApprove},
			},
			wantOutcome:   OutcomeApprove,
			wantDecidedBy: []string{"p-first"},
			wantReason:    ReasonApprovalRequired,
		},
		{
			name:   "actions 省略时匹配该模块全部动作",
			action: "inventory.GetStock",
			args:   `{"sku":"STD-1"}`,
			rules: []PolicyRule{
				{PolicyID: "p-module", Priority: 1, Match: match("inventory"),
					When: mustCond(t, `{"all":[]}`), Outcome: OutcomeAuto},
			},
			wantOutcome:   OutcomeAuto,
			wantDecidedBy: []string{"p-module"},
			wantReason:    ReasonAuto,
		},
		{
			name: "模块不匹配即无命中",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-other", Priority: 100,
					Match: match("procurement", "procurement.CreatePurchaseRequest"),
					When:  mustCond(t, `{"all":[]}`), Outcome: OutcomeAuto},
			},
			wantOutcome:   OutcomeDeny,
			wantDecidedBy: nil,
			wantReason:    ReasonNoMatch,
		},
		{
			name: "动作不在 actions 列表里即无命中",
			args: `{"qty":12}`,
			rules: []PolicyRule{
				{PolicyID: "p-other-action", Priority: 100,
					Match: match("inventory", "inventory.GetStock"),
					When:  mustCond(t, `{"all":[]}`), Outcome: OutcomeAuto},
			},
			wantOutcome:   OutcomeDeny,
			wantDecidedBy: nil,
			wantReason:    ReasonNoMatch,
		},
		{
			name: "条件不成立即无命中",
			args: `{"qty":1}`,
			rules: []PolicyRule{
				{PolicyID: "p-big", Priority: 100, Match: match("inventory", defaultAction),
					When: mustCond(t, `{"arg.qty":{">":10}}`), Outcome: OutcomeDeny},
			},
			wantOutcome:   OutcomeDeny,
			wantDecidedBy: nil,
			wantReason:    ReasonNoMatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action := tc.action
			if action == "" {
				action = defaultAction
			}
			eng := NewPolicyEngine(tc.rules)
			got, err := eng.Evaluate(context.Background(), evalReq(action, tc.args))
			if err != nil {
				t.Fatalf("求值不应报错：%v", err)
			}
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("outcome 不符：期望 %s，得到 %+v", tc.wantOutcome, got)
			}
			if len(got.DecidedBy) != len(tc.wantDecidedBy) ||
				strings.Join(got.DecidedBy, ",") != strings.Join(tc.wantDecidedBy, ",") {
				t.Fatalf("decidedBy 不符：期望 %v，得到 %v", tc.wantDecidedBy, got.DecidedBy)
			}
			if len(got.Approvers) != len(tc.wantApprovers) ||
				strings.Join(got.Approvers, ",") != strings.Join(tc.wantApprovers, ",") {
				t.Fatalf("approvers 不符：期望 %v，得到 %v", tc.wantApprovers, got.Approvers)
			}
			if len(got.Reasons) != 1 || got.Reasons[0] != tc.wantReason {
				t.Fatalf("理由码不符：期望 [%s]，得到 %v", tc.wantReason, got.Reasons)
			}
		})
	}
}

// TestPolicyEngineIsImmutable 锁定「构造时复制规则」：调用方事后改自己的切片不影响引擎，
// 否则策略会随调用方的数据竞争而变，审计结果无法复现。
func TestPolicyEngineIsImmutable(t *testing.T) {
	rules := []PolicyRule{{
		PolicyID: "p-1",
		Match:    match("inventory", defaultAction),
		When:     mustCond(t, `{"all":[]}`),
		Outcome:  OutcomeAuto,
	}}
	eng := NewPolicyEngine(rules)
	rules[0].Outcome = OutcomeDeny
	rules[0].Match.Module = "procurement"

	got, err := eng.Evaluate(context.Background(), evalReq(defaultAction, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != OutcomeAuto || len(got.DecidedBy) != 1 || got.DecidedBy[0] != "p-1" {
		t.Fatalf("引擎不应被调用方的后续修改影响：%+v", got)
	}
}

// TestPolicyCanceledContext 锁定 ctx 取消即返回：上层据此把评估失败当成 deny。
func TestPolicyCanceledContext(t *testing.T) {
	eng := NewPolicyEngine(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.Evaluate(ctx, evalReq(defaultAction, `{}`)); err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
}
