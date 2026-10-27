# M0.5 宿主与 agent 运行时 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一致性用例 C4/C5 由 SKIP 变 PASS，并让 D1 差异测试用真 LLM 跑出「摘掉模块后 agent 行为自适应改变」的结论。

**Architecture:** 三进程——`cmd/host` 只做控制平面（策略/授权/审计链/注册表/共驾帧，不含 LLM）；`cmd/agent` 是独立可替换的 AI 运行时（轮询模块发件箱 → 读 skill → 能力发现 → LLM 规划 → 过闸门 → 执行 → 两段审计）；`cmd/quote` 是触发器模块。协议扩展三处：`POST /tenon/audit`、`GET /tenon/modules`、模块发件箱 `direction=out`。

**Tech Stack:** Go 1.26（标准库 + `github.com/santhosh-tekuri/jsonschema/v6`）、DeepSeek OpenAI 兼容端点（`deepseek-flash`）、既有 `internal/protocol`/`internal/spec`/`internal/module`/`conformance`。

**说明：** Go 代码块给出**接口与关键逻辑**；表格驱动的测试脚手架、`http.Handler` 样板、日志格式一律照 `internal/module/module.go` 与 `tools/lint/lint.go` 的既有风格，不重复粘贴。

**设计依据：** `docs/superpowers/specs/2026-10-27-tenon-m05-host-agent-design.md`

---

## P1 · 控制平面

### Task 1: Policy 引擎

**Files:**
- Create: `internal/controlplane/policy.go`
- Test: `internal/controlplane/policy_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestPolicyMergeOrderDenyWins(t *testing.T) {
    rules := []PolicyRule{
        {PolicyID: "p-auto", Priority: 10, Match: match("inventory", "inventory.DeductStock"),
            When: nil, Outcome: "auto"},
        {PolicyID: "p-approve", Priority: 100, Match: match("inventory", "inventory.DeductStock"),
            When: mustCond(t, `{"all":[{"arg.qty":{">":10}}]}`), Outcome: "approve"},
        {PolicyID: "p-deny", Priority: 1, Match: match("inventory", "inventory.DeductStock"),
            When: mustCond(t, `{"all":[{"arg.qty":{"in":[999]}}]}`), Outcome: "deny"},
    }
    eng := NewPolicyEngine(rules)

    got, err := eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{"qty":12}`))
    if err != nil { t.Fatal(err) }
    if got.Outcome != "approve" || got.DecidedBy[0] != "p-approve" {
        t.Fatalf("期望 approve/p-approve，得到 %+v", got)
    }

    got, _ = eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{"qty":999}`))
    if got.Outcome != "deny" { t.Fatalf("deny 必须压过 approve，得到 %+v", got) }
}

func TestPolicyNoMatchDefaultsToDeny(t *testing.T) {
    eng := NewPolicyEngine(nil)
    got, err := eng.Evaluate(context.Background(), evalReq("inventory.DeductStock", `{}`))
    if err != nil { t.Fatal(err) }
    if got.Outcome != "deny" { t.Fatalf("无命中必须 deny（fail-closed），得到 %q", got.Outcome) }
    if len(got.Reasons) == 0 || got.Reasons[0] != "NO_POLICY_MATCH_DEFAULT_DENY" {
        t.Fatalf("reasons 应说明默认拒绝：%v", got.Reasons)
    }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/controlplane/ -run TestPolicy -count=1`
Expected: FAIL — `undefined: NewPolicyEngine` / `undefined: PolicyRule`

- [ ] **Step 3: 实现**

```go
// PolicyRule 对应 spec/0.1/schemas/policy.schema.json。
type PolicyRule struct {
    PolicyID  string          `json:"policyId"`
    Priority  int             `json:"priority"`
    Match     PolicyMatch     `json:"match"`
    When      json.RawMessage `json:"when,omitempty"`
    Outcome   string          `json:"outcome"` // auto | approve | deny
    Approvers []string        `json:"approvers,omitempty"`
}

// PolicyEvaluation 是 policies:evaluate 的响应体。
type PolicyEvaluation struct {
    Outcome   string   `json:"outcome"`
    DecidedBy []string `json:"decidedBy"`
    Approvers []string `json:"approvers,omitempty"`
    Reasons   []string `json:"reasons"`
}

// Evaluate 的合并规则（设计文档 §4.7）：deny > approve > auto；
// 同 outcome 取 priority 最大者并记入 DecidedBy；无命中 → deny。
func (e *PolicyEngine) Evaluate(ctx context.Context, req PolicyRequest) (PolicyEvaluation, error)
```

