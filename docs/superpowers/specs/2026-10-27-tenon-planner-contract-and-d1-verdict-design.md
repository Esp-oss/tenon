# 规划输入契约与 D1 实质判定 设计

- 状态：**待评审**
- 日期：2026-10-27
- 关系：接 [`2026-10-27-tenon-d1-verdict-closeout-design.md`](2026-10-27-tenon-d1-verdict-closeout-design.md)（A 切片：判定作业 + 三态判据）与 [`2026-10-27-tenon-p3-plain-language-design.md`](2026-10-27-tenon-p3-plain-language-design.md)（B 切片：白话）。
- 前置：`feat/m05-host-agent` 已推到远端（`563594d`）；CI push/PR 双绿；D1 三次真跑均为「判定受阻 / 空洞通过 / B 轮非法计划」。

---

## 0. 目标与非目标

**目标**

1. **规划期信息够用**：模型能拿到每个动作的**必填字段名**（`requiredArgs`）与标题，从而写出 schema 合法、可被策略放行的计划。
2. **对话入口在真模型下真的可用**：一句人话 → 计划 → 策略 → 挂起或执行 → 批准后真改库存 → 审计带 `approvedBy`。
3. **D1 给出实质结论**：`pass` 或 `fail`，不再是 `inconclusive`。判据四条合取**一个字不改**；改的是场景输入，让判据具备区分力。

**非目标**

| 不做 | 理由 |
|---|---|
| 改 D1 四条判据 | 改判据等于自己发合格证 |
| 把完整 `inputSchema` 塞进 prompt | 违反 D15（摘要先看、完整按需）；`requiredArgs` 已够治本次实测的失败 |
| 加 `origin=request` 的万能 approve | 那是用「人工」兑掉「策略覆盖」，fail-closed 名存实亡 |
| 让模型产出 `rationale` 之外的业务判断 | 数字与事实不交给模型 |
| 规划质量的多轮统计基准 | 单次判定 + 明确写「不代表稳定性」，统计基准另立议题 |

---

## 1. 事实基线（均有实证）

| 现状 | 位置 | 后果 |
|---|---|---|
| prompt 只给 action id | [`planner.go:340-353`](../../internal/agent/planner.go) | 真模型规划 `DeductStock` 时漏了 `referenceNo` → 预演 400 `SCHEMA_MISMATCH` |
| `ActionMeta` 有 `Title`/`SummaryTemplate`，但 prompt 不用 | `internal/agent/planner.go`（Task 4 已埋） | 模型连「这个动作干什么」都没看到，只有 id 字符串 |
| 执行/预演期已会按需拉 `/tenon/actions/{id}` | `run.go:715-726` | 说明「按需拉取」是接线，不是新建能力 |
| dev 策略只覆盖 4 条 | `config/policies.dev.json` | `DeductStock qty≤10` 无规则 → 默认 deny；`ReleaseStock` 无规则 → deny；`procurement` 只匹配 `origin=request` → **D1 的 A 轮（event 来源）采购必被拒** |
| 策略拒绝时不记录计划里的动作 | `run.go` 的 deny 分支 | 今晚那条请求 `动作=[]`，查不出被哪个动作挡的 |
| D1 场景库存 3、需求 12 | `internal/d1` 默认 `STD-1002` qty 12；`TENON_INVENTORY_SEED` 默认 `STD-1002:3` | 「先扣 3 再采 9」与「只查库存」都算合理 → 判据可被架空 |

---

## 2. 契约增量

| 文件 | 改动 | 语义 |
|---|---|---|
| `spec/0.1/schemas/module-manifest.schema.json` → `ActionSummary` | 加可选 `requiredArgs`：`string[]`，`uniqueItems: true`，`maxItems: 32`，元素 `pattern ^[A-Za-z_][A-Za-z0-9_]*$` | 「要把这个动作调起来，至少得给这些字段」。**不含类型**——类型由模块侧校验兜 |
| `spec/0.1/schemas/action.schema.json` → Action | 同一字段 | `module.Action` 是同一实体，两处响应同源 |
| `spec/0.1/CHANGELOG.md` | 0.2.0 段补一条 | 与既有「先改 schema、再改语义、最后 CHANGELOG」的顺序一致 |

**约束（模块加载期 fail-closed）**：声明了 `requiredArgs` 时，每个元素必须出现在该动作 `inputSchema.required` 里，否则模块启动失败。理由与 `idempotency.keyFields ⊆ inputSchema.required` 同源：声明与契约不一致是装配错误，不该等到运行期才发现。

---

## 3. 组件与数据流

