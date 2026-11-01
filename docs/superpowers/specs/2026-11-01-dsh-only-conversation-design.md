# 设计：只在 dsh 对话里完成业务

- 日期：2026-11-01
- 版本：**v1.1**（2026-11-01 修订：通知投递契约、planId 订阅、事件入口 Fast Path、外部 Watchdog）
- 状态：**已批准**（入口只在 dsh；本轮保留 Go agent 作为常驻执行者；长期目标 D1：host 拥有计划/队列，执行者可替换）
- 关联：
  - [`2026-11-01-dsh-as-conversation-layer-design.md`](2026-11-01-dsh-as-conversation-layer-design.md)（对话层方案甲）
  - [`2026-10-29-tenon-approve-suspend-resume-design.md`](2026-10-29-tenon-approve-suspend-resume-design.md)（挂起/恢复/confirm）
  - [`2026-10-31-tenon-execution-ticket-design.md`](2026-10-31-tenon-execution-ticket-design.md)（票据约束）
  - [`docs/adr/0001-driver-dsh-vs-go.md`](../../adr/0001-driver-dsh-vs-go.md)（R2/R3、Q1–Q3）
  - [`docs/superpowers/reviews/2026-11-01-super-agent-article-borrowables.md`](../reviews/2026-11-01-super-agent-article-borrowables.md)（v1.1 的外部对照与借用来源）

### v1.1 改了什么（相对已批准的 v1.0）

| # | 改动 | 为什么 |
|---|---|---|
| 1 | 通知消息的来源改为 `{kind:'plugin', form:'notice'}`，不再冒充 `{kind:'user'}` | `{kind:'user'}` 在 dsh 里是"宿主证明这句话是人说的"，通知冒充人类输入会带来权限错位、审计失真 |
| 2 | 投递分忙/闲两路：忙 `inject`、闲 `followup`；并加唤醒预算 | 无条件 `followup` 会在用户对话中途插入新回合；没有预算的唤醒链会自我激励 |
| 3 | 会话选择从"唯一 root / 钉死 sessionId"改为 **planId 订阅表** | 原策略在多会话或重启后静默停摆，打穿验收场景 4 |
| 4 | 事件入口增加 **Fast Path 确定性过滤**（规划前） | 原流程"每个事件先花一个 LLM 回合规划、再判要不要做"，噪音事件成本全额支付 |
| 5 | 新增**外部 Watchdog**（只看进展、只报人，不自动修） | 内环（轮次上限）挡不住 `resuming` 残留、审计断链这类"进程活着但状态坏了" |
| 6 | 废弃 v1.0 的 notifier 去重/游标写法，改成"只 ack 已投递的连续前缀" | v1.0 的三处写法会静默丢结果帧（见计划的任务失败判据） |

## 1. 意图与为什么

目标只有一个：**用户只在 dsh 的对话框里完成业务**——查、写、审批、审计、修改、撤销、事件触发的回合，都不需要打开看板或业务页。

不把 Go agent 拿掉。原因是它现在承担的不是"规划器"一个角色，而是**不依赖 dsh 进程存活的执行循环**：

- 消费入站队列（`request_submit` 投的原话）并 ack；
- 轮询模块发件箱，事件触发回合（如 `quote.completed`）；
- 规划、过闸门、挂起（I7 三次落盘）、恢复、换票、执行、归档；
- 每 tick 重推公告板、清理过期、重试可重试的帧；
- 持有可恢复计划（pending 文件、argsHash、去重账本）。

dsh 关掉、电脑重启之后，已经挂起的审批还要能完成——这要求一个常驻执行者。本轮保留 Go agent 做这个执行者，把产品入口彻底收到 dsh 里；执行者将来可以换（见 §10 D1）。

## 2. 目标 / 非目标 / 反目标

**目标**

1. 七类操作全部在 dsh 对话里完成：查、写、批/驳、审计、amend、补偿、事件触发。
2. 异步结果**主动**回到对话：待批提醒、执行完成、驳回、修改待批、事件回合完成。
3. 页面只保留只读调试/验收用途，不再承担写或批准。
4. 计划只有一个所有者；本轮不新增第二套 plan store。
5. 审批人身份来自宿主 token；模型不能自报身份。
6. **通知不打断正在进行的对话，也不冒充人说话**（v1.1）。

**非目标（本轮不做）**

- 多用户/多租户（Q2 的 A 档之后再说；MVP 单账号）。
- 把策略/授权/票据/审计链/对账搬进 dsh（那是 D2，已否）。
- C7 多步自动补偿；本轮只做**单步、人工发起**的补偿。
- 审计链里记录 amend 的 `supersedes` lineage（MVP 已知限制，见 §9）。
- 共驾 WS（Intent 仍走 REST 轮询；通知由插件负责）。
- 由 Agent 自行修复治理层（Watchdog 只读巡检 + 报人，见 §6.5）。

**反目标（做了算失败）**

- 给模型一个直接执行写动作的工具（`action_execute` 之类）。
- 让页面重新出现批准/写入口，形成第二个写路径。
- 让模型自己编补偿动作或修改已执行步骤。
- 让 notifier/MCP 直接读 Go agent 的磁盘 pending 目录。
- 同时加载旧 `module-host` 插件，形成两套 pending store/两个 confirm 消费者。
- 通知以人类身份进入会话，或让通知回合能够自我触发下一条通知（v1.1）。

## 3. 已核实的事实与约束