条件求值实现要点：算子只支持 `eq/ne/>/>=/</<=/in/notIn/matches`（封闭集合，未知算子返回错误而不是放行）；组合支持 `all/any/not`；路径取值支持 `arg.*` 与 `risk.*` 前缀，取不到值视为不匹配（不报错）。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/controlplane/ -run TestPolicy -count=1`
Expected: PASS（含「未知算子报错」子用例）

- [ ] **Step 5: 提交**

```bash
git add internal/controlplane/policy.go internal/controlplane/policy_test.go
git commit -m "feat(controlplane): policy 引擎（9 算子 + all/any/not + deny>approve>auto + 默认 deny）"
```

---

### Task 2: Grant 引擎（交集 + 配额窗口）

**Files:**
- Create: `internal/controlplane/grant.go`
- Test: `internal/controlplane/grant_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestGrantIsIntersection(t *testing.T) {
    g := NewGrantEngine([]Grant{{GrantID: "gr-1", Agent: "ag_fulfillment",
        Allow: []GrantEntry{{Module: "inventory", Actions: []string{"inventory.DeductStock"}}}}})
    subjects := map[string][]string{
        "user:alice": {"inventory.DeductStock"},
        "user:bob":   {"inventory.GetStock"},   // bob 没有扣减权
    }
    if r, _ := g.Evaluate("ag_fulfillment", "user:alice", "inventory.DeductStock", 1); !r.Allowed {
        t.Fatal("alice 应被允许")
    }
    r, _ := g.Evaluate("ag_fulfillment", "user:bob", "inventory.DeductStock", 1)
    if r.Allowed || r.Reason != "SUBJECT_NOT_PERMITTED" {
        t.Fatalf("交集规则未生效：%+v", r)
    }
}

func TestGrantQuotaWindow(t *testing.T) {
    g := NewGrantEngine([]Grant{{GrantID: "gr-2", Agent: "a",
        Allow: []GrantEntry{{Module: "inventory", Actions: []string{"inventory.DeductStock"},
            Constraints: &GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 10}}}}})
    if r, _ := g.Evaluate("a", "user:x", "inventory.DeductStock", 6); !r.Allowed { t.Fatal("6 ≤ 10 应允许") }
    r, _ := g.Evaluate("a", "user:x", "inventory.DeductStock", 6)
    if r.Allowed || r.Reason != "QUOTA_EXCEEDED" {
        t.Fatalf("累计 12 > 10 应拒：%+v", r)
    }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/controlplane/ -run TestGrant -count=1`
Expected: FAIL — `undefined: NewGrantEngine`

- [ ] **Step 3: 实现**

```go
type GrantResult struct {
    Allowed        bool         `json:"allowed"`
    MatchedGrantID string       `json:"matchedGrantId,omitempty"`
    Quota          *QuotaStatus `json:"quota,omitempty"`
    Reason         string       `json:"reason,omitempty"` // NOT_IN_GRANT|EXPIRED|QUOTA_EXCEEDED|SUBJECT_NOT_PERMITTED
}

// Evaluate 的规则（设计文档 §4.6 + 决策 D5）：
//   effective = agent 被授予 ∩ onBehalfOf 用户权限
//   配额按 (grantID, window) 累计；窗口用 ISO-8601 duration 解析（time.AddDate/Add 手写解析，
//   仅支持 PnD / PnW / PTnH，超出范围返回 UNSUPPORTED_WINDOW 而不是猜测）。
```

实现必须处理：`expiresAt` 过期 → `EXPIRED`；agent 未列该 action → `NOT_IN_GRANT`；配额单位由 action 的 `quotaUnit` 决定（`quantity` 累加参数里的数值）。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/controlplane/ -run TestGrant -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/controlplane/grant.go internal/controlplane/grant_test.go
git commit -m "feat(controlplane): grant 引擎（权限取交集 + 配额窗口 + 四类拒绝原因）"
```

---

### Task 3: 审计存储（两段写 + 哈希链 + 注入失败）

**Files:**
- Create: `internal/controlplane/audit.go`
- Test: `internal/controlplane/audit_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestAuditChainAndTwoStages(t *testing.T) {
    dir := t.TempDir()
    st, err := NewAuditStore(filepath.Join(dir, "audit.jsonl"))
    if err != nil { t.Fatal(err) }

    if err := st.Append(AuditRecord{AuditID: "aud_1", Stage: "dispatched", PlanID: "pln_1"}); err != nil { t.Fatal(err) }
    if err := st.Append(AuditRecord{AuditID: "aud_1", Stage: "result", PlanID: "pln_1"}); err != nil { t.Fatal(err) }

    recs, err := st.Query(Query{PlanID: "pln_1"})
    if err != nil { t.Fatal(err) }
    if len(recs) != 2 { t.Fatalf("期望 2 段，得到 %d", len(recs)) }
    if recs[0].PrevHash != nil { t.Fatal("链首 prevHash 必须为 null") }
    if recs[1].PrevHash == nil || *recs[1].PrevHash != recs[0].Hash {
        t.Fatal("prevHash 未正确串联")
    }
    if err := st.VerifyChain(); err != nil { t.Fatalf("链校验失败：%v", err) }
}

func TestAuditAppendFailureIsObservable(t *testing.T) {
    st, _ := NewAuditStore(filepath.Join(t.TempDir(), "a.jsonl"))
    st.FailNextAppend(errors.New("磁盘满"))     // 测试注入钩子
    if err := st.Append(AuditRecord{AuditID: "aud_x", Stage: "dispatched"}); err == nil {
        t.Fatal("注入失败时 Append 必须返回错误——上层据此拒绝执行")
    }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/controlplane/ -run TestAudit -count=1`
