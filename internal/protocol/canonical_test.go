package protocol

import (
	"encoding/json"
	"testing"
)

// JCS 规范化：对象键排序、数字按最短表示、字符串转义与 JSON 一致。
// 这些断言的意义是「同一份语义内容必须得到同一串字节」——声明哈希与审计哈希都靠它。
func TestCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"键排序", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"嵌套排序", `{"z":{"b":1,"a":2}}`, `{"z":{"a":2,"b":1}}`},
		{"整数不带小数点", `{"n":1.0}`, `{"n":1}`},
		{"数组保持顺序", `{"a":[3,1,2]}`, `{"a":[3,1,2]}`},
		{"中文不转义", `{"t":"库存"}`, `{"t":"库存"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(c.in), &v); err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			got, err := CanonicalJSON(v)
			if err != nil {
				t.Fatalf("规范化失败：%v", err)
			}
			if string(got) != c.want {
				t.Fatalf("canonical(%s) = %s，期望 %s", c.in, got, c.want)
			}
		})
	}
}