| 事实 | 来源 | 影响 |
|---|---|---|
| `tenon-mcp` 有 7 个工具，**没有**写执行工具 | `internal/mcpserver/tools.go:31,37,51,78,96,103,119` | 写只能走 `request_submit` → agent 落卡 |
| MCP 是拉模式，自己不能把消息推进对话 | MCP 协议 | 需要 dsh 侧通知插件 |
| dsh 插件可 `inject: ['agents']`，用 `ctx.agents.get(id)` / `list()` / `roots()` 找会话；`roots()` 只返回无 owner 的根 agent | `dsh` 源码 `packages/core/agent/src/index.ts:267` 及 `get/list/roots` | 订阅可行，但"唯一 root"不能当契约 |
| `agent.followup(m)` = 排队一个普通回合并唤醒；`agent.inject(m)` = 进 next-step 收件箱、不唤醒 | `packages/core/agent/src/runtime-types.ts` | 忙/闲分流有原生手段 |
| `MessageSourceMap` 里除 `user` 还有 `{kind:'plugin', plugin}`，可带 `form:'notice'` + `summary` | `packages/llm/llm/src/message.ts`；`packages/jobs/tool-jobs/src/index.ts:281-300` | 通知有正确来源，不必冒充人类 |
| `tool-jobs` 已经实现了"忙 inject / 闲 followup + 唤醒预算"这套投递 | 同上 | v1.1 直接沿用规则，但**不接管 job 语义**（见 §6.4） |
| dsh headless 是"一次一个任务、跑完退出"，支持 `--session-id` 续会话，不是守护进程 | `@deepseek-ai/dsh-headless` | 本轮不靠它做常驻执行；D1 迁移时要另选形态 |
| Go agent tick 四段固定：入站 → 自检 → 公告板 → 事件 | `cmd/agent/main.go:605` | 事件回合与公告板自愈只有它在做 |
| 事件回合当前**在规划之后**才过闸门 | `internal/agent/run.go` 回合主流程 | Fast Path 要前移到规划前（§6.3） |
| pending 里有 `state=resuming`，含义是"confirm 已接受、进程崩在执行中途"，且不会自愈 | `internal/agent/board.go` 的 `RepushBoard` 注释 | 需要外环发现（§6.5） |
| host 控制平面没有 LLM 调用 | `README.md` | 不把执行循环塞进 host 的裁决代码 |
| 页面已收口为只读（T11） | `ui/app.js:19,424,522` | 只需固化为约束，不再加写入口 |
| 页面轮询 `/tenon/intents/poll` 但**从不 ack**（只推进自己的 sessionStorage 游标） | `ui/app.js:485-505` | notifier ack 只会让调试页看不到历史，不存在"互相吞帧" |
| `/tenon/intents` 的 poll/ack/enqueue **不做 bearer 校验**（只校验版本头） | `internal/controlplane/http.go:1445-1480` | 本地任意进程可注入/吞掉通知 → §9 风险与部署前提 |
| 旧 `module-host` 插件有自己的 pending store 与 `tenon_action` 工具，**当前 profile 未加载** | `packages/dsh-plugin-module-host`、`~/.dsh/profiles/web/cordis.patch.yml:233-249` | 不进入产品路径；保留为测试资产 |

## 4. 架构与职责

| 组件 | 职责 | 不做什么 |
|---|---|---|
| dsh 对话 | 理解人话、调用 MCP 工具、用 `ask_user_question` 让人选择、展示通知 | 不持有执行权；不直接调模块写 |
| `tenon-mcp` | 读注册中心、只读动作、预检、投请求、列待批（P1.5 起批/驳移到 `tenon_confirm`）、amend、补偿请求、查审计 | 不执行写；不造卡 |
| `dsh-tenon-notifier`（新插件） | 轮询 host 的 intent/pending，按 **planId 订阅**找到会话，按 **忙/闲 + 预算**投递通知 | 不注册模型工具；不执行任何业务；不冒充人类来源 |
| `dsh-tenon-confirm`（P1.5 新插件） | 一个 `tenon_confirm` 工具：弹 DSH 原生审批 → 换 host 一次性确认令牌 → 提交决策 | 不执行任何业务；不替用户选；拿不到原生审批就不批 |
| Go agent（本轮保留） | 入站/事件消费、**事件入口 Fast Path 过滤**、规划、三闸门、挂起/恢复、签票、执行、审计、公告板自愈 | 不直接面对用户 |
| Watchdog（新，只读） | 巡检"进程活着但状态坏了"：resuming 残留、审计断链、过期卡、回合空转 | 不写库、不改策略、不自动重试；只报人 |
| Tenon host | 策略、授权、票据、审计链、公告板 | 不规划、不调 LLM、不调模块 |
| 模块 | 业务动作 | 不信任客户端自报身份 |

```
用户 ↔ dsh 对话（Router：只对话、只分派、不带执行工具）
        ├─ tenon-mcp ──HTTP──> host ──> Go agent ──> modules
        └─ tenon-notifier ◀─轮询── host
                 └─ planId 订阅表 ──(闲 followup / 忙 inject)──> 发起该计划的会话
Watchdog（只读）──巡检──> host 审计 / agent pending 目录 ──报人──> dsh 对话
```

**四条硬边界**

1. 模型永不直接调模块写接口；写只走 `request_submit` → Go agent → 卡 → 人批 → host 签票。
2. 审批身份来自 host token；模型参数里没有 `by` / `approvedBy` / `onBehalfOf`。P1.5 起，批准还必须是 dsh 插件里的一次**原生审批**换来的 host 一次性令牌；模型不能直接调用决策工具。
3. 页面只读；产品入口只有 dsh。
4. **通知不是人类发言**：来源必须是 `kind:'plugin'`，且通知不得形成"通知→回合→新请求→通知"的自激链（v1.1）。

