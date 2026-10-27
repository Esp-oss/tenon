package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestAllPatternsAreRE2Compatible 是一道**可移植性门禁**。
//
// 起因：v0.2 的 Iso8601Duration 用了前瞻 `(?=...)`，ECMA-262 合法，但 Go 的 RE2
// 编译不了，于是整份 schema 在 Go 侧无法编译——一个 RE2 系实现（Go、部分 Java/Python）
// 就此被挡在协议之外。协议条文写得再对，也敌不过一个无法被消费的正则。
//
// 门禁规则：schema 里所有 pattern 必须能被 regexp.Compile 接受（即 RE2 子集）。
func TestAllPatternsAreRE2Compatible(t *testing.T) {
	dir := "../../spec/0.1/schemas"
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 schema：%v", err)
	}

	type found struct{ file, path, pattern string }
	var patterns []found

	var walk func(file, path string, node any)
	walk = func(file, path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				if k == "pattern" {
					if s, ok := v.(string); ok {
						patterns = append(patterns, found{file, path, s})
					}
					continue
				}
				walk(file, path+"/"+k, v)
			}
		case []any:
			for i, v := range n {
				walk(file, path+"/"+itoa(i), v)
			}
		}
	}

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s: %v", f, err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", f, err)
		}
		walk(filepath.Base(f), "", doc)
	}

	if len(patterns) == 0 {
		t.Fatal("没有扫到任何 pattern，门禁形同虚设")
	}

	for _, p := range patterns {
		if _, err := regexp.Compile(p.pattern); err != nil {
			t.Errorf("%s%s 的 pattern 不能被 RE2 编译：%v\n  pattern: %s",
				p.file, p.path, err, p.pattern)
		}
	}
	t.Logf("检查了 %d 条 pattern，全部 RE2 兼容", len(patterns))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestIso8601DurationSemantics 锁住修好之后的行为：形状对、退化形态排除。
func TestIso8601DurationSemantics(t *testing.T) {
	s := mustLoad(t)
	sch, err := s.Schema("common.schema.json")
	if err != nil {
		t.Fatalf("取 common schema: %v", err)
	}
	// 从编译后的 schema 里拿不到 pattern，改用注解在本地重建一份等价校验：
	// 这里直接断言「用 spec 里的实例校验」的间接结果——Grant 的 window 字段走这个 def。
	_ = sch

	accept := []string{"P1D", "PT24H", "P1W", "P1Y2M3DT4H5M6.5S", "PT30S", "P0D", "PT0.5S", "P2W3D"}
	reject := []string{"P", "PT", "P1YT", "1D", "P1H", "PT1D", "P-1D", "P1.5Y"}

	pat := durationPatternFromSchema(t)
	re := regexp.MustCompile(pat)
	for _, v := range accept {
		if !re.MatchString(v) {
			t.Errorf("应接受但被拒：%q", v)
		}
	}
	for _, v := range reject {
		if re.MatchString(v) {
			t.Errorf("应拒绝但被接受：%q", v)
		}
	}
}

// durationPatternFromSchema 从源文件里取 pattern（编译后的 schema 不再暴露源正则）。
func durationPatternFromSchema(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../spec/0.1/schemas/common.schema.json")
	if err != nil {
		t.Fatalf("读取 common.schema.json: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 common.schema.json: %v", err)
	}
	d, ok := doc.Defs["Iso8601Duration"]
	if !ok || d.Pattern == "" {
		t.Fatal("common.schema.json 里找不到 $defs/Iso8601Duration.pattern")
	}
	return d.Pattern
}