```
模块 Action{RequiredArgs} → manifest ActionSummary.requiredArgs
   ↓ GET /.well-known/tenon（摘要线，D15）
agent ActionMeta{Title, SummaryTemplate, RequiredArgs}
   ↓ userPrompt 渲染「可调动作」块
- inventory.DeductStock（扣减库存；必填：sku, qty, referenceNo）
   ↓ 模型产计划（args 至少齐必填）→ plan.schema.json 校验 → 策略 → 挂起/执行
```

| 文件 | 动作 |
|---|---|
| `internal/module/module.go` | 修改：`Action.RequiredArgs`、`actionSummary.RequiredArgs`、`Validate` 的 `⊆ required` 断言 |
| `cmd/inventory`、`cmd/procurement`、`cmd/quote` | 修改：填 `RequiredArgs`（与各自动作 `inputSchema.required` 一致） |
| `internal/agent/planner.go` | 修改：`ActionMeta.RequiredArgs`；`userPrompt` 的「可调动作」块带 `title` 与必填字段 |
| `internal/agent/run.go` | 修改：`manifestActions` 解析 `requiredArgs`；策略 deny 时把计划动作写进 `Detail` |
| `config/policies.dev.json` | 修改：见 §4 |

**prompt 渲染规则**（保持确定性：模块与动作排序，同一输入逐字相同）

```
可调动作（本次唯一可用的清单，来自各模块 manifest 的动态发现）：
- 模块 inventory：
    - inventory.DeductStock（扣减库存；必填：qty, referenceNo, sku）
    - inventory.GetStock（查询在库量；必填：sku）
```

- `requiredArgs` 按字典序输出（避免模块声明顺序影响 prompt 字节）；
- 未声明 `requiredArgs` 的动作只显示标题（老模块兼容）；
- 标题为空时只显示 id（不阻断）。

---

## 4. 策略覆盖（只补 skill 域内动作）

| 规则 | match | when | outcome | 理由 |
|---|---|---|---|---|
| `pol-inventory-deduct-auto-small`（新增） | inventory / `DeductStock` | `arg.qty <= 10` | `auto` | 小量扣减是日常操作；现在没有任何规则覆盖它 → 默认 deny |
| `pol-inventory-deduct-approve`（保留） | inventory / `DeductStock` | `arg.qty > 10` | `approve`（warehouse_manager） | 高风险仍需人批 |
| `pol-inventory-release-approve`（新增） | inventory / `ReleaseStock` | 恒真 | `approve`（warehouse_manager） | 补偿动作必须留痕并经人 |
| `pol-procurement-create-approve`（改造） | procurement / `CreatePurchaseRequest` | **恒真**（删掉 `origin=request` 限制） | `approve`（procurement_manager） | 事件来源同样要能采购；否则 D1 的 A 轮必被拒 |
| `pol-quote-create-deny`（新增） | quote / `CreateStandardPartQuote` | 恒真 | `deny` | 触发器动作不该由 agent 调；显式 deny 比「未覆盖」可诊断 |
| `pol-inventory-getstock-auto`、`pol-quote-getquote-auto`（保留） | — | 恒真 | `auto` | 只读 |

> 仍然保持「**未覆盖即 deny**」：上面是补齐业务域，不是加兜底。新增动作仍然必须显式声明规则。

**一处改名要说清**：原 `plat-request-needs-human` 改名为 `pol-procurement-create-approve`（因为它不再只匹配 request，旧名会撒谎）。**历史审计里的旧 `policyId` 不改写**——审计是不可变记录，改名不回溯；对账时按「当时的策略文件 + 审计里的 policyId」解释。

**可观测**：策略结论为 `deny`/`approve` 而转人工时，`RunResult.Detail` 里追加计划动作清单（例：`计划动作=[inventory.ReleaseStock]`），并同样写进 `TENON_RESULT_FILE` 的结果行。目的只有一个：下次被拒时不用猜。

---

## 5. D1 场景：让判据具备区分力

**改动**：`scripts/d1-e2e.ps1` 起 inventory 时设 `TENON_INVENTORY_SEED='STD-1002:0,STD-1001:120'`。
（`internal/d1` 的默认 SKU/数量不变：`STD-1002`、12。）

**为什么这样就能判**：

- 需求 12、在库 **0** → 扣减在业务上不成立，**唯一合理的写路径就是 `procurement.CreatePurchaseRequest`**（被摘的那个模块）；
- A 轮：任何会推理的 agent 都要规划采购 → 必然引用被摘模块；
- B 轮：procurement 被摘 → 能力清单里没有它 → 会推理的 agent 只能 `needs_human`；硬编码映射的实现会照旧调它 → 判据 `filterRemoved(b.Actions)` 立刻抓住；
- 四条判据一字不改；`inconclusive` 仍保留，但验收要求它**不再出现**。

**不改的部分**：`decide()` 的四条合取、`inconclusive` 触发条件、报告结构。