## 5. 业务流

### 5.1 查

模型 `action_invoke`（宿主先查注册表确认 `kind=read`）→ 模块 → 对话给出数值与来源。读动作不进宿主审计（已知边界）。

### 5.2 写 + 审批（主路径）

1. 用户：“把 STD-1001 扣 12 件”。
2. 模型可先 `plan_preview` 给结论（策略/授权/预演）。**允许投机预检**：用户还在描述时后台先跑一次预检（无副作用、不落卡），把"会发生什么/要谁批"提前备好。
3. 模型 `request_submit(text)` → 宿主入队，回 `requestId` / `syntheticEventId`。
4. Go agent：规划 → 三闸门 → `approve` 挂起：pending 文件 + 计划级 dispatched 审计 + 公告板卡（带 `steps[].argsHash`）→ intent。
5. notifier 轮询到新卡/intent → 查订阅表（planId → 发起会话）→ **会话空闲则 `followup` 开一个通知回合，会话忙则 `inject` 进下一步** → 文案："pln_x 待批：扣 12 件；要批准还是驳回？"
6. 模型 `pending_list` → `ask_user_question` → `pending_decide(approve)`。
7. 宿主用 token 覆写 `by=user:alice` 入队 confirm → Go agent 校验角色/过期 → 重跑策略/授权 → `tickets:issue` → 带 `Tenon-Ticket` 调模块 → result 审计 → 归档 done → 终态卡 + intent。
8. notifier 唤醒：“已执行：库存 108→96；approvedBy=user:alice”。
9. 模型在对话里给结论。

失败分支（策略 deny、预演 unsafe、执行报错）都产生 intent + 审计；notifier 只负责送进对话，不改业务结局。

### 5.3 驳回

同 5.2 前 6 步，`pending_decide(reject)` → confirm → agent 归档 rejected → notifier“已驳回，未执行”，库存不变。

### 5.4 Amend（批准前改参数）

1. 用户：“不是 12，改成 8 件”。
2. 模型 `pending_amend(planId, patch)` → 宿主按 `tenon.confirm{decision:'amend', patch}` 入队（人的 token）。
3. Go agent：校验 patch（RFC 6902；只允许未执行步骤的 `steps[i].args`；允许 `add/replace/remove/test`，禁 `move/copy` 与改 `planId/action/stepId/dependsOn`）→ 按 action inputSchema 校验新 args → 重跑策略/授权/预演 → **生成新 planId**（`supersedes` 指回旧计划）→ 旧计划归档 `amended` → 新卡 + intent（“已修改待批：qty 12→8”）。
4. **amend 后的新计划强制再批一次**：即使新参数落进 auto 区间，也不自动执行。否则“把 12 改成 5”就能绕过审批。
5. 人批准新卡 → 按 8 件执行。旧计划 archived `amended`，新计划正常两段审计。
6. 订阅表随 `supersedes` 迁移：新 planId 继承旧 planId 的会话绑定（否则新卡会推给"当前唯一 root"，在多会话下就是错的人）。

### 5.5 补偿（执行后回滚）

1. 用户：“把刚才那笔撤销”。
2. 模型 `request_compensation(planId, reason)` → `tenon.request` 带 `intent:{kind:'compensate', compensatesPlanId}` 入队。
3. Go agent 读归档计划，**不交给 LLM 现编**：校验原计划 done、该步有 `compensation`（`plan.schema.json` 已支持）或按模块约定映射（`inventory.DeductStock` → `inventory.ReleaseStock`），从原参数派生逆操作参数（sku/qty/referenceNo），生成新计划 → 三闸门 → 卡 + intent。
4. 人在对话里批准 → 签票 → `ReleaseStock` → result 审计。
5. `ReleaseStock` 目前没有策略规则、永远 deny；本设计新增“补偿必须人批”的规则。

**范围**：本轮只支持单步计划的补偿；多步 saga/C7 的 `partially_failed` 不在本轮。MVP 只映射 `inventory.DeductStock` → `inventory.ReleaseStock`；其它动作没有声明 `compensation`、也没有约定映射时，agent 明确拒绝并给出原因（不猜一个“差不多”的反向动作）。

### 5.6 事件触发

模块发件箱（如 `quote.completed`）→ Go agent 轮询 → **事件入口 Fast Path 过滤（§6.3）** → 通过者才开回合 → 规划 → 需要批准就落卡 → notifier 按订阅投递（用户没说话也会看到）→ 人在对话里批。

被 Fast Path 判为"直通（auto）"或"忽略"的事件**不调用 LLM**：前者走确定性执行路径（仍然过策略/授权/票据），后者只记日志与计数。两者都不产生 LLM 回合。

### 5.7 审计 / 状态

模型 `audit_query(planId)` 查两段记录；`pending_list` 看终态。对话里把 `approvedBy`、结果、错误码讲清楚。

### 5.8 异常巡检（Watchdog，只读）

见 §6.5。它不参与业务流，只在对话里"有人问或到点巡检"时报出可疑项。

## 6. 契约与改动

### 6.1 工具面（P1：MCP 9 个；P1.5 起 8 个 MCP + 1 个插件工具）