Expected: FAIL — `undefined: NewAuditStore`

- [ ] **Step 3: 实现**

```go
// AuditRecord 是 spec/0.1/schemas/audit-event.schema.json 的运行期子集。
type AuditRecord struct {
    AuditID  string          `json:"auditId"`
    Stage    string          `json:"stage"` // dispatched | result
    At       time.Time       `json:"at"`
    Actor    json.RawMessage `json:"actor"`
    PlanID   string          `json:"planId,omitempty"`
    StepID   string          `json:"stepId,omitempty"`
    Action   string          `json:"action,omitempty"`
    Decision json.RawMessage `json:"decision,omitempty"`
    Result   json.RawMessage `json:"result,omitempty"`
    Effects  json.RawMessage `json:"effects,omitempty"`
    Hash     string          `json:"hash"`
    PrevHash *string         `json:"prevHash"`
}

// Append 计算 hash = sha256(规范化 JSON 去掉 hash 字段)，并把 prevHash 指向链尾。
// 链首 prevHash = null。写入用 O_APPEND 单行 JSONL；失败必须返回错误。
// Query 支持按 planId、since（游标=行号）过滤。
// VerifyChain 重算全链并比对。
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/controlplane/ -run TestAudit -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/controlplane/audit.go internal/controlplane/audit_test.go
git commit -m "feat(controlplane): 审计存储（两段写 + prevHash 链 + 失败注入可观测）"
```

---

### Task 4: 模块注册表

**Files:**
- Create: `internal/controlplane/registry.go`
- Test: `internal/controlplane/registry_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestRegistryAddRemoveList(t *testing.T) {
    r := NewRegistry()
    r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})
    r.Upsert(ModuleEntry{ID: "procurement", BaseURL: "http://p:8082"})
    if got := r.List(); len(got) != 2 { t.Fatalf("期望 2 个，得到 %d", len(got)) }

    r.SetEnabled("procurement", false)
    list := r.List()
    if len(list) != 2 { t.Fatal("摘除应保留条目但标记 disabled") }
    enabled := r.Enabled()
    if len(enabled) != 1 || enabled[0].ID != "inventory" {
        t.Fatalf("Enabled() 只应返回 enabled 的模块：%+v", enabled)
    }
}
```

- [ ] **Step 2: 运行确认失败** → Run: `go test ./internal/controlplane/ -run TestRegistry -count=1`
Expected: FAIL — `undefined: NewRegistry`

- [ ] **Step 3: 实现**（内存 + `sync.RWMutex`；`Enabled()` 给 agent 用，`List()` 给管理面用）

- [ ] **Step 4: 运行确认通过** → `go test ./internal/controlplane/ -run TestRegistry -count=1` → PASS

- [ ] **Step 5: 提交**

```bash
git add internal/controlplane/registry.go internal/controlplane/registry_test.go
git commit -m "feat(controlplane): 模块注册表（enabled 开关支撑 D1 摘除）"
```

---

### Task 5: `cmd/host` 组装

**Files:**
- Create: `cmd/host/main.go`
- Create: `internal/controlplane/http.go`
- Create: `config/policies.dev.json`（默认策略：DeductStock qty>10 → approve；其余无命中 → deny）
- Create: `config/grants.dev.json`（`ag_fulfillment` 对 `inventory.GetStock/DeductStock`、`procurement.CreatePurchaseRequest` 的授予 + 配额）
- Create: `config/subjects.dev.json`（`user:alice` 拥有上述 action —— 交集规则的测试题材）
- Test: `internal/controlplane/http_test.go`

> 这三份配置文件是 Task 15 真跑 D1 的输入，必须在实现 host 时一起落地，否则 host 起不来。

- [ ] **Step 1: 写失败测试**

```go
func TestHostEndpoints(t *testing.T) {
    h := newTestHost(t)          // 构造带默认策略/授权的 controlplane server
    srv := httptest.NewServer(h.Handler())
    defer srv.Close()

    // 无命中策略 → deny（fail-closed）
    st, body := postJSON(t, srv.URL+"/tenon/policies:evaluate", `{"plan":{}}`)
    if st != 200 || !strings.Contains(body, `"outcome":"deny"`) { t.Fatalf("C5 语义不成立：%d %s", st, body) }

    // 审计写入 + 查询
    st, _ = postJSON(t, srv.URL+"/tenon/audit", `{"auditId":"aud_1","stage":"dispatched","planId":"pln_1"}`)
    if st != 200 { t.Fatalf("写审计失败：%d", st) }
    st, body = getJSON(t, srv.URL+"/tenon/audit?planId=pln_1")
    if !strings.Contains(body, `"aud_1"`) { t.Fatalf("审计查询未命中：%s", body) }

    // 注册表
    st, body = getJSON(t, srv.URL+"/tenon/modules")
    if st != 200 || !strings.Contains(body, "inventory") { t.Fatalf("注册表读取失败：%s", body) }

    // TEST_MODE 关闭时 admin 端点不存在
    if st, _ := postJSON(t, srv.URL+"/tenon/admin/modules", `{}`); st != 404 {
        t.Fatalf("非测试模式不应暴露 admin，得到 %d", st)
    }
}
```

