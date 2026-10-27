# 执行语义（§5.2）

本文件规定「一次动作调用」在协议层面的行为：超时、重试、熔断、幂等、预演、补偿、审计、对账。这些语义不依赖任何传输、数据库或运行时，因此可以在任意实现上被一致性用例断言。

---

## 1. 总则

1. **一切默认值都只是兜底，不是许可。** §5.2 的参数是协议默认值；接入方**可以覆盖，但必须声明**（在模块文档或 `capabilities` 中列出实际取值）。宿主不得假设默认值，也不得在未读到声明时把默认值当成对方的承诺。
2. **全部 fail-closed。** 任何一环不可判定时，结论是「不执行」或「转人工」，不是「照旧执行」。降级矩阵见 §12。
3. **一次调用 = 一对审计。** 先写 `dispatched` 再执行，结果出来写 `result`。没有 `dispatched` 记录的调用在协议上等于没发生（§10）。

---

## 2. 超时

| 项 | 值 | 说明 |
|---|---|---|
| 默认值 | `30000` ms | 兜底值：仅当读不到 `action.timeoutMs` 时使用（例如拉取完整 Action 失败的降级路径），正常路径必须使用模块声明的值 |
| 声明位置 | `Action.timeoutMs` | schema 中为必填整数（正整数） |
| 计时起点 | 请求发出时刻 | 重试的每一次尝试独立计时，不累计 |
| 计时终点 | 收到完整响应（含反序列化完成） | 只收到响应头不算超时解除 |
| 超时归类 | `retryable` | §5.2 明文规定 |

**超时后的状态不是「失败」。** 超时只说明「没等到回答」，写入可能已经落库。因此：步骤进入 `unknown`（[`state-machines.md`](state-machines.md) §3.2 S5），按幂等键回查，重试时**幂等键不变**。把超时直接判 `failed` 会让对账出现「审计说没做、模块说做了」的差异。

---

## 3. 重试

### 3.1 参数

| 参数 | 默认值 | 含义 |
|---|---|---|
| `maxRetries` | `3` | 首次尝试之外的最大重试次数；总尝试次数 = `1 + maxRetries` = 4 |
| `baseMs` | `200` | 第一次退避的基准毫秒 |
| `factor` | `2` | 退避倍率 |
| `jitter` | `0.2` | 抖动比例，取值 `[0, 1]` |

### 3.2 退避公式

第 n 次重试（n 从 1 开始）的等待时间为：

```
delay(n) = baseMs × factor^(n−1) × (1 + jitter × r)，  r ~ U(−1, +1)
```

按默认值即约 `160–240ms`、`320–480ms`、`640–960ms`。抖动是必须的：不加抖动的固定退避会让一批 agent 在同一毫秒集体重试，把瞬时故障放大成雪崩。

### 3.3 约束

1. **只对 `retryable` 重试。** `business_rejection` 重试会反复撞同一面墙；`policy_denied` 重试等于绕过闸门；`contract_violation` 重试不会让报文变合法（[`errors.md`](errors.md) §3）。
2. **幂等键不变。** 同一笔业务的所有尝试共用一个 `Idempotency-Key`，这是模块返回首次结果、避免重复副作用的前提（§4.9）。
3. **重试期间状态不变。** 步骤停在 `dispatched`，不产生迁移；每次尝试各写一对审计。
4. **耗尽即 `unknown`。** 重试次数用尽仍为 `retryable`，说明结果未定，转 `unknown` 等回查，不得判 `failed`（[`state-machines.md`](state-machines.md) §3.3）。
5. **重试不得改变参数。** 除传输层字段（如时间戳）外，`args` 必须逐字节一致；要改参数就是新计划，不是重试。

---

## 4. 熔断

### 4.1 参数

| 参数 | 默认值 | 含义 |
|---|---|---|
| `failureThreshold` | `5` | 连续失败达到该次数即打开熔断 |
| `cooldownMs` | `60000` | 打开后静默时长，期间不再调度 |
| `halfOpenProbes` | `1` | 冷却结束后允许的探测调用数 |

### 4.2 状态与行为

