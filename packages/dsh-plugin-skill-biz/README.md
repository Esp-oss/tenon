# @tenon/dsh-plugin-skill-biz

业务流程 skill 仓库：复用 `dsh-skill`，把「标准件报价完成后核对库存并履约」这类流程写成可审阅、可版本化的**知识**，而不是可执行流程（§6.4）。

## 职责边界

- 负责：
  - skill 的存储与版本化：`SKILL.md` + frontmatter，在 dsh skill 之上增加 `tenon:` 业务元数据（`subscribes` / `expectedModules` / `outcome`，§6.4）。
  - 订阅声明：`subscribes` 只决定「是否开启一个回合」，不决定「做什么」（§2.2）。
  - 发布治理（D8）：单人可发布，每次发布进审计，可一键回滚到上一版本。
  - 版本可追溯：为每次运行固定「用了哪个 skill 版本」，供复现与审计回放（§6.2 T4 加载 `quote-to-fulfillment@1.2`、R7）。
- 不负责：
  - 不执行 skill 正文，执行是 agent 的职责。
  - 不扫描正文里的 `模块.action` 标识符——规则 3 属 `@tenon/lint`（C10），本包只要求在发布前调用它并记录结果。
  - 不规定 skill 怎么写业务语义：那是知识，不是协议。
  - 不设审批卡口：D8 明确用「强审计 + 可回滚」替代发布审批。

## 依赖

- 对 dsh：`peerDependencies` 声明 `@deepseek-ai/cordis: ">=3"`；直接复用 `dsh-skill`（skill 加载与 frontmatter 解析，§0 ①），不自研 skill 格式。
- 包间依赖：→ `@tenon/core`（`tenon:` 元数据结构校验，M3 起）；→ `@tenon/lint`（发布前 C10 规则 3 检查）；→ `@tenon/dsh-plugin-governance` 的审计写入端点（发布与回滚进审计链）。
- 方向：被 agent 运行时读取（T4 加载 skill）；本包不反向调用模块或控制平面的内部实现。

## 阶段状态

M0（当前）：只有骨架，未实现。M0 计划实现**只读** skill 加载与版本固定，够 demo 在 T4 加载 `quote-to-fulfillment@1.2` 即可，不提供发布流程。

M1 计划实现审计链记录 skill 版本引用，使每次 action 调用都能追溯到「当时用的是哪版知识」。

M2 计划实现非 dsh 宿主的只读 skill 分发（HTTP 拉取 + 校验），让侧车接入方也能解释自己的行为依据。

M3 计划实现完整仓库：版本化、单人发布进审计、一键回滚、发布前调用 C10 规则 3 检查（§12 M3 出口判据：新业务流程只改 skill 即可上线）。
