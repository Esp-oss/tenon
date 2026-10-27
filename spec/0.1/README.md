# Tenon 协议规范 · 0.1

| 项 | 值 |
|---|---|
| 协议名 | Tenon（榫卯） |
| 版本 | 0.1 |
| 状态 | 初始草案 |
| 许可 | Apache-2.0 |
| 线格式真源 | [`schemas/`](schemas)（10 个 JSON Schema 文件） |
| 设计依据 | [Tenon 设计文档](../../docs/superpowers/specs/2026-10-27-tenon-ai-driven-system-design.md) |

Tenon 是一个**业务治理与执行语义协议**：模块按协议暴露能力，agent 通过能力发现自行规划，执行前依次过 dry-run、策略、授权、模块前置条件四道闸门，全过程留可回放的审计。传输（HTTP / MCP / Webhook）只是它的承载，业务语义不依赖于任何传输或运行时。

---

## 1. 本目录的边界

**JSON Schema 是唯一真源。** 每个契约的字段名、类型、必填性、取值域、条件约束只写在 `schemas/*.json` 里。本目录的 Markdown 只写三件 schema 写不下的东西：

1. **语义**：这个字段在做决定时意味着什么（例如 `dryRun.status=unsafe` 会强制审批）。
2. **理由**：为什么这样约束（例如为什么 `expectedDiff[].entity` 与审计的 `effects.entities` 必须同一种寻址——否则对账两侧无法比对）。
3. **流程**：状态迁移、重试/补偿顺序、版本协商与弃用这类跨报文的行为。

因此本目录的 Markdown **不写字段表**：字段表由构建脚本从 schema 生成（设计文档 §11 仓库结构中的 `scripts/build-spec.mts`，尚未落地）。手抄字段表会让真源分叉，这是 [CONTRIBUTING](../../CONTRIBUTING.md) 的硬规则。

修改顺序也由此确定：**先改 schema，再改语义文档，最后更新 [`CHANGELOG.md`](CHANGELOG.md)**。

---

## 2. 九类契约

计数口径见设计文档 §4：Manifest / Action / Event / Plan / Intent-Context-Confirm / Grant / Policy / Audit / Error = **9 类**。

| # | 契约 | schema | 方向 | 它承诺什么 | 语义在哪 |
|---|---|---|---|---|---|
| 1 | Module Manifest（发现） | [`module-manifest.schema.json`](schemas/module-manifest.schema.json) | 模块 → 系统 | 摘要级能力清单：有哪些动作、什么风险、能否预演。**不含 inputSchema**，命中候选后再按 id 拉完整定义，控上下文成本 | 本文 §4、§5 |
| 2 | Action（动作） | [`action.schema.json`](schemas/action.schema.json) | 模块 → 系统 | 一个动作的完整约定：读写分类、入参出参 schema、风险、幂等键、补偿、超时、预演能力、配额计量单位 | [`semantics.md`](semantics.md) §3–§5 |
| 3 | Domain Event（领域事件） | [`domain-event.schema.json`](schemas/domain-event.schema.json) | 模块 → agent | 业务事实已发生：至少一次投递、可重放、按 `eventId` 去重。**不含「该调哪个动作」的暗示** | [`semantics.md`](semantics.md) §8 |
| 4 | Plan（计划） | [`plan.schema.json`](schemas/plan.schema.json) | agent → 系统 | 可审阅的执行计划：DAG 步骤、预期数据差异、预演四态结论、理由。必须人能读懂、可被策略评估、可回放重建 | [`state-machines.md`](state-machines.md) §2 |
| 5 | Intent / Context / Confirm（三帧） | [`frames.schema.json`](schemas/frames.schema.json) | 双向 | 人机共驾的唯一三种报文。**帧内不含 DOM 选择器、不含坐标**，只表达界面应进入什么状态 | [`semantics.md`](semantics.md) §9 |
| 6 | Grant（授权） | [`grant.schema.json`](schemas/grant.schema.json) | 控制平面 | agent 能力白名单与配额；生效权限 = agent 被授予 ∩ onBehalfOf 用户权限 | [`semantics.md`](semantics.md) §7 |
| 7 | Policy（策略） | [`policy.schema.json`](schemas/policy.schema.json) | 控制平面 | 风险分级规则：`auto \| approve \| deny`，合并顺序 `deny > approve > auto`，**无命中即 deny** | [`semantics.md`](semantics.md) §6 |
| 8 | Audit Event（审计） | [`audit-event.schema.json`](schemas/audit-event.schema.json) | 系统 → 存储 | 不可变记录，两段写（`dispatched` 先落盘才允许执行）+ `prevHash` 哈希链 | [`semantics.md`](semantics.md) §10 |
| 9 | Error（错误） | [`error.schema.json`](schemas/error.schema.json) | 模块 → agent | 四类错误分类，决定 agent 是重试、重新规划、转人工还是告警 | [`errors.md`](errors.md) |