- [ ] **Step 2: 运行确认失败** → `go test ./internal/controlplane/ -run TestHost -count=1` → FAIL

- [ ] **Step 3: 实现**

挂载：`POST /tenon/policies:evaluate`、`POST /tenon/grants:evaluate`、`POST /tenon/audit`、`GET /tenon/audit`、`GET /tenon/modules`、`GET /healthz`、`GET /readyz`。
`TENON_TEST_MODE=1` 时额外挂 `POST /tenon/admin/modules`（`{id, baseURL, enabled}`）与 `GET /tenon/intents/poll`。
WS `/tenon/intents` 最小实现：接受连接后把收到的帧追加到内存环形队列，供 poll 回放。

`cmd/host/main.go` 读：`PORT`(8080)、`TENON_POLICY_FILE`、`TENON_GRANT_FILE`、`TENON_AUDIT_PATH`、`TENON_MODULES`（`id=url,id=url`）、`TENON_TEST_MODE`。

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交**

```bash
git add cmd/host internal/controlplane/http.go internal/controlplane/http_test.go
git commit -m "feat(host): 控制平面端点组装（含 TEST_MODE admin 与健康探针）"
```

---

### Task 6: conformance 增加宿主模式，C4/C5 落地

**Files:**
- Modify: `conformance/conformance.go`
- Modify: `conformance/types.go`
- Test: `conformance/conformance_test.go`

- [ ] **Step 1: 写失败测试**（新增用例，复用 Task 5 的 httptest host 思路：在 `conformance` 包里再写一个最小 host stub）

```go
func TestC4AndC5AgainstHost(t *testing.T) {
    srv := httptest.NewServer(hostStub())   // 实现 policies/grants/audit 三个端点
    defer srv.Close()

    rep := Run(context.Background(), Options{HostTarget: srv.URL, Target: srv.URL, Spec: mustSpec(t)})
    byCase := map[string]Status{}
    for _, r := range rep.Results { byCase[r.Case] = r.Status }

    if byCase["C4"] != Pass { t.Fatalf("C4 应 PASS，得到 %q（detail=%s）", byCase["C4"], detailOf(rep, "C4")) }
    if byCase["C5"] != Pass { t.Fatalf("C5 应 PASS，得到 %q", byCase["C5"]) }
    if byCase["D1"] != Skip { t.Fatalf("D1 在无 agent 场景应仍为 SKIP") }
}
```

- [ ] **Step 2: 运行确认失败** → `go test ./conformance/ -run TestC4AndC5 -count=1` → FAIL（`unknown field HostTarget`）

> 测试辅助：`mustSpec(t)` 与 `detailOf(rep, case)` 加到 `conformance` 包的测试文件里（`mustSpec` 内部调 `spec.Load`，`detailOf` 返回该用例的 Detail 便于断言失败信息）。

- [ ] **Step 3: 实现**

`Options` 增加 `HostTarget string`。当非空时：
- **C4**：写一条 `stage=dispatched` → 写一条 `stage=result` → `GET /tenon/audit?planId=` 断言两段齐备、`prevHash` 首为 null 且次条等于首次 `hash`；若 host 暴露 `verifyChain` 语义则调用之。
- **C5**：把 host 的 policies/grants 关掉（stub 提供 `/tenon/test/deny-all` 或在 stub 里直接返回 deny），断言写操作路径拿到 `deny`。模块模式下仍记 SKIP。

`hostSideSkips()` 改为按 `HostTarget != ""` 决定 C4/C5 是 Pass 还是 Skip。

- [ ] **Step 4: 运行确认通过** → `go test ./conformance/ -count=1` → PASS，且原有 `TestRunAgainstCompliantFakeModule` 仍绿

- [ ] **Step 5: 提交**

```bash
git add conformance
git commit -m "feat(conformance): 宿主模式使 C4/C5 可判定（模块模式仍 SKIP 并写明原因）"
```

---

## P2 · 框架缺口与 quote 模块

### Task 7: 模块发件箱（`direction=out`）

**Files:**
- Modify: `internal/module/module.go`
- Test: `internal/module/module_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestOutboxDirection(t *testing.T) {
    m := newTestModule(t)
    m.EmitEvent(Event{EventID: "evt_1", Type: "quote.completed"})
    srv := httptest.NewServer(m.Handler())
    defer srv.Close()

    _, out := getJSON(t, srv.URL+"/tenon/events?direction=out")
    if !strings.Contains(out, "quote.completed") { t.Fatalf("outbox 未返回模块自有事件：%s", out) }

    _, in := getJSON(t, srv.URL+"/tenon/events")     // 缺省 in
    if strings.Contains(in, "quote.completed") { t.Fatalf("缺省方向应为 in，不应看到自有事件：%s", in) }
}
```

