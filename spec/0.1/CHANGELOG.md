# 变更记录

本文件记录 Tenon 协议规范的变更。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)；协议版本只有 `major.minor`，版本位怎么判定见 [`versioning.md`](versioning.md) §5。（标题省略 Keep a Changelog 中的方括号链接，因为本仓库尚未为规范版本打 tag。）

变更分节用 `Added` / `Changed` / `Deprecated` / `Removed` / `Fixed` / `Security`。

## 0.2.0 — 提案，尚未冻结

> **状态：提案。** 本段记录 M0.5 实现过程中对 0.1 的三处扩展与一处语义澄清。它**尚未冻结**：措辞可能随评审改动，实现仍以 `Tenon-Version: 0.1` 报版本（扩展端点不改动 0.1 的任何既有线格式）。冻结之前，不要把本段当成已发布的对外承诺。

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

## 0.1.0 - 2026-10-27

### Added

- 初始草案：Tenon 协议 0.1 的完整规范目录——九类契约的 JSON Schema（Module Manifest / Action / Domain Event / Plan / Intent-Context-Confirm / Grant / Policy / Audit Event / Error）与通用类型库 `common.schema.json`；§5.1 三个状态机（Plan / Approval / Execution Step）的状态枚举、合法与非法迁移；§5.2 执行语义（超时 30000ms、重试 `3/200/2/0.2`、熔断 `5/60000/1`、补偿逆序、不可逆约束）；幂等与 `keyFields ⊆ inputSchema.required` 的跨文档约束及其校验归属；dry-run 四态与降级；审计两段写（`dispatched` 先落盘才允许执行，写失败即拒绝执行）与哈希链；版本演进规则（只增不删、未知字段忽略、`Tenon-Version` / `Tenon-Min-Version` 协商、两段式弃用）；错误四分类的判定顺序、agent 应对策略与 HTTP 映射。

### Fixed

- **`common.schema.json#/$defs/Iso8601Duration` 的 `pattern` 不可移植**：原写法用了前瞻 `(?=...)`，在 ECMA-262 下合法，但 RE2 系实现（Go 的 `regexp`、以及依赖它的整个校验链）无法编译，导致**整份 `common.schema.json` 在这些实现里加载失败**——一个 Go 实现会因此在第一步就被挡在协议外。改为无前瞻的等价写法（枚举首个非空分量），语义更严：`P`、`PT`、`P1YT` 这类退化形态一律拒绝。
- 新增可移植性门禁：`internal/spec/pattern_interop_test.go` 要求所有 schema 的 `pattern` 都能被 `regexp.Compile` 接受。**任何引入前瞻/后顾/反向引用的 PR 会直接 CI 失败。**
