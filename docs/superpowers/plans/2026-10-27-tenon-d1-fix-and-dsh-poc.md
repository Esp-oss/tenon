# D1 修复 + 目录清理 + DSH 前置闸门 POC 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 D1 给出实质结论（pass/fail）、让真模型下的对话入口可用，并按 ADR-0001 删掉占位包、用实验定下 DSH 侧前置闸门与审批的形状。

**Architecture:** 三块彼此独立：**A** 修规划输入契约（`requiredArgs` 进摘要线与 prompt）+ 补策略覆盖 + 收紧 D1 场景种子；**B** 按 ADR 搬迁并删除 5 个无 `src` 的占位包；**C** 用 DSH 的 `PreToolUse` 外部钩子协议做两个半天 POC（闸门 fail-open/closed、审批跨重启）。A 不依赖 DSH，C 不改产品代码。

**Tech Stack:** Go 1.26（`internal/module`、`internal/agent`、`internal/controlplane/policy`）、JSON Schema（`internal/spec` 校验器）、PowerShell 5.1（脚本，必须带 UTF-8 BOM）、DSH `0.2.0-rc.2` 钩子协议（`dsh-hook-protocol`）。

---

## 执行顺序（建议）

1. **A（D1 修复）** —— 收益最大、与 DSH 零耦合；一天内可拿到第 4 次真判定。
2. **B（清理）** —— 半小时，顺手做完，防止评审者误判 DSH 路径在推进。
3. **C（POC）** —— 需要先摸 DSH 钩子/审批的真实语义，风险最高，放最后；结果只改 ADR 的 §5 附录，不改方向。

---

## 文件结构

| 文件 | 职责 | 动作 |
|---|---|---|
| `spec/0.1/schemas/module-manifest.schema.json` | 摘要线契约 | 修改：`requiredArgs` |
| `spec/0.1/schemas/action.schema.json` | 完整 Action 契约 | 修改：`requiredArgs` |
| `spec/0.1/CHANGELOG.md` | 0.2.0 提案段 | 修改：补条目 |
| `internal/module/module.go` | `Action`/`actionSummary`/`Validate` | 修改：字段 + `⊆ required` 断言 |
| `internal/agent/planner.go` | `ActionMeta`、`userPrompt` | 修改：`RequiredArgs` + 渲染标题与必填 |
| `internal/agent/run.go` | 发现解析、策略拒绝的可观测 | 修改 |
| `config/policies.dev.json` | dev 策略 | 修改：4 条规则（2 新增 / 1 新增 deny / 1 改造去 origin 限制） |
| `scripts/d1-e2e.ps1` | D1 编排 | 修改：库存种子改 0、断言排除 inconclusive |
| `scripts/verify-ai.ps1` | 真模型对话验收 | 新建 |
| `docs/designs/dsh-adapter-boundaries/*.md` | 5 个占位包 README 的落点 | 新建（git mv） |
| `docs/designs/poc-pre-tool-gate.md`、`docs/designs/poc-approval-restart.md` | POC 结果 | 新建 |

---

# Part A：D1 修复

## Task A1: 两处 schema 接受 `requiredArgs`

**Files:**
- Modify: `spec/0.1/schemas/module-manifest.schema.json`（`$defs/ActionSummary.properties`）
- Modify: `spec/0.1/schemas/action.schema.json`（根 `properties`）
- Modify: `spec/0.1/CHANGELOG.md`（0.2.0 段）
- Test: `internal/spec/spec_test.go`

- [ ] **Step 1: 写失败用例**

追加到 `internal/spec/spec_test.go`：

```go
// requiredArgs 是「调用这个动作至少要给哪些字段」：摘要线（agent 唯一读的）与完整 Action
// 两处都要认，且必须是字符串数组、不能重复。
func TestRequiredArgsInBothShapes(t *testing.T) {
	s := mustLoad(t)

	manifest := `{
	  "tenon":"0.1",
	  "module":{"id":"inventory","version":"1.4.0","title":"库存"},
	  "actions":[{"id":"inventory.DeductStock","version":"1.0.0","title":"扣减",
	              "kind":"write","risk":{"level":"medium","reversible":true},
	              "dryRun":{"supported":true},
	              "requiredArgs":["sku","qty","referenceNo"]}],
	  "events":[],"transports":["http"],"capabilities":["dry-run"]
	}`
	if err := s.Validate("module-manifest.schema.json", []byte(manifest)); err != nil {
		t.Fatalf("摘要线应接受 requiredArgs：%v", err)
	}

	duplicated := strings.Replace(manifest,
		`"requiredArgs":["sku","qty","referenceNo"]`,
		`"requiredArgs":["sku","sku"]`, 1)
	if err := s.Validate("module-manifest.schema.json", []byte(duplicated)); err == nil {
		t.Fatal("requiredArgs 重复元素必须被拒（uniqueItems）")
	}

	nonString := strings.Replace(manifest,
		`"requiredArgs":["sku","qty","referenceNo"]`,
		`"requiredArgs":[1,2]`, 1)
	if err := s.Validate("module-manifest.schema.json", []byte(nonString)); err == nil {
		t.Fatal("requiredArgs 必须是字符串数组")
	}

	action := strings.Replace(validWriteAction, `"kind": "write",`,
		`"kind": "write", "requiredArgs": ["sku","qty"],`, 1)
	if err := s.Validate("action.schema.json", []byte(action)); err != nil {
		t.Fatalf("完整 Action 应接受 requiredArgs：%v", err)
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/spec/ -run TestRequiredArgsInBothShapes -count=1`
Expected: FAIL，错误含 `additional properties 'requiredArgs' not allowed`

- [ ] **Step 3: 改两处 schema**

`module-manifest.schema.json` 的 `$defs/ActionSummary/properties/title` 之后插入（上一项补逗号）：

```json
        "requiredArgs": {
          "description": "调用该动作至少要给的字段名（取自本动作 inputSchema.required 的子集）。摘要线只有字段名、没有类型：类型由模块侧入参校验兜住，这样才是「摘要先看、完整按需」（D15）。",
          "type": "array",
          "uniqueItems": true,
          "maxItems": 32,
          "items": {
            "type": "string",
            "pattern": "^[A-Za-z_][A-Za-z0-9_]*$"
          }
        },
```