| 工具 | 状态 | 参数 / 返回 |
|---|---|---|
| `registry_list` | 现有 | 注册中心 + 动作 kind |
| `action_invoke` | 现有 | 仅 `kind=read` |
| `plan_preview` | 现有 | 策略/授权/预演结论，不落卡 |
| `request_submit` | 现有 | `text`；投人的原话 |
| `pending_list` | 现有，v1.1 扩参 | `planId?`、`full?`；列表只给摘要；`full` 时返回该单未执行步骤的 `args` + `argsHash`（amend 构造 patch 必须有它）。见 §6.6 |
| `pending_decide` | 现有，**P1.5 删除** | `planId`、`decision: approve\|reject`；P1 临时保留，P1.5 起由插件 `tenon_confirm` 取代 |
| `audit_query` | 现有 | `planId`，返回审计记录 |
| **`pending_amend`** | 新增（P2） | `planId`、`patch`（RFC 6902 数组）、可选 `reason`；只提交修改，不执行 |
| **`request_compensation`** | 新增（P3） | `planId`、可选 `reason`；投结构化补偿请求，不执行 |
| **`tenon_confirm`** | 新增（P1.5，DSH 插件工具） | `planId`；弹原生审批 → 换 host 一次性确认令牌 → 提交 approve/reject；不执行业务 |

工具描述固定：**amend 不是批准**；**补偿请求也要人批**；**`tenon_confirm` 必须弹原生审批，不能替用户选**。`pending_decide` 不塞 patch，避免“改”和“批”混成一步。

### 6.2 Host

- **confirm 校验**：`decision ∈ {approve,reject,amend}`；amend 必须带非空 patch；approve/reject 必须 `patch=null/缺省`。`by/roles` 继续由 token 覆写。
- **确认令牌（P1.5）**：`POST /tenon/approvals:challenge`（human token）先校验 pending 存在、状态 `awaiting_approval`、角色匹配，再发一次性 token（绑定 `by/planId/decision`，TTL 60s）；confirm 必须带 `confirmationToken`，宿主校验并消费、剥离后再入队；缺 token/过期/重放/串单一律 403。该端点不暴露给 MCP。
- **request intent**：`tenon.request` 增加可选 `intent`（0.2.0 增量），只接受 `kind:"compensate"` + `compensatesPlanId` 词法；原样进队列，不影响 origin 登记。
- **策略**：新增 `ReleaseStock` 的 approve 规则；审批角色沿用现有策略口径（dev 为 `role:warehouse_clerk` / `role:warehouse_manager`，生产由策略文件定）。
- **公告板**：amend 产生新 planId + 新卡；旧卡推终态 `amended`。`FreezeSteps` 不改（新计划新快照）。
- **审计口径**：旧计划按 reject 路径归档，计划级 dispatched 留 `unknown`，不补 result；新计划正常两段。`supersedes` 在计划文件/卡上，**审计链不新增字段**（MVP 已知限制）。
- **notices 端点（v1.1 新增，P4）**：`POST /tenon/notices`（role:agent），body `{refId, level, text}`；只承载通知（Watchdog 报告），不进业务队列、不产生计划、不进公告板。
- **intents 端点鉴权（v1.1 新增，P1 并行）**：`POST /tenon/intents` 与 `POST /tenon/intents/ack` 要求 `role:agent`；`GET /tenon/intents/poll` 保持只读可匿名。原因见 §9。若本轮不改，部署前提写死"host 只监听 127.0.0.1，且不与不可信进程共机"。

### 6.3 Go agent：事件入口 Fast Path（v1.1 新增）

现状：事件 → 开回合 → 规划 → 三闸门。问题是**"要不要管这件事"的判定发生在花钱之后**。

改为两级（Fast / Slow Path）：

| 层 | 判据 | 成本 | 结果 |
|---|---|---|---|
| Fast Path（确定性代码） | 事件类型白名单、模块是否已注册、payload 是否带必需字段、来源是否可信、`plan_preview` 级策略三态 | 零 LLM | `ignore`（只计数）/ `auto`（走确定性执行，仍过策略+票据）/ `candidate`（进 Slow Path） |
| Slow Path（LLM） | 现有规划器 | 一个回合 | 现有三闸门不变 |

约束（防走形）：

1. Fast Path **只做否定与直通**，不得放宽任何审批；`auto` 分支仍要过策略、授权、票据、两段审计——它不是"跳过闸门"，而是"跳过 LLM 规划"。
2. Fast Path 的规则表是配置（`config/event-filter.dev.json`），不是散落在代码里的 if。
3. **`auto` 的范围先收紧**：只接受"单步动作 + 全部 args 由事件 payload 字段映射得到"的事件（映射表同在 `event-filter` 里）。需要多步、需要推断参数、或要读别的对象才能定参的事件一律 `candidate`。否则"确定性执行"其实是在代码里现编一份计划，`auto` 就失去了可审计性，而且"零 LLM 回合"这条断言也没法写。
4. 每个事件都要能在审计/日志里回答"被哪一层、哪条规则处置"——否则排查"事件丢了"会变成玄学。

### 6.4 `dsh-tenon-notifier`（新包，v1.1 重写）

**定位**：把 host 的"待批/结果"翻译成 dsh 会话里的一条**通知**。它不是 job 生产者，也不是第二个执行者。

