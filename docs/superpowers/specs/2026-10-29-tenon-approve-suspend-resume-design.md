# approve 从「只报告」到「真挂起 + 可续跑」

- 日期：2026-10-29
- 状态：**已实现**（2026-10-29）。D1 / D3 按证据推定（可否决）、D2 裁决 A。实测：`node scripts/verify-module-host.mjs` **77 条断言全过**，`pnpm check` 全绿
- 关联：[ADR-0001 §6 第 3 步](../../adr/0001-driver-dsh-vs-go.md)（approval 接 DSH + 公告板为真相）、[POC-2](../designs/poc-approval-restart.md)（审批跨重启不存活）、[R2 验收 Q2](2026-10-27-tenon-acceptance-and-module-registration.md)（操作员身份绑定）、[威胁模型 M7](2026-10-27-tenon-security-threat-model.md)（禁止 agent 自批）

---

## 1. 目标与边界

**目标**：`approve` 结局不再是死胡同——挂起有 durable 记录、能被人在回合内批准、批准后按原计划执行，审计写明「谁批的」。

**本轮做**：
1. `approve` 时把整份计划落盘（`pending/<planId>.json`），写一条**计划级** `dispatched` 审计；
2. 同回合内，若本会话操作员有资格，经 DSH 审批 UI 问人，`allowed-once` 即按原计划执行（同一 planId、同一幂等键）；
3. 问不到人（拒/超时/无应答者/无资格）时保持挂起，返回 `needs_approval`；
4. `tenon_pending`（列待批）与 `tenon_resume(planId)`（新回合内续跑）两个工具；
5. 续跑成功写 step 级两段审计（带 `approvedBy`）+ 计划级 `result`，归档 `done`。

**本轮不做**：公告板推送（§3.1 证据说明为何做不了）、后台轮询 / 自动唤醒、事件回合（R3）、多步计划（TS 侧只有 `s1`）、grant 校验（TS 侧从未实现，本轮不假装做）、DSH 前端面板（`package.json` 无 `dsh.client`）。

## 2. 现状事实（决定了为什么这么设计）

| 事实 | 证据 | 含义 |
|---|---|---|
| TS 侧 `approve` **什么都不留**：planId 每次现生成，返回后即丢弃 | `packages/dsh-plugin-module-host/src/orchestrate.ts:134`、`:154-168` | 今天的 `needs_approval` 里那个 `planId` 无物可续，**续跑必须先有落盘** |
| Go 侧挂起是落盘 + **一条**计划级 `dispatched`（`stepId:null`、`action:""`、`inputHash:""`、`outcome=approve`、`approvedBy:null`、`result.status=unknown`），I7 三步：存 pending → 写审计 → 回写 `auditSettled` | `internal/agent/suspend.go:115-163`、`internal/agent/run.go:363-382` | 这是要复刻的语义基线 |
| Go 侧续跑顺序：查归档（done 即幂等返回）→ 读回 plan → 验审批人 → 判过期 → **重跑** policy → 执行 → 归档 | `internal/agent/resume.go:123-250`、`:208-212` | 续跑**不信任**挂起时的结论，要重跑闸门 |
| Go 侧 TTL 15 分钟，过期只写 result 后归档 `expired`，**绝不执行** | `internal/agent/suspend.go:18`、`resume.go:215-227` | 过期是硬失败，不是重试 |
| 公告板 `POST /tenon/pending` 只允许 `role:agent` token（无 token 401、普通用户 403） | `internal/controlplane/pending.go:116-120` | **adapter 无 token → 推不了卡片**（D1 的直接原因） |
| DSH 审批：`ctx.approval.request(req): Promise<'allowed-once'｜'rejected'｜'cancelled'｜'unavailable'>` | `dsh-user-approval/lib/types/index.d.ts:135`、`types.d.ts:26` | 四个结局全封闭，`allowed-once` 是唯一放行 |
| `request()` **必须在开着的回合内**；闲置时提问直接拒绝 | `dsh-user-approval/lib/types/index.d.ts:121-124`；[POC-2](../designs/poc-approval-restart.md) | 「后台等批再唤醒」不能用它；只有回合内问 |
| 工具执行上下文带 `exec.agent?`（可选）与 `exec.signal` | `dsh-tools/lib/types/index.d.ts:229`、`:241` | 审批请求要的 `agent` 拿得到；取消信号可透传 |
| 工具可自愿声明 `timeoutMs`，不声明则不超时 | `dsh-tools/lib/types/schema.d.ts:197` | 阻塞等批不会被默认超时剁掉，但**取消**要自己观察 `exec.signal` |
| 审计必须回答「是哪个人按的」 | [验收 spec §Q2](2026-10-27-tenon-acceptance-and-module-registration.md)（`acceptance:63`） | `approvedBy` 不能写 `agent`、不能空 |