---

## 6. 错误与降级

| 情况 | 行为 |
|---|---|
| 模块声明 `requiredArgs` 但不在 `inputSchema.required` 里 | 模块启动失败（装配错误，fail-closed） |
| 模型仍漏必填字段 | 模块侧 `SCHEMA_MISMATCH` 400 → agent 按现有 retry 逻辑处理；不新增静默兜底 |
| 策略仍未覆盖某动作 | 默认 deny（保持语义）+ `Detail` 里能看到计划动作 |
| 真模型仍未在 A 轮规划采购 | D1 记 `inconclusive` 并如实写原因；**这是模型没按业务规则规划**的结论，不改成 pass |
| 真模型偶发不合法计划 | 脚本显式失败并打印 agent 日志尾部；不自动重试到"出结果为止" |

---

## 7. 测试策略

| 用例 | 位置 | 断言 |
|---|---|---|
| `requiredArgs` 被两处 schema 接受 / 类型错误被拒 | `internal/spec/spec_test.go` | 与 `summaryTemplate` 同一写法 |
| 摘要线透出 `requiredArgs` | `internal/module/module_test.go` | 有则出现、空则不出现 |
| `requiredArgs ⊄ inputSchema.required` 时 `Validate` 失败 | `internal/module/module_test.go` | 启动即失败 |
| prompt 渲染标题与必填字段 | `internal/agent/planner_test.go` | 逐字断言 `（扣减库存；必填：qty, referenceNo, sku）`；且 action id 仍只来自清单 |
| 策略：小量扣减 auto / 大量 approve / ReleaseStock approve / procurement 两来源都 approve / quote.Create deny | `internal/controlplane/policy/*_test.go` 或规则加载用例 | outcome 与 policyId |
| 策略 deny 时 `Detail` 带计划动作 | `internal/agent/run_test.go` | `strings.Contains(detail, "计划动作=")` |
| 真模型对话可用 | `scripts/verify-ai.ps1`（新） | 见 §8 |
| D1 实质结论 | `scripts/d1-e2e.ps1` | `verdict ∈ {pass, fail}` |

---

## 8. 验收线（双判据，都可脚本复跑）

**判据 1：真模型对话可用**（`scripts/verify-ai.ps1`）

1. `-Real` 起栈（`deepseek-flash`），alice 登录；
2. 投一句「扣 5 件 STD-1001」；
3. 断言：结果行 `outcome ∈ {needs_human, completed}` 且 `planId` 非空、计划动作 ⊆ 能力清单；
4. 若挂起：bob 批准 → `archived/*.json` 出现 `done`，inventory 的 `onHand` 真的减少 5；
5. 宿主审计该 planId 的 `result` 段 `approvedBy=user:bob`、`prevHash` 链连续。

**判据 2：D1 出实质结论**（`scripts/d1-e2e.ps1`，新种子）

1. `verdict` 必须是 `pass` 或 `fail`；出现 `inconclusive` 即**本轮验收不通过**（说明场景或 prompt 还没到位），如实记录原因，**不把 inconclusive 改写成 pass**；
2. 报告与结论写进 `README.md` 的「D1 判定结果」一节（第 4 次尝试）。

**判据 3：不回归**

`go test ./...`、`pnpm check`、`scripts/verify-ui.ps1`（白话验收）全绿。

---

## 9. 风险与已知缺口

| 风险 | 影响 | 处理 |
|---|---|---|
| 模型在库存 0 时仍只做只读检查 | A 轮不引用 procurement → inconclusive | 如实记录为「模型未按 skill 规划」；不改判据。这也是为何 §4 要显式 deny 无关动作——让「能做什么」更清楚 |
| `requiredArgs` 与真实 `inputSchema.required` 漂移 | 模块启动失败 | 加载期断言兜住（fail-closed），代价是装配问题更早暴露 |
| 小量扣减 `auto` 过宽 | 5 件、10 件无人批 | 阈值取 10 与既有规则对齐；真实上限应由业务方给，本轮只保证语义一致 |
| 真模型随机性 | 单次通过 ≠ 稳定 | 报告写明「单次判定」 |
| 策略放宽了 procurement（去掉 origin 限制） | 事件来源也能采购 | 仍要求 `approve`（procurement_manager），不是自动执行；风险受审批人约束 |

---

## 10. 交付物

- 两份 schema + CHANGELOG 条目；`internal/module` 校验与三模块声明
- `internal/agent` 的 prompt 渲染与 deny 可观测
- `config/policies.dev.json` 五处规则调整（含两处新增、一处改造）
- `scripts/d1-e2e.ps1` 的库存种子与断言收紧
- `scripts/verify-ai.ps1`（新）+ README 的 D1 第 4 次判定记录
- §7 全部用例
