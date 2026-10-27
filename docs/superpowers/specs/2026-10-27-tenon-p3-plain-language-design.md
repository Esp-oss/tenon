# P3 设计：白话渲染（`summaryTemplate` 从声明到实现）

- 状态：**待评审**
- 日期：2026-10-27
- 关系：落实 [`2026-10-27-tenon-codrive-chat-entry-design.md`](2026-10-27-tenon-codrive-chat-entry-design.md) §5 的 R1–R5 与 §10 验收线第 3 步；P1/P2 已落地（`74182cd`），同源静态页已在 `586de2f`。
- 前置：`skills/` 与 `config/` 不动；本切片只改契约（两处 schema）、模块声明、agent 渲染与静态页。

---

## 0. 目标与非目标

**目标**

1. **卡片默认是业务人话**：审批人看到的每个 step 是一句「扣减 STD-1002 库存 3 件（单据 Q-2026-0417）」，不是 `inventory.DeductStock（{...}）`。
2. **数字只来自 `args`**：模型不参与数字（R3 的延伸），渲染器不做求和、不推断缺口、不读 `outputSchema`。
3. **技术标识只在折叠区可见**（R5）：卡面正文不出现 action id / `planId` / 闸门结论。
4. **契约与实现一致**：CHANGELOG 0.2.0 已声明 `summaryTemplate`，但 schema 里**没有**这个字段（本次核查：`spec/` 全目录只有 CHANGELOG 命中它）。本切片把它补进 schema 并由三个模块真的填上。

**非目标（本切片不做）**

| 不做 | 归属 / 理由 |
|---|---|
| plan 级模板、跨 step 取数 | 已否决：会让「12 件」这类合计凭空出现，破坏 R1 |
| 加载期模板占位符校验（fail-closed） | 已否决：模板只影响可读性，写错不该让审批路径瘫痪 |
| i18n / 多语言 | 无需求 |
| 从 `outputSchema` 渲染结果值 | 结果值属 Intent，不属审批卡 |
| 让模型写模板 | 模板是模块作者的责任，模型只写 `rationale` |
| `quote.lines`（数组）的白话 | 数组不可渲染（R1），明细进折叠区 |
| UI 构建链 / 框架 | 页面仍是原生 JS 静态文件 |

---

## 1. 事实基线

| 现状 | 位置 | 后果 |
|---|---|---|
| `summaryTemplate` 只在 CHANGELOG 里声明 | `spec/0.1/CHANGELOG.md:73,77`；两份 schema 均无此字段 | 模块想填也填不进去：`ActionSummary` 与 `Action` 都是 `additionalProperties:false` |
| `module.Action` 无该字段 | `internal/module`（`Action{ID,Version,Title,Kind,InputSchema,…}`） | 序列化不出模板，摘要线拿不到 |
| agent 的能力发现只带 action id | `internal/agent/planner.go:77-82` `ModuleDigest{ID, Actions []string}` | agent 手里没有任何模板文本 |
| 渲染是兜底串 | `internal/agent/suspend.go:166-186` | 输出 `action（args 摘要）`，正是验收线禁止的样子 |
| `summary` 是 `labels` 拼接 | `internal/agent/run.go:450-451` | 一处渲染、两处复用（公告板 / Intent 帧 / 归档） |
| 公告板条目 `items` 是 `[]map[string]any` | `internal/controlplane/pending.go:25` | 可以在**不改 host 结构体**的前提下多带一个 `detail` 键 |
| 卡片把 `planId` / `requestId` 写在可见 meta | `ui/app.js:488-492` | 违反 R5 |
| 验收线要求卡上同时有「12 件」「9 件」 | `…chat-entry-design.md:607` | 12 不在任何 step 的 `args` 里（`skills/quote-to-fulfillment/SKILL.md:29` 与 `demo/mini-erp/README.md:82` 都是「可用 3、需求 12、扣 3、缺 9」），按 R1 渲染不出 |

---

## 2. 契约增量（spec 0.1 → 0.2）

| 文件 | 改动 | 为什么必须两处都改 |
|---|---|---|
| `spec/0.1/schemas/module-manifest.schema.json` → `$defs/ActionSummary` | 加可选 `summaryTemplate`：`string`，`maxLength: 200`，说明「人话模板，占位符 `{argName}` 只从该动作 `args` 取值」 | agent 走摘要线（`GET /.well-known/tenon`）做发现；模板必须在这条线上到位，否则每次渲染都要回拉完整 Action（违反 D15「摘要先看、完整按需」） |
| `spec/0.1/schemas/action.schema.json` → Action | 同一字段、同一语义 | `module.Action` 是**同一实体**，完整 Action 响应也从它序列化；`additionalProperties:false`，不加就无法透出 |

