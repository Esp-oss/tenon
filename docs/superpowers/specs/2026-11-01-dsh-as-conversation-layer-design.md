# 设计：dsh 作为对话层，Tenon 保留裁决

- 日期：2026-11-01
- 状态：**已批准**（方案 A + 决策「甲：对话层只预检，不造卡」），未实施
- 关联：`docs/interfaces.md`、`docs/superpowers/specs/2026-10-31-tenon-execution-ticket-design.md`

## 1. 意图与为什么

要让「AI 提议、系统裁决、人审批、全程可审」有一个**像样的对话入口**，而不是只靠自写的
`ui/index.html` 一次性页面；同时要把「有哪些后端服务在心跳、各自声明了什么能力」变成看得见、
可被 agent 当场调用并验证的东西。

现状的三个断点：

1. 对话入口是自己写的静态页，规划能力完全绑死在 `cmd/agent` 里的单次 `ChatJSON` 调用；
2. 注册中心有数据（`/tenon/modules`），但没有面向人的视图，也没有给模型看的工具化入口；
3. 「agent 能不能真的调这些能力」只能靠读日志判断。

dsh 已经提供了成熟的对话 UI、工具编排（MCP）、技能与会话管理。把**对话层**交给它，
把**裁决层**留在 Go，是这个项目当前性价比最高的切法。

## 2. 目标 / 非目标 / 反目标

**目标**

1. dsh Web UI 跑在 `127.0.0.1:3080`，对话在 dsh 里进行。
2. 一个 `tenon-mcp`（Go，stdio）把 Tenon 的注册中心与闸门暴露成工具。
3. 对话里能列出「谁在心跳、声明了什么能力」，并**当场调用只读能力**验证可达。
4. 写动作在对话里只能变成**待批卡**，执行仍由宿主签票；人在对话里可以批准/驳回。
5. `lastSeen` 带时区（当前输出 `11/01/2026 00:54:30`，无偏移，非 +08:00 的消费方会读错）。

**非目标（本轮不做）**

- 不把策略/授权/预演/票据/审计重写成 Node 插件（那是方案 B，见 §4）。
- 不做多租户与细粒度 RBAC；仍用 `config/users.dev.json` 的三个账号。
- 不改模块协议、不改 `/.well-known/tenon` 契约。
- 不做 dsh 参与事件回合（`quote.completed` 仍由 `cmd/agent` 触发）。

**反目标（做了就算失败）**

- 给模型一个「直接执行写动作」的工具。那等于把裁决权交给对话层。
- 把 dsh 自己的 approval 当成 Tenon 的审批（两套机制各自为政，审计链会断）。
- 为了让演示顺畅而放松策略/授权（例如为 qty≤10 加 auto 规则）。

## 3. 约束（已核实的事实）

| 事实 | 来源 | 影响 |
|---|---|---|
| dsh 自带 Web UI，默认 `127.0.0.1:3080`，`host` 只接受 `127.0.0.1`/`0.0.0.0` | `docs/subsystems/web-server.zh.md` | 换端口＝改配置，不改 dsh 本体 |
| dsh 是 cordis 插件架构，developer preview，明示会有破坏性变更 | `README.md` | 不能把裁决逻辑押在它的内部 API 上 |
| dsh 支持 MCP，`stdio` 与 `streamable-http` 两种传输；工具名投影为 `mcp__<serverName>__<rawName>` | `docs/config-catalog.zh.md`（`dsh-mcp-client`） | **MCP 是稳定边界**，dsh 内部怎么变都不影响我们 |
| 本地 dsh checkout 不完整（工作区大面积空缺），但 `git` 对象里有 | `git ls-tree` | 从这份源码 `pnpm build` 不可靠；优先 `npx @deepseek-ai/dsh` |
| Tenon 的裁决与票据全在 Go（21 个包 + C1–C12） | 仓库现状 | 裁决层不能被替换掉，只能被调用 |

## 4. 采用方案 A，及被否的两个

**A（采用）：dsh = 对话与编排；tenon-mcp = 能力边界；Tenon = 裁决与执行。**

对话里模型能做三类事，权限逐级收紧：读注册中心 → 调**只读**动作 → 提交**计划**（只落待批卡）。
执行永远发生在宿主签票之后，由 `cmd/agent` 完成。

被否：

- **B：dsh 全面接管 agent。** 要在 Node 重写 Ed25519 票据、哈希链、六类对账与公告板，
  现有测试与一致性用例大半作废，且踩在 preview 的破坏性变更上。收益是「一个运行时」，
  代价是把已经验证过的安全属性重赌一次。
- **C：dsh 只当规划器。** 只替换 `planner.go` 的 LLM 调用，最小改动，但完全不产生
  「在 dsh 对话框里对话」的效果，dsh 的技能/工具/会话能力全用不上。

## 5. 组件与端口

