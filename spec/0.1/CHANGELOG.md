# 变更记录

本文件记录 Tenon 协议规范的变更。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)；协议版本只有 `major.minor`，版本位怎么判定见 [`versioning.md`](versioning.md) §5。（标题省略 Keep a Changelog 中的方括号链接，因为本仓库尚未为规范版本打 tag。）

变更分节用 `Added` / `Changed` / `Deprecated` / `Removed` / `Fixed` / `Security`。

## 0.2.0 — 提案，尚未冻结

> **状态：提案。** 本段记录 M0.5 实现过程中对 0.1 的三处扩展与一处语义澄清，以及 M1 切片 1（对话入口）落地时补出的入站面扩展。它**尚未冻结**：措辞可能随评审改动，实现仍以 `Tenon-Version: 0.1` 报版本（扩展端点不改动 0.1 的任何既有线格式）。冻结之前，不要把本段当成已发布的对外承诺。

### Added — `POST /tenon/audit`（写审计）

**为什么 0.1 不够**：0.1 只定义了 `GET /tenon/audit`（查询），却同时把「审计两段写」写成硬约束——`stage=dispatched` 必须先落盘才允许执行、写失败即拒绝执行。**没有写入口，这条约束无法实现**：调用方要么不审计，要么自己造一条私有写通道，两种都让「两段写 + 哈希链」无法被机械判定（C4 只能永远 SKIP）。

- 语义：追加一条审计记录；**返回 200 表示已落盘**（宿主在 fsync 之后才应答）。非 2xx 时调用方必须拒绝执行下游调用。
- 请求体是 `audit-event.schema.json` 的子集；`auditId` 必填，`stage` 只能是 `dispatched` 或 `result`。
- `hash` / `prevHash` 由宿主计算并写回自己的日志，调用方不得自带——链的唯一权威是宿主的审计存储。
- 宿主**不校验记录是否 schema 完整**（只校验上面两个字段）：`trigger` / `effects` 等字段缺失时照原样落盘。这条边界见 `docs/interfaces.md` §9 缺口 6。

### Added — `GET /tenon/modules`（注册表读取）

**为什么 0.1 不够**：0.1 假定调用方预先知道模块在哪（地址写死在配置里）。一旦模块集合可变（差异测试要摘掉其中一个），就必须有一个「现在有哪些模块」的读取口；否则「摘掉模块 → 行为改变」这条判据只能靠改配置来伪造。

- 响应：`{"modules": [{"id", "baseURL", "enabled"}]}`。
- **返回的是管理面视图，含 `enabled:false` 的条目。** 调用方必须自己按 `enabled` 过滤后再做能力发现；宿主不替调用方裁剪视野——否则「这个模块被摘除了」这件事在审计与排障里不可见。
- 注册表在 M0.5 是纯内存态：进程重启即回到启动配置，不持久化。

### Added — 模块发件箱：`GET /tenon/events?direction=in|out`

**为什么 0.1 不够**：0.1 只定义了宿主 → 模块方向（`POST /tenon/events` 投递、`GET /tenon/events?since=` 补课）。模块自产事件（如 `quote.completed`）**没有出口**——唯一出路是模块反向调宿主，而 0.1 的调用矩阵明确「模块不主动调宿主」。

- `direction=in`（**缺省**）：宿主投递进来的事件，保持 0.1 语义，向后兼容。
- `direction=out`：模块自己产生的事件（发件箱），供 agent 轮询。
- 响应形状不变：`{events, nextCursor}`。
- 两张事件表各有独立序号与游标，游标带方向标签（`cur_in_` / `cur_out_`）；把一个方向的游标用在另一个方向 → **400 `INVALID_CURSOR`**，而不是被当成同一张表的序号静默跳过一段事件。0.1 的无标签游标（`cur_`）仍可用于 `in`。

### Changed — `dry-run` 404 的语义澄清

**原来的模糊处**：0.1 定义了 `dry-run` 的四态（`verified` / `unsafe` / `unsupported` / `pending`）与降级矩阵，却没写「模块根本没有这个端点」算什么。实现里两种读法都出现过。

