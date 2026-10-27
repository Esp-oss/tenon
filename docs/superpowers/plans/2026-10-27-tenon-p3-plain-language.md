# P3 白话渲染 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让审批卡默认显示业务人话（`summaryTemplate` 渲染），技术标识只在折叠区，数字只来自 `args`。

**Architecture:** 模块在 manifest 里声明模板 → agent 的能力发现（`GET /.well-known/tenon`）把 `title + summaryTemplate` 一起带走 → agent 在 `buildPending` 时渲染一次并落盘（`Pending.Labels` 白话 / `Pending.Details` 技术原文）→ 公告板以 `items[{label, detail}]` 透传（host 的 `items` 本就是 `[]map[string]any`，**不改 host 结构体**）→ 页面把技术信息放进 `<details>`。

**Tech Stack:** Go 1.26（`net/http`、`encoding/json`、`testing` + `httptest`）；JSON Schema 0.1（`internal/spec` 校验器）；原生 JS 静态页（无构建链）。

**对应 spec:** [`docs/superpowers/specs/2026-10-27-tenon-p3-plain-language-design.md`](../specs/2026-10-27-tenon-p3-plain-language-design.md)

---

## 文件结构

| 文件 | 职责 | 动作 |
|---|---|---|
| `spec/0.1/schemas/module-manifest.schema.json` | 摘要线契约（agent 只读这条） | 修改：`ActionSummary` 加 `summaryTemplate` |
| `spec/0.1/schemas/action.schema.json` | 完整 Action 契约 | 修改：加同一字段 |
| `internal/spec/spec_test.go` | 契约回归 | 修改：新增两处形状的用例 |
| `internal/module/module.go` | Action 结构 + manifest 序列化 | 修改：加字段、透出、`TemplatePlaceholders` |
| `internal/module/template_test.go` | 占位符解析 | 新建 |
| `cmd/inventory/main.go` | 模板声明 | 修改 |
| `cmd/procurement/main.go` | 模板声明 | 修改 |
| `cmd/quote/main.go` | 模板声明 | 修改 |
| `cmd/inventory`/`cmd/procurement`/`cmd/quote` 的 `main_test.go` | 占位符 ⊆ `inputSchema.properties` | 新建 |
| `internal/agent/planner.go` | `ModuleDigest.ActionMeta` | 修改 |
| `internal/agent/run.go` | 发现填 `ActionMeta`；`buildPending` 用新渲染器 | 修改 |
| `internal/agent/suspend.go` | `renderLabels` → `renderSteps`（含 `details`） | 修改 |
| `internal/agent/state.go` | `Pending.Details` | 修改 |
| `internal/agent/board.go` | 推送 `{label, detail}` | 修改 |
| `ui/app.js` | 卡面只留白话；技术信息进 `<details>` | 修改 |
| `docs/interfaces.md` §4.7 | 公告板条目补 `detail` | 修改 |

**关键类型约定（后面任务都按这个写）**

```go
// internal/agent/planner.go
type ActionMeta struct {
	Title           string
	SummaryTemplate string
}
type ModuleDigest struct {
	ID         string
	Actions    []string
	ActionMeta map[string]ActionMeta
}

// internal/agent/state.go（Pending 内新增）
Details []string `json:"details,omitempty"`

// internal/agent/suspend.go
func renderSteps(plan Plan, meta map[string]ActionMeta) (labels, details []string)
```

---

## Task 1: 契约——两处 schema 接受 `summaryTemplate`

**Files:**
- Modify: `spec/0.1/schemas/module-manifest.schema.json`（`$defs/ActionSummary.properties`）
- Modify: `spec/0.1/schemas/action.schema.json`（根 `properties`）
- Test: `internal/spec/spec_test.go`

- [ ] **Step 1: 写失败用例**

在 `internal/spec/spec_test.go` 末尾追加：

```go
// TestSummaryTemplateInBothShapes 锁住契约：白话模板在「摘要线」与「完整 Action」
// 两处都要能被接受——agent 只读摘要线，模块只序列化完整 Action，少一处就等于没有。
func TestSummaryTemplateInBothShapes(t *testing.T) {
	s := mustLoad(t)

	manifest := `{
	  "tenon":"0.1",
	  "module":{"id":"inventory","version":"1.4.0","title":"库存"},
	  "actions":[{"id":"inventory.DeductStock","version":"1.0.0","title":"扣减",
	              "kind":"write","risk":{"level":"medium","reversible":true},
	              "dryRun":{"supported":true},
	              "summaryTemplate":"扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）"}],
	  "events":[],
	  "transports":["http"],
	  "capabilities":["dry-run"]
	}`
	if err := s.Validate("module-manifest.schema.json", []byte(manifest)); err != nil {
		t.Fatalf("摘要线应接受 summaryTemplate：%v", err)
	}

	nonString := strings.Replace(manifest,
		`"summaryTemplate":"扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）"`,
		`"summaryTemplate":42`, 1)
	if err := s.Validate("module-manifest.schema.json", []byte(nonString)); err == nil {
		t.Fatal("summaryTemplate 必须是字符串")
	}

	action := strings.Replace(validWriteAction, `"kind": "write",`,
		`"kind": "write", "summaryTemplate": "扣减 {sku} 库存 {qty} 件",`, 1)
	if err := s.Validate("action.schema.json", []byte(action)); err != nil {
		t.Fatalf("完整 Action 应接受 summaryTemplate：%v", err)
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/spec/ -run TestSummaryTemplateInBothShapes -count=1 -v`
Expected: FAIL，错误含 `additionalProperties 'summaryTemplate' not allowed`（两处 schema 都不认）