- [ ] **Step 2: 运行确认失败** → `go test ./internal/module/ -run TestOutbox -count=1` → FAIL

- [ ] **Step 3: 实现**

`Module` 增加 `outbox []Event` 与 `outboxCursor`；新导出方法 `EmitEvent(Event)`（并发安全，分配 `seq`）。`GET /tenon/events` 增加 `direction` 参数（`in` 缺省 / `out`），两套游标互不干扰。**响应形状不变**（`{events, nextCursor}`）。

- [ ] **Step 4: 运行确认通过** → `go test ./internal/module/ -count=1` → PASS（原有 17 个测试不得回归）

- [ ] **Step 5: 提交**

```bash
git add internal/module
git commit -m "feat(module): 发件箱（direction=out），补上模块→agent 的事件出口"
```

---

### Task 8: 测试态重置

**Files:**
- Modify: `internal/module/module.go`
- Test: `internal/module/module_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestResetRestoresSeedAndClearsCaches(t *testing.T) {
    m := newTestModuleWithReset(t)      // testMode=true，带 reset 回调
    m.Handle(context.Background(), "test.Bump", nil)
    m.Handle(context.Background(), "test.Bump", nil)
    srv := httptest.NewServer(m.Handler())
    defer srv.Close()

    if st, _ := postJSON(t, srv.URL+"/tenon/admin/reset", `{}`); st != 200 { t.Fatalf("reset 失败 %d", st) }
    if got := m.Calls(); got != 0 { t.Fatalf("reset 后业务状态未恢复：%d", got) }
}

func TestResetAbsentWhenTestModeOff(t *testing.T) {
    m := newTestModule(t)               // testMode=false
    srv := httptest.NewServer(m.Handler())
    defer srv.Close()
    if st, _ := postJSON(t, srv.URL+"/tenon/admin/reset", `{}`); st != 404 {
        t.Fatalf("非测试模式不应暴露 reset，得到 %d", st)
    }
}
```

- [ ] **Step 2: 运行确认失败** → FAIL

- [ ] **Step 3: 实现**

`Module` 增加 `TestMode bool` 与 `Reset func()`。`Handler()` 在 `TestMode` 时挂 `POST /tenon/admin/reset`：清幂等表、收件箱、发件箱与游标，调用 `Reset()`；返回 `{"reset":true}`。

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交**

```bash
git add internal/module
git commit -m "feat(module): 测试态重置端点（仅 TENON_TEST_MODE=1），让 D1 两轮输入一致"
```

---

### Task 9: `cmd/quote` 触发器模块

**Files:**
- Create: `cmd/quote/main.go`
- Test: `cmd/quote/main_test.go`

- [ ] **Step 1: 写失败测试**（在包内构造 module 并直接断言发件箱）

```go
func TestQuoteCompletedEmitsEvent(t *testing.T) {
    m := newQuoteModule()
    srv := httptest.NewServer(m.Handler())
    defer srv.Close()

    st, body := postJSON(t, srv.URL+"/tenon/actions/quote.CreateStandardPartQuote",
        `{"quoteNo":"Q-1","lines":[{"sku":"STD-1002","qty":12}]}`,
        map[string]string{"Idempotency-Key": "tenon:t/Q-1"})
    if st != 200 { t.Fatalf("报价失败 %d %s", st, body) }

    _, out := getJSON(t, srv.URL+"/tenon/events?direction=out")
    if !strings.Contains(out, "quote.completed") { t.Fatalf("未发出 quote.completed：%s", out) }
    if !strings.Contains(out, "STD-1002") { t.Fatalf("事件载荷缺少明细：%s", out) }
}
```

- [ ] **Step 2: 运行确认失败** → FAIL

- [ ] **Step 3: 实现**

模块 `quote`，端口 `8083`（`PORT` 可覆盖）：
- `quote.CreateStandardPartQuote`（write，`idempotency.keyFields=["quoteNo"]`，dryRun 支持）→ 校验 lines 非空 → 生成 `quoteId` → **`EmitEvent`** `quote.completed`（`correlationId: "cor:quote/<quoteNo>"`，payload 含 lines）→ 返回 `{quoteId, status:"completed"}`。
- `quote.GetQuote`（read）→ 返回已建报价。

- [ ] **Step 4: 运行确认通过** → `go test ./cmd/quote/ -count=1` → PASS

- [ ] **Step 5: 提交**

```bash
git add cmd/quote
git commit -m "feat(quote): 触发器模块（报价完成 → 发件箱 quote.completed）"
```

---

## P3 · agent 运行时

### Task 10: LLM 客户端

