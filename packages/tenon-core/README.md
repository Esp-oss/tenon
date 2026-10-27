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

M0（当前）：只有类型骨架 `src/types.ts`，未实现。M0 计划实现 schema 校验（zod 或 ajv 选型后落地）、`EntityRef` 解析/格式化、幂等键构造、`sha256:` 哈希、HMAC-SHA256 签名与验签（含 ±300s 时钟容差）。

M1 计划实现 Grant / Policy / Approval / 审计查询报文的 schema 校验，以及 `PlanState` / `ApprovalState` / `StepState` 的合法迁移校验（§5.1）。

M2 计划实现版本协商工具（`Tenon-Version` / `Tenon-Min-Version`，只增不删 + 未知字段忽略，§4.10）与错误分类工具（§4.9），供非 TS 的 HTTP 侧车复用同一语义。

M3 计划实现 skill frontmatter 中 `tenon:` 业务元数据的校验（`subscribes` / `expectedModules` / `outcome`，§6.4）。