- [ ] **Step 3: 改 `module-manifest.schema.json` 的 `ActionSummary`**

在 `$defs/ActionSummary/properties/title` 之后插入（注意上一项补逗号）：

```json
        "summaryTemplate": {
          "description": "人话模板：占位符 {argName} 只从该动作的 args 取值，不做跨 step、不做计算。占位符名须匹配 ^[A-Za-z_][A-Za-z0-9_]*$ 且应出现在 inputSchema.properties 里（约定，不在加载期强校验）。渲染是 agent 的职责。",
          "type": "string",
          "maxLength": 200
        },
```

- [ ] **Step 4: 改 `action.schema.json` 的根 `properties`**

在 `"title"` 之后同级插入同一段 JSON（字段名、语义、`maxLength` 完全一致）。

- [ ] **Step 5: 跑用例确认通过**

Run: `go test ./internal/spec/ -run TestSummaryTemplateInBothShapes -count=1 -v`
Expected: PASS

- [ ] **Step 6: 跑契约全量**

Run: `go test ./internal/spec/ -count=1 && pnpm check`
Expected: 全 ok；spec 校验 11 项 0 失败

- [ ] **Step 7: Commit**

```bash
git add spec/0.1/schemas/module-manifest.schema.json spec/0.1/schemas/action.schema.json internal/spec/spec_test.go
git commit -m "feat(spec): action 与摘要线接受 summaryTemplate（白话模板的契约）"
```

---

## Task 2: `module.Action.SummaryTemplate` 并透出摘要线

**Files:**
- Modify: `internal/module/module.go`（`Action` 结构；`actionSummary` 结构；`module.go:830` 附近构造处）
- Test: `internal/module/module_test.go`

- [ ] **Step 1: 写失败用例**

追加到 `internal/module/module_test.go`：

```go
// 模板是模块声明的一部分：必须出现在摘要线里（agent 只读这条线），
// 空模板不出现（omitempty）——老模块的 manifest 不该多一个空字段。
func TestManifestCarriesSummaryTemplate(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	f.module.Actions[1].SummaryTemplate = "写一个 {name}"
	srv := serve(t, f.module)

	_, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil, nil)
	if !bytes.Contains(raw, []byte(`"summaryTemplate":"写一个 {name}"`)) {
		t.Fatalf("摘要线没带模板：%s", raw)
	}

	empty := newFixture()
	srv2 := serve(t, empty.module)
	_, raw2 := doJSON(t, http.MethodGet, srv2.URL+"/.well-known/tenon", nil, nil)
	if bytes.Contains(raw2, []byte("summaryTemplate")) {
		t.Fatalf("空模板不该出现：%s", raw2)
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/module/ -run TestManifestCarriesSummaryTemplate -count=1`
Expected: 编译失败——`f.module.Actions[1].SummaryTemplate undefined`

- [ ] **Step 3: 加字段并透出**

`internal/module/module.go` 的 `Action`：在 `Title` 之后加

```go
	// SummaryTemplate 是给人看的一句话模板（module-manifest/action schema 的可选字段）。
	// 占位符只从本次 args 取值：它渲染的是「这一次要做什么」，不是动作说明书。
	SummaryTemplate string `json:"summaryTemplate,omitempty"`
```

`actionSummary`（同文件，`module.go:454` 附近）加

```go
	// SummaryTemplate 必须出现在摘要线：agent 的能力发现只读这条（D15 上下文成本），
	// 少了它，agent 每次渲染都要回拉完整 Action。
	SummaryTemplate string `json:"summaryTemplate,omitempty"`
```

构造处（`module.go:830` 附近）：

```go
		out.Actions = append(out.Actions, actionSummary{
			ID: a.ID, Version: a.Version, Title: a.Title, Kind: a.Kind,
			Risk: a.Risk, DryRun: a.DryRun, SummaryTemplate: a.SummaryTemplate,
		})
```

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/module/ -run TestManifestCarriesSummaryTemplate -count=1 -v && go test ./internal/module/ -count=1`
Expected: PASS；模块包全绿

- [ ] **Step 5: Commit**

```bash
git add internal/module/module.go internal/module/module_test.go
git commit -m "feat(module): Action 声明 summaryTemplate 并透出摘要线"
```

---

## Task 3: 三个模块填模板 + 占位符机械校验

**Files:**
- Modify: `internal/module/module.go`（新增 `TemplatePlaceholders`）
- Create: `internal/module/template_test.go`
- Modify: `cmd/inventory/main.go`、`cmd/procurement/main.go`、`cmd/quote/main.go`
- Create: `cmd/inventory/main_test.go`、`cmd/procurement/main_test.go`、`cmd/quote/main_test.go`

- [ ] **Step 1: 写占位符解析的用例**

新建 `internal/module/template_test.go`：

```go
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
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/module/ -run TestTemplatePlaceholders -count=1`
Expected: 编译失败——`undefined: TemplatePlaceholders`

- [ ] **Step 3: 实现 `TemplatePlaceholders`**

加到 `internal/module/module.go` 末尾：

```go
// TemplatePlaceholders 按出现顺序返回模板里的占位符名（去重）。
//
// 导出是给**测试**用的：模块侧的用例要机械断言「模板里每个占位符都真的在
// inputSchema.properties 里」。这条约束不放进加载期校验——模板写错只影响可读性，
// 不该让整条审批路径瘫痪（P3 spec §6）。
func TemplatePlaceholders(tpl string) []string {
	var out []string
	seen := make(map[string]bool)
	for i := 0; i < len(tpl); i++ {
		if tpl[i] != '{' {
			continue
		}
		rel := strings.IndexByte(tpl[i+1:], '}')
		if rel < 0 {
			// 落单的 '{' 不是占位符：渲染侧会因此退 R2，这里不替它报错。
			break
		}
		name := tpl[i+1 : i+1+rel]
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i += rel + 1
	}
	return out
}
```

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/module/ -run TestTemplatePlaceholders -count=1 -v`
Expected: PASS（5 个子用例）