| 状态 | 行为 |
|---|---|
| 闭合 | 正常调度 |
| 打开 | 该 action 立即失败（不发出真实调用），并在能力清单标 `unavailable`（§5.2、§8） |
| 半开 | 放行 `halfOpenProbes` 个探测：全部成功 → 闭合；任一失败 → 重新打开并重置 `cooldownMs` |

**计数口径：只有 `retryable`（含超时）计入失败。** `business_rejection` 说明模块正常工作并正确拒绝了业务，`policy_denied` 是闸门在起作用，`contract_violation` 的常见原因是调用方传错参——把它们算作模块不健康，会在「重复撞墙」时把健康模块摘掉，反而扩大故障面。

**熔断的可见性。** 宿主必须把打开状态的 action 反映到能力发现结果里（`unavailableActions`，见 [`module-manifest.schema.json`](schemas/module-manifest.schema.json)），否则 agent 会持续规划出无法执行的步骤。

---

## 5. 幂等

### 5.1 声明与传输

- **声明**在 `Action.idempotency`：`keyFields`（非空，取自入参）、`scope`（`module` | `system`）、`duplicateBehavior`（0.1 只有 `return_first_result`）。
- **传输**用请求头 `Idempotency-Key`（§4.10）。`kind=write` 的调用**必须**携带；缺失时模块必须拒绝并返回 `contract_violation`。`kind=read` 不携带。

### 5.2 行为

| 情形 | 模块行为 |
|---|---|
| 首次收到该键 | 执行，记录「键 → 首次结果」，返回结果 |
| 再次收到该键 | **不执行**，返回首次结果（`return_first_result`），副作用只有一次 |
| 键相同但 `args` 不同 | 视为调用方 bug，返回 `contract_violation`（同一笔业务不可能有两种参数）；不得静默按首次执行 |
| 键在留存期内查不到 | 按首次处理（这正是不允许把留存期设短的原因） |

**`scope` 的差别。** `module` 表示键在单个模块内唯一，模块可以自行压缩键空间；`system` 表示键在全系统唯一，宿主与各模块共享同一键空间，用于「一笔业务跨多模块只做一次」的场景（例如同一 `referenceNo` 在库存与采购两侧都不得重复扣）。

### 5.3 `keyFields ⊆ inputSchema.required`（本规范唯一的跨文档集合约束）

`idempotency.keyFields` 的每一项都必须是 `inputSchema.required` 的成员。理由直接而机械：**若幂等键依赖一个可选字段，调用方可以省略它，幂等键就退化为「每次都不相同」，重试变成重复副作用**。

JSON Schema 表达不了这条约束——`inputSchema` 是嵌套 schema，JSON Schema 没有「某数组的每一项必须是另一个文档某处数组的成员」这种能力。因此：

| 项 | 内容 |
|---|---|
| 规范位置 | 本小节（§5.3） |
| schema 里的近似表达 | `keyFields` 已约束为「非空、字符串、字段名词法」，但无法与 `inputSchema.required` 比对 |
| 机械校验者 | 一致性用例 **C1**：读取完整 Action，取 `idempotency.keyFields` 与 `inputSchema.required`，断言前者是后者的子集 |
| 违反后果 | C1 失败，该模块不可获得 `tenon-conformant-0.1` badge |

实现方不要试图用自定义关键字补这个洞：0.1 不引入非标准关键字，否则第三方校验器会把合法文档判为非法。

### 5.4 `write` 步骤必须携带 `idempotencyKey`

Plan 中的 `write` 步骤必须给出 `idempotencyKey`（约定 `tenon:<planId>/<stepId>`），因为崩溃恢复后唯一能把 `unknown` 收敛的手段就是拿它回查。该要求属语义层，不写死在 Plan schema 里（理由见 [`README.md`](README.md) §6）。

### 5.5 留存期

协议不规定具体时长，但**留存期必须覆盖「崩溃恢复窗口 + 至少一次日切对账」**，否则回查会把「其实生效了」误判为「从未生效」，直接产生重复副作用。该值是部署参数，接入方必须声明。

---

## 6. 预演（dry-run）

### 6.1 端点与约束

