# 设计：只在 dsh 对话里完成业务

- 日期：2026-11-01
- 状态：**已批准**（入口只在 dsh；本轮保留 Go agent 作为常驻执行者；长期目标 D1：host 拥有计划/队列，执行者可替换）
- 关联：
  - [`2026-11-01-dsh-as-conversation-layer-design.md`](2026-11-01-dsh-as-conversation-layer-design.md)（对话层方案甲）
  - [`2026-10-29-tenon-approve-suspend-resume-design.md`](2026-10-29-tenon-approve-suspend-resume-design.md)（挂起/恢复/confirm）
  - [`2026-10-31-tenon-execution-ticket-design.md`](2026-10-31-tenon-execution-ticket-design.md)（票据约束）
  - [`docs/adr/0001-driver-dsh-vs-go.md`](../../adr/0001-driver-dsh-vs-go.md)（R2/R3、Q1–Q3）

## 1. 意图与为什么

目标只有一个：**用户只在 dsh 的对话框里完成业务**——查、写、审批、审计、修改、撤销、事件触发的回合，都不需要打开看板或业务页。

不把 Go agent 拿掉。原因是它现在承担的不是“规划器”一个角色，而是**不依赖 dsh 进程存活的执行循环**：

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

**非目标（本轮不做）**

- 多用户/多租户（Q2 的 A 档之后再说；MVP 单账号）。
- 把策略/授权/票据/审计链/对账搬进 dsh（那是 D2，已否）。
- C7 多步自动补偿；本轮只做**单步、人工发起**的补偿。
- 审计链里记录 amend 的 `supersedes` lineage（MVP 已知限制，见 §9）。
- 共驾 WS（Intent 仍走 REST 轮询；通知由插件负责）。

**反目标（做了算失败）**

- 给模型一个直接执行写动作的工具（`action_execute` 之类）。
- 让页面重新出现批准/写入口，形成第二个写路径。
- 让模型自己编补偿动作或修改已执行步骤。
- 让 notifier/MCP 直接读 Go agent 的磁盘 pending 目录。
- 同时加载旧 `module-host` 插件，形成两套 pending store/两个 confirm 消费者。

## 3. 已核实的事实与约束

| 事实 | 来源 | 影响 |
|---|---|---|
| `tenon-mcp` 有 7 个工具，**没有**写执行工具 | `internal/mcpserver/tools.go` | 写只能走 `request_submit` → agent 落卡 |
| MCP 是拉模式，自己不能把消息推进对话 | MCP 协议 | 需要 dsh 侧通知插件 |
| dsh 插件可 `inject: ['agents']`，用 `ctx.agents.get(id)` / `list()` / `roots()` 找会话，`agent.followup(message)` 唤醒 | `dsh-agent` 类型；`module-host` 的 wake 已验证 | notifier 可行，且不需要注册模型工具 |
| dsh headless 是“一次一个任务、跑完退出”，支持 `--session-id` 续会话，不是守护进程 | `@deepseek-ai/dsh-headless` | 本轮不靠它做常驻执行；D1 迁移时要另选形态 |
| Go agent tick 四段固定：入站 → 自检 → 公告板 → 事件 | `cmd/agent/main.go` | 事件回合与公告板自愈只有它在做 |
| host 控制平面没有 LLM 调用 | `README.md` | 不把执行循环塞进 host 的裁决代码 |
| 页面已收口为只读（T11） | `ui/app.js`、`ui/index.html` | 只需固化为约束，不再加写入口 |
| 旧 `module-host` 插件有自己的 pending store 与 `tenon_action` 工具，**当前 profile 未加载** | `packages/dsh-plugin-module-host`、`~/.dsh/profiles/web/cordis.patch.yml` | 不进入产品路径；保留为测试资产 |

## 4. 架构与职责