- [ ] **Step 5: 写三个模块的占位符用例**

新建 `cmd/inventory/main_test.go`：

```go
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
	}
	if err := json.Unmarshal(a.InputSchema, &schema); err != nil {
		t.Fatalf("%s 的 inputSchema 读不出：%v", a.ID, err)
	}
	for _, name := range module.TemplatePlaceholders(a.SummaryTemplate) {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("%s 的模板占位符 {%s} 不在 inputSchema.properties 里", a.ID, name)
		}
	}
}

func TestActionTemplatesUseDeclaredArgs(t *testing.T) {
	assertTemplateArgs(t, deductStockAction())
	assertTemplateArgs(t, getStockAction())
	assertTemplateArgs(t, releaseStockAction())
}
```

（三个构造函数名已核实：`getStockAction()` / `deductStockAction()` / `releaseStockAction()` 都在 `cmd/inventory/main.go`。）

`cmd/procurement/main_test.go` 复制同一份 `assertTemplateArgs`（Go 测试不跨包共享小工具），只断言 `createPurchaseRequestAction()`。
`cmd/quote/main_test.go` 同上，断言 `createStandardPartQuoteAction()`。

- [ ] **Step 6: 跑三个用例确认失败**

Run: `go test ./cmd/inventory/ ./cmd/procurement/ ./cmd/quote/ -count=1`
Expected: FAIL，`没声明 summaryTemplate`

- [ ] **Step 7: 填模板**

`cmd/inventory/main.go` 三个动作各加一行（放在 `Title` 之后）：

```go
		SummaryTemplate: "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）",
```
```go
		SummaryTemplate: "查询 {sku} 在库量",
```
```go
		SummaryTemplate: "回补 {sku} 库存 {qty} 件（单据 {referenceNo}）",
```

`cmd/procurement/main.go`：

```go
		SummaryTemplate: "缺 {qty} 件 {sku}，发起采购申请（单据 {referenceNo}）",
```

`cmd/quote/main.go`（`lines` 是数组，按 R1 不进模板，明细留给 `details`）：

```go
		SummaryTemplate: "按报价单 {quoteNo} 生成需求",
```

- [ ] **Step 8: 跑用例确认通过**

Run: `go test ./cmd/inventory/ ./cmd/procurement/ ./cmd/quote/ -count=1 -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/module/module.go internal/module/template_test.go cmd/inventory cmd/procurement cmd/quote
git commit -m "feat(modules): 三个模块声明人话模板，并用测试锁占位符与 args 一致"
```

---

## Task 4: 能力发现携带 `ActionMeta`

**Files:**
- Modify: `internal/agent/planner.go`（`ModuleDigest`、新增 `ActionMeta`）
- Modify: `internal/agent/run.go`（`discover`、`manifestActions`）
- Test: `internal/agent/run_test.go`（扩展 `fakeModule`）

- [ ] **Step 1: 扩展假模块，让它能带模板**

`internal/agent/run_test.go` 的 `fakeModule` 结构体加字段：

```go
	// Templates 是 action id → 人话模板；空表示该动作没声明（走 R2）。
	Templates map[string]string
```

`serve` 里构造 actions 的那段（`run_test.go:176-179`）改成：

```go
		actions := make([]map[string]any, 0, len(m.Actions))
		for _, id := range m.Actions {
			item := map[string]any{"id": id, "version": "0.1.0", "title": id, "kind": "write"}
			if tpl, ok := m.Templates[id]; ok {
				item["summaryTemplate"] = tpl
			}
			actions = append(actions, item)
		}
```

- [ ] **Step 2: 写失败用例**

追加到 `internal/agent/run_test.go`：