**结论**：DSH 侧没有「Durable pending 表」，Go 的 `cmd/agent` 那套（轮询入站队列 + confirm 帧）在 adapter 无 token 的前提下也借不到。**挂起的 durable 副本只能由 adapter 自己持有**——这与 ADR 不变量 3 的措辞一致：「pending 的权威副本在 agent 状态目录 / 公告板」，adapter 就是 DSH 侧的 agent。

## 3. 三条裁决

### 3.1 D1 挂起的权威副本落在哪（建议：adapter 本地状态目录）— 证据推定

| 选项 | 代价 / 证据 |
|---|---|
| **A. adapter 本地 `pending/` 目录（建议）** | 与 Go 同构（`state.go:82-84` 的 `$TENON_AGENT_STATE_DIR/pending/`）；不依赖 token；缺点是公告板上看不见这张卡 |
| B. 推 Go 公告板做真相 | **被证据否掉**：`pending.go:116-120` 要求 `role:agent` token，adapter 没有；且 `pending.go:34-38` 明写公告板是 read-model、刻意不落盘 |
| C. 新增 host 端点并存 pending | 动已冻结的控制平面（ADR §2），且与 `interfaces.md:494` 的既有裁决冲突 |

**建议 A**，并在验收里显式记一条缺口：**DSH 侧的待批卡片在公告板上不可见**，要看只能问 `tenon_pending`。
状态目录复用 `TENON_AGENT_STATE_DIR` 的语义（默认 `out/agent-state`），新增配置 `stateDir`，缺省 `out/module-host-state`，与 Go 的分开（两份实现不共用一份盘上状态，避免互相踩）。

### 3.2 D2 谁批准、`approvedBy` 写什么（**已裁决：A**，2026-10-29）

约束是两条硬要求打架：审计必须写「哪个人按的」（`acceptance:63`），而 DSH 的审批结局**只给 outcome + requestId，不给身份**（`dsh-user-approval/lib/types/types.d.ts:26-51`）。

| 选项 | 做法 | 代价 |
|---|---|---|
| **A. 显式角色配置（建议）** | 新增 `config.operatorRoles: string[]`，视为「本会话操作员实际持有的角色」。续跑前要求它与 `requiredRoles` **有交集**，无交集则不提问、直接拒绝（fail-closed）；有交集才问 DSH，`allowed-once` 后执行，`approvedBy = config.onBehalfOf` | 配置成为**授权输入**（比今天「身份只进审计」更强），必须写明这是 Q2 未定前的显式绑定；未配置 `operatorRoles` 的部署**无法批准任何东西**（默认拒绝） |
| B. 有 DSH 审批即放行，角色只展示 | 不校验角色，`approvedBy = onBehalfOf` | 治理降级：仓管审批形同虚设，销售可自批超额扣减——与 `requiredRoles` 的语义冲突 |
| C. `approvedBy` 写审批引用 `dsh-approval:<id>` | 不编人名 | 违反 `acceptance:63`（必须能回答「哪个人」），审计里只有技术 id |
| D. 本轮只落盘挂起，不续跑 | 等 Q2 定 | 保守，但 `needs_approval` 仍是死胡同，等于没走这条岔路 |

**建议 A**，并在 spec / README 里如实写：「DSH 不 attest 身份，`operatorRoles` 是部署声明而非会话证明；这正是 Q2 要定的东西，定下来之前它是有意的临时答案。」

### 3.3 D3 挂起时是否写计划级 `dispatched` 审计（建议：写，与 Go 同构）— 证据推定

