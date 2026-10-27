# 变更记录

本文件记录 Tenon 协议规范的变更。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)；协议版本只有 `major.minor`，版本位怎么判定见 [`versioning.md`](versioning.md) §5。（标题省略 Keep a Changelog 中的方括号链接，因为本仓库尚未为规范版本打 tag。）

变更分节用 `Added` / `Changed` / `Deprecated` / `Removed` / `Fixed` / `Security`。

## 0.1.0 - 2026-10-27

### Added

- 初始草案：Tenon 协议 0.1 的完整规范目录——九类契约的 JSON Schema（Module Manifest / Action / Domain Event / Plan / Intent-Context-Confirm / Grant / Policy / Audit Event / Error）与通用类型库 `common.schema.json`；§5.1 三个状态机（Plan / Approval / Execution Step）的状态枚举、合法与非法迁移；§5.2 执行语义（超时 30000ms、重试 `3/200/2/0.2`、熔断 `5/60000/1`、补偿逆序、不可逆约束）；幂等与 `keyFields ⊆ inputSchema.required` 的跨文档约束及其校验归属；dry-run 四态与降级；审计两段写（`dispatched` 先落盘才允许执行，写失败即拒绝执行）与哈希链；版本演进规则（只增不删、未知字段忽略、`Tenon-Version` / `Tenon-Min-Version` 协商、两段式弃用）；错误四分类的判定顺序、agent 应对策略与 HTTP 映射。

### Fixed

- **`common.schema.json#/$defs/Iso8601Duration` 的 `pattern` 不可移植**：原写法用了前瞻 `(?=...)`，在 ECMA-262 下合法，但 RE2 系实现（Go 的 `regexp`、以及依赖它的整个校验链）无法编译，导致**整份 `common.schema.json` 在这些实现里加载失败**——一个 Go 实现会因此在第一步就被挡在协议外。改为无前瞻的等价写法（枚举首个非空分量），语义更严：`P`、`PT`、`P1YT` 这类退化形态一律拒绝。
- 新增可移植性门禁：`internal/spec/pattern_interop_test.go` 要求所有 schema 的 `pattern` 都能被 `regexp.Compile` 接受。**任何引入前瞻/后顾/反向引用的 PR 会直接 CI 失败。**