```go
// 模板必须随能力发现一起到 agent 手里：它是渲染白话的唯一来源，
// 少了它就只能退回 R2（标题 + 参数），卡上读不到「扣 12 件」这种话。
func TestDiscoverCarriesActionMeta(t *testing.T) {
	mod := newFakeModule(t, "inventory", []string{"inventory.DeductStock"}, new([]string))
	mod.Templates = map[string]string{
		"inventory.DeductStock": "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）",
	}
	r := newRunnerForDiscover(t, mod.Server.URL)

	got, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("能力发现失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 个模块，得到 %d", len(got))
	}
	meta, ok := got[0].ActionMeta["inventory.DeductStock"]
	if !ok {
		t.Fatalf("ActionMeta 缺 inventory.DeductStock：%+v", got[0])
	}
	if meta.SummaryTemplate != "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）" {
		t.Fatalf("模板不符：%q", meta.SummaryTemplate)
	}
	if meta.Title == "" {
		t.Fatalf("Title 也要带走（R2 兜底要用）")
	}
}
```

并加一个只服务 `GET /tenon/modules` 的宿主桩：

```go
// newRunnerForDiscover 造一个只够跑 discover 的 runner：宿主注册表指向假模块。
func newRunnerForDiscover(t *testing.T, moduleBase string) *runner {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenon/modules" {
			http.NotFound(w, r)
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"modules": []map[string]any{{"id": "inventory", "baseURL": moduleBase, "enabled": true}},
		})
	}))
	t.Cleanup(srv.Close)
	return &runner{host: srv.URL, client: srv.Client()}
}
```

- [ ] **Step 3: 跑用例确认失败**

Run: `go test ./internal/agent/ -run TestDiscoverCarriesActionMeta -count=1`
Expected: 编译失败——`got[0].ActionMeta undefined`

- [ ] **Step 4: 改类型与发现**

`internal/agent/planner.go`：

```go
// ActionMeta 是渲染白话所需的最小信息：标题（R2 兜底）与模板（R1 主路径）。
type ActionMeta struct {
	Title           string
	SummaryTemplate string
}

type ModuleDigest struct {
	ID      string
	Actions []string
	// ActionMeta 按 action id 索引；发现失败或老模块没声明时为空 map。
	ActionMeta map[string]ActionMeta
}
```

`internal/agent/run.go` 的 `discover` 里：

```go
		actions, meta, err := r.manifestActions(ctx, base)
		if err != nil {
			return nil, fmt.Errorf("模块 %q 的能力发现失败：%w", id, err)
		}
		r.modules[id] = base
		out = append(out, ModuleDigest{ID: id, Actions: actions, ActionMeta: meta})
```

`manifestActions` 改为：

```go
// manifestActions 从模块的 GET /.well-known/tenon 取动作清单（摘要级，不含 schema），
// 顺带把渲染白话要用的 title 与 summaryTemplate 一起带走。
func (r *runner) manifestActions(ctx context.Context, baseURL string) ([]string, map[string]ActionMeta, error) {
	status, raw, err := r.getJSON(ctx, baseURL+"/.well-known/tenon")
	if err != nil {
		return nil, nil, fmt.Errorf("GET /.well-known/tenon 失败：%w", err)
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("GET /.well-known/tenon 返回 %d：%s", status, snippet(raw))
	}
	var manifest struct {
		Actions []struct {
			ID              string `json:"id"`
			Title           string `json:"title"`
			SummaryTemplate string `json:"summaryTemplate"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, nil, fmt.Errorf("manifest 不是合法 JSON：%w", err)
	}
	ids := make([]string, 0, len(manifest.Actions))
	meta := make(map[string]ActionMeta, len(manifest.Actions))
	for _, a := range manifest.Actions {
		id := strings.TrimSpace(a.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
		meta[id] = ActionMeta{Title: a.Title, SummaryTemplate: a.SummaryTemplate}
	}
	return ids, meta, nil
}
```

- [ ] **Step 5: 跑用例确认通过**

Run: `go test ./internal/agent/ -run 'TestDiscoverCarriesActionMeta' -count=1 -v && go test ./internal/agent/ -count=1`
Expected: PASS；agent 包全绿（`checkActionsDiscovered` 与 prompt 仍用 `Actions []string`，不受影响）

- [ ] **Step 6: Commit**

```bash
git add internal/agent/planner.go internal/agent/run.go internal/agent/run_test.go
git commit -m "feat(agent): 能力发现带走 title 与 summaryTemplate"
```

---

## Task 5: 渲染器 `renderSteps` 与 `Pending.Details`

**Files:**
- Modify: `internal/agent/suspend.go`（替换 `renderLabels`）
- Modify: `internal/agent/state.go`（`Pending.Details`）
- Test: `internal/agent/suspend_test.go`

- [ ] **Step 1: 写失败用例（表驱动）**

追加到 `internal/agent/suspend_test.go`：

```go
// 渲染规则表（P3 spec §4）：模板只取 args；任何一步失败退 R2；R2 不含 action id。
func TestRenderSteps(t *testing.T) {
	meta := map[string]ActionMeta{
		"inventory.DeductStock": {Title: "扣减库存", SummaryTemplate: "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）"},
		"inventory.GetStock":    {Title: "查询在库量"},                                        // 无模板 → R2
		"inventory.ReleaseStock": {Title: "回补库存", SummaryTemplate: "回补 {sku} {qty}"},      // 缺占位符 → R2
		"procurement.CreatePurchaseRequest": {Title: "创建采购申请", SummaryTemplate: "缺 {qty} 件 {sku}（明细 {lines}）"}, // {lines} 是数组 → R2
		"quote.CreateStandardPartQuote":     {Title: "报价", SummaryTemplate: "按报价单 {quoteNo} 生成需求"},
		"inventory.Broken":                  {Title: "坏模板", SummaryTemplate: "落单的 {sku"},
		"inventory.Unnamed":                 {}, // Title 与模板都缺
	}

	cases := []struct {
		name       string
		action     string
		args       string
		wantLabel  string
		wantDetail string
	}{
		{"模板正常", "inventory.DeductStock",
			`{"sku":"STD-1002","qty":3,"referenceNo":"Q-2026-0417"}`,
			"扣减 STD-1002 库存 3 件（单据 Q-2026-0417）",
			`inventory.DeductStock（{"sku":"STD-1002","qty":3,"referenceNo":"Q-2026-0417"}）`},
		{"无模板退 R2", "inventory.GetStock", `{"sku":"STD-1002"}`,
			"查询在库量（sku=STD-1002）", ""},
		{"缺占位符退 R2", "inventory.ReleaseStock", `{"sku":"STD-1002"}`,
			"回补库存（sku=STD-1002）", ""},
		{"数组参数退 R2", "procurement.CreatePurchaseRequest", `{"sku":"STD-1002","qty":9,"lines":[{"sku":"STD-1002","qty":9}]}`,
			"创建采购申请（qty=9, sku=STD-1002）", ""},
		{"未闭合退 R2", "inventory.Broken", `{"sku":"X"}`,
			"坏模板（sku=X）", ""},
		{"标题与模板都缺", "inventory.Unnamed", `{"a":1}`,
			"未命名动作（参数见详情）", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := Plan{PlanID: "pln_1", Steps: []PlanStep{{StepID: "s1", Action: c.action, Args: json.RawMessage(c.args)}}}
			labels, details := renderSteps(plan, meta)
			if len(labels) != 1 {
				t.Fatalf("期望 1 条 label，得到 %v", labels)
			}
			if labels[0] != c.wantLabel {
				t.Fatalf("label = %q，期望 %q", labels[0], c.wantLabel)
			}
			if strings.Contains(labels[0], c.action) {
				t.Fatalf("R2 不许泄露 action id：%q", labels[0])
			}
			if !strings.Contains(details[0], c.action) {
				t.Fatalf("详情必须带 action id：%q", details[0])
			}
			if c.wantDetail != "" && details[0] != c.wantDetail {
				t.Fatalf("detail = %q，期望 %q", details[0], c.wantDetail)
			}
		})
	}
}