- cordis 插件，`inject: ['agents']`，**不注册任何模型工具**。
- 配置：`hostURL`、`token` 或 `user+password`、`sessionId?`、`intervalMs`（缺省 2000）、`batch`（缺省 5）、`minGapMs`（缺省 2000）、`delivery`（`wakeup|quiet`，缺省 `wakeup`）、`wakeBudget`（缺省 3）、`stateFile`（缺省 `~/.dsh/tenon-notifier/state.json`，绝对路径）、`pendingScan`（缺省 true）。
- 轮询：
  - `GET /tenon/intents/poll?after=<cursor>` 拿"待批/执行完成/驳回/预检失败"；
  - `GET /tenon/pending` 兜底发现 missed 的 awaiting 卡（按 planId 去重）。
  - 响应里的 `dropped`（宿主队列满时丢的累计帧数）必须消费：变化时写日志并在通知里注明"期间有 N 条通知因积压丢失"。**不消费 `dropped` 等于把丢消息变成不可观测。**

**投递（v1.1 核心，照 dsh `tool-jobs` 的规则，不接管它的语义）**

1. **来源**：`{ kind:'plugin', plugin:'tenon-notifier', form:'notice', summary:<一句话> }`。禁止 `{kind:'user'}`。
2. **分流**：`agent.status === 'idle'`（或 status 不可得）且预算未用尽 → `followup`；否则 `inject`（进下一步，不打断正在进行的回合）。
   - status 不可得时按 idle 处理，理由是"待批必须被看见"；这一条的代价（可能插一个回合）写进 §9。
3. **预算**：每个会话连续自行唤醒计数，达 `wakeBudget`（缺省 3）后降级为 `inject`。预算**只在人类输入被领取时回填**（`agent/inbox/claimed` 且 `message.source.kind === 'user'`）。
   - 这条防的是自激链：通知 → 回合 → 模型提交请求 → 请求完成 → 通知 → 回合 …
   - 若来源仍写成 `{kind:'user'}`，预算会把自己的通知当成人类输入而永远回填 —— 这正是第 1 条不能妥协的原因。
4. **节流**：两次唤醒至少隔 `minGapMs`；一轮最多 `batch` 条；超出的**留到下一轮**（不是丢弃）。
5. **文案按 Inline & Spill**（§6.6）：每条只给 `planId｜一句结论｜需要的角色`，全文留给 `pending_list` / `audit_query`。

6. **预算耗尽不等于"看不见"，但也不等于"看得见"**：`inject` 会在会话**下一个回合开始**时被领取（收件箱在回合开始时读取），所以人类下一次开口就能看到；人一直不开口，降级后的通知就是静默的。这条代价必须明说，同时：state 里记 `unread` 计数，`pending_list` 保持权威补齐路径，UI 未读徽标列入 P1.5。

**会话选择（v1.1 改为 refId 订阅表）**

```
resolve(refId):            // refId = planId | requestId
  1. 订阅表 refId → sessionId，且 agents.get(sessionId) 存活  → 用它
  2. config.sessionId                                          → 用它
  3. 上一次成功投递的会话（sticky）                            → 用它
  4. roots() 恰好 1 个                                          → 用它
  5. 否则 → 本轮不投递，日志写明"多根会话且该 refId 无订阅"
投递成功后：订阅表 refId → 该会话（planId 与 requestId 都记）；amend 的 supersedes 关系迁移订阅
```

订阅表持久化在 `stateFile`（`{cursor, subscriptions}`），所以"关掉 dsh 再打开"仍能把旧卡推回**原来那个会话**，而不是碰运气。

**首条通知的限制（builder check 抓出来的）**：订阅是"投递成功后"才写入的，而一张卡第一次出现时还没有任何绑定。所以**首条通知在多会话下仍可能落到 sticky/单 root 上**，订阅只保证"同一条计划的后续帧"回到同一个会话。P1 接受这条并写进文档；P1.5 让 `request_submit` 把发起会话回传给 host（`tenon.request` 带 `origin.sessionId`），pending 卡与 intent 帧都带上，订阅从第一张卡就正确。

**游标与确认（v1.1 重写，三条不变量）**

1. **只 ack 已投递的连续前缀**：`delivered` 只在"该帧已被推送/被有意丢弃/是重复帧"时前进；遇本轮因 `batch` 超限未推的帧就**停止前进**。禁止"读完就 ack"。
2. **不按 planId 折叠帧**：同一计划的多条结论（如 `needs_human` 后又 `completed`）都要送达；折叠只用于文案合并，不用于丢帧。
3. **ack 与本轮有无新内容解耦**：`delivered > cursor` 就尝试 ack；ack 失败保持 `delivered`，下一轮继续重试——不允许出现"ack 失败后游标永远不动"。

其余：单 tick 互斥（慢响应时不允许并发 tick）；失败只记日志不抛穿定时器；dsh 不在线时消息留在 host，下次开 dsh 从 pending/新 intent 补推；**不与旧 `module-host` 插件同时加载**。

**为什么不用 `ctx.jobs` 直接投递**（被否的替代）：`jobs` 的语义是"模型启动的后台工作，可用 `job_output/job_kill` 管理"。host 事件不是模型启动的工作，套进 job 会让模型误以为可以 `job_kill` 一条待批通知。v1.1 只借它的**投递规则**（来源、忙闲、预算），不借它的身份。

### 6.5 外部 Watchdog（v1.1 新增，P4）

内环已有的"观察轮次上限 / 重规划上限"只在进程健康、代码可控时有效。真正没人管的是**中间态残留**。Watchdog 是 **Go 侧的一个只读 tick**——`pending/` 只有它该读（dsh 侧读目录会破 §10 边界规则 1），且它不需要模型判断。按表巡检：