**Files:**
- Create: `internal/llm/client.go`
- Test: `internal/llm/client_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestChatSendsJSONModeAndParsesContent(t *testing.T) {
    var gotBody map[string]any
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        _ = json.NewDecoder(r.Body).Decode(&gotBody)
        if r.Header.Get("Authorization") != "Bearer k" { t.Errorf("鉴权头不对") }
        w.Header().Set("Content-Type", "application/json")
        io.WriteString(w, `{"choices":[{"message":{"content":"{\"plan\":1}"}}]}`)
    }))
    defer srv.Close()

    c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "deepseek-flash", Timeout: 5 * time.Second})
    out, err := c.ChatJSON(context.Background(), "sys", "user")
    if err != nil { t.Fatal(err) }
    if strings.TrimSpace(string(out)) != `{"plan":1}` { t.Fatalf("内容解析错误：%s", out) }
    if gotBody["temperature"] != float64(0) { t.Fatalf("temperature 必须为 0，实际 %v", gotBody["temperature"]) }
    if rf, ok := gotBody["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
        t.Fatalf("必须要求 json_object：%v", gotBody["response_format"])
    }
}

func TestChatSurfacesHTTPError(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(429); io.WriteString(w, `{"error":{"message":"rate limited"}}`)
    }))
    defer srv.Close()
    c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
    if _, err := c.ChatJSON(context.Background(), "s", "u"); err == nil {
        t.Fatal("429 必须返回错误（上层据此判定 retryable）")
    }
}
```

- [ ] **Step 2: 运行确认失败** → `go test ./internal/llm/ -count=1` → FAIL

- [ ] **Step 3: 实现**

```go
type Config struct {
    BaseURL, APIKey, Model string
    Timeout                time.Duration
}
// ConfigFromEnv 读 TENON_LLM_BASE_URL / _API_KEY / _API_KEY_FILE（文件优先）/ _MODEL / _TIMEOUT。
// 缺 key 时返回错误——**不允许静默降级**（见设计 §5）。
func ConfigFromEnv() (Config, error)
// ChatJSON 发 /chat/completions，temperature=0、response_format={"type":"json_object"}，
// 返回 choices[0].message.content 的原始字节，由调用方做 schema 校验。
func (c *Client) ChatJSON(ctx context.Context, system, user string) ([]byte, error)
```

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交**

```bash
git add internal/llm
git commit -m "feat(llm): OpenAI 兼容客户端（temperature=0 + json_object + 缺 key 即失败）"
```

---

### Task 11: 规划器（上下文组装 + Plan 校验）

**Files:**
- Create: `internal/agent/planner.go`
- Test: `internal/agent/planner_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestPlanValidatedAgainstSchema(t *testing.T) {
    p := &Planner{LLM: fakeLLM(`{"planId":"pln_1","steps":"oops"}`), Spec: mustSpec(t)}
    _, err := p.Plan(context.Background(), PlanInput{Event: quoteCompleted(12)})
    if err == nil { t.Fatal("不合 schema 的 Plan 必须报错") }
}

func TestPromptMentionsOnlyDiscoveredActions(t *testing.T) {
    f := &recordingLLM{}
    p := &Planner{LLM: f, Spec: mustSpec(t)}
    _, _ = p.Plan(context.Background(), PlanInput{
        Event: quoteCompleted(12),
        Modules: []ModuleDigest{{ID: "inventory", Actions: []string{"inventory.GetStock", "inventory.DeductStock"}}},
    })
    if !strings.Contains(f.LastUser, "inventory.DeductStock") {
        t.Fatal("prompt 必须包含动态发现的 action id")
    }
    // 未注册的模块不得出现在 prompt 里（这是 D1 能成立的前提）
    if strings.Contains(f.LastUser, "procurement") {
        t.Fatal("prompt 里出现了未注册模块的 action")
    }
}
```

- [ ] **Step 2: 运行确认失败** → FAIL

- [ ] **Step 3: 实现**

`PlanInput` = 事件 + skill 文本 + `[]ModuleDigest`（id + action 摘要）。prompt 要求模型只输出 plan.schema.json 结构；输出用 `spec.Set.Validate("plan.schema.json", out)` 校验，失败则**带上校验错误重试 1 次**，再失败返回错误。`Plan` 结构复用 `conformance`/`internal` 的字段定义（不重复定义）。

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交** → `git commit -m "feat(agent): LLM 规划器（只用动态发现的 action，Plan 过 schema，坏输出重试一次）"`

---

### Task 12: 执行循环（闸门顺序 + 两段审计 + 结局分类）

**Files:**
- Create: `internal/agent/run.go`
- Test: `internal/agent/run_test.go`

- [ ] **Step 1: 写失败测试**（全部用 fake LLM + stub host/module，不花 token）