`POST /tenon/actions/{id}:dry-run` 返回 [`DryRunReport`](schemas/plan.schema.json)。预演**不得产生副作用**：不写业务库、不发领域事件、不消耗配额、不推进任何状态机。

### 6.2 四态与降级

`Plan.dryRun.status` 是四态而非布尔，因为它要承载降级矩阵（§4.4、§8）：

| status | 含义 | 后果 |
|---|---|---|
| `verified` | 预演通过，并给出预期差异 | 允许自动执行 |
| `unsafe` | 预演发现风险（超配额、负库存、不可逆步骤被前置等） | **强制审批** |
| `unsupported` | 模块未实现预演端点，或端点返回不支持的语义 | **强制审批** |
| `pending` | 尚未预演（预演超时、依赖未就绪） | **强制审批** |

只有 `verified` 通行，其余三种一律强制审批——这就是「无 dry-run 也不报错，但风险不许被静默放大」的落点（用例 C8：未实现预演时用例 pass，运行时强制审批）。

### 6.3 多步计划的合并

一个 Plan 含多个 `write` 步骤时，对每个步骤分别预演，`Plan.dryRun.status` 取**最差**结论，顺序为：

```
verified < pending < unsupported < unsafe
```

只含 `read` 步骤的计划视为 `verified`（无副作用可预演），但仍必须写出该结论，不得留空。

### 6.4 预演与预期的对账

宿主必须把 `dryRun.diff` 与 `expectedDiff` 比对：**若出现 `expectedDiff` 未预期的变化，或预期的变化没有出现，必须把 status 降为 `unsafe`**。预演说「会改 A」而计划说「会改 B」，正是最该转人工的情形。

---

## 7. 补偿与不可逆约束

### 7.1 补偿顺序

1. 多步 Plan 中途失败时，对**已成功**的步骤按**逆序**（后成先进）执行各自的 `compensation`。
2. **失败步骤本身不补偿**：它可能未生效，补偿一个从未发生的写入会制造新错误（§5.2）。
3. 补偿调用本身也要走完整协议：声明 `dispatched` 审计、携带幂等键（形如 `tenon:<newPlanId>/undo-<stepId>`）、返回结果写 `result` 审计。
4. 补偿不是删记录，而是业务语义上的抵销（例如 `ReleaseStock`）。协议不规定补偿内部怎么做。

### 7.2 补偿失败的终局

补偿调用失败时，Plan 终止于 `partially_failed`（不是 `failed`）：写入已部分生效且未完全撤销，**必须人工介入**。宿主必须告警并保留可回放的审计链。

### 7.3 不可逆约束

| 约束 | 判定时机 | 违反后果 |
|---|---|---|
| `risk.reversible=false` 的步骤**不得出现在多步 Plan 的非末位** | Plan 进入状态机之前的静态检查 | 宿主**拒绝该 Plan**，返回 `contract_violation`（§5.2） |
| `risk.reversible=false` 且**无 `compensation`** → **强制审批** | 策略评估 | 策略层自动升为 `approve`，不可被任何策略下调 |

两条约束的动机不同：第一条是**结构上不可能回滚**（后面还有步骤要跑，中间留下不可逆写入就永远收不了场）；第二条是**人在回路**（无法自动撤销的动作，必须有人按过手）。二者都由宿主在评估阶段判定，**不在执行阶段判**——执行阶段的拒绝只能靠模块前置条件（G4）兜底。

### 7.4 幂等与补偿的关系

补偿自己也需要幂等：同一次撤销被重试，不能把库存加两次。补偿动作本身必须是 `kind=write` 并声明 `idempotency`（这是 schema 强制要求）。

---

## 8. 事件投递与三帧

| 项 | 语义 |
|---|---|
| 投递保证 | 至少一次（§4.3）。重复由消费侧按 `eventId` 去重 |
| 补课 | `GET /tenon/events?since=cursor`（§4.10），不丢事件；无 LLM 时事件积压可重放（§8 降级矩阵） |
| 顺序 | 协议不保证全局顺序，只保证 `causationId` 链可追溯 |
| 三帧 | `Intent` 是**可丢弃的增强**：无前端 SDK 时直接丢帧，业务结果不变（§3.2）；`Context` 是事实回传；`Confirm` 是决定，`amend` 会产生新 Plan 并置 `supersedes` |