| 组件 | 职责 | 不做什么 |
|---|---|---|
| dsh 对话 | 理解人话、调用 MCP 工具、用 `ask_user_question` 让人选择、展示通知 | 不持有执行权；不直接调模块写 |
| `tenon-mcp` | 读注册中心、只读动作、预检、投请求、列待批、批/驳、amend、补偿请求、查审计 | 不执行写；不造卡 |
| `dsh-tenon-notifier`（新插件） | 轮询 host 的 intent/pending，按会话 `followup` 唤醒，批量+去重 | 不注册模型工具；不执行任何业务 |
| Go agent（本轮保留） | 入站/事件消费、规划、三闸门、挂起/恢复、签票、执行、审计、公告板自愈 | 不直接面对用户 |
| Tenon host | 策略、授权、票据、审计链、公告板 | 不规划、不调 LLM、不调模块 |
| 模块 | 业务动作 | 不信任客户端自报身份 |

```
用户 ↔ dsh 对话
        ├─ tenon-mcp ──HTTP──> host ──> Go agent ──> modules
        └─ tenon-notifier ◀─轮询── host ──followup──> 当前会话
```

**三条硬边界**

1. 模型永不直接调模块写接口；写只走 `request_submit` → Go agent → 卡 → 人批 → host 签票。
2. 审批身份来自 host token；模型参数里没有 `by` / `approvedBy` / `onBehalfOf`。
3. 页面只读；产品入口只有 dsh。

## 5. 业务流

### 5.1 查

模型 `action_invoke`（宿主先查注册表确认 `kind=read`）→ 模块 → 对话给出数值与来源。读动作不进宿主审计（已知边界）。

### 5.2 写 + 审批（主路径）

1. 用户：“把 STD-1001 扣 12 件”。
2. 模型可先 `plan_preview` 给结论（策略/授权/预演）。
3. 模型 `request_submit(text)` → 宿主入队，回 `requestId` / `syntheticEventId`。
4. Go agent：规划 → 三闸门 → `approve` 挂起：pending 文件 + 计划级 dispatched 审计 + 公告板卡（带 `steps[].argsHash`）→ intent。
5. notifier 轮询到新卡/intent → `followup`：“pln_x 待批：扣 12 件；要批准还是驳回？”
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

### 5.5 补偿（执行后回滚）

1. 用户：“把刚才那笔撤销”。
2. 模型 `request_compensation(planId, reason)` → `tenon.request` 带 `intent:{kind:'compensate', compensatesPlanId}` 入队。
3. Go agent 读归档计划，**不交给 LLM 现编**：校验原计划 done、该步有 `compensation`（`plan.schema.json` 已支持）或按模块约定映射（`inventory.DeductStock` → `inventory.ReleaseStock`），从原参数派生逆操作参数（sku/qty/referenceNo），生成新计划 → 三闸门 → 卡 + intent。
4. 人在对话里批准 → 签票 → `ReleaseStock` → result 审计。
5. `ReleaseStock` 目前没有策略规则、永远 deny；本设计新增“补偿必须人批”的规则。

**范围**：本轮只支持单步计划的补偿；多步 saga/C7 的 `partially_failed` 不在本轮。MVP 只映射 `inventory.DeductStock` → `inventory.ReleaseStock`；其它动作没有声明 `compensation`、也没有约定映射时，agent 明确拒绝并给出原因（不猜一个“差不多”的反向动作）。

### 5.6 事件触发

模块发件箱（如 `quote.completed`）→ Go agent 轮询 → 自动开回合 → 规划 → 需要批准就落卡 → notifier 主动唤醒对话（用户没说话也会看到）→ 人在对话里批。

### 5.7 审计 / 状态

模型 `audit_query(planId)` 查两段记录；`pending_list` 看终态。对话里把 `approvedBy`、结果、错误码讲清楚。

## 6. 契约与改动

### 6.1 MCP 工具面（9 个，没有 `action_execute`）

| 工具 | 状态 | 参数 / 返回 |
|---|---|---|
| `registry_list` | 现有 | 注册中心 + 动作 kind |
| `action_invoke` | 现有 | 仅 `kind=read` |
| `plan_preview` | 现有 | 策略/授权/预演结论，不落卡 |
| `request_submit` | 现有 | `text`；投人的原话 |
| `pending_list` | 现有 | 待批/已处理 |
| `pending_decide` | 现有 | `planId`、`decision: approve\|reject` |
| `audit_query` | 现有 | `planId`，返回审计记录 |
| **`pending_amend`** | 新增 | `planId`、`patch`（RFC 6902 数组）、可选 `reason`；只提交修改，不执行 |
| **`request_compensation`** | 新增 | `planId`、可选 `reason`；投结构化补偿请求，不执行 |

