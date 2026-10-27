# 错误分类（§4.9）

错误分类决定 **agent 的下一步动作**，所以它比错误码重要：错误码告诉人发生了什么，分类告诉 agent 该不该继续。四类之外没有第五类——无法归类的错误一律按最保守的一类处理。

---

## 1. 线格式

```json
{ "tenonError": { "class": "business_rejection", "code": "STOCK_INSUFFICIENT", "message": "可用 3，需求 12", "details": { "available": 3 }, "retryable": false } }
```

- 错误体**恒在唯一的顶层键 `tenonError` 下**，与成功响应体天然可区分，不与业务字段撞名。
- `details` 可选；`class`、`code`、`message`、`retryable` 必填。
- 五项的语义见 [`error.schema.json`](schemas/error.schema.json)；本文件只写判定与应对。

---

## 2. 判定：按顺序问四个问题

判定必须**按下面的顺序**，因为同一个现象可能同时满足多个条件（例如「库存服务返回了 schema 不符的报文」既是业务问题也是契约问题）。顺序保证同类现象永远归到同一类，否则 agent 的行为不可预测。

| 顺序 | 问题 | 若「是」→ | 典型例子 |
|---|---|---|---|
| 1 | 报文本身不合法吗？（schema 不符、未知 action、写调用缺幂等键、Plan 静态不可执行） | `contract_violation` | 入参少字段、`dependsOn` 指向不存在的步骤 |
| 2 | 是治理闸门拒绝的吗？（超授权、超配额、策略 deny、审批过期） | `policy_denied` | `NOT_IN_GRANT`、`QUOTA_EXCEEDED`、`APPROVAL_EXPIRED` |
| 3 | 是业务前置条件不满足吗？（模块正常工作了，但业务上不允许） | `business_rejection` | `STOCK_INSUFFICIENT`、`DOCUMENT_ALREADY_LOCKED` |
| 4 | 是瞬时基础设施故障吗？（网络、限流、超时、依赖不可用） | `retryable` | 连接超时、上游 503、限流 429 |

四问全否 → 视为 `contract_violation`（无法归类即不可信，交给人看）。**这条兜底规则很重要**：它阻止实现方发明第五类，或把「不知道」写成 `retryable` 去赌运气。

---

## 3. `retryable` 与 `class` 的对应

| class | `retryable` | 为什么 |
|---|---|---|
| `retryable` | **必须 `true`** | 瞬时故障，退避后大概率成功 |
| `business_rejection` | **必须 `false`** | 重试会反复撞同一面墙：库存不会因为再问一次而变多 |
| `policy_denied` | **必须 `false`** | 重试 policy_denied 等于反复敲门等人开门，实质是绕过闸门 |
| `contract_violation` | **必须 `false`** | 报文不会因为重发而合法 |

**这不是约定，是 schema 强制。** [`error.schema.json`](schemas/error.schema.json) 用 `allOf + if/then` 把 `class` 与 `retryable` 绑死：`class=retryable` 时 `retryable` 只能为 `true`，其余三类只能为 `false`。写反了报文就不合法，C9 用例正断言这一点。

于是 `retryable` 字段看起来冗余（读 `class` 就知道），但它保留的理由是**机械冗余**：让「分类」和「可重试」两处独立表达，写错一处立刻被 schema 拒绝，比只靠实现方记住一张表更可靠。

---

## 4. agent 的应对策略

| class | 立即动作 | 允许换路（改走别的 action） | 审计留痕 | 终局 |
|---|---|---|---|---|
| `retryable` | 按 [`semantics.md`](semantics.md) §3 退避重试；**幂等键不变**；次数上限 `maxRetries` | 重试耗尽后才允许（此时应视为结果未定先回查，不是直接换路） | 步骤停在 `dispatched`；耗尽转 `unknown`；`errorClass=retryable` | 回查收敛，或转人工 |
| `business_rejection` | **重新规划**：改预留、拆单、发起采购、降量履约；拿不准就问人 | **允许，且这是 AI 驱动的价值所在**（§6.3） | 步骤 `failed`，`errorClass=business_rejection`；Plan 终止，新 Plan 的 `supersedes` 指回旧 `planId` | 通常进入新 Plan；若命中高风险策略则转审批 |
| `policy_denied` | **终止**，转人工 | **禁止**。不得换一个动作、换一个模块、换一条参数去达到同一目的 | Plan → `rejected`；`decision.outcome=deny` 入审计 | 人工 |
| `contract_violation` | **死信 + 告警**，不做任何自动修复 | 禁止 | Plan → `failed`；告警事件 | 修 bug（协议或接入方） |

两条最容易写错的：

1. **`business_rejection` 不是错误处理，是重新规划的信号。** stock 不足时 agent 去发起采购申请，正是「AI 驱动而非程序驱动」的体现（§6.3）。如果实现把它写成 `catch (e) { 走采购分支 }`，那就是 §2.2 明令禁止的「错误码 → action 映射」，会被 C10 lint 扫出来。
2. **`policy_denied` 不允许换路。** 「超授权就换个不超授权的动作」是绕过治理，不是解决问题。闸门拒绝的正确回应是人。

