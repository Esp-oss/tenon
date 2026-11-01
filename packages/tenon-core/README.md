# @tenon/core

Tenon 协议的类型定义、schema 校验，以及实体寻址 / 幂等键 / 哈希 / HMAC 工具。这是协议层，不依赖 dsh。

## 职责边界

- 负责：
  - 9 类契约的 TypeScript 类型（`src/types.ts`），对应设计文档 §4 与 `spec/0.1/schemas/*.json`。
  - JSON Schema 校验的运行时实现；校验库在 zod 与 ajv 之间选型，M0 落定时一并确定。
  - `EntityRef` 的解析与格式化：对象形式 `{ module, type, id }` ↔ 字符串形式 `module:Type/id`（§4.0）。
  - 幂等键构造（`tenon:<planId>/<stepId>`，§4.4）与 `keyFields ⊆ inputSchema.required` 的机械校验（§4.2）。
  - `sha256:` 哈希工具，供审计的 `inputHash` / `outputHash` / `prevHash` 使用（§4.8）。
  - Webhook 的 HMAC-SHA256 签名与验签：`X-Tenon-Signature: t=<unix>,v1=<hex>`，时钟容差 ±300s（§4.6）。
- 不负责：
  - 不生成 Plan、不判定策略与授权、不存储审计——这些属宿主侧控制平面（`@tenon/dsh-plugin-governance`）。
  - 不做传输：不发 HTTP、不开 WS、不挂 MCP（属 `@tenon/sidecar-http` 与各插件）。
  - 不 import dsh 或 cordis，也不依赖任何 dsh 包。

## 依赖

- 对 dsh：**无依赖**。§9 明确规定「协议层不 import dsh」，dsh 只是第一个宿主实现；这条也对应风险 R6 的缓解。
- 包间依赖：本包是依赖图上的叶子，不引用其他 `@tenon/*` 包。方向为其他包 → `@tenon/core`（类型与工具单向被引用），因此改动本包会影响所有包，反向不成立。
- 第三方依赖：当前 `peerDependencies` 为空。M0 落地 schema 校验时会引入 zod 或 ajv 之一，届时再写入依赖。

## 阶段状态

M0（进行中）：

- ✅ `src/types.ts`：9 类契约的 TS 类型骨架。
- ✅ `src/entity-ref.ts`：`EntityRef` 解析/格式化 + `toEntityRef`（对象/字符串两种写法统一入口）。判据逐条镜像 Go 的 `protocol.ParseEntityRef`。
- ✅ `src/idempotency.ts`：`tenon:<planId>/<stepId>`。
- ✅ `src/hash.ts`：`sha256:` 规范摘要（审计 `inputHash` / `outputHash` / `prevHash`、模块 `argsHash` 共用）。
- ✅ `src/hmac.ts`：`X-Tenon-Signature` 签名与验签，±300s 容差，`format` / `expired` / `invalid` 三类失败原因。
- ✅ `src/validate/`：schema 校验运行时（`ajv` 2020-12）。同一批 schema 编**两个视图**——接收侧 `removeAdditional: true`（封闭对象剥离未知字段、开放对象原样放行，都**不报错**，符合 `versioning.md` §3.2），一致性侧 `removeAdditional: false`（未知字段即错，用于校验自产报文）。9 类契约各一个类型安全函数 + `validateByContract` 动态入口；错误统一成 `{path, keyword, message}`，`path` 是 JSON Pointer（与 Go 的 `at '/actions/0'` 同形）。
- ✅ `schemas/` + `scripts/sync-schemas.mjs`：schema 从 `spec/0.1/schemas/` 同步进包内（发布后不能引用仓库相对路径），`--check` 比对哈希，已纳入根 `pnpm check`——**规范改了没同步就红**。

**为什么没用 zod**：`@tenon/core` 的职责是「校验规范本身」。zod 需要在 TS 里重声明一份 schema，等于引入第二份真源（实测：重声明里不含新加的 `requiredArgs`，规范改了它不报错），且体积 3.5 MB vs ajv 1.0 MB。

**为什么没引 `ajv-formats`**：全仓 schema 只用到一个 `format`（`date-time`），为一个 format 付 55 KB + 一个 CJS 互操作窟窿不值。改为自己注册 RFC 3339 校验器（正则与 `Iso8601Timestamp.pattern` 同源）。

**交叉校验**：`src/protocol.test.ts` 里的 HMAC 十六进制向量、幂等键、实体寻址字符串都是**用 Go 参考实现生成的**（`protocol.Sign` / `IdempotencyKey` / `EntityRef.String`），不是手算的——两个实现必须字节一致，否则协议分叉。

**测试**：`pnpm --filter @tenon/core test`（先 `tsc` 再 `node --test`）；根目录 `pnpm check` 已把它纳入（validate + typecheck + schema:check + test），CI 会跑。

M1 计划实现 Grant / Policy / Approval / 审计查询报文的 schema 校验，以及 `PlanState` / `ApprovalState` / `StepState` 的合法迁移校验（§5.1）。

M2 计划实现版本协商工具（`Tenon-Version` / `Tenon-Min-Version`，只增不删 + 未知字段忽略，§4.10）与错误分类工具（§4.9），供非 TS 的 HTTP 侧车复用同一语义。

M3 计划实现 skill frontmatter 中 `tenon:` 业务元数据的校验（`subscribes` / `expectedModules` / `outcome`，§6.4）。