工具描述固定：**amend 不是批准**；**补偿请求也要人批**。`pending_decide` 不塞 patch，避免“改”和“批”混成一步。

### 6.2 Host

- **confirm 校验**：`decision ∈ {approve,reject,amend}`；amend 必须带非空 patch；approve/reject 必须 `patch=null/缺省`。`by/roles` 继续由 token 覆写。
- **request intent**：`tenon.request` 增加可选 `intent`（0.2.0 增量），只接受 `kind:"compensate"` + `compensatesPlanId` 词法；原样进队列，不影响 origin 登记。
- **策略**：新增 `ReleaseStock` 的 approve 规则；审批角色沿用现有策略口径（dev 为 `role:warehouse_clerk` / `role:warehouse_manager`，生产由策略文件定）。
- **公告板**：amend 产生新 planId + 新卡；旧卡推终态 `amended`。`FreezeSteps` 不改（新计划新快照）。
- **审计口径**：旧计划按 reject 路径归档，计划级 dispatched 留 `unknown`，不补 result；新计划正常两段。`supersedes` 在计划文件/卡上，**审计链不新增字段**（MVP 已知限制）。

### 6.3 Go agent

- `Confirm` 增加 `Patch`；`decision=amend` 走独立处理器（校验、重跑、新计划、强制重批）。
- 强制重批的落点：带 `supersedes` 的新计划状态固定 `awaiting_approval`，agent 的 auto 分支**跳过**这类计划；只有 confirm 才能推进。
- 补偿：读归档计划 → 校验 done + compensation 映射 → 派生逆操作 → 新计划 → 三闸门 → 卡。
- intent 文案补齐“待批/已执行/已驳回/已修改待批/补偿待批”，给 notifier 直接用。
- 不新增第二套 store；不把 plan 状态写到 notifier/MCP 能直接读的位置。

### 6.4 `dsh-tenon-notifier`（新包）

- cordis 插件，`inject: ['agents']`，**不注册任何模型工具**。
- 配置：`hostURL`、`token`（MVP 用 alice 的 host token）、`sessionId?`、`intervalMs`（缺省 2000）、`stateFile`。
- 轮询：
  - `GET /tenon/intents/poll?after=<cursor>` 拿“待批/执行完成/驳回/预检失败”；
  - `GET /tenon/pending` 兜底发现 missed 的 awaiting 卡（按 planId 去重）。
- 会话选择：`config.sessionId` → `ctx.agents.get(id)`；否则仅当 `ctx.agents.roots()` 恰好一个时用它；多于一个跳过并日志（MVP 单会话，不广播）。
- 唤醒：`agent.followup(createUserMessage({content:[{type:'text',text}],source:{kind:'user'}}))`；消息开头“【Tenon 通知】仅通知，不要自动批准”；批量最多 5 行，两次唤醒至少隔 2 秒。
- **游标持久化，pending 去重只在内存**：intent 游标写 `stateFile`；插件每次启动先扫一遍 `/tenon/pending`，把仍 `awaiting_approval` 的卡各提醒一次（内存集合去重），保证“关掉 dsh 再打开仍能看到待批”。重复提醒一次比漏掉一张卡好。
- 唤醒成功后 `POST /tenon/intents/ack`（页面是调试视图，看不到已 ack 帧是可接受代价）；唤醒失败不 ack，下轮重试。
- 失败只写日志、不抛穿定时器；dsh 不在线时消息留在 host，下次开 dsh 从 pending/新 intent 补推。
- **不与旧 `module-host` 插件同时加载**：产品 profile 只挂 `tenon-mcp` + `tenon-notifier`。

## 7. 身份与审计