```go
func TestAuditDispatchedFailureAbortsExecution(t *testing.T) {
    env := newFakeEnv(t)
    env.Host.FailAuditPost = true
    res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})

    if res.Outcome != OutcomeFailed { t.Fatalf("期望 failed，得到 %q", res.Outcome) }
    if env.Module.WriteCalls != 0 { t.Fatal("审计 dispatched 写失败时必须拒绝执行（写调用必须为 0）") }
}

func TestGateOrder(t *testing.T) {
    env := newFakeEnv(t)
    _ = Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})
    want := []string{"policies:evaluate", "grants:evaluate", "dry-run", "audit:dispatched", "execute", "audit:result"}
    if !equalSeq(env.Trace, want) { t.Fatalf("闸门顺序不对：%v", env.Trace) }
}

func TestPolicyDeniedGoesNeedsHumanWithoutWorkaround(t *testing.T) {
    env := newFakeEnv(t)
    env.Host.PolicyOutcome = "deny"
    res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})
    if res.Outcome != OutcomeNeedsHuman { t.Fatalf("期望 needs_human，得到 %q", res.Outcome) }
    if env.Module.WriteCalls != 0 { t.Fatal("被策略拒绝后不得尝试替代路径") }
}

func TestBusinessRejectionReplansAtMostTwice(t *testing.T) {
    env := newFakeEnv(t)
    env.Module.AlwaysReject = "STOCK_INSUFFICIENT"
    res := Run(context.Background(), env.Deps(), RunInput{Event: quoteCompleted(12)})
    if res.Outcome != OutcomeNeedsHuman { t.Fatalf("期望 needs_human，得到 %q", res.Outcome) }
    if env.PlannerCalls != 3 { t.Fatalf("期望 1 次初始 + 2 次重规划，实际 %d", env.PlannerCalls) }
}
```

- [ ] **Step 2: 运行确认失败** → FAIL

> 测试辅助：`equalSeq(got, want []string) bool` 加到 `internal/agent` 的测试文件。

- [ ] **Step 3: 实现**

```go
type Outcome string

const (
    OutcomeCompleted  Outcome = "completed"
    OutcomeNeedsHuman Outcome = "needs_human"
    OutcomeFailed     Outcome = "failed"
)

// Run 的顺序（不得调换，Task 12 的 TestGateOrder 锁定）：
//   1 规划（Plan 过 schema）
//   2 policies:evaluate → deny 立即 needs_human
//   3 grants:evaluate  → not allowed 立即 needs_human
//   4 每个 write 的 :dry-run（unsupported 忽略；unsafe → needs_human）
//   5 每步执行前：POST /tenon/audit stage=dispatched，非 200 即终止
//   6 执行（Idempotency-Key 由 planId/stepId 派生）
//   7 POST /tenon/audit stage=result
//   8 按分类处理：retryable 退避≤3；business_rejection 重新规划≤2；policy_denied 直接 needs_human
//   9 发 Intent 帧（open + highlight）
```

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交** → `git commit -m "feat(agent): 执行循环（闸门顺序/两段审计/结局分类/重规划上限）"`

---

### Task 13: `cmd/agent`

**Files:**
- Create: `cmd/agent/main.go`
- Create: `skills/quote-to-fulfillment/SKILL.md`（`tenon.subscribes: ["quote.completed"]`；正文写业务语义，**不得出现 `模块.action` 形式**——C10 的 lint 会扫 skill 正文）
- Test: `cmd/agent/main_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestAgentRefusesToStartWithoutLLMKey(t *testing.T) {
    t.Setenv("TENON_LLM_API_KEY", "")
    t.Setenv("TENON_LLM_API_KEY_FILE", "")
    if err := run(context.Background(), nil); err == nil {
        t.Fatal("缺 key 必须启动即失败，不得静默降级")
    }
}
```

- [ ] **Step 2: 运行确认失败** → FAIL

- [ ] **Step 3: 实现**

配置：`TENON_HOST_URL`、`TENON_SKILL_DIR`（缺省 `skills/`）、`TENON_AGENT_ID`、`TENON_POLL_INTERVAL`（缺省 1s）、`TENON_ONCE`（跑一轮即退出，供 D1 用）。主循环：轮询各 enabled 模块 `events?direction=out` → 对每个新事件调 `agent.Run` → 记录结局。

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交** → `git commit -m "feat(agent): CLI 与轮询循环（--once 供 D1 使用）"`

---

## P4 · D1 与收口

### Task 14: `tenon d1 run`

**Files:**
- Create: `internal/d1/runner.go`
- Modify: `cmd/tenon/main.go`
- Test: `internal/d1/runner_test.go`

- [ ] **Step 1: 写失败测试**（用 stub host/quote 验判定逻辑，不跑真 LLM）

```go
func TestD1FailsWhenPlansIdentical(t *testing.T) {
    st := stubWorld(t)
    st.PlanA = []string{"inventory.DeductStock"}
    st.PlanB = []string{"inventory.DeductStock"}      // 摘掉模块后行为没变 → 说明没用清单
    rep, err := Run(context.Background(), st.Config())
    if err != nil { t.Fatal(err) }
    if rep.Verdict != "fail" { t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason) }
}

func TestD1PassesWhenAdaptive(t *testing.T) {
    st := stubWorld(t)
    st.PlanA = []string{"inventory.DeductStock", "procurement.CreatePurchaseRequest"}
    st.PlanB = []string{"inventory.DeductStock"}
    st.OutcomeB = "needs_human"
    rep, _ := Run(context.Background(), st.Config())
    if rep.Verdict != "pass" { t.Fatalf("期望 pass，得到 %s（%s）", rep.Verdict, rep.Reason) }
}
```

- [ ] **Step 2: 运行确认失败** → FAIL

- [ ] **Step 3: 实现**