**帧内不含 DOM 选择器与坐标**，这是协议边界而非实现细节：一旦帧里出现选择器，共驾就退化成 UI 自动化，与 §1.2 的非目标冲突。

---

## 9. 接入面（§4.10）的调用约定

模块侧六个端点（任意语言实现）：

| 方法 | 路径 | 语义 |
|---|---|---|
| GET | `/.well-known/tenon` | 返回 manifest（摘要级） |
| GET | `/tenon/actions/{id}` | 返回完整 Action（含 `inputSchema`） |
| POST | `/tenon/actions/{id}` | 执行；头 `Idempotency-Key`、`X-Tenon-Grant` |
| POST | `/tenon/actions/{id}:dry-run` | 预演；缺失则降级为强制审批 |
| POST | `/tenon/events` | 系统 → 模块投递事件（Webhook，HMAC） |
| GET | `/tenon/events?since=cursor` | 系统 ← 模块补课/重放 |

宿主侧四个端点（控制平面实现，模块不必实现）：`GET /tenon/audit?planId=&since=`、`POST /tenon/grants:evaluate`、`POST /tenon/policies:evaluate`、`WS /tenon/intents`（REST 降级为 `GET /tenon/intents/poll` + `POST /tenon/intents/ack`）。

> **实现侧现状（不是 0.1 的规范内容）**：`cmd/host` 到 0.2.0 提案为止实际有 **13 个协议端点 + 2 个探针 + 静态页**——除上面四个之外还实现了 `POST /tenon/audit`、`GET /tenon/modules`、模块发件箱方向，以及 M1 切片 1 的入站面六个（`POST /tenon/session`、`POST /tenon/requests`、`GET /tenon/requests/poll`、`POST /tenon/requests/ack`、`POST /tenon/pending`、`GET /tenon/pending`）；`policies:evaluate` 还多收一个 `triggerEventId`。清单见 [`docs/interfaces.md`](../../docs/interfaces.md) §4，提案正文见 [`CHANGELOG.md`](CHANGELOG.md) 的 0.2.0 段。上面那句「四个端点」请按 0.1 的历史文本读，不要当成当前端点数。

**Webhook 签名**（§4.6）：`HMAC-SHA256(secret, timestamp + "." + rawBody)`，头 `X-Tenon-Signature: t=<unix>,v1=<hex>`，**时钟容差 ±300s**，超时即拒（防重放）。签名验证必须基于原始字节，不能基于反序列化后再序列化的结果。

**MCP 映射**（§4.10 D7）：以 MCP server 暴露时 tool 名固定为 `tenon_<module>_<action>`（下划线），避免点号在部分客户端被截断。

---

## 10. 审计：两段写（WAL）

### 10.1 两段

| 阶段 | 写入时机 | 必填结论 |
|---|---|---|
| `dispatched` | **真实调用发出之前** | `result.status = unknown`（必然未定）；有该记录才允许执行 |
| `result` | 拿到结果（或判定结果未定）之后 | `result.status` 为 `ok` / `error` / `partially_failed` / `unknown` |

同一次调用产生两条记录：`auditId` 相同，`stage` 不同（§4.8）。这也是 schema 里 `stage=dispatched → result.status=unknown` 的条件约束的来源。

### 10.2 写失败必须拒绝执行

`dispatched` 审计**写成功之后**才允许发出真实调用；写入失败（磁盘、配额、序列化任一环节）→ **拒绝执行**：

| 项 | 规定 |
|---|---|
| 是否发出调用 | **否**。没有 WAL 记录的执行在崩溃后无法回放，也无法与模块对账 |
| 返回错误类别 | `retryable`（存储瞬时故障），错误码约定 `AUDIT_WRITE_FAILED` |
| 为什么可以重试 | 本次没有任何副作用发生，整体重试安全 |
| 连续失败 | 宿主必须把该 Plan 终止于 `aborted` 并告警：审计链路长期不可写是治理失效，不是普通故障 |