| 症状 | 判据 | 动作 |
|---|---|---|
| 批了不知执行没执行 | `pending/` 里 `state=resuming` 超过 N 分钟 | 报人 + 建议重新 confirm（幂等键挡副作用） |
| 审计断链 | 有 dispatched 无 result，且计划已归档 | 报人 + 给 planId |
| 卡过期仍挂 | `expiresAt < now` 且 `state=awaiting_approval` | 建议 reject 或重提 |
| 回合空转 | request 入队后长时间无 intent/卡/审计 | 报人（模型卡在循环里） |
| 公告板漂移 | 板上有卡、`pending/` 无对应件（或反之） | 报人（read-model 与权威状态不一致） |

边界（写死）：

- **只读**：不写库、不改策略、不重试执行、不"在线修正主系统行为"。
- 报人走 `POST /tenon/notices`（role:agent；`refId` + `level` + 文本），notifier 按 `refId` 解析会话并投递，同样受 §6.4 的预算与分流约束。不新增第二条对话入口。
- 巡检结果本身要可查（写入 `out/watchdog/` 的只读报告），否则"watchdog 没报"无法区分"没问题"和"没跑"。

### 6.6 上下文预算：Inline & Spill（v1.1 新增，贯穿）

有限的上下文窗口当 Cache 管：**内联 ID + 摘要，全文 Spill 到工具后面**。

| 位置 | 现状 | 改为 |
|---|---|---|
| notifier 通知 | 每条塞整段 `summary`，最多 5 条 | `planId｜≤60 字结论｜角色`；末尾给"详情可查 `<planId>`" |
| `pending_list` | 全量卡片（含 `items/steps/expiresAt/mine`） | 列表只给 `planId + 一句 + 状态 + mine`；详情走 `pending_list(planId, full=true)`，含未执行步骤的 `args`/`argsHash` |
| `audit_query` | 返回两段原文 | 默认返回摘要 + 记录数 + 下一跳 ID（`auditId`），全文按需 |

链式召回：`intent → planId → audit 记录 → module.action → result` 天然是引用图，每次召回都内联下一跳的 ID 与摘要，模型可逐跳展开，而不是一次性把 20 条审计灌进窗口。

## 7. 身份与审计

- MVP 单账号：请求/审批用 alice 的 host token；宿主覆写 `by`；Go agent 用 agent token 签票；notifier 只读。
- **P1 的临时口径**：写审批仍走 `pending_decide → confirm`；token 只证明身份，**不证明是谁点的**。P1 验收不得声称“已有人类见证”。
- **P1.5 确认令牌（硬约束）**：`ctx.approval` 是 DSH 插件服务，MCP 进程拿不到；因此把审批入口从 MCP 搬到 dsh 插件工具 `tenon_confirm`：
  1. 模型调 `tenon_confirm(planId)`；插件从 host 取 pending 摘要，调 `ctx.approval.request({agent, toolName:'tenon_confirm', reason})`。
  2. `allowed-once` → decision=approve；`rejected` → decision=reject；`cancelled/unavailable` → 不提交，卡保持待批。
  3. 插件调 `POST /tenon/approvals:challenge` 换一次性 token（绑定 `by/planId/decision`，TTL 60s）。
  4. 插件带 token 调 `POST /tenon/requests` 的 `tenon.confirm`；宿主校验并消费 token，缺少/过期/重放一律 403；token 不进模型上下文。
  5. `pending_decide` 从 `tenon-mcp` 移除；`pending_list` 仍只读。P1.5 验收：不给原生审批、直接调决策端点 → 403。
  - 威胁模型：防“模型跳过提问就批准”；**不防**“同时拥有 host token 与本机文件权限的恶意模型”。后者不在 MVP 威胁模型内。
  - 审计：`approvedBy` 仍来自 token；host 侧记录 `confirmationId` 与消费时间；审计链字段暂不动。
- 模型不能传 `by/approvedBy/onBehalfOf`；沿用现有参数白名单。
- **通知来源**：`kind:'plugin'`（§6.4）；不会让通知回合获得"包含人类直接输入"的资格。
- 已知限制：读动作不进宿主审计；amend 的 lineage 不在审计链；通知是 best-effort。

## 8. 分阶段与验收

| 阶段 | 内容 | 出口判据 |
|---|---|---|
| **P0 通知通道** | notifier 骨架：inject agents、轮询 intents/pending、**planId 订阅**、**忙/闲 + 预算投递**、游标/去重 | 单测：假 agent 收到正确消息且 **忙时是 inject 不是 followup**、**预算耗尽后降级**、**超 batch 不丢**；活 dsh：手工塞一条 intent → 对话出现通知 |
| **P1 对话闭环（MVP）** | 查/写/批/驳/审计全部在对话；通知覆盖待批与结果；页面只读 | 场景 1–5、8 全过；全程不打开看板/业务页；**用户对话中收到通知，原回合不被打断** |
| **P1.5 确认令牌** | `dsh-tenon-confirm` 的 `tenon_confirm`（原生审批 → host challenge → confirm）；MCP 删除 `pending_decide`；intents 端点加 `role:agent` | 不弹原生审批/无 token 直接调决策 → 403；token 过期/重放 → 403；只读 poll 仍可用 |
| **P2 amend** | `pending_amend` + host confirm patch + agent 新计划 + 强制重批 | 场景 6；另测"12 改 5 仍待批"；订阅随 supersedes 迁移 |
| **P3 补偿** | `request_compensation` + ReleaseStock 策略 + agent 确定性逆计划 | 场景 7；补偿失败路径有 result 审计 |
| **P4 事件硬化 + Watchdog** | 事件入口 Fast Path + 通知去重 + 真模型规划质量；只读巡检器 | 场景 5 在真模型下可重复；噪音事件零 LLM 回合；resuming 残留能被报出 |
| **未来 D1** | host 拥有计划/队列，执行者可替换 | 不在本 spec 实施，见 §10 |

