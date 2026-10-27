package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