```
127.0.0.1:3080   dsh web（对话 UI，人用浏览器打开）
   └─ stdio 拉起 tenon-mcp.exe（dsh 管理其生命周期）
127.0.0.1:8080   Tenon 宿主（策略/授权/票据/审计/公告板/inbound）
127.0.0.1:8081   inventory    （页面 + 动作协议 + 心跳）
127.0.0.1:8082   procurement  （同上）
127.0.0.1:8083   quote        （同上）
```

`tenon-mcp` 是**瘦客户端**：不持有业务状态，只把 MCP 调用翻译成对宿主的 HTTP 调用。
宿主仍是唯一真值；重启 dsh 不影响任何业务状态。

**dsh 的入口不是裸地址（2026-11-01 实测补记）。** `http://127.0.0.1:3080/` 直接打开会回
401 `dsh web authentication required; reopen the URL printed by dsh web.` —— dsh 的浏览器
信任栅栏要求启动时打印的那条 `http://host:port/?token=<随机>`：token 每进程随机、只在内存
里（`dsh-client-connection` 的 `PROCESS_LAUNCH_TOKENS`），访问它才种下 30 天、绑定 authority
的 HttpOnly cookie。所以：① 任何页面里的 dsh 链接都只能算「有 cookie 时可用」；
② 换端口/换 host（`127.0.0.1` ↔ `localhost`）等于换会话，要重新用那条 URL；
③ `scripts/dsh-up.ps1` 把 URL 抓进 `out/dev/dsh-url.txt` 并打印，重启 dsh 后入口会换。

## 6. MCP 工具契约

| 工具（rawName） | 权限 | 落到宿主 | 会产生副作用？ |
|---|---|---|---|
| `registry_list` | 读 | `GET /tenon/modules`（含 `manifest.actions`、`capabilities`、`lastSeen`） | 否 |
| `action_invoke` | **仅 kind=read** | `POST /tenon/actions/{id}`，调用前先查注册表确认该动作 `kind=read` | 否（只读动作在协议上无副作用） |
| `plan_preview` | 提议（只预检） | `policies:evaluate(triggerEventId)` → `grants:evaluate(charge:false)` → 每步 `:dry-run` → 回三个闸门的结论 | 否（**不执行，也不造卡**，见 §7.3） |
| `request_submit` | 人 | `POST /tenon/requests` 发 `tenon.request`（需 human token），原话不改写 | 间接：agent 自己规划、落卡 |
| `pending_list` | 读 | `GET /tenon/pending`（需 human token） | 否 |
| `pending_decide` | 人审 | `POST /tenon/requests` 发 `tenon.confirm`（需 human token，且角色需匹配 `requiredRoles`） | 间接：批准后由 `cmd/agent` 恢复执行 |
| `audit_query` | 读 | `GET /tenon/audit?planId=&since=` | 否 |

**没有 `action_execute`。** 这是设计的核心约束，不是遗漏：如果给了它，模型一次调用就能
绕开策略、授权、预演与票据，前四个月的加固全部作废。

**`request_submit` 与「造卡」是两件事（2026-11-01 追加）。** 它只把人的原话投进入站队列，
计划仍由 agent 生成、卡仍由 agent 落——`argsHash` 快照与 `store.Load` 依旧只在 agent 手里。
加它的原因很实际：共驾前端把输入框收掉、对话只保留 dsh 之后，**入站帧就没有发信人了**，
没有它，对话层能看到待批、能批，却永远没有东西可批。工具面因此是 7 个。

**也没有 `plan_propose`（造卡）。** 造卡不是接口问题，是所有权问题：公告板是宿主的
read-model，但写入要求 `role:agent`，而**可恢复的卡**还依赖两样只在 agent 手里的东西——
`steps[].argsHash` 冻结快照（`tickets:issue` 的 approve 分支要它）与 `store.Load(planId)`
（`resume` 要它）。MCP 拿 human token 写公告板会被 403；拿 agent token 就等于同时拿到
`tickets:issue`，反目标当场作废。

所以写路径唯一合法形态是：

```
对话里的"我想这么做" → plan_preview（三个闸门给结论，不落卡）
人的请求帧（POST /tenon/requests） → agent 规划 → 落卡 → 人批准 → 宿主签票 → 执行
```

对话层能说清「这么做的后果是什么」，但**提议的所有权仍在 agent**。这条不是缩小功能，
而是把「谁的计划被审计记录」定死在一处。

## 7. 三条演示路径

**7.1 谁在心跳、声明了什么**

```
人：「现在有哪些服务注册了？」
dsh → registry_list → host /tenon/modules
← [{id:inventory, state:alive, heartbeatAgeMs:2100,
    actions:[{id:"inventory.GetStock",kind:"read"}, …],
    capabilities:["dry-run","ticket","event-replay"]}, …]
```

