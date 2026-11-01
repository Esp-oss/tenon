package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tenon.dev/internal/controlplane/policy"
)

// fakeModule 提供一个带 inputSchema 的 action 定义，供规则路径校验使用。
func fakeModule(t *testing.T, moduleID string, schemas map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/actions/", func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Base(r.URL.Path)
		schema, ok := schemas[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "inputSchema": json.RawMessage(schema),
			// risk 是 action.schema.json 的必填项；给一份真实的形状，
			// 好让 risk.* 路径校验有东西可比对。
			"risk": map[string]any{"level": "medium", "reversible": true},
		})
	})
	return httptest.NewServer(mux)
}

func TestLoadApprovalRulesAcceptsValidPaths(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "quote-to-fulfillment")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(`[
	  {"policyId":"biz-1","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"arg.qty":{"<=":5}}]},"outcome":"auto"}
	]`), 0o600)

	rules, errs := LoadApprovalRules(filepath.Join(dir, "skills"), func(module string) (string, bool) {
		return mod.URL, true
	})
	if len(errs) != 0 {
		t.Fatalf("不应有错误：%+v", errs)
	}
	if len(rules) != 1 || rules[0].PolicyID != "biz-1" {
		t.Fatalf("规则未加载：%+v", rules)
	}
}

// 字母级笔误在 not 取反下会把「大额要批」翻成「大额自动过」，必须加载期就拒。
func TestLoadApprovalRulesRejectsUnknownPath(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "quote-to-fulfillment")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(`[
	  {"policyId":"biz-typo","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"not":{"arg.amout":{"<=":5000}}},"outcome":"auto"}
	]`), 0o600)

	rules, errs := LoadApprovalRules(filepath.Join(dir, "skills"), func(module string) (string, bool) {
		return mod.URL, true
	})
	if len(rules) != 0 {
		t.Fatalf("路径不存在的规则必须整份丢弃，得到 %+v", rules)
	}
	if len(errs) == 0 {
		t.Fatalf("必须报告错误")
	}
}

// 模块离线时该业务规则不生效，但不能让平台级规则也失效。
func TestLoadApprovalRulesSkipsOfflineModule(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "s1")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(`[
	  {"policyId":"biz-offline","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[]},"outcome":"approve"}
	]`), 0o600)

	rules, errs := LoadApprovalRules(filepath.Join(dir, "skills"), func(module string) (string, bool) {
		return "", false
	})
	if len(rules) != 0 || len(errs) == 0 {
		t.Fatalf("模块离线应跳过并报错：rules=%d errs=%d", len(rules), len(errs))
	}
}

// writeSkill 写一份 approval.json 并返回 skills 根目录。
func writeSkill(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "s1")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写规则失败：%v", err)
	}
	return filepath.Join(dir, "skills")
}

// 未知算子必须在加载期被拦下：它会让规则进池后拖垮整个宿主启动，
// 而 spec §3.8 承诺的是「只丢这个 skill 的规则」。
func TestLoadApprovalRulesRejectsUnknownOperator(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()
	skills := writeSkill(t, `[
	  {"policyId":"biz-between","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"arg.qty":{"between":[1,2]}}]},"outcome":"auto"}
	]`)

	rules, errs := LoadApprovalRules(skills, func(string) (string, bool) { return mod.URL, true })
	if len(rules) != 0 {
		t.Fatalf("编译不过的规则必须整份丢弃，得到 %+v", rules)
	}
	if len(errs) == 0 {
		t.Fatalf("必须报告编译错误")
	}
}

// 模块级规则（省略 actions）无法逐个动作校验入参，业务级文件要求写明 actions。
func TestLoadApprovalRulesRejectsModuleLevelRule(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{})
	defer mod.Close()
	skills := writeSkill(t, `[
	  {"policyId":"biz-module-level","priority":1,
	   "match":{"module":"procurement"},
	   "when":{"all":[]},"outcome":"approve"}
	]`)

	rules, errs := LoadApprovalRules(skills, func(string) (string, bool) { return mod.URL, true })
	if len(rules) != 0 || len(errs) == 0 {
		t.Fatalf("模块级规则应被拒：rules=%d errs=%d", len(rules), len(errs))
	}
}