M0 的出口判据之一就是这条可测：**注入 `dispatched` 写失败，必须观察到调用未发出**。

### 10.3 哈希链

- 每条记录新增后按写入顺序串联：`prevHash` 指向前一条记录的哈希，链首为 `null`。
- 记录哈希的计算：对整条记录（含其 `prevHash`）按 **RFC 8785 JSON 规范化**后取 SHA-256，写为 `sha256:<hex>`。
- 校验：按顺序重算 `hash(record[i−1]) == record[i].prevHash`；任何一处被改即断链。
- 存储 append-only：**不提供更新与删除接口**。

> 设计文档只规定「`prevHash` 串联成链」，未规定哈希的输入与规范化方式。本规范补上这一条，否则 C4「`prevHash` 链连续」无法被第三方实现独立验证——链的验证必须只依赖公开数据，不能依赖实现内部细节。

### 10.4 双写与对账

- **双写**：agent 侧与模块侧各写一份审计（§3.2）。两侧不是冗余，而是互相制约——只信 agent 的自证等于让被审计者自己出题。
- **日切对账**：比对 `Audit.effects`（声称改了什么）与模块终态（实际变成了什么）。发现差异即**冻结该 agent 在该模块的写权限**再告警（§8 #4），而不是先告警后处理：差异意味着有人或某个实现绕过了协议路径。
- **寻址一致性**：对账两侧必须使用同一实体寻址（§4.0），否则比对本身无效。

---

## 11. 双层授权

1. 调用前宿主向控制平面求值（`POST /tenon/grants:evaluate`），拿到 `allowed=true` 与 `matchedGrantId`。
2. 调用时把 `grantId` 放进 `X-Tenon-Grant` 头。
3. **模块侧必须再校验一次**：该 `grantId` 是否存在、是否包含本次动作、是否过期、配额是否足够。校验失败返回 `policy_denied`。

为什么模块要重复一遍：控制平面不可达时宿主必须默认拒绝（fail-closed），但「宿主被绕过」的情形仍然存在（内部直连、调试脚本）。模块侧自检是最后一道闸门，代价只有一次查表。

**权限取交集**（D5）：`effective = agent 被授予 ∩ onBehalfOf 用户权限`。agent 不会因为操作人手上有财务权限而多出财务权限；审计能追责，但不放大权限。

---

## 12. 降级矩阵（全部 fail-closed）

| 缺失/失败 | 行为 |
|---|---|
| 无前端 SDK | 丢 `Intent` 帧，业务结果不变 |
| 无 dry-run 或预演 `unsafe` | Plan 标 `unsupported`/`unsafe` → 强制审批 |
| 策略服务不可用 | 默认 **deny**（不是 `auto`） |
| 授权服务不可用 | 默认 **deny** |
| 无 LLM | 事件积压可重放，不丢 |
| 模块超时/熔断打开 | 从能力清单摘掉（`unavailableActions`） |
| 事件重复投递 | 按 `eventId` 去重 |
| `dispatched` 审计写失败 | **拒绝执行**（§10.2） |

---

## 13. 参数与声明对照

| 参数 | 协议默认 | 谁能覆盖 | 声明位置 |
|---|---|---|---|
| `action.timeoutMs` | `30000` | 模块 | `Action.timeoutMs`（必填，覆盖即声明） |
| 重试四参数 | `3 / 200 / 2 / 0.2` | 宿主部署 | 宿主配置 + 模块文档 |
| 熔断三参数 | `5 / 60000 / 1` | 宿主部署 | 宿主配置 + 模块文档 |
| 补偿顺序 | 逆序、跳过失败步骤 | **不可覆盖** | 本文件 §7.1 |
| 不可逆约束 | 非末位即拒、无补偿即审批 | **不可覆盖** | 本文件 §7.3 |
| Webhook 时钟容差 | `±300s` | 接入方（更严可，更松不可） | 模块文档 |
| 幂等留存期 | 未规定下限公式 | 接入方（须覆盖恢复窗口与一次对账） | 模块文档 |

「不可覆盖」的两项是协议的骨架：允许覆盖等于允许实现方把 saga 与不可逆约束取消，那就不再有可机械验证的执行语义。