// R4：单条白话上限 120 rune，超出截断并带省略号。
func TestRenderStepsTruncates(t *testing.T) {
	long := strings.Repeat("很", 200)
	meta := map[string]ActionMeta{"inventory.DeductStock": {Title: "扣减库存", SummaryTemplate: "{sku}"}}
	plan := Plan{PlanID: "pln_1", Steps: []PlanStep{{
		StepID: "s1", Action: "inventory.DeductStock",
		Args: json.RawMessage(`{"sku":"` + long + `"}`),
	}}}
	labels, _ := renderSteps(plan, meta)
	if n := len([]rune(labels[0])); n > 120 {
		t.Fatalf("超过 120 rune：%d", n)
	}
	if !strings.HasSuffix(labels[0], "…") {
		t.Fatalf("截断要带省略号：%q", labels[0])
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/agent/ -run 'TestRenderSteps' -count=1`
Expected: 编译失败——`undefined: renderSteps`

- [ ] **Step 3: 实现渲染器**

删掉 `suspend.go` 里的 `renderLabels`，换成：

```go
// renderSteps 逐 step 出一句白话（labels）与一条技术原文（details）。
//
// 规则见 P3 spec §4：模板是主路径，任何一步失败都退 R2（标题 + 参数），
// 绝不输出半个句子；白话里不出现 action id（那是 details 的事）；数字只抄 args。
func renderSteps(plan Plan, meta map[string]ActionMeta) (labels, details []string) {
	labels = make([]string, 0, len(plan.Steps))
	details = make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		m := meta[step.Action]
		// truncateRunes 会在截断结果后**再**补一个省略号，所以这里传 limit-1，
		// 保证最终长度（含「…」）不超过 120。
		labels = append(labels, truncateRunes(renderStepLabel(step, m), labelMaxRunes-1))
		details = append(details, stepDetail(step))
	}
	return labels, details
}

// labelMaxRunes 是 R4 的上限：与 Intent 帧 badge 的 120 对齐。
const labelMaxRunes = 120

func renderStepLabel(step PlanStep, m ActionMeta) string {
	args, ok := parseArgsObject(step.Args)
	if ok && strings.TrimSpace(m.SummaryTemplate) != "" {
		if out, done := fillTemplate(m.SummaryTemplate, args); done {
			return out
		}
	}
	return fallbackLabel(m.Title, args, ok)
}

// fillTemplate 只在**所有**占位符都能取到标量值时才返回 done=true：
// 少一个就整句作废，避免「缺 件 STD-1002」这种半个句子。
func fillTemplate(tpl string, args map[string]any) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(tpl); i++ {
		if tpl[i] != '{' {
			b.WriteByte(tpl[i])
			continue
		}
		rel := strings.IndexByte(tpl[i+1:], '}')
		if rel < 0 {
			return "", false
		}
		name := tpl[i+1 : i+1+rel]
		raw, ok := args[name]
		if !ok {
			return "", false
		}
		s, ok := scalarText(raw)
		if !ok {
			return "", false
		}
		b.WriteString(s)
		i += rel + 1
	}
	return b.String(), true
}