**7.2 能力能不能真被调用**

```
人：「查一下 STD-1001 的库存」
dsh → action_invoke(actionId="inventory.GetStock", args={sku:"STD-1001"})
tenon-mcp：查注册表 → kind=read ✓ → 宿主不必过闸门（读动作）→ 模块
← {onHand:108}
且宿主审计出现该读动作的 result 行（可审）
```

**7.3 写动作只能变成待批**

对话里问到写动作，模型只能拿到**预检结论**，不能造卡（§6）。真正落卡要靠人的请求帧：

```
人：「把 STD-1001 的 12 件扣掉」
dsh → plan_preview(plan=…DeductStock qty=12…)
← {policy:{outcome:"approve", approvers:["role:warehouse_manager"]},
   grant:{allowed:true}, dryRun:{status:"verified"}}
库存不变（预检不是执行）

人（在对话里确认后）→ dsh 调 action_invoke 是不允许的（写动作），
                    所以这一步由人自己发请求帧：
人 → POST /tenon/requests {"text":"把 STD-1001 的 12 件扣掉"} → agent 规划 → 落卡
人：「批准 pln_…」
dsh → pending_decide(planId, decision="approve", token=bob)
→ 宿主发 confirm → cmd/agent 恢复 → 签票 → 模块 → 库存 108→96 + 两段审计
```

为什么不让对话层替人发那条请求帧：那等于让模型自己决定「要不要落地一笔业务」。
它可以让「预检结果」与「是否执行」分开，这正是要人点头的那一步。

### 已否的另一条路（乙）

新增 `tenon.plan` 入站帧，让 agent 原样采纳对话层给出的计划，换来「说扣 12 就是扣 12」
的精确对应。否决理由：

1. 要同时解决「谁证明这份计划没被中间层改过」——否则等于给对话层开一条绕过规划权的路，
   而这正是本设计要守的东西；
2. 改 `spec/0.1` 的帧类型、宿主入站白名单、agent 分支，还要补一致性用例；
3. 收益只是「审计里记的是模型写的那份而不是 agent 写的那份」，而两者的闸门结论本来就由
   宿主逐项复核——精确对应换来的是纸面一致，不是更强保证。

留作将来若真需要「计划作为证据原件」时再提。

## 8. 权限与安全边界

**试点口径（2026-11-01 决定，仅用于验证闭环）**：本轮把「批准」的审批人从
`role:warehouse_manager` 放宽为 `["role:warehouse_clerk", "role:warehouse_manager"]`，
于是**发起人本人（alice，`role:warehouse_clerk`）就能点「允许执行」，不需要第二个人**。

这不是取消闸门：策略命中、授权、预演、票据、审计链全都还在，卡也仍然要人点一次。
被放宽的只有「谁可以点」。**代价必须写明**：自批等于没有第二双眼睛，四眼原则在这条路径上
失效；它只适合闭环验证，不适合生产。恢复四眼只需把 `approvers` 改回单一审批角色，
代码不用动——这正是把审批人写成策略数据而不是硬编码的理由。


1. **tool 级白名单**：`action_invoke` 只放行 `kind=read`；写动作一律走 `plan_preview` + 人的请求帧。
2. **human token 只在 `pending_*` 用**：token 由人在页面登录后粘贴到 dsh 会话（或由
   `tenon-mcp` 读取 `TENON_MCP_HUMAN_TOKEN` 环境变量）。没有 token → 拒绝，不降级。
3. **来源判定仍由宿主做**：`plan_preview` 提交时带 `triggerEventId`。现在这个 id 只在
   `POST /tenon/requests` 入队时签发、且**不回给调用方**，所以本轮要把 `syntheticEventId`
   加进入队响应（与已加的 `requestId` 同一行代码位置）。否则 `origin=request` 类策略
   （`plat-request-needs-human`）在预检里永不命中，采购路径会被判成 `event` → 无规则 → deny。
4. **提议主体与执行主体是同一个身份**：`grants:evaluate` 必填 `agent`/`onBehalfOf`，签票时
   宿主用的是 `cmd/agent` 的 `TENON_AGENT_ID`/`TENON_ON_BEHALF_OF`。因此 `tenon-mcp`
    **读同一份环境变量**，不写死、也不把 `onBehalfOf` 做成工具参数（那等于把身份交给模型）。
   人的身份只在 `pending_decide` 出现。两条身份混用会出现「预检说可以、签票却 GRANT_DENIED」。
4. **不新增执行权**：`tenon-mcp` 不持有宿主签名私钥，也不调用 `tickets:issue`。
5. **审计**：凡是经过宿主的调用都留痕；MCP 层不写自己的日志作为「真相」。

## 9. 注册中心视图

两个出口，同一份数据：