| 选项 | 理由 |
|---|---|
| **A. 写（建议）** | 「挂起发生过」需要审计痕迹：否则一次 pending 只剩盘上 JSON，审计链看不到请求存在过。与 Go 同形（`stepId:null` / `action:""` / `inputHash:""` / `outcome:approve` / `approvedBy:null` / `result.status:unknown`），便于 D1/C 系列比对 |
| B. 不写 | 审计只记「真发生的事」；但会与 Go 侧产生形状差异，且挂起无审计痕迹 |

已知代价（照抄 README 已有记录）：计划级记录**不满足** `audit-event.schema.json` 的完整 required 集，是刻意取舍（[README:265](../../../README.md)），不是疏漏。本轮沿用同一取舍，**不新增**第二种偏离。

## 4. 状态机与盘上布局

```
<pending>/<planId>.json      state = awaiting_approval | resuming
<pending>/archived/<planId>.json   state = done | rejected | expired | stale
```

- 单文件即一条待批：`{planId, module, action, args(规范 JSON), inputHash, idempotencyKey, requiredRoles, policyId, createdAt, expiresAt, state, auditSettled, dispatchedAuditId, approvedBy?, result?}`；
- **原子写**：写临时文件 + `rename`（同目录，跨平台 rename 原子）；
- 归档 = 写 `archived/` 再删 `pending/` 原件（与 `state.go:154-173` 同一顺序，先写后删）；
- TTL 缺省 15 分钟（与 Go 一致，`suspend.go:18`），配置 `pendingTTLMinutes` 可改。

## 5. 工具面

| 工具 | 参数 | 说明 |
|---|---|---|
| `tenon_action` | `module` / `action` / `args` | **不变**（不含身份、不含 planId），`approve` 分支行为变：先挂起，再视资格决定是否当场问人 |
| `tenon_pending` | 无（或 `limit?`） | 只读列出本 adapter 的待批：`planId`、`module.action`、入参摘要、`requiredRoles`、`expiresAt`、`canApprove`（由 `operatorRoles` 现算） |
| `tenon_resume` | `planId` | 续跑。`planId` 是模型从 `tenon_pending` / `tenon_action` 得到的**标识**，不是身份：模型能指定「续哪一单」，但不能指定「我是谁」 |

安全形状不变：三个工具的参数里**没有** `onBehalfOf` / `operatorRoles` / `approvedBy`，身份与资格只来自部署配置（与 `index.ts:121-148` 的白名单同一执行点）。

## 6. 编排

### 6.1 `approve` 分支（挂起）

```
1. 生成 planId / inputHash / 幂等键（沿用现有逻辑）
2. 落盘 pending（auditSettled=false，写 createdAt/expiresAt）
3. I7：POST /tenon/audit 计划级 dispatched
     ∨ 失败 → 归档 stale，抛 AUDIT_WRITE_FAILED（本轮什么都没执行）
4. 回写 auditSettled=true
5. 资格判定：operatorRoles ∩ requiredRoles
     ∨ 空 → 不提问，返回 needs_approval（明写「本会话无审批资格」+ 需要什么角色）
     ∨ 非空且 ctx.approval 可用 → 回合内 request()
          - allowed-once → 转 6.2（同一 plan）
          - rejected/cancelled/unavailable → 保持挂起，返回 needs_approval（带结局原因）
     ∨ 无 ctx.approval（未装载） → 保持挂起，返回 needs_approval
```

### 6.2 执行已批准的计划（`resume` 与「当场批准」共用同一条路径）

```
1. 读回 plan；过期 → 写计划级 result(policy_denied/expired) 并归档 expired，绝不执行
2. 重跑 policy（不信任挂起时的结论，与 resume.go:164-171 同构）
     ∨ 非 approve → 归档 stale，拒绝执行
3. 预演 → 写 step 级 dispatched（decision.outcome=approve, approvedBy=<操作员>）
4. 调模块（沿用原 planId/stepId 幂等键，重复续跑由模块幂等挡住）
5. 写 step 级 result（成功或失败都写）
6. 写计划级 result
7. 归档 done / stale
```

已归档 `done` 的 `planId` 再续跑 → 返回归档时的结论（幂等重放，不重新执行）。

## 7. 安全（档位 A 不变）