### 验收场景（全部在 dsh 对话里）

1. **查**：“STD-1001 还有多少” → 正确数值 + 来源；注册中心三模块。
2. **写 + 批**：“扣 12 件” → 待批通知 → 允许执行 → 库存 −12 → 完成通知 → 审计 `approvedBy=user:alice`。
3. **驳回**：同一请求选“驳回” → 库存不变 → 通知“未执行”。
4. **断线恢复**：发请求后关 dsh/重启 → 重开后**在原来那个会话**看到待批通知 → 批 → 执行；审计完整。
5. **事件**：建报价单触发 `quote.completed` → Fast Path 判 candidate → Go agent 开回合落卡 → 对话通知 → 批准 → 库存/采购变化。
6. **amend**：“改成 8 件” → 新卡 → 批准 → 只扣 8；旧卡 `amended`；审计两单；12 改 5 仍必须再批。
7. **补偿**：“撤销刚才那笔” → 补偿卡（ReleaseStock）→ 批准 → 库存恢复；审计可查。
8. **负向**：`action_invoke` 传写动作被拒；无 token 批准被拒；页面无写/批入口；notifier 工具列表里不存在；**通知不是 `kind:'user'`**。
9. **不打搅（v1.1 新增）**：用户正在长回合里（模型跑工具）时收到待批通知 → 不插新回合，通知在下一步出现。
10. **不自激（v1.1 新增）**：连续 5 条通知后，第 4 条起降级为 inject；期间不出现"通知自己触发新通知"的连环回合。

### 测试

- Go：amend patch 校验/新计划/强制重批；补偿派生/策略/审计；confirm 校验；**Fast Path 三态与"零 LLM"断言**；Watchdog 判据（喂构造的 pending/审计目录）。
- TS：notifier 的订阅解析、忙/闲分流、预算、去重、游标前缀、ack 重试、文案截断；MCP 两个新工具的入参与错误。
- 集成：扩展 `scripts/e2e-dsh.ps1` 覆盖 MCP amend/补偿；notifier 用假 host/假 agent 单测；活 dsh 手工清单 + 截图。
- 安全：模型参数白名单；无 token 批准 401；页面只读（沿用 cdp-check）；**intents 端点越权 ack 被拒**（P1.5）。

## 9. 风险与对策

| 风险 | 对策 |
|---|---|
| 通知在用户对话中途插一个回合 | 忙 → `inject`；只有空闲才 `followup`（§6.4 第 2 条）；验收场景 9 |
| 通知链自我激励（通知→回合→请求→通知） | 唤醒预算 + 只在人类输入时回填（§6.4 第 3 条）；验收场景 10 |
| 通知被伪装成人类发言 | 来源 `kind:'plugin'`；若写成 `user`，预算回填规则会把通知当人话，防自激失效（§6.4 第 1 条） |
| 通知被模型当成指令并自行批准 | 消息固定"仅通知、不要自动批准"；P1.5 把决策入口移出 MCP：`tenon_confirm` 弹原生审批 + host 一次性令牌，模型拿不到 token |
| `/tenon/intents` 无鉴权 | P1.5 加 `role:agent`；在此之前部署前提：只监听回环、不与他机共享 |
| 多会话/重启后通知找不到人 | refId 订阅表 + sticky 回落 + 持久化（§6.4）；验收场景 4 |
| 首条通知在多会话下可能推错会话 | P1 记为已知限制（§6.4）；P1.5 `request_submit` 回传 `origin.sessionId` |
| `inject` 后无人开口 → 通知静默滞留 | 明说该代价；state 记 `unread`；`pending_list` 权威补齐；未读徽标列 P1.5 |
| 通知丢帧 | 只 ack 已投递的连续前缀、不按 planId 折叠、ack 与内容解耦、消费 `dropped`（§6.4） |
| 事件把成本花在噪音上 | Fast Path 前移（§6.3）；规则表可配、可审计 |
| 中间态残留无人发现 | Watchdog 只读巡检（§6.5） |
| dsh preview 的 `ctx.agents`/`followup`/`status` 变化 | notifier 独立成包、版本钉死；`status` 不可得时按 idle 处理；失效降级为"用户问一句再查"，不影响业务执行 |
| 会话绑定/多标签页 | 订阅优先；多根且无订阅时跳过并日志（不广播、不猜） |
| amend 绕过审批 | 新计划强制重批，即使落进 auto；单测锁死 |
| 补偿算错 | agent 从原计划确定性派生；不让 LLM 现编；补偿自身过三闸门 |
| 两套 pending store | 产品 profile 不加载旧 `module-host`；notifier 不碰 plan store |
| 真实模型规划质量 | P1 可用 stub 验收链路；上生产前必须过 D1 的入参契约阻塞项 |
| 审计 lineage | amend 的 `supersedes` 在计划/卡上，不在审计链；MVP 接受 |

## 10. 长期目标 D1：host 拥有计划/队列，执行者可替换

**目标形态**

```
dsh（对话 + 可选的执行者 worker）
        │ 通过 host API 换票/写审计
        ▼
Tenon host（裁决 + 持久工作队列 + 计划状态）
        ▲
        └── Go agent（当前 worker #1，将来可退场）
```