- MVP 单账号：请求/审批用 alice 的 host token；宿主覆写 `by`；Go agent 用 agent token 签票；notifier 只读。
- **回合内 DSH 的 `ctx.approval` 不作为写动作的审批来源**（它会落回部署配置身份）；所有写审批统一走 `ask_user_question → pending_decide → confirm`，`approvedBy` 永远来自 token。
- 模型不能传 `by/approvedBy/onBehalfOf`；沿用现有参数白名单。
- 已知限制：读动作不进宿主审计；amend 的 lineage 不在审计链；通知是 best-effort。

## 8. 分阶段与验收

| 阶段 | 内容 | 出口判据 |
|---|---|---|
| **P0 通知通道** | notifier 骨架：inject agents、轮询 intents/pending、会话绑定、followup、游标/去重 | 单测：假 agent 收到正确消息；活 dsh：手工塞一条 intent → 对话出现通知 |
| **P1 对话闭环（MVP）** | 查/写/批/驳/审计全部在对话；通知覆盖待批与结果；页面只读 | 场景 1–5、8 全过；全程不打开看板/业务页 |
| **P2 amend** | `pending_amend` + host confirm patch + agent 新计划 + 强制重批 | 场景 6；另测“12 改 5 仍待批” |
| **P3 补偿** | `request_compensation` + ReleaseStock 策略 + agent 确定性逆计划 | 场景 7；补偿失败路径有 result 审计 |
| **P4 事件硬化** | 事件回合通知去重、真实模型下的规划质量（D1 阻塞项） | 场景 5 在真模型下可重复 |
| **未来 D1** | host 拥有计划/队列，执行者可替换 | 不在本 spec 实施，见 §10 |

### 验收场景（全部在 dsh 对话里）

1. **查**：“STD-1001 还有多少” → 正确数值 + 来源；注册中心三模块。
2. **写 + 批**：“扣 12 件” → 待批通知 → 允许执行 → 库存 −12 → 完成通知 → 审计 `approvedBy=user:alice`。
3. **驳回**：同一请求选“驳回” → 库存不变 → 通知“未执行”。
4. **断线恢复**：发请求后关 dsh/重启 → 重开后看到待批通知 → 批 → 执行；审计完整。
5. **事件**：建报价单触发 `quote.completed` → Go agent 自动开回合落卡 → 对话通知 → 批准 → 库存/采购变化。
6. **amend**：“改成 8 件” → 新卡 → 批准 → 只扣 8；旧卡 `amended`；审计两单；12 改 5 仍必须再批。
7. **补偿**：“撤销刚才那笔” → 补偿卡（ReleaseStock）→ 批准 → 库存恢复；审计可查。
8. **负向**：`action_invoke` 传写动作被拒；无 token 批准被拒；页面无写/批入口；notifier 工具列表里不存在。

### 测试

- Go：amend patch 校验/新计划/强制重批；补偿派生/策略/审计；confirm 校验。
- TS：notifier 的会话选择、去重、游标、唤醒消息、失败重试；MCP 两个新工具的入参与错误。
- 集成：扩展 `scripts/e2e-dsh.ps1` 覆盖 MCP amend/补偿；notifier 用假 host/假 agent 单测；活 dsh 手工清单 + 截图。
- 安全：模型参数白名单；无 token 批准 401；页面只读（沿用 cdp-check）。

## 9. 风险与对策

| 风险 | 对策 |
|---|---|
| dsh preview 的 `ctx.agents`/`followup` 变化 | notifier 独立成包、版本钉死；失效时降级为“用户问一句再查”，不影响业务执行 |
| 通知被模型当成指令 | 消息固定“仅通知、不要自动批准”；notifier 无工具；验收专门看它不自行执行 |
| 会话绑定/多标签页 | MVP 单会话（配置 sessionId 或唯一 root）；多标签只推一个，其余靠 `pending_list` 补 |
| 通知丢失/重复 | 游标 + ack + planId 去重；权威是 pending/审计 |
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

**为什么必须先让 host 拥有计划状态**：如果计划还在 Go agent 磁盘上，dsh 接管时带不走在途计划，只能停机迁移或两套 store 并存。D1 的第一个里程碑不是“写 dsh worker”，而是“把计划/队列的所有权移到 host”。

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