- 只注册 3 个工具，仍不 import 任何 shell / fs / web / subagent 能力；`check-profile-capabilities.mjs` 的判据不变；
- **模型不能自批的证明**：批准只能由 `ctx.approval.request()` 的 `allowed-once` 产生，而该调用只有回合内的**人**能应答；模型能做的只有「请求提问」与「指定续哪一单」，两件事都不构成批准；
- **资格来自配置**：`operatorRoles` 与 `onBehalfOf` 同为部署侧声明，模型不可见不可写（参数白名单机械保证）；
- 未配置 `operatorRoles` ⇒ 任何 approve 都停在待批（默认拒绝），不会静默变成「谁问都给」。

## 8. 测试与验收

**单测（假 host + 假 approval，`node --test`）**：
1. 挂起：plan 落盘、计划级 dispatched 字段形状（`stepId:null`/`action:""`/`approvedBy:null`）、`auditSettled` 回写；
2. I7：dispatched 写失败 → 归档 `stale`、无执行、无 pending 残留；
3. 无资格：`operatorRoles=[]` → 不提问（approval 未被调用）、返回 needs_approval；
4. 当场批准：`allowed-once` → 真执行、step 级两段带 `approvedBy`、归档 done；
5. 拒批：`rejected` / `unavailable` / `cancelled` → 不执行、pending 仍在、返回 needs_approval；
6. 续跑：过期 → 归档 expired 且无执行；policy 改判 deny → 归档 stale 且无执行；重复续跑已 done → 幂等返回不重发请求；
7. 参数白名单：`tenon_resume` 的多余字段（`onBehalfOf`/`approvedBy`）在发请求前被拒。

**端到端（真 host + 真 inventory + 假 answerer）**：扩 `scripts/verify-module-host.mjs`：`qty=12` 挂起（库存不变、pending 在盘上、计划级审计 1 条）→ 无资格续跑被拒 → 有资格续跑（answerer 返 `allowed-once`）→ 库存真减、审计带 `approvedBy=user:bob`、step 级两段 + 计划级两段齐全。

**证据层级（诚实声明）**：本机开发会话的审批策略是 **disabled**（`ApprovalPolicy='never'`，问也只会得到 `rejected`），因此「真 DSH UI 里点批准」这条**无法在本机端到端证明**；e2e 用假 answerer 证明机制，真机点批需在有 answerer 的会话里另验。这一条会写进 README。

## 9. 风险与不做

| 风险 | 处理 |
|---|---|
| 阻塞等批拖住回合 | 观察 `exec.signal`：取消即中止等待并保持挂起（不抛「已执行」） |
| 配置成为授权输入被当成「已解决 Q2」 | spec / README 双处写明这是临时绑定，Q2 定了要替换 |
| 两份 pending（adapter 与 Go agent）互相漂移 | 状态目录分开（`stateDir` 默认 `out/module-host-state`），不共用、不互相读取 |
| 公告板上看不到卡片 | 记为已知缺口，`tenon_pending` 是唯一入口 |
| 模型反复续跑耗尽模块配额 | 归档后幂等返回；过期硬失败；额度由模块侧配额控制（本轮不引入新闸门） |

## 10. 追加：公告板可见性（已实现）与「消费 confirm」的治理问题（未实现）

### 10.1 已实现：可选 agent 身份 + 公告板上推

- 配置新增 `agentUser`/`agentPassword`（换 token）或 `agentToken`（现成 token）；**都不配 = 功能关闭**，业务路径完全不受影响；
- 挂起成功后推一张 `awaiting_approval` 卡，终态（done/rejected/expired/stale）用同 planId 覆盖（宿主 `Upsert` 幂等，`pending.go:52-59`）；
- 推送失败**不升级为业务失败**，只把原因拼进 `detail`/归档 note——公告板是可丢的 read-model（`pending.go:34-38`）；
- 实测：`node scripts/verify-module-host.mjs` 用真 host + dev 账号表，以 bob（`role:warehouse_manager`）的 token 从 `GET /tenon/pending` **读回**卡片，续跑后再读回终态 `done`（83 条断言）。

**这也是一条权限**：配置了 agent 身份，adapter 就能以 `role:agent` 写公告板。这是显式配置、缺省关闭的。

### 10.2 未实现：消费 `tenon.confirm` 让批准真正落地

