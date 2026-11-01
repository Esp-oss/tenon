package main

import (
	"encoding/json"
	"testing"

	"tenon.dev/internal/module"
)

// 模板里的每个占位符都必须真的在 inputSchema.properties 里：否则渲染必然退 R2，
// 卡片上永远读不到人话（P3 spec §4 的 R1）。
func assertTemplateArgs(t *testing.T, a module.Action) {
	t.Helper()
	if a.SummaryTemplate == "" {
		t.Fatalf("%s 没声明 summaryTemplate", a.ID)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(a.InputSchema, &schema); err != nil {
		t.Fatalf("%s 的 inputSchema 读不出：%v", a.ID, err)
	}
	for _, name := range module.TemplatePlaceholders(a.SummaryTemplate) {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("%s 的模板占位符 {%s} 不在 inputSchema.properties 里", a.ID, name)
		}
	}
	// 必填字段：规划期写 args 的唯一依据，必须真的来自 inputSchema.required。
	if len(a.RequiredArgs) == 0 {
		t.Fatalf("%s 没声明 requiredArgs（规划期无法保证 args 完整）", a.ID)
	}
	for _, name := range a.RequiredArgs {
		found := false
		for _, r := range schema.Required {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s 的 requiredArgs 含 %q，但它不在 inputSchema.required 里", a.ID, name)
		}
	}
}

func TestActionTemplatesUseDeclaredArgs(t *testing.T) {
	assertTemplateArgs(t, deductStockAction())
	assertTemplateArgs(t, getStockAction())
	assertTemplateArgs(t, releaseStockAction())
}
