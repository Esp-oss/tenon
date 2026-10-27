package module

import (
	"reflect"
	"testing"
)

func TestTemplatePlaceholders(t *testing.T) {
	cases := []struct {
		name string
		tpl  string
		want []string
	}{
		{"正常", "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）", []string{"sku", "qty", "referenceNo"}},
		{"重复只留一次", "{qty} 件，{qty} 件", []string{"qty"}},
		{"没有占位符", "查询在库量", nil},
		{"未闭合不抛错", "扣减 {sku 件", nil},
		{"空模板", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TemplatePlaceholders(c.tpl); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("TemplatePlaceholders(%q) = %v，期望 %v", c.tpl, got, c.want)
			}
		})
	}
}