第 10 个 schema 文件 [`common.schema.json`](schemas/common.schema.json) 不是契约，而是上面 9 类共用的类型库（见 §3）。

---

## 3. 通用类型

`common.schema.json` 只有 `$defs`，不对任何报文做校验，作用相当于协议的 `std` 命名空间。

| 类型 | 用途 |
|---|---|
| `EntityRef` / `EntityRefString` / `EntityRefLike` | 实体寻址。对象形式与字符串简写形式语义等价，`EntityRefLike` 是「二者皆可」的联合类型（§4.0） |
| `Actor` | 行为主体：agent 代行、真人、系统进程，并显式承载身份代理关系 |
| `CorrelationId` | 贯穿一条业务链的关联标识，跨契约串联的唯一手段 |
| `IdempotencyKey` | 幂等键，重试与崩溃恢复的锚点 |
| `Iso8601Timestamp` / `Iso8601Duration` | 时间戳与时长（配额窗口） |
| `Sha256Hash` | `sha256:<hex>` 摘要。大对象只存摘要，原文入对象存储 |
| `TenonVersion` / `SemVer` | 协议版本（`0.1`）与契约自身的语义化版本（`1.4.0`） |
| `ActionId` / `ModuleId` | 动作与模块的标识词法 |
| `JsonValue` / `JsonObject` / `JsonPatch` / `JsonPatchOperation` | 自由结构与 RFC 6902 JSON Patch |
| `PlanState` / `ApprovalState` / `ExecutionStepState` | §5.1 三个状态机枚举的机器可读副本，[`state-machines.md`](state-machines.md) 与一致性用例共用 |

**关于实体寻址的双形式。** §4.0 定义对象形式 `{"module","type","id"}` 与字符串简写 `module:Type/id`，并声明二者等价、逐点列举了使用位置（`Event.subject`、`Plan.expectedDiff[].entity`、`Intent.target`、`Audit.effects.entities`）。schema 在这四个位置接受两种形式；`Context.view` 未被 §4.0 列举，只接受对象形式。生产方可以任选一种，但**同一条调用链、同一份对账的两侧必须用同一种**，否则无法比对。

---

## 4. 怎么引用

跨文件引用一律用绝对 URI（`$id` 即解析地址，不依赖目录结构）：

```json
{ "$ref": "https://tenon.dev/spec/0.1/schemas/common.schema.json#/$defs/EntityRef" }
```

同文件内部用相对片段引用：

```json
{ "$ref": "#/$defs/Risk" }
```

实现方校验时，把 10 个文件全部注册进同一个校验器实例（按各自的 `$id`），引用即可离线解析。协议不要求 `https://tenon.dev` 实际可访问——它只是稳定的标识符，不是下载地址。

---

## 5. 生产方校验 vs 消费方容忍

这两件事经常被混为一谈，但它们约束的是**不同的角色**：

| | 生产方（发报文的一方） | 消费方（收报文的一方） |
|---|---|---|
| 未知字段 | schema 里所有对象都写了 `additionalProperties: false`，**多写字段即不合法** | **必须忽略未知字段**，不得因未知字段拒绝报文（§4.10） |
| 目的 | 让「我产出的东西符合本版本协议」可被机械验证，防止拼写错误悄悄变成新字段 | 保证新版本生产方写出的报文能被旧版本消费方正常处理，即前向兼容 |
| 机械检查 | `node scripts/validate-spec.mjs`；一致性用例逐条按 schema 校验 | 反例用例 C3：往合法报文里注入未知字段，断言消费方正常处理而非报错 |

也就是说：`additionalProperties: false` 是**给生产方自检用的封闭性声明**，不是给消费方拒收的理由。二者不矛盾——同一个报文，生产方必须「不多不少」，消费方必须「多也不怕」。

**为什么消费方必须容忍。** §4.10 规定协议演进「只增不删字段」，任何一次 minor 升级都可能带来旧消费方没见过的字段。若消费方严格拒收，协议就只能靠 major 升级演进，而 major 升级意味着全网同步升级，这与「模块可插拔」直接冲突。详见 [`versioning.md`](versioning.md)。

**例外：值域封闭的字段。** 少数取值域是封闭枚举（如 `duplicateBehavior` 当前只有 `return_first_result`），它们也会在 minor 版本里扩展。消费方遇到未知枚举值时，必须按「无法判定」降级处理（例如未知 `duplicateBehavior` 视为 `return_first_result`），而不是拒绝报文。

---

## 6. 静态 schema 表达不了的约束

JSON Schema 描述的是**单个文档的形状**，管不到跨文档、跨字段的集合运算与执行顺序。下列约束是协议的一部分，但只能写在语义文档里并由一致性用例机械校验。列在这里是为了让实现方知道：**只跑 schema 校验不等于合规**。