**本轮钉死**：

- 模块未实现 `POST /tenon/actions/{id}:dry-run`（404）→ 等价于 `unsupported`，**调用方必须强制审批（fail-closed）**。
- 这**不是免检**。把 404 读成「没有预演就放行」，等于给供应商一个自助旁路：删掉端点即可跳过预演。任何允许这种读法的实现，都在事实上取消了预演闸门。

### Added — 入站面：`POST /tenon/session`、`/tenon/requests`（+`poll`/`ack`）、`/tenon/pending`（`GET`/`POST`）

**为什么 0.1 不够**：0.1 的三帧里 `Intent` 是 agent→前端，而 `Context` / `Confirm` 虽然定义了前端→agent 的方向，却**没有任何入站通道**（实现里 agent 也不读）。于是「人说话 → agent 做一回合」这条路径不存在，审批帧也无处投递。而 0.1 把 `/tenon/intents` 写成 WS 端点，其 REST 降级的 `ack` 是**累积游标**——两个消费者共用一个游标会互相吞帧，所以入站不能复用它。

- `POST /tenon/session`：`{user, password}` → `{token, user, roles, exp}`；token 是无状态 HMAC（`base64url(payload).base64url(HMAC-SHA256(…))`），不引存储。**这是开发态身份，不是认证**（无口令强度、无锁定、无吊销、无 SSO）。
- `POST /tenon/requests` 收 `tenon.request`（新帧）或 `tenon.confirm`；响应 `{ok, seq, durable}`。`durable:false` 表示只进了内存队列没落盘——**此时仍回 200**：500 会诱导客户端重试，而 `requestId` 由宿主签发，重试等于把用户那句话再说一遍。
- **三个入站端点都要求** `Authorization: Bearer`；**消费侧**（`poll` / `ack` / 公告板写入）还要求 `role:agent`。用户 token 若能 ack 就能吞掉别人的请求；若能写公告板，就能给审批人塞伪造的待批卡片。
- 入站队列**落盘**（JSONL + 序号水位 meta），与出站内存队列刻意不对称：入站承载「人说过的话」，丢了没人知道；出站只是界面提示，丢了可由公告板重建。
- `POST /tenon/pending` / `GET /tenon/pending`：待批公告板，**只读 read-model**（权威状态在 agent 盘上的 pending 文件）。`POST` 是幂等 upsert，`GET` 按 token 角色过滤并给 `mine`——仅供显示，不构成授权。

### Added — 新帧 `tenon.request`（人 → agent）

```json
{"type":"tenon.request","requestId":"req_…","text":"Q-2026-0417 这单货不够，先扣现有的，缺的发起采购",
 "context":{"view":{"module":"quote","type":"Quote","id":"Q-2026-0417"}},"by":"user:alice","at":"2026-10-27T09:15:00.000Z"}
```

- `by` 由宿主从 token 解析后**覆写**，客户端自报一律丢弃；`text` 是原话，不做意图分类；`context.view` 可选，给「这单」这类指代一个实体锚点。
- 宿主为每条 request 签发 `requestId` 与**合成事件 id**（`evt_`），并登记 `eventId → origin:"request"`。`Plan.trigger` 沿用既有规定——`plan.schema.json` 的 `PlanTrigger` 注解本就写了「人工发起时使用宿主生成的合成事件 id」，**不需要改 plan schema**。
- 登记表是**治理状态而非缓存**：判定规则是「未登记即视为 `event`」，所以删掉它会把在途 request 静默降级成事件驱动，绕开「对话发起必须审批」。清理它必须与清空 pending 一起做。

### Added — `origin` 成为策略输入（`policies:evaluate` 增 `triggerEventId`）

