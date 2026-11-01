> **状态：未实现（占位包的职责边界；M2 之后按 [ADR-0001](../../adr/0001-driver-dsh-vs-go.md) §6 重建）。**
> 本文件原在 `packages/<pkg>/README.md`。ADR-0001 删除无 `src` 的占位包，但保留职责边界——实现时按本文件重建，不要照抄目录结构。
# @tenon/dsh-plugin-module-host

cordis 插件（参考实现#1）：把 `defineModule({ id, actions, events })` 挂进 dsh。模块一变，agent 通过能力发现就看见它，不需要为业务流程写事件处理器。

## 职责边界

- 负责：
  - `defineModule({ id, actions, events })` 的宿主侧接线：把模块声明的 action 注册成 dsh tool，把 events 注册成可订阅的领域事件。
  - 能力发现：产出 manifest 摘要（§4.1），完整 Action（含 `inputSchema`）按需拉取——摘要优先以控上下文成本（D15）。
  - 事件订阅：只声明「关心哪些事实」，即 §2.2 允许的绑定；绝不承担「事件 → action」的映射。
  - 审计两段写：执行前写 `stage=dispatched`，写失败即拒绝执行；执行后写 `stage=result`（§4.8、§8 #5）。
  - 执行语义：幂等键注入、`timeoutMs` 超时、仅对 `retryable` 的退避重试、熔断、saga 逆序补偿（§5.2）。
  - HMR 热插拔：模块重载后 action / event 清单立即更新，不必重启 dsh。
- 不负责：
  - 不做授权与策略判定：一律请求控制平面（G2 / G3，§6.2）。
  - 不实现模块的业务逻辑，也不替模块校验前置条件——「库存够不够」永远由模块判定，agent 不可绕过（§3.2 #2）。
  - 不定义协议类型与 schema（属 `@tenon/core`），不做前端（属 `@tenon/dsh-plugin-intent-ui`）。
  - 不写「某事件/某错误 → 调某 action」的胶水代码（§2.2 反模式判据）。

## 依赖

- 对 dsh：`peerDependencies` 声明 `@deepseek-ai/cordis: ">=3"`，复用 dsh 现成原语而不自研——`dsh-jobs-local`（任务队列，承载超时与重试）、`dsh-otel`（审计与轨迹埋点）、`dsh-session-persistence-jsonl`（回放用轨迹，§0 表）。
- 包间依赖：→ `@tenon/core`（类型、幂等键构造、`sha256:` 哈希）；→ `@tenon/dsh-plugin-governance`（运行时请求 G2/G3 评估，方向单向：调用其公开端点，不 import 内部实现）。
- 不依赖 `@tenon/dsh-plugin-intent-ui`：Intent 帧由运行时发出，模块宿主不感知前端是否存在（§3.2 #1 前端是可选增强）。

## 阶段状态

M0（当前）：只有骨架，未实现。M0 计划实现 `defineModule` 接线、action → dsh tool 注册、事件订阅、审计两段写（含「注入 `dispatched` 写失败必须拒绝执行」这条可测出口判据，§12）与 HMR 热插拔。

M1 计划实现治理闸门接入：G2 策略评估、G3 授权评估与审批等待（§6.2 T8–T9），以及 saga 补偿与 `partially_failed` 的审计标记（C7）。

M2 计划实现 MCP 双向映射（tool 名 `tenon_<module>_<action>`，§4.10 / D7），并与 `@tenon/sidecar-http` 对接，使非 TS 模块获得与内嵌模块相同的挂载语义。

M3 计划支持按 skill 版本固定能力清单快照，供「从审计回放到 dsh 会话轨迹」使用（§7.1）。