模板语法（写进 schema 的 `description`，由实现遵守）：

- 占位符形如 `{qty}`，名字须匹配 `^[A-Za-z_][A-Za-z0-9_]*$`；
- 占位符**只能**取该动作 `inputSchema.properties` 里出现的名字（不做跨 step、不做计算）——这是给模块作者的约定，**不做加载期强校验**（见 §6）；
- 模板不得含技术标识（action id、planId、闸门名）：这些东西由 `details` 承载，只在折叠区出现。

`CHANGELOG.md` 的 0.2.0 段**不改**：它声明的内容本切片正是要兑现。

---

## 3. 组件与数据流

```
模块 manifest（Action.SummaryTemplate）
   ↓ GET /.well-known/tenon（摘要线，带模板）
agent discovery → ModuleDigest.ActionMeta{Title, SummaryTemplate}
   ↓ buildPending → renderSteps(plan, meta)
Pending{Labels 白话行, Details 技术行, Summary = join(Labels,"；")}
   ↓ 落盘 pending/*.json（渲染一次，恢复时复用，不重算）
   ↓ 公告板推送 items[{label, detail}]
host 公告板（read-model，原样存 map）→ GET /tenon/pending
   ↓
ui/app.js：卡面 = summary + 徽章；折叠区 = details + planId/requestId/角色/到期
```

| 文件 | 动作 |
|---|---|
| `spec/0.1/schemas/module-manifest.schema.json` | 修改：`ActionSummary` 加 `summaryTemplate` |
| `spec/0.1/schemas/action.schema.json` | 修改：Action 加同一字段 |
| `internal/module/*.go` | 修改：`Action.SummaryTemplate` + `actionSummary` 透出 |
| `cmd/inventory/main.go`、`cmd/procurement/main.go`、`cmd/quote/main.go` | 修改：填模板（见 §4 表） |
| `internal/agent/planner.go` | 修改：`ModuleDigest` 加 `ActionMeta map[string]ActionMeta`（`Actions []string` 保留，`checkActionsDiscovered` 与 prompt 依赖它） |
| `internal/agent/run.go` | 修改：discovery 填 `ActionMeta`；`buildPending` 调新的 `renderSteps` |
| `internal/agent/suspend.go` | 修改：`renderLabels` → `renderSteps(plan, meta) (labels, details []string)` |
| `internal/agent/state.go` | 修改：`Pending` 加 `Details []string`（`omitempty`） |
| `internal/agent/board.go` | 修改：推送 `items[{label, detail}]`（`detail` 为空则省略） |
| `ui/app.js` | 修改：卡面只留 summary + 徽章；`planId`/`requestId`/`labels`/`detail` 进 `<details>` |
| `docs/interfaces.md` §4.7 | 修改：公告板条目 `items[]` 补可选 `detail`（技术原文，只给折叠区用） |

---

## 4. 渲染规则（R1–R5 落地细则）

| 情况 | 行为 |
|---|---|
| 模板存在，且所有占位符都在 `args` 里、值都是标量（string / number / bool） | 代入渲染 |
| 模板缺失 | **R2**：`Title + 关键参数`，形如 `扣减库存（sku=STD-1002, qty=3）`；**不含 action id** |
| 占位符在 `args` 里不存在 / 值为数组、对象、null | 该 step 退 R2（不输出半个句子） |
| 模板未闭合（落单 `{`）或占位符名非法 | 该 step 退 R2 |
| 渲染结果超 120 个 rune | 截断加 `…`（R4，与 Intent `badge` 上限一致） |
| `Title` 与模板都缺 | 输出「未命名动作（参数见详情）」；action id 只进 `details`，**不进** `labels` |

三个模块的模板（占位符严格取各自 `inputSchema.properties`）：

| 动作 | `args` 字段 | 模板 |
|---|---|---|
| `inventory.DeductStock` | `sku`、`qty`、`referenceNo` | `扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）` |
| `inventory.GetStock` | `sku` | `查询 {sku} 在库量` |
| `inventory.ReleaseStock` | `sku`、`qty`、`referenceNo` | `回补 {sku} 库存 {qty} 件（单据 {referenceNo}）` |
| `procurement.CreatePurchaseRequest` | `sku`、`qty`、`referenceNo` | `缺 {qty} 件 {sku}，发起采购申请（单据 {referenceNo}）` |
| `quote.CreateStandardPartQuote` | `quoteNo`、`lines`（数组） | `按报价单 {quoteNo} 生成需求`（`lines` 明细进 `details`） |