- 给模型：`registry_list`（见 §6）。
- 给人：宿主 `/ui/registry.html` 静态页（与业务页同一套壳），显示模块 / 状态 / 心跳年龄 /
  动作数与能力标签，10 秒轮询。

**更正（撤回一条误判）**：我先前说 `lastSeen` 是「本地时间、无时区」，那是错的。
`ModuleEntry.LastSeen` 本身是 `time.Time`，JSON 序列化就是 RFC3339 带偏移；
我看到的 `11/01/2026 00:54:30` 是 PowerShell `ConvertFrom-Json` 把字符串转成 DateTime
之后的本地化显示，`docs/interfaces.md` 与 `verify-registration.ps1` 都按带偏移解析。
所以这里**不改输出**（改了反而撞契约），只补一条序列化回归用例：断言
`lastSeen` 匹配 `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`。

**可见性口径**：视图只能列「活着的」。`GET /tenon/modules` 返回的是 `enabled && alive`，
掉线的模块是**消失**而不是显示 `lost`（全量视图只挂在 `TENON_TEST_MODE=1` 的
`/tenon/admin/modules`）。所以页面必须自己写明这一行，否则运维第一眼看到的是
「三个模块都在」——而实际可能有一个已经掉线、只是看不见了。本轮不新增非测试态的管理端点。

## 10. 验收标准

| # | 判据 | 判定方式 |
|---|---|---|
| AC1 | `http://127.0.0.1:3080/` 打开 dsh Web UI，且只监听 loopback | `Get-NetTCPConnection` 看绑定地址 |
| AC2 | 对话问「有哪些服务注册了」→ 三个模块 + 心跳年龄 <10s + 每个动作的 kind | 对照 `/tenon/modules` 同值 |
| AC3 | 对话说「查 STD-1001 库存」→ 返回真实 onHand，且与业务页显示同值 | 两个来源比对（读动作在协议上不写审计，见 §11） |
| AC4 | 对话说「扣 12 件」→ `plan_preview` 返回 `approve/verified`，**库存不变**，且**没有**产生卡 | `ListStock` 前后同值 + 公告板条目数不变 |
| AC5 | 人发请求帧 → agent 落卡 → 对话里「批准 pln_…」（bob）→ 卡 done，库存 −12，审计两段 | 前后差值 + `dispatched`/`result` 各一条 |
| AC6 | 无 token 调 `pending_decide` → 401，且无任何副作用 | 负向用例 |
| AC7 | `lastSeen` 序列化为带偏移的 RFC3339 | 单测断言正则（不改输出，见 §9） |
| AC8 | `action_invoke` 传写动作 → 拒绝，且模块收不到任何请求 | 负向用例（计数模块执行次数） |

AC6 与 AC4 是**负向**判据，缺了它们，「能对话」就只是能聊天。

## 11. 风险与开放问题

| 风险 | 处理 |
|---|---|
| dsh 需要 Node + 联网安装 npm 包 | 先 `npx @deepseek-ai/dsh web` 试跑；离线环境退化为「用 MCP inspector / 自带 CLI 对话」，裁决边界不变 |
| dsh preview 变更破坏配置 | 只依赖 MCP 契约（稳定）；dsh 升级只改 settings 里的 server 条目 |
| Windows 上 stdio 子进程路径与编码 | `tenon-mcp.exe` 用绝对路径；stdout 只输出 JSON-RPC（日志走 stderr） |
| 两套 UI 并存（自写页面与 dsh） | 自写页面保留为「业务系统」与「公告板」；dsh 只承担对话。两者都读同一份宿主状态 |
| 人把 token 贴进对话会不会泄漏 | token 不进模型上下文：`tenon-mcp` 从环境变量读，工具参数只收 planId/decision |

## 12. 已知代价（接受，不修）

- **读动作不进宿主审计**：模块对 `kind=read` 不写审计行，宿主也没有 `/tenon/actions/{id}`
  代理（那是模块端点）。所以 `action_invoke` 的读不会在 `audit-host.jsonl` 留痕。
  若强行补一条「MCP 自述的审计」，`actor` 只能填自己、`planId/stepId` 为 null——那不是审计，
  是客户端日志。本轮选择**不伪造**，并在文档里把这条写成已知缺口。
- **对话层看不到掉线模块**（§9 可见性口径）。

## 13. 留给实施者的决定

- `tenon-mcp` 用 stdio 还是 streamable-http：**默认 stdio**（进程生命周期归 dsh，不占端口）；
  若 Windows 上子进程不稳定，再切 streamable-http（宿主侧加 `/mcp`）。
- `registry.html` 放宿主 `ui/` 还是模块侧：**放宿主**（注册中心本来就是宿主的状态）。
- `plan_preview` 的入参用完整 `plan` 还是「自然语言 + 动作列表」：**用 plan 结构**，与
  `plan.schema.json` 同形，避免在 MCP 层再造一套计划语义。