// scalarText 只接受字符串/数字/布尔：数组与对象渲染不出人话（R1），一律判失败。
func scalarText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case json.Number:
		return x.String(), true
	default:
		return "", false
	}
}

// fallbackLabel 是 R2：标题 + 关键参数；标题也缺时给一句可读的占位话，不泄露 action id。
func fallbackLabel(title string, args map[string]any, hasArgs bool) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "未命名动作（参数见详情）"
	}
	if !hasArgs || len(args) == 0 {
		return title
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if s, ok := scalarText(args[k]); ok {
			parts = append(parts, k+"="+s)
		}
	}
	if len(parts) == 0 {
		return title
	}
	return title + "（" + strings.Join(parts, ", ") + "）"
}

// stepDetail 是折叠区用的技术原文：action id + args（截断，避免整卡被一份 args 淹没）。
func stepDetail(step PlanStep) string {
	args := strings.TrimSpace(string(step.Args))
	if args == "" || args == "{}" || args == "null" {
		return step.Action
	}
	return fmt.Sprintf("%s（%s）", step.Action, truncateRunes(args, 120))
}

// parseArgsObject 解出 args 对象；空、非法、非对象一律返回 ok=false（走 R2）。
func parseArgsObject(raw json.RawMessage) (map[string]any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}
```

需要的 import（加进 `suspend.go`）：`bytes`、`math`、`sort`、`strconv`（`fmt`、`json`、`strings` 已有）。

- [ ] **Step 4: 加 `Pending.Details`**

`internal/agent/state.go` 的 `Pending` 里、`Labels` 之后：

```go
	// Details 与 Labels 一一对应，是折叠区用的技术原文（action id + args）。
	// 它是**展示数据**，不参与任何判定：审批、恢复、审计都不读它。
	Details []string `json:"details,omitempty"`
