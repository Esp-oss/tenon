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