`details` 每 step 一条：`<action id>（<args 原文，≤120 rune>）`。它只服务折叠区，不参与 `summary`。

---

## 5. 渲染只发生一次

`buildPending` 渲染并落盘，`Resume` **不重算**（`run.go:410-413` 已写死这条性质）：模板日后被改，审批人看到的仍是当初那份文字。若 `details` 缺失（旧 pending 文件），渲染退化为 `labels` 单列，不报错。

---

## 6. 错误与降级

| 情况 | 行为 |
|---|---|
| 模板写坏（未闭合 / 非法占位符 / 缺字段 / 值非标量） | 该 step 退 R2，**挂起照常**（可读性失败不阻断审批） |
| 摘要线里模块没给模板（老模块） | 全部 step 走 R2，行为与今天一致但不再泄露 action id |
| `ActionMeta` 为空（未做能力发现） | 全部走 R2 |
| 公告板条目没带 `detail`（老 agent） | UI 折叠区只显示 `planId` 等，不报错 |
| 模板超长 | 截断（R4），不报错 |

---

## 7. 测试策略

| 用例 | 位置 | 断言 |
|---|---|---|
| 模板正常渲染 | `internal/agent/suspend_test.go`（表驱动） | 输出等于期望中文句 |
| 缺占位符 / 值非标量 / 未闭合 / 非法名 | 同上 | 退 R2，**且输出不含 action id** |
| 无模板 | 同上 | R2 用 `Title`，不含 action id |
| `Title` 也缺 | 同上 | 输出「未命名动作…」，action id 只在 `details` |
| 超 120 rune | 同上 | 截断且带 `…`，长度 ≤120 |
| discovery 带模板 | `internal/agent/run_test.go` | 假模块 `/.well-known/tenon` 带 `summaryTemplate` → `ActionMeta` 非空 |
| 挂起路径落到 pending | `internal/agent/run_test.go` / `resume_test.go` | `Pending.Labels` 是白话、`Summary` 是拼接、`Details` 非空 |
| 公告板推送形状 | `internal/agent/board_test.go` | `items[0]` 同时有 `label` 与 `detail` |
| 宿主原样透传 | `internal/controlplane/pending_test.go` | 推 `{label,detail}` → GET 原样带回 |
| UI 折叠 | 人工验收（无 JS 测试） | 卡面正文不含 `planId`，展开后可见 |

---

## 8. 验收线（替换 `…chat-entry-design.md` §10 第 3 步的第 1 项）

1. 卡面 `summary` 由模板渲染，本场景期望「扣减 STD-1002 库存 3 件」与「缺 9 件 STD-1002，发起采购申请」；
2. **卡上每个数字都能在对应 step 的 `args` 里找到**（机械判据：从 pending 文件取 args，逐数字比对卡面文本）；
3. 卡面正文（`summary` + `labels`）不含 `DeductStock`、`planId`、`G1`、`policy`；
4. 展开详情能看到 action id 与参数原文；
5. 非审批人（alice）看不到「批准」按钮（P2 已有，回归确认）。

> 「12 件」不再作为验收项：它不在任何一个 step 的 `args` 里，按 R1 渲染它只能靠编。设计 §10 的措辞据此更正，理由记在本文件 §1 末行。

---

## 9. 风险与已知缺口

| 风险 | 影响 | 处理 |
|---|---|---|
| 模块改了 `inputSchema` 忘改模板 | 该动作永久退 R2 | 接受（已否决加载期校验）；模板与 schema 同文件、同一处 review |
| 模板措辞质量依赖模块作者 | 白话可能读起来别扭 | 三个模板随本切片一次性评审；后续改措辞只动模块 |
| 数组/嵌套对象不可渲染 | `quote.lines` 只能进详情 | 已知；若将来需要，另立「结构化参数渲染」议题 |
| `details` 让 `items` 形状变化 | 老 agent 不推 `detail` | `omitempty` + UI 容错，已列入测试 |
| 白话里带单据号可能变长 | 卡面变长 | 有 120 rune 上限兜底 |

---

## 10. 交付物

- 两份 schema + `internal/module` + 三个模块的模板
- `internal/agent`：`ActionMeta`、`renderSteps`、`Pending.Details`、board 推送
- `ui/app.js` 折叠改造
- `docs/interfaces.md` §4.7 补 `detail`
- 上表全部测试
- 一份实测截图或 pending 文件证据，证明卡面文字与数字来源