**D1 的含义**：host 拥有持久工作队列与计划快照/审批状态，执行者只是一个可替换的 worker。Go agent 是第一个 worker；将来 dsh 可以成为第二个 worker。host 仍然不规划、不调 LLM；票据/审计/对账不搬。

**为什么必须先让 host 拥有计划状态**：如果计划还在 Go agent 磁盘上，dsh 接管时带不走在途计划，只能停机迁移或两套 store 并存。D1 的第一个里程碑不是"写 dsh worker"，而是"把计划/队列的所有权移到 host"。

**本 spec 的三条边界规则**（P0–P3 现在就要遵守）：

1. notifier / MCP / UI 不得直接读 agent 的 pending 目录，只通过 host 的 board/audit/新 API。
2. 不新增第二套 plan store；旧 `module-host` 插件的 store 不进入产品路径。
3. 计划相关的新字段/状态先想清楚 host 侧怎么表达，再决定 agent 侧怎么缓存。

**D1 迁移步骤（未来项目，不在本 spec 实施）**

1. host 增加 `plans` 持久化 + 队列 claim/ack/heartbeat；把 `pending/`、argsHash、归档从 agent 磁盘迁过来（一次性迁移 + 双读校验）。**事件入口也收进队列**：host 轮询模块发件箱（或 worker 轮询后 claim），不再假设只有 Go agent 会开事件回合。
2. Go agent 改为 worker：claim → 规划 → 挂起/恢复 → 签票 → 执行 → 完成；本地只留缓存。
3. dsh worker：用 `dsh --profile headless` 逐任务消费，或常驻 dsh web 轮询；规划在 dsh，票据/审计仍调 host；Q2 必须有答案。
4. 双跑 canary：同一队列、单写者 claim，验证不重复执行、可互相接管；再切默认、最后移除 Go agent。

**触发条件**：host-owned 状态完成、dsh 有稳定 headless/常驻形态、Q2 有答案、出现多 worker/HA 需求。四条不同时满足就不启动迁移。

**明确不做**：不把策略/票据/审计链搬进 dsh（D2，已否）。

## 11. 决策记录

| # | 决策 | 被否的替代 |
|---|---|---|
| 1 | 保留 Go agent 作为本轮常驻执行者 | 立刻去掉 Go agent（失去 request/event/自愈） |
| 2 | 模型面只用 `tenon-mcp`，不加载 `module-host` 的写工具 | 插件主导（给模型写执行面、两套 store） |
| 3 | amend 用独立 `pending_amend` 工具 | 塞进 `pending_decide`（改/批混淆） |
| 4 | amend 后强制重批，即使策略回 auto | 按新参数自动执行（可绕过审批） |
| 5 | 补偿由 agent 确定性派生 | LLM 现编逆操作（不可审计/易错） |
| 6 | 通知插件不注册模型工具 | 给通知插件一个 `tenon_notify` 工具（多一个模型面） |
| 7 | 页面只读，产品入口只认 dsh | 页面保留写/批（第二个写入口） |
| 8 | 长期 D1：host 拥有计划/队列，执行者可替换 | D2 连 host 一起搬进 dsh（重写安全核心） |
| 9 | 通知来源用 `kind:'plugin'` + `form:'notice'` | 用 `kind:'user'`（冒充人类；且会让唤醒预算永远回填） |
| 10 | 忙 `inject` / 闲 `followup`，加唤醒预算 | 一律 `followup`（打断对话、可自激） |
| 11 | 会话按 planId 订阅解析，订阅表持久化 | 钉死 `sessionId` 或"唯一 root"（重启/多会话即静默停摆） |
| 12 | 只 ack 已投递的连续前缀 | 读完就 ack（超 batch 与同 plan 多帧会静默丢结果） |
| 13 | 借用 `tool-jobs` 的投递规则，但不接管其 job 语义 | 把 host 事件伪装成 job（模型可 `job_kill` 一条待批） |
| 14 | 事件入口 Fast Path 只做否定与直通 | Fast Path 顺便放宽审批（跳过闸门，不可接受） |
| 15 | Watchdog 只读巡检 + 报人 | 让 Watchdog 在线修正主系统（给 LLM 改治理层的权限） |
| 16 | 订阅键用 `refId`（`planId` 或 `requestId`） | 只按 planId 键：Watchdog 报告、预检失败帧无处可去 |
| 17 | 首条通知前无绑定，P1 接受并写明，P1.5 由 `request_submit` 回传会话 | P1 就改 host 契约（已选：先记限制） |
| 18 | 审批入口从 MCP 移到 dsh 插件：`tenon_confirm` 弹原生审批，再换 host 一次性确认令牌 | `pending_decide` + `ctx.approval`（MCP 进程拿不到 DSH 服务；模型可跳过提问） |
| 19 | 确认令牌单次、短 TTL、绑定 by/plan/decision；宿主消费并剥离 | 长期 bearer token 或模型可见 token（可重放/可伪造） |
| 18 | `pending_list` 扩 `planId`/`full`，不加第 10 个工具 | 新增 `pending_detail`（工具面 +1） |
| 19 | Watchdog = Go 只读 tick；报告走 `POST /tenon/notices` | dsh 会话读 `pending/`（破 D1 边界规则 1） |
| 20 | Fast Path 的 `auto` 只允许"单步 + args 全由 payload 映射" | 任意事件都能 auto（等于现编计划，不可审计） |