- 条件叶子路径前缀从 `arg` / `risk` 扩为 `arg` / `risk` / `origin`；`origin` 是**单段路径**（不带点），取值 `request` | `event`。`policy.schema.json` 的 `ConditionLeaf` propertyName 相应允许单段。
- `origin` 由宿主查登记表判定，**不接受调用方自报**；缺 `triggerEventId` 时按 `event` 处理，`TENON_REQUIRE_TRIGGER=1` 时改为 `200 + deny + reasons:["MISSING_TRIGGER"]`（不是 4xx——处置与其它策略结论同形）。
- 用途：把「对话发起必须人按」与「事件驱动按阈值自动」做成两条正交轴，合并仍取更严（`deny > approve > auto`）。

### Added — 业务级规则文件 `skills/<skillId>/approval.json`、`summaryTemplate`

- `approval.json` 与平台 `config/policies*.json` **同 schema、同加载器、同池合并**（先按 `outcome` 强弱，同 outcome 内按 `priority`），**不设文件级覆盖**。因此业务级规则**只能收紧、不能放宽**：平台若有「该动作一律 approve」，业务级写 auto 是无效的。
- 加载期校验：`arg.*` 必须存在于该动作 `inputSchema.properties`（只比首段）；`risk.*` 必须存在于动作声明的 `risk`；规则还要**试编译**一次（未知算子/非法路径语法在此暴露，按 skill 粒度整份丢弃，而不是让整个宿主起不来）。业务级文件**必须写明 `match.actions`**——模块级规则的覆盖面无法枚举，其 `arg.*` 无从校验，一律拒收。
- `module-manifest.schema.json` 的 action 增可选 `summaryTemplate`（人话模板，占位符取自 `args`），供渲染「从标准件仓扣掉 12 件」这类白话；`title` 早已是必填项，本轮未改。
- `module-manifest.schema.json` 与 `action.schema.json` 的 action 增可选 `requiredArgs`（**必填字段名**数组，取自 `inputSchema.required`）。它只给字段名、不给类型：agent 的规划 prompt 需要它才能写出合法 `args`（2026-10-27 真模型实测：漏 `referenceNo` 导致预演 400 `SCHEMA_MISMATCH`）。模块加载期断言 `requiredArgs ⊆ inputSchema.required`，声明与契约不一致即启动失败（与 `idempotency.keyFields` 同源）。
- **威胁模型变化**：规则放进 `skills/` 等于「改文档即改权限」，需要 CODEOWNERS + review 或只读挂载。

## 0.1.0 - 2026-10-27

### Added

- 初始草案：Tenon 协议 0.1 的完整规范目录——九类契约的 JSON Schema（Module Manifest / Action / Domain Event / Plan / Intent-Context-Confirm / Grant / Policy / Audit Event / Error）与通用类型库 `common.schema.json`；§5.1 三个状态机（Plan / Approval / Execution Step）的状态枚举、合法与非法迁移；§5.2 执行语义（超时 30000ms、重试 `3/200/2/0.2`、熔断 `5/60000/1`、补偿逆序、不可逆约束）；幂等与 `keyFields ⊆ inputSchema.required` 的跨文档约束及其校验归属；dry-run 四态与降级；审计两段写（`dispatched` 先落盘才允许执行，写失败即拒绝执行）与哈希链；版本演进规则（只增不删、未知字段忽略、`Tenon-Version` / `Tenon-Min-Version` 协商、两段式弃用）；错误四分类的判定顺序、agent 应对策略与 HTTP 映射。

### Fixed

- **`common.schema.json#/$defs/Iso8601Duration` 的 `pattern` 不可移植**：原写法用了前瞻 `(?=...)`，在 ECMA-262 下合法，但 RE2 系实现（Go 的 `regexp`、以及依赖它的整个校验链）无法编译，导致**整份 `common.schema.json` 在这些实现里加载失败**——一个 Go 实现会因此在第一步就被挡在协议外。改为无前瞻的等价写法（枚举首个非空分量），语义更严：`P`、`PT`、`P1YT` 这类退化形态一律拒绝。
- 新增可移植性门禁：`internal/spec/pattern_interop_test.go` 要求所有 schema 的 `pattern` 都能被 `regexp.Compile` 接受。**任何引入前瞻/后顾/反向引用的 PR 会直接 CI 失败。**