今天的批准只能发生在**DSH 回合内**（`ctx.approval`）。而公告板那套异步批准——人在页面上点「批准」→ 宿主入队 `tenon.confirm` → 被消费 → 执行——在 DSH 路径上**没有任何人消费这个队列**（`cmd/agent` 已冻结）。要让它通，需要 adapter 轮询：

```
GET  /tenon/requests/poll?after=<cursor>   # role:agent
POST /tenon/requests/ack {upto}            # role:agent
帧：{ type:'tenon.confirm', planId, decision:'approve'|'reject', by, roles }
     by/roles 由宿主用提交者 token 的 claims **覆写**（inbound.go:152）⇒ 这是可信身份
```

收益是决定性的：`approvedBy` 与角色校验都来自宿主验过的 token，而不是部署配置——**这条路径能真正解掉 Q2**，而不只是绕过它。技术上也已摸清：`Agent.followup(message)` 可唤醒会话，`agent/status` 等事件可取。

**但有一个必须先裁决的治理问题**：`poll` 返回的是**该 agent 的全部入站帧**，包括 `tenon.request`（用户说的话）。adapter 一旦消费，就同时具备「读所有提交给这个 agent 的用户原话」与「ack 掉它们」的能力——而 ack 之后 Go agent 再也拿不到。这不是加个 token 就完事的小改动，是**把入站队列的所有权从 agent 转到 adapter**。

三条可选路径：

| 选项 | 含义 | 代价 |
|---|---|---|
| **A. 只读不 ack**（建议先做） | adapter 轮询并处理 confirm，但**从不 ack**；游标只在内存里，重启从头读 | 会重复读到旧帧 → 必须靠 `archived/done` 幂等挡住；队列无界增长（宿主侧无 GC） |
| B. 接管整条入站队列 | adapter 成为该 agent 的唯一消费者，ack 交还 | Go agent 若同时运行会互相抢帧；所有权变更要写进 ADR |
| C. 宿主新增「confirm 专用只读端点」 | 只回 confirm 帧、不 ack | 要动已冻结的控制平面（ADR §2），但边界最干净 |

**建议 A 先行**（零治理变更、可回退），把 B/C 留给 ADR 决定。唤醒则用「挂起时把 `exec.agent` 存在内存 Map 里」：同一进程存活期内可 `followup` 唤醒，重启后没有 handle ⇒ 只执行不唤醒（业务完成，聊天里看不到），这条限制要写进 README。

### 10.3 已按 A 实现（2026-10-29）

- `createConfirmPoller`：`GET /tenon/requests/poll?after=<内存游标>` → 只处理 `tenon.confirm`，**从不调 `/tenon/requests/ack`**；`tenon.request` 帧一律不碰，但游标照样前进（否则每轮重读同一批）；
- confirm 帧的 `by`/`roles` 直接作为批准依据：`resumePlan` 新增 `grant` 入参，给了就**不再问 DSH**（人已经批过），但仍校验 `roles ∩ requiredRoles`——宿主验的是「他是谁、有什么角色」，不是「他够不够格批这一单」；
- 幂等：没有 ack ⇒ 重启后从头读，靠「已归档就跳过」挡住重放（不依赖模块业务幂等键兜底，但那个也在）；
- 生命周期：只有配了 agent 凭据**且**宿主给了 `ctx.effect` 才启动定时器（缺一个都不起，避免定时器泄漏）；
- 实测：`node scripts/verify-module-host.mjs` 的 f) 段——以 bob 的 token 提交 `tenon.confirm` → 轮询器执行 → 库存 103→91、审计 `approvedBy=user:bob`（**来自宿主 token，不是配置**）→ 卡片覆盖成 done → 再用 agent token 读队列，**帧仍在**（证明从未 ack）→ 新轮询器重放不产生动作。合计 93 条断言。

**仍未做：唤醒**。`agent.followup(message)` 需要一个 `UserMessage` 对象，本包不引 `@deepseek-ai/dsh-llm`（计划约束只允许 dsh-tools/schemastery），而按结构猜一个消息形状属于「没验证过的能力」——不猜。因此公告板批准后**业务会完成，但聊天里不会自动出现一句话**；要知道结果得问 `tenon_pending` 或再续一次（`resumePlan` 会返回归档结论）。
