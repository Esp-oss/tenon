// Package spec 把 spec/0.1/schemas/*.json 编译成可复用的校验器。
//
// 它承担 M0 的一条出口判据：**Go 侧 JSON Schema 库的正反对拍**——
// 同一份 schema，一条合法实例必须过，一条已知应失败的实例必须挂。
// 库选型见设计文档 §15 未决问题 6；这里锁定的是 santhosh-tekuri/jsonschema v6
// （支持 draft 2020-12）。若换成别的库，spec_test.go 的对拍用例应当全部保留。
//
// 约定：schema 是唯一真源，本包不复制字段定义，只做加载与校验。
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// DefaultDir 是仓库内 schema 的约定位置（相对仓库根）。
const DefaultDir = "spec/0.1/schemas"

// idPrefix 用于没有 $id 的文件派生一个稳定标识。
const idPrefix = "https://tenon.dev/spec/0.1/schemas/"

// Set 是一组已编译的 schema，按文件名索引（如 "action.schema.json"）。
type Set struct {
	dir     string
	schemas map[string]*jsonschema.Schema
	ids     map[string]string
}

// Load 读取 dir 下全部 .json 并编译。跨文件 $ref 依赖 $id，故先全部 AddResource 再 Compile。
func Load(dir string) (*Set, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("扫描 schema 目录 %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("schema 目录 %s 下没有 .json 文件", dir)
	}
	sort.Strings(entries)

	compiler := jsonschema.NewCompiler()
	s := &Set{dir: dir, schemas: map[string]*jsonschema.Schema{}, ids: map[string]string{}}

	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", path, err)
		}
		name := filepath.Base(path)
		id := idPrefix + name
		var probe struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.ID != "" {
			id = probe.ID
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%s 不是合法 JSON: %w", name, err)
		}
		if err := compiler.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("%s 注册失败: %w", name, err)
		}
		s.ids[name] = id
	}

	for name, id := range s.ids {
		sch, err := compiler.Compile(id)
		if err != nil {
			return nil, fmt.Errorf("%s 编译失败: %w", name, err)
		}
		s.schemas[name] = sch
	}
	return s, nil
}

// Dir 返回加载目录。
func (s *Set) Dir() string { return s.dir }

// Names 返回全部 schema 文件名（已排序）。
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.schemas))
	for n := range s.schemas {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Schema 按文件名取已编译的 schema。
func (s *Set) Schema(name string) (*jsonschema.Schema, error) {
	sch, ok := s.schemas[name]
	if !ok {
		return nil, fmt.Errorf("未知 schema %q，可用：%s", name, strings.Join(s.Names(), ", "))
	}
	return sch, nil
}

// Validate 校验一段 JSON 字节。返回的错误是可读的多行摘要。
func (s *Set) Validate(name string, instance []byte) error {
	sch, err := s.Schema(name)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance))
	if err != nil {
		return fmt.Errorf("实例不是合法 JSON: %w", err)
	}
	return formatValidationError(sch.Validate(doc))
}

// ValidateValue 校验一个已反序列化的值（调用方负责用 UnmarshalJSON 保留数字类型）。
func (s *Set) ValidateValue(name string, v any) error {
	sch, err := s.Schema(name)
	if err != nil {
		return err
	}
	return formatValidationError(sch.Validate(v))
}

// formatValidationError 把 jsonschema 的嵌套错误树压成人类可读的若干条。
//
// 只依赖 ValidationError 的公开字段（InstanceLocation / Causes / Error()），
// 不去碰 ErrorKind 的本地化接口——那是版本间最容易变的表面。
func formatValidationError(err error) error {
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	leaves := collectLeaves(ve)
	if len(leaves) == 0 {
		return errors.New(truncate(ve.Error(), 1200))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "校验失败（%d 处）：", len(leaves))
	for i, leaf := range leaves {
		if i >= 8 {
			fmt.Fprintf(&b, "\n  … 另有 %d 处", len(leaves)-8)
			break
		}
		loc := "(根)"
		if len(leaf.InstanceLocation) > 0 {
			loc = "/" + strings.Join(leaf.InstanceLocation, "/")
		}
		fmt.Fprintf(&b, "\n  - %s: %s", loc, truncate(leaf.Error(), 300))
	}
	return errors.New(b.String())
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func collectLeaves(ve *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(ve.Causes) == 0 {
		return []*jsonschema.ValidationError{ve}
	}
	var out []*jsonschema.ValidationError
	for _, c := range ve.Causes {
		out = append(out, collectLeaves(c)...)
	}
	return out
}
