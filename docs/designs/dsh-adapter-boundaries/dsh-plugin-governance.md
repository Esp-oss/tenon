> **状态：未实现（占位包的职责边界；M2 之后按 [ADR-0001](../../adr/0001-driver-dsh-vs-go.md) §6 重建）。**
> 本文件原在 `packages/<pkg>/README.md`。ADR-0001 删除无 `src` 的占位包，但保留职责边界——实现时按本文件重建，不要照抄目录结构。
# @tenon/dsh-plugin-governance

治理控制平面（§7）：模块 / agent 注册、Grant 授权矩阵、Policy 引擎、Approval、审计存储与查询、对账 job。与运行时无关，可被任意宿主复用。

## 职责边界

- 负责（§7.1）：
  - Registry：agent 身份与模块注册，含可调用的 `模块 × action` 白名单与配额。
  - Grant 矩阵：`effective = agent 被授予 ∩ onBehalfOf 用户权限`（§4.6、D5，取交集不放大权限），对外提供 `POST /tenon/grants:evaluate`。
  - Policy 引擎：9 个算子 + `all` / `any` / `not` 递归条件，多策略命中合并规则 `deny > approve > auto`，同 outcome 取 `priority` 最高者，无命中即 deny（fail-closed）（§4.7），对外提供 `POST /tenon/policies:evaluate`。
  - Approval：`pending → approved | rejected | amended | expired`，`expiresAt` 到期自动 `expired` 且等价 reject（§5.1）。
  - 审计：append-only + `prevHash` 哈希链、两段写存储、`GET /tenon/audit?planId=&since=` 查询（§4.8、§4.10，支撑 C4）。
  - 对账 job：日切比对 `Audit.effects` 与模块终态，差异即**冻结该 agent 写权限**再告警（§8 #4）。
- 不负责：
  - 不执行 action、不碰业务数据（属模块与 `@tenon/dsh-plugin-module-host`）。
  - 不存储或编辑 skill 内容（属 `@tenon/dsh-plugin-skill-biz`）。
  - 不渲染审批界面（属 `@tenon/dsh-plugin-intent-ui`）：本包只产出决策与查询端点。
  - 不允许 agent 读取或改写策略，agent 只能请求评估（§4.7）。

## 依赖

- 对 dsh：`peerDependencies` 声明 `@deepseek-ai/cordis: ">=3"`；复用 dsh 现成原语而不自研——`dsh-authorization`、`dsh-permission-presets`、`dsh-user-approval`（授权与审批原语）、`dsh-session-persistence-jsonl`（审计回放到会话轨迹）、`dsh-otel`（§0 表）。
- 包间依赖：→ `@tenon/core`（Grant / Policy / Audit 的 schema 校验、`EntityRef`、`sha256:` 哈希链）。
- 方向：被 `@tenon/dsh-plugin-module-host` 与 `@tenon/dsh-plugin-intent-ui` 调用；本包不反向依赖这两个插件，以免控制平面被运行时绑死（D11「协议内核 + 多运行时」）。

## 阶段状态

M0（当前）：只有骨架，未实现。M0 计划实现 registry 最小实现、审计两段写与 `GET /tenon/audit` 查询（支撑 C4）、策略与授权评估端点的**桩**（C5 在 M0 只要求「端点不可达时写操作被拒」）。

M1 计划实现 Grant 矩阵与 Policy 引擎真引擎（C5 由桩转真）、Approval 流程与到期失效、审计查询与回放到 dsh 会话轨迹、日切对账 job，并补齐 C6 / C7 依赖的事件重放与补偿审计（§12 M1 出口判据：高风险路径必须人工；对账能发现人为篡改）。

M2 计划实现一致性 badge `tenon-conformant-0.1` 的签发与查询，并对非 TS 模块（HTTP 侧车、MCP 服务）提供同等治理。

M3 计划实现 skill 发布的审计留痕与回滚查询（与 `@tenon/dsh-plugin-skill-biz` 共用同一条哈希链，D8）。