| 约束 | 为什么 schema 表达不了 | 规范位置 | 由谁校验 |
|---|---|---|---|
| **`idempotency.keyFields ⊆ inputSchema.required`** | `inputSchema` 是嵌套的 JSON Schema 文档，`required` 在其中任意深度；JSON Schema 无法表达「A 数组的每一项必须是另一个文档某处数组的成员」 | [`semantics.md`](semantics.md) §3.2 | C1（读 Action，取 `idempotency.keyFields` 与 `inputSchema.required` 做集合包含） |
| `kind=write` 的动作必须声明 `idempotency` | **可以**表达，已用 `allOf + if/then` 写进 schema | [`action.schema.json`](schemas/action.schema.json) | schema 本身 + C1 |
| `risk.reversible=false` 的步骤不得出现在多步 Plan 的非末位 | 需跨文档查询 Plan 中每个动作的 `risk`，并与步骤位置比较 | [`semantics.md`](semantics.md) §5.3 | 宿主评估阶段 + 反例用例（宿主必须拒绝该 Plan 并返回 `contract_violation`） |
| `risk.reversible=false` 且无 `compensation` → 强制审批 | 同上，且属策略层行为 | [`semantics.md`](semantics.md) §5.3 | 策略用例 |
| 步骤 `kind` 必须与目标动作声明的 `kind` 一致 | 跨文档比对 | [`semantics.md`](semantics.md) §2.1 | 宿主评估 + 用例 |
| `dependsOn` 引用的 `stepId` 必须在同一 Plan 内存在且无环 | 需要图算法 | [`state-machines.md`](state-machines.md) §2.2 | 宿主评估 + 用例 |
| `write` 步骤必须携带 `idempotencyKey` | 可以表达，但 0.1 有意不写进 schema：字段可选性以 `packages/tenon-core` 的类型投影为准，写死在 schema 里会让「计划期还没生成键」的合法中间态无法表达 | [`semantics.md`](semantics.md) §3.4 | 宿主评估 + C2 |
| `quotaUnit=quantity` 时必须给出 `quotaField` | 可以表达，但 §4.2 的 Action 示例本身就省略了该字段；写进 schema 会让范本示例不合法，故降为语义要求 | [`action.schema.json`](schemas/action.schema.json) 的 `quotaField` 说明 | 授权计量用例 |
| 审计 `prevHash` 链连续、`dispatched` 先于 `result` | 需要跨记录、跨时间的顺序关系 | [`semantics.md`](semantics.md) §10.3 | C4 |
| `dispatched` 审计写失败必须拒绝执行 | 属执行时序 | [`semantics.md`](semantics.md) §10.2 | M0 出口用例（注入写失败） |
| 配额窗口的滚动语义、并发下的余量 | 属运行时计算 | [`semantics.md`](semantics.md) §7.3 | 授权用例 |

> 判据（§2.1）：任何一条规范如果写不出一致性测试用例，就不该进协议。上表每一条都指定了校验者。

---

## 7. 文件清单

| 文件 | 内容 |
|---|---|
| [`README.md`](README.md) | 本索引 |
| [`state-machines.md`](state-machines.md) | §5.1 三个状态机：状态表、合法/非法迁移、Mermaid 图 |
| [`semantics.md`](semantics.md) | §5.2 执行参数、幂等、预演、补偿、审计两段写、对账 |
| [`versioning.md`](versioning.md) | 语义化版本、只增不删、版本协商头、兼容性判定、弃用流程 |
| [`errors.md`](errors.md) | 四类错误的判定标准、agent 应对策略、HTTP 映射 |
| [`CHANGELOG.md`](CHANGELOG.md) | 变更记录（Keep a Changelog） |
| `schemas/common.schema.json` | 通用类型库 |
| `schemas/module-manifest.schema.json` | 契约 1 |
| `schemas/action.schema.json` | 契约 2 |
| `schemas/domain-event.schema.json` | 契约 3 |
| `schemas/plan.schema.json` | 契约 4 |
| `schemas/frames.schema.json` | 契约 5 |
| `schemas/grant.schema.json` | 契约 6 |
| `schemas/policy.schema.json` | 契约 7 |
| `schemas/audit-event.schema.json` | 契约 8 |
| `schemas/error.schema.json` | 契约 9 |

---

## 8. 怎么校验

```bash
node scripts/validate-spec.mjs
```

它做四件事：schema 结构合法性（元数据、`$id` 与文件名一致）、本地 `$ref` 可解析、跨文件引用指向真实存在的 `$defs`、九类契约齐备。**它不做实例校验**——那属于一致性测试套件的职责（`conformance/`），用真实报文逐条断言 schema 与语义。

---

## 9. 版本与许可

- 协议版本由 `major.minor` 构成，当前 `0.1`。演进规则、请求头与弃用流程见 [`versioning.md`](versioning.md)。
- 本规范与参考实现以 Apache-2.0 发布，见根目录 [`LICENSE`](../../LICENSE)。
