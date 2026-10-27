# @tenon/dsh-plugin-intent-ui

共驾三帧的前端通道：WS 主通道 + REST 降级 + 前端 UI 插件。**可选增强**——不接入也不影响业务正确性（§3.2 #1）。

## 职责边界

- 负责：
  - `WS /tenon/intents` 三帧通道，承载 Intent / Context / Confirm（§4.5、§4.10）。
  - REST 降级：`GET /tenon/intents/poll` + `POST /tenon/intents/ack`，WS 不可用时等价工作（§4.10）。
  - 前端 UI 插件：消费 Intent 帧（`open` / `highlight`，含 badge 与 `undoable`），产出 Context（视图 + 编辑）与 Confirm（`approve` / `reject` / `amend`，amend 带 RFC 6902 patch）。
  - demo 共驾面板：`demo/mini-erp` 页面上显示「已扣 3 件、缺口 9 件已发起采购申请」这类可撤销提示（§6.3）。
- 不负责：
  - 不决定 Plan、不做策略或授权判定（G2 / G3 属控制平面）。
  - 不定义业务页面数据，不碰模块内部实现。
  - 帧内不含 DOM 选择器、不含坐标；不做 UI / DOM 自动化（§1.2 非目标、§4.5）。
  - 不判定 approve 是否合法：本包只承载人的决定，合法性由 governance 的 Approval 判定。

## 依赖

- 对 dsh：`peerDependencies` 声明 `@deepseek-ai/cordis: ">=3"`；复用 dsh 现成原语——`dsh-client-ui-*`（前端插件挂载，§0 ③）。
- 包间依赖：→ `@tenon/core`（三帧类型与 `EntityRef`）；→ `@tenon/dsh-plugin-governance`（读取 Approval 状态与 `expiresAt`，只读端点）。
- 方向：被 dsh 的 UI 装配；`@tenon/dsh-plugin-module-host` 不依赖本包（前端缺席时 Intent 帧直接丢弃）。

## 阶段状态

M0（当前）：只有骨架，未实现。M0 计划实现 WS 三帧通道 + REST 降级端点 + demo 共驾面板（Intent 展示、highlight、撤销按钮）。

M1 计划实现审批 UI：Confirm 帧的编解码、amend 的 patch 输入与校验、审批队列与 `expiresAt` 到期提示（§12 M1「审批 UI」）。

M2 计划实现与框架无关的最小前端 SDK（只做三帧编解码，不绑任何前端框架），并补齐 WS 断线重连与 REST 轮询路径的端到端用例。

M3 计划把 Intent 帧与 skill 轨迹对齐，用于轨迹蒸馏结果的可视化复核（§12 M3）。