// risk.* 的笔误同样会在 not 取反下翻转闸门，必须按动作声明的 risk 字段校验。
func TestLoadApprovalRulesRejectsUnknownRiskPath(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()
	skills := writeSkill(t, `[
	  {"policyId":"biz-risk-typo","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"risk.reversibel":{"eq":true}}]},"outcome":"auto"}
	]`)

	rules, errs := LoadApprovalRules(skills, func(string) (string, bool) { return mod.URL, true })
	if len(rules) != 0 || len(errs) == 0 {
		t.Fatalf("risk.* 笔误应被拒：rules=%d errs=%d", len(rules), len(errs))
	}
}

// 合法的 risk.* 路径不该被误杀。
func TestLoadApprovalRulesAcceptsRiskPath(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()
	skills := writeSkill(t, `[
	  {"policyId":"biz-risk-ok","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"risk.reversible":{"eq":true}},{"arg.qty":{"<=":5}}]},"outcome":"auto"}
	]`)

	rules, errs := LoadApprovalRules(skills, func(string) (string, bool) { return mod.URL, true })
	if len(errs) != 0 {
		t.Fatalf("合法 risk 路径不应报错：%+v", errs)
	}
	if len(rules) != 1 {
		t.Fatalf("规则应加载，得到 %+v", rules)
	}
}

// 业务规则延迟校验（裁决 ①a）：引用未注册模块的规则整份挂起，不丢、不阻塞；
// 模块到齐之前每次补校验都只回答「还缺谁」；全到齐才生效。
//
// 一份文件引用两个模块是刻意的：分组按 moduleId，但判定单位是**整份文件**——最后一个
// 模块注册前它不能生效（那时它的另一条动作定义还没得校验），也不能因此被丢弃。
func TestDeferredRulesWaitForEveryReferencedModule(t *testing.T) {
	proc := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer proc.Close()
	inv := fakeModule(t, "inventory", map[string]string{
		"inventory.GetStock": `{"type":"object","properties":{"sku":{"type":"string"}}}`,
	})
	defer inv.Close()

	skills := writeSkill(t, `[
	  {"policyId":"biz-proc","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"arg.qty":{"<=":5}}]},"outcome":"auto"},
	  {"policyId":"biz-inv","priority":1,
	   "match":{"module":"inventory","actions":["inventory.GetStock"]},
	   "when":{"all":[{"arg.sku":{"matches":"^STD-"}}]},"outcome":"auto"}
	]`)

	registered := map[string]string{} // module → baseURL（模拟注册表）
	lookup := func(module string) (string, bool) {
		base, ok := registered[module]
		return base, ok
	}

	res := LoadApprovalRulesDeferred(skills, lookup)
	if len(res.Rules) != 0 || len(res.Errors) != 0 {
		t.Fatalf("两个模块都没注册：不该生效也不该报错（rules=%d errs=%v）", len(res.Rules), res.Errors)
	}
	if len(res.Pending["procurement"]) != 1 || len(res.Pending["inventory"]) != 1 {
		t.Fatalf("同一份文件应同时挂在两个模块名下：%+v", res.Pending)
	}

	// 只注册 procurement：文件仍在 pending，且只剩 inventory 这一个缺口。
	registered["procurement"] = proc.URL
	retry := RetryPendingRules(context.Background(), res.Pending["procurement"], lookup)
	if len(retry.Accepted) != 0 || len(retry.Rejected) != 0 || len(retry.StillPending) != 1 {
		t.Fatalf("只到齐一个模块时不该生效（accepted=%d still=%d rejected=%d）",
			len(retry.Accepted), len(retry.StillPending), len(retry.Rejected))
	}
	if got := retry.StillPending[0].Modules; len(got) != 1 || got[0] != "inventory" {
		t.Fatalf("剩余缺口应换到 inventory 名下，得到 %v", got)
	}

	// 两个都到齐 → 整份生效，两条规则都进规则池。
	registered["inventory"] = inv.URL
	retry = RetryPendingRules(context.Background(), retry.StillPending, lookup)
	if len(retry.StillPending) != 0 || len(retry.Rejected) != 0 || len(retry.Accepted) != 1 {
		t.Fatalf("模块到齐后应整份生效：accepted=%d still=%d rejected=%d",
			len(retry.Accepted), len(retry.StillPending), len(retry.Rejected))
	}
	if got := len(retry.Accepted[0].Rules); got != 2 {
		t.Fatalf("生效的规则条数 = %d，期望 2（整份文件）", got)
	}
}