```

- [ ] **Step 5: 跑用例确认通过**

Run: `go test ./internal/agent/ -run 'TestRenderSteps' -count=1 -v`
Expected: PASS（6 个子用例 + 截断用例）

- [ ] **Step 6: Commit**

```bash
git add internal/agent/suspend.go internal/agent/state.go internal/agent/suspend_test.go
git commit -m "feat(agent): 白话渲染器（模板优先、失败退 R2、不泄露 action id）"
```

---

## Task 6: `buildPending` 接入渲染器

**Files:**
- Modify: `internal/agent/run.go`（`buildPending`）
- Test: `internal/agent/run_test.go`

- [ ] **Step 1: 写用例**

追加到 `internal/agent/run_test.go`：

```go
// 挂起时渲染一次并落盘：labels 是白话、details 是技术原文、summary 是白话拼接。
//
// 注意 runner.modules 是 map[string]string（模块 → 基地址），**不携带**模板；
// 发现结果（[]ModuleDigest）只在 run() 里以局部变量 digest 存在，所以元信息
// 是以参数传进 buildPending 的，不挂在 runner 上。
func TestBuildPendingRendersPlainLanguage(t *testing.T) {
	meta := map[string]ActionMeta{
		"inventory.DeductStock": {Title: "扣减库存", SummaryTemplate: "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）"},
	}
	r := &runner{
		deps:  Deps{OnBehalfOf: "user:alice"},
		event: DomainEvent{Subject: "inventory:SKU/STD-1002"},
	}
	plan := Plan{PlanID: "pln_1", Trigger: PlanTrigger{EventID: "evt_1"}, Steps: []PlanStep{{
		StepID: "s1", Action: "inventory.DeductStock", Kind: "write",
		Args: json.RawMessage(`{"sku":"STD-1002","qty":3,"referenceNo":"Q-2026-0417"}`),
	}}}
	got, err := r.buildPending(plan, policyEvaluation{Approvers: []string{"role:warehouse_manager"}}, meta)
	if err != nil {
		t.Fatalf("buildPending 失败：%v", err)
	}
	if want := "扣减 STD-1002 库存 3 件（单据 Q-2026-0417）"; got.Labels[0] != want {
		t.Fatalf("label = %q，期望 %q", got.Labels[0], want)
	}
	if !strings.Contains(got.Details[0], "inventory.DeductStock") {
		t.Fatalf("details 必须带 action id：%q", got.Details[0])
	}
	if got.Summary != got.Labels[0] {
		t.Fatalf("单步计划的 summary 应等于该步白话：%q", got.Summary)
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/agent/ -run TestBuildPendingRendersPlainLanguage -count=1`
Expected: FAIL，label 仍是 `inventory.DeductStock（{...}）`

- [ ] **Step 3: 改 `buildPending`**

改函数签名（元信息走参数，不挂到 runner 上：`runner.modules` 是 `map[string]string`，装不下模板）：

```go
func (r *runner) buildPending(plan Plan, policy policyEvaluation, meta map[string]ActionMeta) (Pending, error) {
```

函数体内把原来的渲染换成：

```go
	labels, details := renderSteps(plan, meta)
```

调用点在 `internal/agent/run.go:363`（`run()` 里，本轮发现结果 `digest` 就在作用域内）：

```go
		pending, err := r.buildPending(plan, policy, actorMeta(digest))
```

并把返回体加上 `Details: details`（`Labels` 那一行下面）：

```go
		Labels:         labels,
		Details:        details,
		Summary:        strings.Join(labels, "；"),
```

同文件加一个把 `[]ModuleDigest` 压平成 `map[string]ActionMeta` 的小函数：

```go
// actorMeta 把本轮发现结果压平成 action id → 元信息，供渲染器按 step 查。
// 模块间 action id 全局唯一（module.action 的格式），所以不需要再带模块维度。
func actorMeta(modules []ModuleDigest) map[string]ActionMeta {
	out := make(map[string]ActionMeta)
	for _, m := range modules {
		for id, meta := range m.ActionMeta {
			out[id] = meta
		}
	}
	return out
}
```

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/agent/ -run 'TestBuildPendingRendersPlainLanguage' -count=1 -v && go test ./internal/agent/ -count=1`
Expected: PASS；agent 包全绿（旧用例里断言兜底串的地方若失败，改成断言白话——那是本任务要改的期望值）

- [ ] **Step 5: Commit**

```bash
git add internal/agent/run.go internal/agent/run_test.go
git commit -m "feat(agent): 挂起时按模板渲染白话并落 details"
```

---

## Task 7: 公告板透传 `detail`

**Files:**
- Modify: `internal/agent/board.go`（`boardPayloadAt`）
- Test: `internal/agent/board_test.go`
- Test: `internal/controlplane/pending_test.go`（只加用例，不改结构体）

- [ ] **Step 1: 写 agent 侧用例**

追加到 `internal/agent/board_test.go`：

```go
// 公告板每个 step 一条 item，同时带白话（label）与技术原文（detail）。
func TestBoardPayloadCarriesDetail(t *testing.T) {
	p := samplePending()
	p.Details = []string{"inventory.DeductStock（{\"sku\":\"STD-1002\"}）"}

	got := boardPayload(p, StateAwaitingApproval)
	items, ok := got["items"].([]map[string]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items 形状不对：%#v", got["items"])
	}
	if items[0]["label"] != p.Labels[0] {
		t.Fatalf("label = %v，期望 %q", items[0]["label"], p.Labels[0])
	}
	if items[0]["detail"] != p.Details[0] {
		t.Fatalf("detail = %v，期望 %q", items[0]["detail"], p.Details[0])
	}
}

// 旧 pending 没有 details：不许推空 detail，也不许把 label 顶上去。
func TestBoardPayloadOmitsMissingDetail(t *testing.T) {
	p := samplePending()
	items := boardPayload(p, StateAwaitingApproval)["items"].([]map[string]any)
	if _, has := items[0]["detail"]; has {
		t.Fatalf("没有 details 时不该出现 detail 键：%#v", items[0])
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/agent/ -run 'TestBoardPayload' -count=1`
Expected: FAIL，`detail 缺失`

- [ ] **Step 3: 改 `boardPayloadAt`**

`internal/agent/board.go:53-56` 换成：

```go
	items := make([]map[string]any, 0, len(p.Labels))
	for i, l := range p.Labels {
		item := map[string]any{"label": l}
		if i < len(p.Details) && strings.TrimSpace(p.Details[i]) != "" {
			item["detail"] = p.Details[i]
		}
		items = append(items, item)
	}
```

- [ ] **Step 4: 写宿主侧用例（透传即可）**

追加到 `internal/controlplane/pending_test.go`：

```go
// host 不解释 items 的内容：带 detail 进来就要原样带回去（页面靠它渲染折叠区）。
// 夹具沿用本文件已有的 SignToken / postJSONAuth / getJSONAuth 与 roleAgent 常量。
func TestPendingBoardKeepsItemDetail(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	agentTok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", []string{roleAgent}, time.Now().UTC(), time.Hour)
	body := `{"planId":"pln_1","summary":"扣减 STD-1002 库存 3 件","state":"awaiting_approval",
	          "requiredRoles":["role:warehouse_manager"],"expiresAt":"2099-01-01T00:00:00Z",
	          "items":[{"label":"扣减 STD-1002 库存 3 件","detail":"inventory.DeductStock（{\"qty\":3}）"}]}`
	if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", agentTok, body); status != http.StatusOK {
		t.Fatalf("推公告板失败 %d：%s", status, resp)
	}
	code, raw := getJSONAuth(t, ts.URL+"/tenon/pending", agentTok)
	if code != http.StatusOK || !strings.Contains(raw, `"detail":"inventory.DeductStock`) {
		t.Fatalf("detail 没有被透传：%d %s", code, raw)
	}
}
```

（`pending_test.go` 的 import 里补 `"strings"`；其余四个包已用过。）

- [ ] **Step 5: 跑用例确认通过**

Run: `go test ./internal/agent/ ./internal/controlplane/ -run 'TestBoardPayload|TestPendingBoardKeepsItemDetail' -count=1 -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/agent/board.go internal/agent/board_test.go internal/controlplane/pending_test.go
git commit -m "feat(board): 公告板条目带 detail（折叠区的技术原文）"
```

---

## Task 8: 页面折叠技术信息

**Files:**
- Modify: `ui/app.js`（`renderCard`）

- [ ] **Step 1: 改 `renderCard`**

`ui/app.js:464-550` 的 `renderCard` 按下面改：

1. 删掉 `meta`（`planId` / `requestId` / 发起人）与 `labels` 直接追加到卡片的那几段（488-501 行）；
2. 卡片头只留 `summary` + 状态徽章（471-486 行保持不动）；
3. 在「需要角色」与「到期」两行之前插入一个折叠块：

```js
  // 技术详情：R5 要求 action id / planId / 参数原文只在这个折叠区里出现。
  // 用原生 <details>，不加 JS 状态、不加构建链。
  const tech = document.createElement('details');
  tech.className = 'tech';
  const techSum = document.createElement('summary');
  techSum.textContent = '详情 ▾';
  tech.append(techSum);

  const techList = document.createElement('ul');
  techList.className = 'tech-list';
  if (item.planId) techList.append(h('li', {}, h('span', { text: 'planId ' }), h('code', { text: item.planId })));
  if (item.requestId) techList.append(h('li', {}, h('span', { text: 'requestId ' }), h('code', { text: item.requestId })));
  if (item.by) techList.append(h('li', {}, h('span', { text: '发起人 ' }), h('code', { text: item.by })));
  labels.forEach((label, i) => {
    const entry = item.items && item.items[i] ? item.items[i] : null;
    const detail = entry && typeof entry.detail === 'string' ? entry.detail : label;
    techList.append(h('li', {}, h('code', { text: detail })));
  });
  if (!techList.childNodes.length) techList.append(h('li', { class: 'muted', text: '（没有技术明细）' }));
  tech.append(techList);
  card.append(tech);
```

4. `labels` 仍在函数开头取一次（`const labels = itemLabels(item);`），但不再渲染在卡面。

- [ ] **Step 2: 加样式**

`ui/style.css` 追加：

```css
/* 技术详情默认收起：审批人先看到人话，需要时才展开。 */
.tech { margin: 8px 0; }
.tech > summary { cursor: pointer; color: #555; font-size: 13px; }
.tech-list { margin: 6px 0 0; padding-left: 18px; font-size: 12px; color: #666; }
.tech-list code { word-break: break-all; }
```

- [ ] **Step 3: 起 host 人工验收**

```bash
go run ./cmd/host  # 环境变量与 README 的「最小链路」一致，另加 TENON_UI_DIR=ui
```

浏览器打开 `http://127.0.0.1:8080/`，逐项确认：

1. 卡面只看到一句白话 + 状态徽章；
2. 卡面**看不到** `planId` / `DeductStock` / 参数 JSON；
3. 点「详情 ▾」能看到 `planId` 与每步的 `action（args）`；
4. 非审批角色登录后，仍然看不到「批准」按钮。

- [ ] **Step 4: Commit**

```bash
git add ui/app.js ui/style.css
git commit -m "feat(ui): 技术信息进折叠区，卡面只留白话"
```

---

## Task 9: 文档与全量回归

**Files:**
- Modify: `docs/interfaces.md` §4.7
- Test: 全仓

- [ ] **Step 1: 补 `docs/interfaces.md` §4.7**

在公告板条目字段表里补一行：

```markdown
| `items[]` | `[{label, detail?}]` | 逐 step 明细。`label` 是给人看的白话（由模块 `summaryTemplate` 渲染），`detail` 是技术原文（action id + args），只供页面折叠区展示；宿主不解释、不校验这两个键 |
```

- [ ] **Step 2: 全量回归**

```bash
gofmt -l .            # 期望无输出
go vet ./...          # 期望 exit 0
go test ./... -count=1
pnpm check
```

Expected: 四项全绿

- [ ] **Step 3: 取证据（证明卡面真的没有技术标识）**

```bash
# 起 host + 模块 + agent（stub 模型即可），投一句话触发挂起，然后看 pending 文件
Get-Content out/agent-state/pending/*.json | Select-String 'summary|labels|details'
```

期望：`summary` / `labels` 是中文白话（含「扣减 … 库存 3 件」这类句子）；`details` 里才有 `inventory.DeductStock`。把这条输出贴进 PR 描述。

- [ ] **Step 4: Commit**

```bash
git add docs/interfaces.md
git commit -m "docs(interfaces): 公告板条目补 detail 字段（折叠区技术原文）"
```

---

## 自检对照（spec → 任务）

| spec 要求 | 落在 |
|---|---|
| §2 两处 schema | Task 1 |
| §3 模块声明与透出 | Task 2 |
| §4 模板与三个模块 | Task 3 |
| §3 发现携带 ActionMeta | Task 4 |
| §4 渲染规则 R1–R5 | Task 5 |
| §5 渲染只发生一次 | Task 6 |
| §3 公告板 `{label,detail}` | Task 7 |
| §3 UI 折叠 | Task 8 |
| §7 文档 §4.7 | Task 9 |
| §8 验收线（数字来自 args、无技术标识） | Task 5（渲染层）+ Task 9 Step 3（证据） |