---

## 5. HTTP 状态码映射（建议）

所有实现都建议按下表映射，以便网关、监控与告警按状态码聚合，而不必解析响应体：

| class | HTTP | 备注 |
|---|---|---|
| `retryable` | `503 Service Unavailable`，或 `429 Too Many Requests` | 429 必须带 `Retry-After`；限流用 429，依赖不可用用 503 |
| `business_rejection` | `422 Unprocessable Entity` | 请求合法但业务上不可执行——这正是 422 的定义 |
| `policy_denied` | `403 Forbidden` | 身份可信但无权做这件事 |
| `contract_violation` | `400 Bad Request` | 报文不符合契约 |

补充规则：

- **无 HTTP 响应**（连接失败、超时、连接被重置）本身就是 `retryable`，不需要模块提供错误体。
- 凡带 `tenonError` 响应体，状态码与 `class` 必须一致；不一致时**以响应体为准**（体是协议语义，码只是传输便利）。
- 不要把 `business_rejection` 映射成 200：200 会让调用链路上所有监控看不到失败。

---

## 6. 错误码约定

| 规则 | 说明 |
|---|---|
| 词法 | 大写蛇形，仅字母数字下划线（`^[A-Z][A-Z0-9_]*$`），最长 64 字符 |
| 唯一性 | 模块内唯一；同一语义**永远**同一码，改语义必须新码 |
| 稳定性 | 码一旦发布不得复用，也不得改含义（错误码是运维与对账面上最长的生命周期资产） |
| 禁止含动作名 | 不得出现 `CALL_DEDUCT_STOCK_FAILED` 这类命名，也不得用码的名字暗示「下一步调什么」——那会让码变相成为动作路由，触碰 §2.2 反模式 |
| `message` | 给人看，含关键数值（「可用 3，需求 12」）；不保证机器可解析 |
| `details` | 结构化补充（如 `available`、`lockedBy`）；**禁止**放凭证、密钥、个人敏感数据 |

示例（非规范，仅供对照）：

| class | code 示例 |
|---|---|
| `retryable` | `UPSTREAM_TIMEOUT`、`RATE_LIMITED`、`AUDIT_WRITE_FAILED` |
| `business_rejection` | `STOCK_INSUFFICIENT`、`DOCUMENT_ALREADY_LOCKED` |
| `policy_denied` | `NOT_IN_GRANT`、`QUOTA_EXCEEDED`、`APPROVAL_EXPIRED`、`GOVERNANCE_UNAVAILABLE` |
| `contract_violation` | `SCHEMA_MISMATCH`、`UNKNOWN_ACTION`、`IDEMPOTENCY_KEY_REQUIRED` |

---

## 7. 与审计的对应

审计记录只存**类别**（`result.errorClass`），不存错误码。这是刻意的：

- 审计要可聚合、可对账，类别是有限的四元集，码的数量是无界的；
- 具体错误码的排障路径是「用审计的 `inputHash` + 时间窗去模块侧日志关联」，不需要复制进审计链（复制还会让错误码的演进反向约束协议）。

`result.status` 与 `errorClass` 的搭配由 schema 约束：`status=error` 时 `errorClass` 必须非空；`status=ok` 时必须为空；`status=unknown`（结果未定）时两者都为空。

`partially_failed` 不强制 `errorClass`：多步计划里可能既有业务拒绝又有补偿失败，单一类别无法概括，具体原因在逐步骤审计里。

---

## 8. 谁抛哪一类

| 抛出方 | 可能抛出的类别 |
|---|---|
| 模块 | 四类都会：`business_rejection`（业务不允许）、`retryable`（自身依赖故障）、`contract_violation`（调用方传错）、`policy_denied`（模块侧二次授权校验失败，§4.6） |
| 控制平面（grant/policy 评估） | `policy_denied`、`contract_violation`（请求非法） |
| 宿主（agent 运行时） | `retryable`（审计写失败等自身设施故障）、`contract_violation`（Plan 静态不可执行、版本不匹配） |

模块侧抛 `policy_denied` 是**必须**的（§4.6 纵深防御）：宿主可能被绕过，模块自己是最后一道闸门。

---

## 9. 与降级矩阵的关系

降级矩阵（§8、[`semantics.md`](semantics.md) §12）规定的是**宿主的行为**，错误分类规定的是**对 agent 的呈现**。两者在「闸门不可用」处交汇：

**策略或授权服务不可达时，宿主拒绝执行，并向 agent 呈现 `policy_denied`（错误码 `GOVERNANCE_UNAVAILABLE`）。**

理由是行为一致性：闸门不可用时的正确动作是「停手、让人来看」，这正是 `policy_denied` 的 agent 行为；若呈现为 `retryable`，agent 会在闸门失效期间持续重试，把 fail-closed 变成 fail-retry。设计文档只规定了「默认 deny」这一行为，未规定呈现的类别，此处按「类别决定行为」的原则定稿。