// 补校验失败要能区分「还有模块没注册」与「真的校验不过」：后者带出肇事 policyId。
func TestRetryPendingRulesReportsOffendingPolicyID(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()

	skills := writeSkill(t, `[
	  {"policyId":"biz-typo","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"arg.amout":{"<=":5}}]},"outcome":"auto"}
	]`)
	res := LoadApprovalRulesDeferred(skills, func(string) (string, bool) { return "", false })
	if len(res.Pending) != 1 {
		t.Fatalf("模块未注册时应挂起：%+v", res.Pending)
	}

	retry := RetryPendingRules(context.Background(), res.Pending["procurement"],
		func(string) (string, bool) { return mod.URL, true })
	if len(retry.Rejected) != 1 {
		t.Fatalf("模块到齐后路径校验失败应被拒：accepted=%d still=%d rejected=%d",
			len(retry.Accepted), len(retry.StillPending), len(retry.Rejected))
	}
	if got := retry.Rejected[0].PolicyID; got != "biz-typo" {
		t.Fatalf("被拒规则应点名 policyId=biz-typo，得到 %q", got)
	}
	if !errors.Is(retry.Rejected[0].Err, ErrArgPathMissing) {
		t.Fatalf("原因应是 ErrArgPathMissing，得到 %v", retry.Rejected[0].Err)
	}
}

// dev 平台规则与业务规范的合并结果：业务阈值必须真的生效。
//
// 这条用例锁的是「合并取更严」的推论——业务文件里若留一条无条件 approve，
// 它会把自己文件里的 auto 压死，加载日志却照样显示"规则已加载"。
// P4 的冒烟若期望「事件驱动 qty=3 走 auto」，靠的就是这里。
func TestDevBusinessRulesTakeEffect(t *testing.T) {
	platform, err := LoadPolicies(filepath.Join("..", "..", "config", "policies.dev.json"))
	if err != nil {
		t.Fatalf("读 dev 平台规则失败：%v", err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "skills", "quote-to-fulfillment", "approval.json"))
	if err != nil {
		t.Fatalf("读业务规范失败：%v", err)
	}
	var biz []policy.PolicyRule
	if err := json.Unmarshal(raw, &biz); err != nil {
		t.Fatalf("业务规范不是合法规则数组：%v", err)
	}
	all := append(append([]policy.PolicyRule{}, platform...), biz...)
	engine := policy.NewPolicyEngine(all)

	cases := []struct {
		name        string
		origin      string
		qty         int
		wantOutcome string
		wantBy      string
	}{
		{"事件驱动小额自动过", originEvent, 3, policy.OutcomeAuto, "biz-purchase-auto-small"},
		{"事件驱动大额要批", originEvent, 12, policy.OutcomeApprove, "biz-purchase-approve-large"},
		{"对话发起小额也要批", originRequest, 3, policy.OutcomeApprove, "plat-request-needs-human"},
		{"对话发起大额也要批", originRequest, 12, policy.OutcomeApprove, "plat-request-needs-human"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]any{"sku": "STD-1002", "qty": tc.qty})
			if err != nil {
				t.Fatalf("编组入参失败：%v", err)
			}
			got, err := engine.Evaluate(context.Background(), policy.PolicyRequest{
				Action: "procurement.CreatePurchaseRequest",
				Args:   args,
				Origin: tc.origin,
			})
			if err != nil {
				t.Fatalf("求值失败：%v", err)
			}
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("期望 %s，得到 %s（decidedBy=%v）", tc.wantOutcome, got.Outcome, got.DecidedBy)
			}
			if len(got.DecidedBy) == 0 || got.DecidedBy[0] != tc.wantBy {
				t.Fatalf("决胜规则应为 %s，得到 %v", tc.wantBy, got.DecidedBy)
			}
		})
	}
}