`action.schema.json` 的根 `properties/title` 之后插入同一段 JSON。

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/spec/ -run TestRequiredArgsInBothShapes -count=1 -v && pnpm check`
Expected: PASS；spec 校验 11 项 0 失败

- [ ] **Step 5: 补 CHANGELOG**

`spec/0.1/CHANGELOG.md` 的 `0.2.0` 段 `Added` 列表末尾追加：

```markdown
- `module-manifest.schema.json` 与 `action.schema.json` 的 action 增可选 `requiredArgs`（必填字段名数组，取自 `inputSchema.required`）。它只给**字段名**、不给类型：agent 的规划 prompt 需要它才能写出合法 args（2026-10-27 真模型实测：漏 `referenceNo` 导致预演 400）。
```

- [ ] **Step 6: Commit**

```bash
git add spec/0.1/schemas/module-manifest.schema.json spec/0.1/schemas/action.schema.json spec/0.1/CHANGELOG.md internal/spec/spec_test.go
git commit -m "feat(spec): action 与摘要线接受 requiredArgs（规划期必填字段契约）"
```

---

## Task A2: `module.Action.RequiredArgs` + 加载期断言

**Files:**
- Modify: `internal/module/module.go`
- Test: `internal/module/module_test.go`

- [ ] **Step 1: 写失败用例**

追加到 `internal/module/module_test.go`：

```go
// 必填字段是模块声明的一部分：摘要线必须带出，空则不出现。
func TestManifestCarriesRequiredArgs(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	f.module.Actions[1].RequiredArgs = []string{"name"}
	srv := serve(t, f.module)

	_, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil, nil)
	if !bytes.Contains(raw, []byte(`"requiredArgs":["name"]`)) {
		t.Fatalf("摘要线没带 requiredArgs：%s", raw)
	}

	empty := newFixture()
	srv2 := serve(t, empty.module)
	_, raw2 := doJSON(t, http.MethodGet, srv2.URL+"/.well-known/tenon", nil, nil)
	if bytes.Contains(raw2, []byte("requiredArgs")) {
		t.Fatalf("空声明不该出现：%s", raw2)
	}
}