```go
// Run 的 A/B 两轮：
//   A: admin 注册 quote+inventory+procurement → reset 全部 → 触发报价 →
//      等 agent 一轮（TENON_ONCE）→ 从 GET /tenon/audit?correlationId= 提取 action 序列与结局
//   B: admin 摘除 procurement → reset 全部 → 同一报价参数 → 同上
// 判定：
//   pass  ⇔ seqA ≠ seqB 且 planB 不含被摘除模块的 action 且 outcomeB == needs_human 且无 error
//   fail  ⇔ 其余（reason 必须具体：序列相同 / B 仍在调 procurement / B 崩了 / B 没转人工）
```

`cmd/tenon` 增加子命令 `d1 run --host --quote --inventory --procurement --report out/d1.json`。

- [ ] **Step 4: 运行确认通过** → PASS

- [ ] **Step 5: 提交** → `git commit -m "feat(d1): A/B 差异测试 runner（摘模块→行为必须改变且转人工）"`

---

### Task 15: 真跑 D1（用真 LLM）

- [ ] **Step 1: 起全套进程**

```bash
go build -o out/ ./cmd/host ./cmd/agent ./cmd/quote ./cmd/inventory ./cmd/procurement ./cmd/tenon
TENON_HMAC_SECRET_FILE=out/hmac.txt TENON_AUDIT_PATH=out/audit.jsonl \
TENON_POLICY_FILE=config/policies.dev.json TENON_GRANT_FILE=config/grants.dev.json \
TENON_MODULES=inventory=http://127.0.0.1:8081,procurement=http://127.0.0.1:8082,quote=http://127.0.0.1:8083 \
TENON_TEST_MODE=1 ./out/host &
```

- [ ] **Step 2: 跑 D1**

```bash
TENON_LLM_API_KEY_FILE=out/deepseek.key ./out/tenon d1 run \
  --host http://127.0.0.1:8080 --quote http://127.0.0.1:8083 \
  --inventory http://127.0.0.1:8081 --procurement http://127.0.0.1:8082 \
  --report out/d1.json
```

Expected: `verdict=pass`，报告里 `planA` 含 `procurement.*`、`planB` 不含、`outcomeB=needs_human`。

- [ ] **Step 3: 若失败，按 reason 定位**（序列相同 → 模型没在推理或 prompt 泄漏；B 仍在调 procurement → 注册表摘除未生效；B 崩了 → 结局分类缺陷）

- [ ] **Step 4: 提交报告与调参记录**

```bash
git add out/d1.json 2>/dev/null || true
git commit --allow-empty -m "test(d1): 真 LLM 差异测试结果（verdict=pass，非确定性重试次数记录在报告里）"
```

---

### Task 16: 文档与容器收口

**Files:**
- Modify: `docs/interfaces.md`（+`POST /tenon/audit`、`GET /tenon/modules`、`direction=out`、TEST_MODE 面）
- Modify: `spec/0.1/CHANGELOG.md`（记录三处扩展，标明属 v0.2 提案）
- Modify: `compose.m0.yaml`（+host/agent/quote 服务，端口 8083）
- Modify: `docker/README.md`、`README.md`（状态表更新）
- Create: `skills/quote-to-fulfillment/SKILL.md`

- [ ] **Step 1: 更新接口文档与 CHANGELOG**（逐条对应 §2 的三处扩展）
- [ ] **Step 2: 补 skill 文件**（`tenon.subscribes: ["quote.completed"]`，正文不得出现 `模块.action` —— C10 会扫）
- [ ] **Step 3: 更新 compose.m0.yaml**，`docker compose -f compose.m0.yaml config` 必须通过
- [ ] **Step 4: 全量检查**

```bash
go vet ./... && go build ./... && go test ./... -count=1 && gofmt -l .
node scripts/validate-spec.mjs
docker compose -f compose.m0.yaml config --quiet
```
Expected: 全部 exit 0，gofmt 空输出。

- [ ] **Step 5: 提交**

```bash
git add -A
git commit -m "docs: M0.5 收口（接口扩展、skill、compose、状态表）"
```

---

## 验收清单（全部满足才算 M0.5 完成）

- [ ] `go test ./... -count=1` 全绿；`gofmt -l .` 空；`go vet ./...` 干净
- [ ] conformance 指向 host 时 **C4、C5 为 PASS**（不再是 SKIP）
- [ ] `tenon d1 run` 输出 `verdict=pass`，报告含 A/B 两轮 action 序列与结局
- [ ] B 轮结局是 `needs_human`，不是 `failed`、不是异常退出
- [ ] 注入 `dispatched` 审计写失败 → 写调用数为 0（`TestAuditDispatchedFailureAbortsExecution`）
- [ ] 缺 LLM key 时 agent 启动即失败（`TestAgentRefusesToStartWithoutLLMKey`）
- [ ] `docs/interfaces.md` 里三处扩展与两个测试面均已记录，且标明是 v0.2 提案而非 0.1 既有
- [ ] 密钥没有出现在任何被提交的文件里（`git log -p | grep -c 'sk-'` 为 0）