// 声明与契约不一致是装配错误：requiredArgs 必须在 inputSchema.required 里。
func TestValidateRejectsRequiredArgsOutsideSchema(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	f.module.Actions[1].RequiredArgs = []string{"nope"}
	if err := f.module.Validate(); err == nil {
		t.Fatal("requiredArgs 不在 inputSchema.required 里时 Validate 必须失败")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("错误里要点名字段，得到：%v", err)
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/module/ -run 'TestManifestCarriesRequiredArgs|TestValidateRejectsRequiredArgs' -count=1`
Expected: 编译失败 `SummaryTemplate` 同级没有 `RequiredArgs`

- [ ] **Step 3: 加字段、透出、断言**

`internal/module/module.go` 的 `Action` 结构，`SummaryTemplate` 之后加：

```go
	// RequiredArgs 是「调起来至少要给哪些字段」，取自 InputSchema.required。
	// 只给字段名不给类型：类型由模块侧入参校验兜住，agent 侧只需要知道必须写什么。
	RequiredArgs []string `json:"requiredArgs,omitempty"`
```

`actionSummary` 加：

```go
	RequiredArgs []string `json:"requiredArgs,omitempty"`
```

构造处（`out.Actions = append(out.Actions, actionSummary{...})`）补：

```go
			RequiredArgs: a.RequiredArgs,
```

在 `build()` 里，取到 `required` 之后（`required, err := requiredOf(a.InputSchema)` 那段之后）加：

```go
		if err := checkRequiredArgs(a, required); err != nil {
			return err
		}
```

同文件加：

```go
// checkRequiredArgs 把「声明与契约不一致」挡在启动期：requiredArgs 必须是
// inputSchema.required 的子集，否则模块装上一个永远规划不出合法 args 的动作。
func checkRequiredArgs(a Action, required []string) error {
	for _, name := range a.RequiredArgs {
		if !slices.Contains(required, name) {
			return fmt.Errorf("action %q 的 requiredArgs 含 %q，但它不在 inputSchema.required 里（声明与契约不一致）", a.ID, name)
		}
	}
	return nil
}
```

import 块加 `"slices"`。

- [ ] **Step 4: 跑用例确认通过**

Run: `gofmt -l internal/module/ && go test ./internal/module/ -run 'TestManifestCarriesRequiredArgs|TestValidateRejectsRequiredArgs' -count=1 -v && go test ./internal/module/ -count=1`
Expected: PASS；模块包全绿

- [ ] **Step 5: Commit**

```bash
git add internal/module/module.go internal/module/module_test.go
git commit -m "feat(module): Action 声明 requiredArgs 并做加载期校验（⊆ inputSchema.required）"
```

---

## Task A3: 三个模块填 `RequiredArgs`

**Files:**
- Modify: `cmd/inventory/main.go`、`cmd/procurement/main.go`、`cmd/quote/main.go`
- Test: `cmd/inventory/main_test.go`、`cmd/procurement/main_test.go`、`cmd/quote/main_test.go`

- [ ] **Step 1: 扩展现有的模板断言助手**

三个 `cmd/*/main_test.go` 里的 `assertTemplateArgs` 函数末尾追加（三份文件都要改，内容相同）：

```go
	// 必填字段同样必须来自 inputSchema.required：这是规划期写 args 的唯一依据。
	for _, name := range a.RequiredArgs {
		required := false
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(a.InputSchema, &schema); err != nil {
			t.Fatalf("%s 的 inputSchema 读不出：%v", a.ID, err)
		}
		for _, r := range schema.Required {
			if r == name {
				required = true
				break
			}
		}
		if !required {
			t.Fatalf("%s 的 requiredArgs 含 %q，但它不在 inputSchema.required 里", a.ID, name)
		}
	}
	if len(a.RequiredArgs) == 0 {
		t.Fatalf("%s 没声明 requiredArgs（规划期就无法保证 args 完整）", a.ID)
	}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./cmd/inventory/ ./cmd/procurement/ ./cmd/quote/ -count=1`
Expected: FAIL，`没声明 requiredArgs`

- [ ] **Step 3: 给五个动作填必填字段**

`cmd/inventory/main.go`：

```go
		RequiredArgs: []string{"sku"},
```
（`getStockAction`）
```go
		RequiredArgs: []string{"sku", "qty", "referenceNo"},
```
（`deductStockAction` 与 `releaseStockAction` 各一处）

`cmd/procurement/main.go`（`createPurchaseRequestAction`）：

```go
		RequiredArgs: []string{"sku", "qty", "referenceNo"},
```

`cmd/quote/main.go`（`createStandardPartQuoteAction`，注意 `lines` 是数组但仍是必填字段名）：

```go
		RequiredArgs: []string{"quoteNo", "lines"},
```

- [ ] **Step 4: 跑用例确认通过**

Run: `gofmt -l cmd/ && go test ./cmd/inventory/ ./cmd/procurement/ ./cmd/quote/ -count=1 -v`
Expected: PASS（三个包各 1 个用例）

- [ ] **Step 5: Commit**

```bash
git add cmd/inventory cmd/procurement cmd/quote
git commit -m "feat(modules): 五个动作声明 requiredArgs，并用测试锁住与 inputSchema 一致"
```

---

## Task A4: 能力发现解析 `requiredArgs`，prompt 渲染标题与必填字段

**Files:**
- Modify: `internal/agent/planner.go`（`ActionMeta`、`userPrompt`）
- Modify: `internal/agent/run.go`（`manifestActions`）
- Test: `internal/agent/planner_test.go`、`internal/agent/run_test.go`

- [ ] **Step 1: 写失败用例**

追加到 `internal/agent/planner_test.go`：

```go
// 规划 prompt 必须给出「这个动作要什么字段」：2026-10-27 真模型就漏了 referenceNo，
// 预演直接 400。标题也一并给——只给 id 时模型无法判断语义。
func TestPromptRendersActionTitleAndRequiredArgs(t *testing.T) {
	in := PlanInput{
		Event: DomainEvent{EventID: "evt_1", Type: "quote.completed", CorrelationID: "cor:quote/Q-1"},
		Modules: []ModuleDigest{{
			ID:      "inventory",
			Actions: []string{"inventory.DeductStock"},
			ActionMeta: map[string]ActionMeta{
				"inventory.DeductStock": {Title: "扣减库存", RequiredArgs: []string{"sku", "qty", "referenceNo"}},
			},
		}},
	}
	got := userPrompt(in)
	want := "    - inventory.DeductStock（扣减库存；必填：qty, referenceNo, sku）"
	if !strings.Contains(got, want) {
		t.Fatalf("prompt 缺动作元信息，期望包含 %q，实际：\n%s", want, got)
	}

	// 老模块只有 id（没有 ActionMeta）：仍然只渲染 id，不得出现空的括号。
	bare := userPrompt(PlanInput{
		Event:   DomainEvent{EventID: "evt_1", Type: "quote.completed", CorrelationID: "cor:quote/Q-1"},
		Modules: []ModuleDigest{{ID: "inventory", Actions: []string{"inventory.GetStock"}}},
	})
	if !strings.Contains(bare, "    - inventory.GetStock\n") {
		t.Fatalf("无元信息时应只渲染 id：\n%s", bare)
	}
}
```

追加到 `internal/agent/run_test.go`：

```go
// 发现结果必须带上 requiredArgs：它是 prompt 渲染必填字段的唯一来源。
func TestDiscoverCarriesRequiredArgs(t *testing.T) {
	mod := newFakeModule(t, "inventory", []string{"inventory.DeductStock"}, new([]string))
	mod.Meta = map[string]map[string]any{
		"inventory.DeductStock": {"requiredArgs": []string{"sku", "qty"}},
	}
	r := newRunnerForDiscover(t, mod.Server.URL)

	got, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("能力发现失败：%v", err)
	}
	meta := got[0].ActionMeta["inventory.DeductStock"]
	if len(meta.RequiredArgs) != 2 || meta.RequiredArgs[0] != "sku" {
		t.Fatalf("requiredArgs 没带回来：%+v", meta)
	}
}
```

同文件给 `fakeModule` 加字段与渲染（紧跟已有的 `Templates`）：

```go
	// Meta 是 action id → manifest 里的额外键（当前用于 requiredArgs）。
	Meta map[string]map[string]any
```

`serve` 里构造 `item` 之后加：

```go
			for k, v := range m.Meta[id] {
				item[k] = v
			}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/agent/ -run 'TestPromptRendersActionTitleAndRequiredArgs|TestDiscoverCarriesRequiredArgs' -count=1`
Expected: 编译失败——`ActionMeta` 没有 `RequiredArgs`、`fakeModule` 没有 `Meta`

- [ ] **Step 3: 实现**

`internal/agent/planner.go` 的 `ActionMeta`：

```go
// ActionMeta 是渲染白话与规划 prompt 所需的最小信息：标题、白话模板、必填字段名。
// 它随能力发现一起回来，不进 `action` 清单之外的任何 API。
type ActionMeta struct {
	Title           string
	SummaryTemplate string
	RequiredArgs    []string
}
```

`userPrompt` 的动作清单循环改成：

```go
		for _, a := range sortedActions(m.Actions) {
			fmt.Fprintf(&b, "    - %s%s\n", a, actionMetaSuffix(m.ActionMeta[a]))
		}
```

同文件加：

```go
// actionMetaSuffix 渲染「（标题；必填：a, b）」。字段名按字典序输出：prompt 必须
// 逐字可复现（temperature=0 的可重复性依赖它），模块声明顺序不能影响字节。
// 两者都缺时返回空串——老模块的 prompt 保持原样，不多一对空括号。
func actionMetaSuffix(meta ActionMeta) string {
	var parts []string
	if title := strings.TrimSpace(meta.Title); title != "" {
		parts = append(parts, title)
	}
	if len(meta.RequiredArgs) > 0 {
		names := append([]string(nil), meta.RequiredArgs...)
		sort.Strings(names)
		parts = append(parts, "必填："+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "；") + "）"
}
```

`internal/agent/run.go` 的 `manifestActions`：结构体加字段并回填。

```go
	var manifest struct {
		Actions []struct {
			ID              string   `json:"id"`
			Title           string   `json:"title"`
			SummaryTemplate string   `json:"summaryTemplate"`
			RequiredArgs    []string `json:"requiredArgs"`
		} `json:"actions"`
	}
```

```go
		meta[id] = ActionMeta{Title: a.Title, SummaryTemplate: a.SummaryTemplate, RequiredArgs: a.RequiredArgs}
```

- [ ] **Step 4: 跑用例确认通过**

Run: `gofmt -l internal/agent/ && go test ./internal/agent/ -run 'TestPromptRendersActionTitleAndRequiredArgs|TestDiscoverCarriesRequiredArgs' -count=1 -v && go test ./internal/agent/ -count=1`
Expected: PASS；agent 包全绿

- [ ] **Step 5: Commit**

```bash
git add internal/agent/planner.go internal/agent/run.go internal/agent/planner_test.go internal/agent/run_test.go
git commit -m "feat(agent): prompt 渲染动作标题与必填字段（requiredArgs 走摘要线）"
```

---

## Task A5: 策略——**不改**平台配置（记录决定）

> **执行期更正（2026-10-27）：本任务初版作废。** 初版要改三条平台规则，核实后是误判：
>
> 1. **event 来源的采购本来就有覆盖**——业务级规则 `skills/quote-to-fulfillment/approval.json` 按 qty 覆盖（≤5 auto / >5 approve），`TestDevBusinessRulesTakeEffect` 断言「事件驱动大额要批 → decidedBy=**biz-purchase-approve-large**」。所以 **D1 的 A 轮不缺策略覆盖**。
> 2. **`plat-request-needs-human` 不是采购规则，是「对话发起一律人批」的守卫**（priority 1000）。同一用例断言 request 来源 qty=3 与 qty=12 的决胜规则都必须是它；删掉它的 origin 限制 = **放宽安全语义**。
> 3. **小量扣减（qty ≤ 10）保持 deny** 是产品所有者 2026-10-27 的决定：有意为之的 fail-closed，不改成 auto。
> 4. quote 触发器动作保持「未登记即 deny」，不加显式规则（YAGNI：可读性由 A6 的 `detail` 解决）。
>
> **实际执行（替代初版）**：
>
> - [x] Step 1: 改写 spec 的 §4，把三条规则改成「一条不改」并附证据——**已完成**
> - [ ] Step 2: README 的「还没做的」补一条已知行为（文本见下）
> - [ ] Step 3: commit `docs: 更正策略覆盖的误判（平台策略不改；小量扣减转人工是有意为之）`
>
> Step 2 要追加到 README「还没做的」的文本：
>
> ```markdown
> **对话发起的小量扣减（`qty ≤ 10`）会转人工**：平台策略没有登记该区间（fail-closed 的必然结果），业务员说「扣 5 件」时会挂起给仓管批。随时可以加一条 `pol-inventory-deduct-auto-small` 改成自动，但那是一次产品语义变更，需单独决定。
> ```
>
> 初版内容（三条规则 + 读真实配置文件的用例）留在下面仅作历史参考，**不要按它执行**。

**Files（初版，已作废）:**
- Modify: `config/policies.dev.json`
- Test: `internal/controlplane/policies_dev_test.go`（新建）

- [ ] **Step 1: 写失败用例**

新建 `internal/controlplane/policies_dev_test.go`：

```go
package controlplane

import (
	"context"
	"testing"

	"tenon.dev/internal/controlplane/policy"
)

// 这条用例读的是**真实的 dev 策略文件**：策略是产品行为，不是测试夹具。
func devPolicyEngine(t *testing.T) *policy.PolicyEngine {
	t.Helper()
	rules, err := LoadPolicies("../../config/policies.dev.json")
	if err != nil {
		t.Fatalf("加载 dev 策略失败：%v", err)
	}
	return policy.NewPolicyEngine(rules)
}

func TestDevPoliciesCoverSkillDomain(t *testing.T) {
	engine := devPolicyEngine(t)

	cases := []struct {
		name        string
		req         policy.PolicyRequest
		wantOutcome string
		wantBy      string
	}{
		{
			name:        "小量扣减自动",
			req:         policy.PolicyRequest{Action: "inventory.DeductStock", Args: []byte(`{"sku":"STD-1001","qty":5}`)},
			wantOutcome: "auto",
			wantBy:      "pol-inventory-deduct-auto-small",
		},
		{
			name:        "大量扣减要人批",
			req:         policy.PolicyRequest{Action: "inventory.DeductStock", Args: []byte(`{"sku":"STD-1001","qty":12}`)},
			wantOutcome: "approve",
			wantBy:      "pol-inventory-deduct-approve",
		},
		{
			name:        "补偿动作要人批",
			req:         policy.PolicyRequest{Action: "inventory.ReleaseStock", Args: []byte(`{"sku":"STD-1001","qty":1}`)},
			wantOutcome: "approve",
			wantBy:      "pol-inventory-release-approve",
		},
		{
			name:        "事件来源也能采购（D1 的 A 轮靠它）",
			req:         policy.PolicyRequest{Action: "procurement.CreatePurchaseRequest", Origin: "event", Args: []byte(`{"sku":"STD-1002","qty":12}`)},
			wantOutcome: "approve",
			wantBy:      "pol-procurement-create-approve",
		},
		{
			name:        "请求来源采购仍要人批",
			req:         policy.PolicyRequest{Action: "procurement.CreatePurchaseRequest", Origin: "request", Args: []byte(`{"sku":"STD-1002","qty":12}`)},
			wantOutcome: "approve",
			wantBy:      "pol-procurement-create-approve",
		},
		{
			name:        "触发器动作显式拒绝",
			req:         policy.PolicyRequest{Action: "quote.CreateStandardPartQuote", Args: []byte(`{"quoteNo":"Q-1","lines":[]}`)},
			wantOutcome: "deny",
			wantBy:      "pol-quote-create-deny",
		},
		{
			name:        "未覆盖的动作仍然默认拒绝",
			req:         policy.PolicyRequest{Action: "inventory.UnknownAction", Args: []byte(`{}`)},
			wantOutcome: "deny",
			wantBy:      "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := engine.Evaluate(context.Background(), c.req)
			if err != nil {
				t.Fatalf("求值失败：%v", err)
			}
			if got.Outcome != c.wantOutcome {
				t.Fatalf("outcome = %q，期望 %q（reasons=%v）", got.Outcome, c.wantOutcome, got.Reasons)
			}
			if c.wantBy == "" {
				if len(got.DecidedBy) != 0 {
					t.Fatalf("无命中时 decidedBy 必须为空，得到 %v", got.DecidedBy)
				}
				return
			}
			if len(got.DecidedBy) == 0 || got.DecidedBy[0] != c.wantBy {
				t.Fatalf("decidedBy = %v，期望首项 %q", got.DecidedBy, c.wantBy)
			}
		})
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/controlplane/ -run TestDevPoliciesCoverSkillDomain -count=1`
Expected: FAIL（小量扣减得到 deny、采购 event 来源得到 deny、quote 动作 deny 但没有 policyId）

- [ ] **Step 3: 改 `config/policies.dev.json`**

在数组里加入两条、并替换 procurement 那条：

```json
  {
    "policyId": "pol-inventory-deduct-auto-small",
    "priority": 100,
    "match": { "module": "inventory", "actions": ["inventory.DeductStock"] },
    "when": { "all": [{ "arg.qty": { "<=": 10 } }] },
    "outcome": "auto"
  },
  {
    "policyId": "pol-inventory-release-approve",
    "priority": 100,
    "match": { "module": "inventory", "actions": ["inventory.ReleaseStock"] },
    "when": { "all": [] },
    "outcome": "approve",
    "approvers": ["role:warehouse_manager"]
  },
```

把原 `plat-request-needs-human` 整条替换为（**去掉 `origin` 限制**，改名）：

```json
  {
    "policyId": "pol-procurement-create-approve",
    "priority": 100,
    "match": { "module": "procurement", "actions": ["procurement.CreatePurchaseRequest"] },
    "when": { "all": [] },
    "outcome": "approve",
    "approvers": ["role:procurement_manager"]
  },
```

再加一条显式 deny：

```json
  {
    "policyId": "pol-quote-create-deny",
    "priority": 1000,
    "match": { "module": "quote", "actions": ["quote.CreateStandardPartQuote"] },
    "when": { "all": [] },
    "outcome": "deny"
  },
```

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/controlplane/ -run TestDevPoliciesCoverSkillDomain -count=1 -v && pnpm check`
Expected: PASS（7 个子用例）；spec 校验通过

- [ ] **Step 5: Commit**

```bash
git add config/policies.dev.json internal/controlplane/policies_dev_test.go
git commit -m "feat(policy): 补齐 skill 域内动作覆盖（小量扣减自动、补偿人批、采购两来源、触发器显式拒绝）"
```

---

## Task A6: 策略拒绝时把计划动作写进 detail

**Files:**
- Modify: `internal/agent/run.go`
- Test: `internal/agent/run_test.go`

- [ ] **Step 1: 先找到现有拒绝分支**

Run: `Select-String -Path internal/agent/run.go -Pattern '策略闸门拒绝' -Context 0,6`
Expected: 看到形如

```go
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("策略闸门拒绝：outcome=%s decidedBy=%v reasons=%v（转人工，不换路绕过）",
					policy.Outcome, policy.DecidedBy, policy.Reasons)}, ""
```

- [ ] **Step 2: 写失败用例**

追加到 `internal/agent/run_test.go`：

```go
// 被拒时必须能看出「是哪个动作被拒」：否则排障只能猜（2026-10-27 那次真实发生）。
func TestPolicyDenyDetailListsPlannedActions(t *testing.T) {
	plan := Plan{PlanID: "pln_1", Steps: []PlanStep{
		{StepID: "s1", Action: "inventory.ReleaseStock", Kind: "write"},
		{StepID: "s2", Action: "inventory.GetStock", Kind: "read"},
	}}
	got := planActionsNote(plan)
	if got != "计划动作=[inventory.ReleaseStock, inventory.GetStock]" {
		t.Fatalf("计划动作描述不符：%q", got)
	}
}
```

- [ ] **Step 3: 跑用例确认失败**

Run: `go test ./internal/agent/ -run TestPolicyDenyDetailListsPlannedActions -count=1`
Expected: 编译失败 `undefined: planActionsNote`

- [ ] **Step 4: 实现并在拒绝分支里使用**

`internal/agent/run.go` 加：

```go
// planActionsNote 把计划里的动作按步骤顺序列出来，供拒绝/失败时写进 detail。
// 用 step 顺序而不是排序：排障时要能对上计划原文。
func planActionsNote(plan Plan) string {
	actions := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		actions = append(actions, step.Action)
	}
	return "计划动作=[" + strings.Join(actions, ", ") + "]"
}
```

把 Step 1 找到的那个 `Detail` 改成：

```go
				Detail: fmt.Sprintf("策略闸门拒绝：outcome=%s decidedBy=%v reasons=%v %s（转人工，不换路绕过）",
					policy.Outcome, policy.DecidedBy, policy.Reasons, planActionsNote(plan)), ""
```

- [ ] **Step 5: 跑用例确认通过**

Run: `gofmt -l internal/agent/ && go test ./internal/agent/ -run TestPolicyDenyDetailListsPlannedActions -count=1 -v && go test ./internal/agent/ -count=1`
Expected: PASS；agent 包全绿（若有既有用例断言旧 detail 文本，按新文本更新——那是本任务要改的期望值）

- [ ] **Step 6: Commit**

```bash
git add internal/agent/run.go internal/agent/run_test.go
git commit -m "feat(agent): 策略拒绝时把计划动作写进 detail（排障不再靠猜）"
```

---

## Task A7: D1 场景收紧（库存种子 0）+ 断言排除 inconclusive

**Files:**
- Modify: `scripts/d1-e2e.ps1`

- [ ] **Step 1: 把 inventory 种子改成 0**

在 `scripts/d1-e2e.ps1` 的 `Start-Svc 'inventory'` 的 env 表里加一项（其余不动）：

```powershell
		TENON_INVENTORY_SEED = 'STD-1002:0,STD-1001:120'
```

- [ ] **Step 2: 在报告打印后加断言**

找到脚本末尾打印 report 的那段，在 `Get-Content $reportPath -Raw -Encoding UTF8` 之后加：

```powershell
		# 判定收紧要落在脚本上：inconclusive 说明场景/契约还没到位，本轮验收不通过。
		$verdict = (Get-Content $reportPath -Raw -Encoding UTF8 | ConvertFrom-Json).verdict
		if ($verdict -eq 'inconclusive') {
			Write-Host '✗ D1 判定为 inconclusive：A 轮没有引用被摘模块，命题未被检验' -ForegroundColor Red
			exit 1
		}
```

- [ ] **Step 3: 语法自检**

Run: `powershell -NoProfile -Command "$null = [scriptblock]::Create((Get-Content scripts/d1-e2e.ps1 -Raw)); 'syntax ok'"`
Expected: `syntax ok`

- [ ] **Step 4: Commit**

```bash
git add scripts/d1-e2e.ps1
git commit -m "feat(scripts): D1 库存种子改 0（让唯一写路径是被摘模块）并拒绝 inconclusive"
```

---

## Task A8: 真模型对话验收脚本 `verify-ai.ps1`

**Files:**
- Create: `scripts/verify-ai.ps1`

前置：栈已用 `-Real` 起好（`scripts/dev-up.ps1 -Real`，常驻）。本脚本只驱动与断言，不负责起栈。

- [ ] **Step 1: 写脚本**

```powershell
#!/usr/bin/env powershell
# 真模型对话验收：假设栈已用 scripts/dev-up.ps1 -Real 起好（8080 常驻）。
# 断言：一句话 → 计划合法 → 挂起 → bob 批准 → 库存真的变了 → 审计 result 带 approvedBy。
[CmdletBinding()]
param(
	[string]$BaseUrl = 'http://127.0.0.1:8080',
	[string]$Sku = 'STD-1001',
	[int]$Qty = 5,
	[int]$TimeoutSec = 180
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root
$work = Join-Path $root 'out/ai-verify'
New-Item -ItemType Directory -Force -Path $work | Out-Null

function Fail([string]$m) { Write-Host "✗ $m" -ForegroundColor Red; exit 1 }
function C([string]$name) { Join-Path $work $name }
function PostJson([string]$path, [string]$bodyFile, [string]$token) {
	$args = @('-s', '-X', 'POST', "$BaseUrl$path", '-H', 'Content-Type: application/json', '--data-binary', "@$bodyFile")
	if ($token) { $args += @('-H', "Authorization: Bearer $token") }
	& curl.exe @args
}

# 1) 两个身份的 token
[System.IO.File]::WriteAllText((C 'alice.json'), '{"user":"alice","password":"alice-dev"}', (New-Object System.Text.UTF8Encoding($false)))
[System.IO.File]::WriteAllText((C 'bob.json'), '{"user":"bob","password":"bob-dev"}', (New-Object System.Text.UTF8Encoding($false)))
$alice = (PostJson '/tenon/session' (C 'alice.json') '' | ConvertFrom-Json).token
$bob = (PostJson '/tenon/session' (C 'bob.json') '' | ConvertFrom-Json).token
if (-not $alice -or -not $bob) { Fail '登录失败（栈没起？）' }

# 2) 投一句话
[System.IO.File]::WriteAllText((C 'req.json'), ('{"type":"tenon.request","text":"扣 {0} 件 {1}"}' -f $Qty, $Sku), (New-Object System.Text.UTF8Encoding($false)))
$enq = PostJson '/tenon/requests' (C 'req.json') $alice
Write-Host "投递：$enq"
if ($enq -notmatch '"ok":true') { Fail "入队失败：$enq" }

# 3) 等结果行出现 planId
$resultFile = Join-Path $root 'out/dev/agent-result.jsonl'
$deadline = (Get-Date).AddSeconds($TimeoutSec)
$line = $null
while ((Get-Date) -lt $deadline) {
	if (Test-Path $resultFile) {
		$last = (Get-Content $resultFile -Encoding UTF8 | Select-Object -Last 1)
		if ($last) { $line = $last | ConvertFrom-Json; if ($line.planId) { break } }
	}
	Start-Sleep -Seconds 2
}
if (-not $line -or -not $line.planId) { Fail "等不到带 planId 的结果行（agent 日志：out/dev/logs/agent.err）" }
Write-Host "结果行：outcome=$($line.outcome) planId=$($line.planId) actions=$($line.actions -join ',')"

# 4) 若挂起，由 bob 批准
$planId = $line.planId
if ($line.outcome -eq 'needs_human') {
	[System.IO.File]::WriteAllText((C 'confirm.json'), ('{"type":"tenon.confirm","planId":"{0}","decision":"approve"}' -f $planId), (New-Object System.Text.UTF8Encoding($false)))
	$cf = PostJson '/tenon/requests' (C 'confirm.json') $bob
	Write-Host "批准：$cf"
	$deadline = (Get-Date).AddSeconds($TimeoutSec)
	while ((Get-Date) -lt $deadline) {
		$archived = Get-ChildItem 'out/dev/agent-state/pending/archived' -Filter "$planId.json" -ErrorAction SilentlyContinue
		if ($archived) { break }
		Start-Sleep -Seconds 2
	}
	if (-not $archived) { Fail "批准后 pending 没有归档（planId=$planId）" }
} elseif ($line.outcome -ne 'completed') {
	Fail "回合没有正常推进：outcome=$($line.outcome) detail=$($line.detail)"
}

# 5) 库存真的变了
[System.IO.File]::WriteAllText((C 'stock.json'), ('{"sku":"{0}"}' -f $Sku), (New-Object System.Text.UTF8Encoding($false)))
$stockRaw = & curl.exe -s -X POST 'http://127.0.0.1:8081/tenon/actions/inventory.GetStock' `
	-H 'Content-Type: application/json' -H 'Tenon-Version: 0.1' --data-binary "@$(C 'stock.json')"
$onHand = ($stockRaw | ConvertFrom-Json).onHand
Write-Host "库存：$Sku onHand=$onHand"

# 6) 审计 result 段必须有 approvedBy（挂起路径）
$auditRaw = & curl.exe -s "$BaseUrl/tenon/audit?planId=$planId" -H "Authorization: Bearer $bob"
if ($auditRaw -notmatch '"stage":"result"') { Fail "审计没有 result 段：$auditRaw" }
if ($auditRaw -notmatch '"approvedBy":"user:bob"') { Fail "审计 result 段缺 approvedBy=user:bob：$auditRaw" }

Write-Host ''
Write-Host '✓ 真模型对话链路通过：计划合法 → 挂起/执行 → 审计可核' -ForegroundColor Green
if ($onHand -ne 120) { Write-Host "  注意：$Sku 的 onHand=$onHand，期望 120-$Qty=$((120-$Qty))；若不一致说明库存被之前的运行改过" -ForegroundColor Yellow }
```

- [ ] **Step 2: 加 BOM 并语法自检**

```powershell
$p=(Resolve-Path scripts/verify-ai.ps1).Path
$c=[System.IO.File]::ReadAllText($p,[System.Text.Encoding]::UTF8)
[System.IO.File]::WriteAllText($p,$c,(New-Object System.Text.UTF8Encoding($true)))
$errs=$null; $null=[System.Management.Automation.Language.Parser]::ParseFile($p,[ref]$null,[ref]$errs)
if ($errs) { $errs | ForEach-Object { $_.Message } } else { 'syntax ok' }
```

Expected: `syntax ok`

- [ ] **Step 3: Commit**

```bash
git add scripts/verify-ai.ps1
git commit -m "feat(scripts): 真模型对话验收（计划合法 → 挂起 → 批准 → 库存与审计可核）"
```

---

## Task A9: 真跑两次判定，写进 README，回归并推送

**Files:**
- Modify: `README.md`（「D1 判定结果」一节：第 4 次尝试）

- [ ] **Step 1: 真跑 D1（新种子）**

Run: `powershell -NoProfile -File scripts/d1-e2e.ps1`
Expected: `verdict` 为 `pass` 或 `fail`；若 `inconclusive`，脚本现在会直接 exit 1（Task A7 加的断言）

- [ ] **Step 2: 真跑对话验收**

先用真模型起栈并保持：`powershell -NoProfile -File scripts/dev-up.ps1 -Real`（另一个终端 / 后台常驻）
然后：`powershell -NoProfile -File scripts/verify-ai.ps1`
Expected: 末行 `✓ 真模型对话链路通过`

- [ ] **Step 3: 把结果写进 README**

在「D1 判定结果」表格追加一行（第 4 次），并如实填写 `verdict` 与四条判据：

```markdown
| #4 | 库存种子改 0 + requiredArgs 进 prompt | `<pass/fail>` | A=<动作>；B=<动作>；B 轮结局=<outcome> |
```

并在该节末尾补一句结论：`第 4 次判定：<结论>；对话链路（真模型）：<通过/失败>。`

- [ ] **Step 4: 全量回归**

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
pnpm check
powershell -NoProfile -File scripts/verify-ui.ps1
```

Expected: 五项全绿

- [ ] **Step 5: Commit 并推送**

```bash
git add README.md
git commit -m "docs(README): D1 第 4 次判定（新种子）与真模型对话验收结论"
git push origin feat/m05-host-agent
```

---

# Part B：第 0 步搬迁清理

## Task B1: 搬迁 5 个 README 并删除占位包

**Files:**
- Create: `docs/designs/dsh-adapter-boundaries/`（5 个文件，由 `git mv` 产生）
- Delete: `packages/dsh-plugin-governance/`、`packages/dsh-plugin-module-host/`、`packages/dsh-plugin-intent-ui/`、`packages/dsh-plugin-skill-biz/`、`packages/sidecar-http/`

- [ ] **Step 1: 先搬迁（保留 git 历史）**

```bash
mkdir -p docs/designs/dsh-adapter-boundaries
git mv packages/dsh-plugin-governance/README.md docs/designs/dsh-adapter-boundaries/dsh-plugin-governance.md
git mv packages/dsh-plugin-module-host/README.md docs/designs/dsh-adapter-boundaries/dsh-plugin-module-host.md
git mv packages/dsh-plugin-intent-ui/README.md docs/designs/dsh-adapter-boundaries/dsh-plugin-intent-ui.md
git mv packages/dsh-plugin-skill-biz/README.md docs/designs/dsh-adapter-boundaries/dsh-plugin-skill-biz.md
git mv packages/sidecar-http/README.md docs/designs/dsh-adapter-boundaries/sidecar-http.md
```

- [ ] **Step 2: 给搬迁后的文档加一行横幅**

给 `docs/designs/dsh-adapter-boundaries/*.md` 每个文件顶部插入（内容相同）：

```markdown
> **状态：未实现（占位包的职责边界，M2 之后按 ADR-0001 §6 重建）。**
> 本文件原在 `packages/<pkg>/README.md`；ADR-0001 决定删除无 `src` 的占位包，但保留职责边界。实现时按本文件重建，不要照抄目录结构。
```

- [ ] **Step 3: 删掉占位包目录**

```bash
git rm -r packages/dsh-plugin-governance packages/dsh-plugin-module-host packages/dsh-plugin-intent-ui packages/dsh-plugin-skill-biz packages/sidecar-http
```

- [ ] **Step 4: 验证 TS 侧仍然可用**

```bash
pnpm check
pnpm run typecheck
```

Expected: 两项均通过（`@tenon/core` 未受影响）

- [ ] **Step 5: Commit**

```bash
git add -A packages docs/designs
git commit -m "chore: 按 ADR-0001 删占位包，职责边界搬到 docs/designs（先搬迁后删除）"
```

---

## Task B2: README 与文档里的引用对齐

**Files:**
- Modify: `README.md`（仓库结构行）

- [ ] **Step 1: 改仓库结构描述**

找到 `packages/         @tenon/core 类型 + dsh 插件（TypeScript，M1 的进程内路径，本轮未接）`，替换为：

```markdown
packages/         @tenon/core（协议类型与工具，TS）；dsh 插件的职责边界见 docs/designs/dsh-adapter-boundaries/
```

- [ ] **Step 2: 确认没有残留引用**

Run: `Select-String -Path README.md,docs\*.md -Pattern 'packages/dsh-plugin|packages/sidecar-http'`
Expected: 无命中（若有，改成指向 `docs/designs/dsh-adapter-boundaries/`）

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(README): 仓库结构对齐 ADR-0001（占位包已删，边界文档在新位置）"
```

---

# Part C：两个 POC（结果只改 ADR §5 附录）

> **本部分是实验，不是产品代码。** 所有产物落在 `docs/designs/` 与 `out/poc/`；不碰 `internal/`、`cmd/`。
> **POC-2 必须用独立 DSH 实例**（自建 profile + 独立数据目录与端口）：**不得重启当前会话所在的 DSH**。
> 结果文档里的 `<fail-closed / fail-open>` 一类尖括号字段是**实验待观测值**，必须由实验填入；这不是计划占位符——计划无法预知实验结果，编造结果才是失败。

## Task C1: 侦察 DSH 钩子与审批的真实语义

**Files:**
- Create: `docs/designs/dsh-hook-recon.md`

- [ ] **Step 1: 读钩子协议的类型定义**

Run:

```powershell
$nm='E:\npm-global\node_modules\@deepseek-ai\dsh\node_modules\@deepseek-ai'
Get-Content "$nm\dsh-hook-protocol\lib\types\events.d.ts" -Encoding UTF8 | Select-Object -First 80
Get-Content "$nm\dsh-hook-protocol\lib\types\codec.d.ts" -Encoding UTF8 | Select-Object -First 60
Get-Content "$nm\dsh-hook-protocol\lib\types\merge.d.ts" -Encoding UTF8 | Select-Object -First 60
```

记录到 `docs/designs/dsh-hook-recon.md`：①有哪些 hook 事件（`PreToolUse` 的确切拼写）；②退出码语义表（`0/2/其它` 分别是什么）；③stdout 的 JSON 形状（allow/deny/block 字段）；④超时与崩溃时的行为（若文档没写，标「待 POC-1 实测」）。

- [ ] **Step 2: 读钩子的配置方式**

Run:

```powershell
$nm='E:\npm-global\node_modules\@deepseek-ai\dsh\node_modules\@deepseek-ai'
Get-Content "$nm\dsh-hooks-claude-code\README.md" -Encoding UTF8 | Select-Object -First 80
Get-Content "$nm\dsh-hooks-claude-code\lib\types\config.d.ts" -Encoding UTF8 | Select-Object -First 80
```

记录：钩子配置文件路径与 schema、命令如何被调用、工作目录与环境变量。

- [ ] **Step 3: 记录并提交**

```bash
git add docs/designs/dsh-hook-recon.md
git commit -m "docs(poc): DSH 钩子协议侦察（事件/退出码/配置）"
```

---

## Task C2: POC-1 —— 前置闸门抛错/崩溃时是否 fail-closed

**Files:**
- Create: `out/poc/guard.ps1`（不进仓库）
- Create: `docs/designs/poc-pre-tool-gate.md`

- [ ] **Step 1: 准备一个「副作用可观测」的工具调用**

用 DSH 的 `pwsh` 工具执行一条写文件命令（副作用 = 文件出现）：

```
在 out/poc/side-effect.txt 写入 "executed"
```

- [ ] **Step 2: 实验 A —— 钩子 exit 2**

`out/poc/guard.ps1`：

```powershell
# POC-1/A：显式阻断。按 C1 记录的协议，exit 2 表示 block，stderr 是理由。
[Console]::Error.WriteLine("tenon-poc: 由前置闸门阻断")
exit 2
```

按 C1 记录的配置方式挂到 `PreToolUse`（匹配 pwsh 工具），执行 Step 1 的工具调用。
Expected（若协议如文档）：`out/poc/side-effect.txt` **不存在**，模型看到阻断理由。

- [ ] **Step 3: 实验 B —— 钩子崩溃（真正的待验语义）**

`out/poc/guard.ps1` 改成：

```powershell
# POC-1/B：模拟闸门自身故障（未捕获异常 ⇒ 非 0、非 2 的退出码）。
throw "tenon-poc: 闸门崩了"
```

再执行一次 Step 1。
Expected 之一，并把实际观察写进结果文档：
- 文件不存在 ⇒ **fail-closed**（可以在这里做 I7）；
- 文件出现 ⇒ **fail-open**（I7 必须由 Go host 强制）。

- [ ] **Step 4: 写结果文档**

`docs/designs/poc-pre-tool-gate.md` 内容：

```markdown
# POC-1：PreToolUse 闸门的失败语义（DSH 0.2.0-rc.2）

- 日期：2026-10-27
- 结论：<fail-closed / fail-open / 部分>

## 实验与观察

| 实验 | 钩子行为 | side-effect.txt | 模型可见结果 |
|---|---|---|---|
| A 显式 block | exit 2 + stderr | <有/无> | <...> |
| B 闸门崩溃 | 未捕获异常 | <有/无> | <...> |

## 对 Tenon 的含义

- 若 B 也 fail-closed：I7（先写 dispatched 审计、失败即拒）可以放在 DSH 的 PreToolUse 钩子里，钩子调 `POST /tenon/audit`，失败时 exit 2。
- 若 B fail-open：钩子只能做「显式阻断」，**不能承担安全语义**；I7 必须由 Go host 在 `/tenon/audit` 返回非 200 时拒绝签发执行许可，adapter 拿到许可才允许工具执行。
```

- [ ] **Step 5: Commit**

```bash
git add docs/designs/poc-pre-tool-gate.md
git commit -m "docs(poc): POC-1 结论（PreToolUse 在崩溃时是否 fail-closed）"
```

---

## Task C3: POC-2 —— 审批跨重启是否存活

**Files:**
- Create: `docs/designs/poc-approval-restart.md`

- [ ] **Step 1: 起一个独立 DSH 实例**

在一个**独立数据目录**与端口上起 DSH（不要碰当前会话的实例）：

```powershell
$env:DSH_HOME = Join-Path (Get-Location) 'out/poc/dsh-home'
New-Item -ItemType Directory -Force -Path $env:DSH_HOME | Out-Null
# 端口按 DSH 的 web 配置项指定；若不支持，用 CLI 的 headless 模式，不启 web
dsh --help
```

记录可用的启动方式（web / headless）到结果文档。若 `dsh` 不在 PATH，用 `node "E:\npm-global\node_modules\@deepseek-ai\dsh\lib\bin\..."`（以 Step 2 的 `--help` 输出为准）。

- [ ] **Step 2: 制造一个 pending 审批**

用该实例执行一个**需要审批**的危险工具调用（写 `out/poc/approval-target.txt` 的 pwsh 命令），在审批弹出后**不要回答**。

- [ ] **Step 3: 重启该实例**

结束进程 → 用同一 `DSH_HOME` 重启 → 观察：该 pending 审批是否还在？能否仍被回答？

- [ ] **Step 4: 写结果文档**

```markdown
# POC-2：审批跨重启（DSH 0.2.0-rc.2）

- 结论：<存活 / 不存活 / 无法判定（原因）>

| 步骤 | 观察 |
|---|---|
| 重启前 | 审批 pending，目标文件未出现 |
| 重启后 | <审批仍在且可回答 / 审批丢失 / 回合作废> |
| 回答后 | <文件出现 / 无效果> |

## 对 Tenon 的含义

- 不存活（预期）：DSH 的 `ctx.get('approval')` 只做**一次性入口**；`pending` 的权威副本必须在 Tenon 公告板，重启后由 adapter 按公告板重建审批入口。
- 存活：公告板仍是真相（协议要求），但 DSH UI 可直接复用，adapter 只需做映射。
```

- [ ] **Step 5: Commit**

```bash
git add docs/designs/poc-approval-restart.md
git commit -m "docs(poc): POC-2 结论（审批是否跨重启存活）"
```

---

## Task C4: 把两个结论写回 ADR

**Files:**
- Modify: `docs/adr/0001-driver-dsh-vs-go.md`（§5 末尾加「实测结论」）

- [ ] **Step 1: 追加实测结论**

在 ADR 的 §5 末尾加：

```markdown
### 实测结论（2026-10-27，DSH `0.2.0-rc.2`）

- **POC-1**：<fail-closed / fail-open / 部分>，详见 [`docs/designs/poc-pre-tool-gate.md`](../designs/poc-pre-tool-gate.md)。
  因此 I7 的前置审计强制点落在：<DSH 钩子 / Go host>。
- **POC-2**：审批跨重启<存活 / 不存活>，详见 [`docs/designs/poc-approval-restart.md`](../designs/poc-approval-restart.md)。
  因此审批真相：<公告板为唯一真相 / 公告板为真相且 DSH UI 可复用>。
```

- [ ] **Step 2: Commit**

```bash
git add docs/adr/0001-driver-dsh-vs-go.md
git commit -m "docs(adr): ADR-0001 补 POC 实测结论（闸门失败语义、审批存活）"
```

---

## 自检对照（spec / ADR → 任务）

| 来源 | 要求 | 落在 |
|---|---|---|
| D1 修复 spec §2 | 两处 schema 的 `requiredArgs` | A1 |
| D1 修复 spec §2 | 模块加载期 `⊆ required` 断言 | A2 |
| D1 修复 spec §3 | 三模块声明 + prompt 渲染标题/必填 | A3、A4 |
| D1 修复 spec §4 | 策略五处调整（含 procurement 去 origin 限制、quote 显式 deny） | A5 |
| D1 修复 spec §4 | deny 时 detail 带计划动作 | A6 |
| D1 修复 spec §5 | 库存种子 0、判据不变、拒绝 inconclusive | A7 |
| D1 修复 spec §8 | 双判据脚本（对话 + D1） | A8、A9 |
| ADR-0001 §4 | 先搬迁后删除占位包 | B1 |
| ADR-0001 §4 | 文档不再误读 | B2 |
| ADR-0001 §5 | 两个 POC 的判据与结论回写 | C1–C4 |
| ADR-0001 §6 第 0 步 | 清理完成 | B1、B2 |
