# 实施计划：dsh 对话层 + tenon-mcp（方案甲）

- 日期：2026-11-01
- 设计：[2026-11-01-dsh-as-conversation-layer-design.md](../specs/2026-11-01-dsh-as-conversation-layer-design.md)
- 口径：**方案甲**（对话层只预检，不造卡）+ 试点审批（发起人本人可点「允许执行」）

## 原则

1. **每一步都能单独验证**：判据是可执行命令或可观察输出，不是「看起来对」。
2. **先写会失败的断言**：凡是有判据的地方，先让它在旧代码上红。
3. **不新增执行权**：`tenon-mcp` 不持宿主私钥、不调 `tickets:issue`、不写公告板。
4. **不做「为了演示顺畅」的放行**：负向用例（拒绝、无权、越界）与正向同等重要。

---

## T0（前置，探索性）确认 dsh 的 MCP 配置位置与格式

**为什么先做**：dsh 的 MCP 不是 `mcpServers` JSON，而是 cordis 插件条目
（`id` / `name: '@deepseek-ai/dsh-mcp-client'` / `config: {serverName, transport, command, args, env}`）。
配置文件在哪个路径、用什么语法，本地 checkout 不完整，不能猜。

**做**：`npx -y @deepseek-ai/dsh@latest --version`；`dsh web --help`；找到配置文件（候选
`~/.dsh/`、项目 `.dsh/`）；把一条**假** MCP server（`node -e` 回固定 tools）注册进去，
确认 dsh 侧能看到工具名 `mcp__poc__*`。

**判据**：dsh 的工具清单里出现 `mcp__poc__…`；配置文件路径与最小片段记进本文档附录。

**若失败**：dsh 起不来（网络/Node 版本）→ 停在这里报告，不硬造替代方案；
MCP 侧仍可用 `tests/mcp` 的 transcript 用例独立验证（T4–T8 不依赖 dsh）。

### T0 实测结论（2026-11-01）

| 事项 | 实测值 |
|---|---|
| 版本 | `npx -y @deepseek-ai/dsh@latest --version` → **0.2.1-rc.5**（首次拉包约 4 分钟） |
| 启动 | `dsh web --host 127.0.0.1 --port <port> --no-open`。**不要再写 `--profile web`**：`web` 子命令已经隐含 profile，重复指定直接报 `select a profile only once` |
| 配置 | `C:\Users\ASUS\.dsh\profiles\web\cordis.patch.yml` —— 顶层是 **loader patch 数组**，MCP 服务用 `- insert: [{id, name: '@deepseek-ai/dsh-mcp-client', config: {serverName, transport, command, args, env}}]` |
| 工具名 | `mcp__<serverName>__<toolName>`，本项目为 `mcp__tenon__registry_list` 等 |
| 端口 | 默认 3080 **可能已被用户自己的 dsh web 占用**（实测 pid 24712，10-31 13:14 起）。本机改用 **3081**，不杀用户的进程 |
| 失败形态 | 端口冲突时 dsh 会在 spawn 完 MCP 子进程后报 `startup failed: ... EADDRINUSE`，日志里能看到 `tenon-mcp: host=...` 这一行——它同时证明「条目被解析、握手成功」 |
| 已知噪音 | `@anionex/dsh-vision-toolkit@0.1.46` 与 0.2.1-rc.5 不兼容（dsh 自己跳过并警告）；与本项目无关 |

---

## T1 入队响应补 `syntheticEventId`

**改动**
- `internal/controlplane/inbound.go`：`inboundEnqueueResponse` 增加 `SyntheticEventID string \`json:"syntheticEventId,omitempty"\``；
  值取宿主规范化后写入帧的那个 id（与 `requestId` 同一处）。
- `docs/interfaces.md` §4 入站表下的响应字段说明。
- 单测：`TestInboundEnqueueReturnsRequestAndSyntheticIDs`。

**判据**
- `POST /tenon/requests` 返回体同时含 `requestId` 与 `syntheticEventId`，且后者与
  `GET /tenon/requests/poll` 里该帧的 `syntheticEventId` 一致。
- `go test ./internal/controlplane/` 绿。

**依赖**：无。**理由**：`plan_preview` 要带 `triggerEventId`，否则 `origin=request` 类策略
预检永不命中（已验证过的坑）。

---

## T2 `lastSeen` 序列化回归用例（不改输出）

**改动**：`internal/controlplane/` 新增断言——`GET /tenon/modules` 的 `lastSeen` 匹配
`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`。

**判据**：用例先红（若当前实现已满足，则立即绿，并在提交信息里写明「已满足，撤回误判」）；
**不允许**为了让它绿去改序列化格式。

**依赖**：无。

---

## T3 注册中心视图页（给人看）

**改动**
- 新增 `ui/registry.html`：三模块表（id / 状态 / 心跳年龄秒 / 动作数 / 动作列表含 kind / 能力标签），
  10 秒轮询 `GET /tenon/modules`；顶部一行灰字明说「只列出 `enabled && alive`，掉线的模块会消失」。
- 四个既有页面顶栏加「注册中心」入口。

**判据**
- `GET /ui/registry.html` 200；页面显示三个模块，心跳年龄 <10s；
- 断开一个模块（`Stop-Process`）后 10 秒内该行**消失**，且页面上的说明能解释这一现象。

**依赖**：无。

---

## T4 `tenon-mcp` 骨架：stdio JSON-RPC

**改动**
- 新增 `cmd/tenon-mcp/main.go`：读 stdin 行 → `initialize` / `tools/list` / `tools/call`；
  stdout **只**写 JSON-RPC，日志走 stderr。
- 新增 `internal/mcpserver/`：协议结构、分发、错误码（`-32601` 等）。
- 测试 `internal/mcpserver/session_test.go`：喂 3 条消息，断言响应 id 对齐、`tools/list` 含声明工具。

**判据**
- `go test ./internal/mcpserver/` 绿；
- `echo '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | tenon-mcp.exe` 输出单行合法 JSON-RPC。

**依赖**：T0 只影响接线，不影响本步。

---

## T5 `registry_list` + `action_invoke`（只读白名单）

**改动**：`internal/mcpserver/tools_registry.go`、`tools_action.go`。
`action_invoke` 先查注册表确认该动作 `kind=read`，否则直接拒绝，**不发任何请求**。

**判据**
- `registry_list` 返回与 `/tenon/modules` 同值（模块、动作、能力、`lastSeen`）。
- 正向：`action_invoke("inventory.GetStock",{sku:"STD-1001"})` → 真实 `onHand`。
- **负向**：`action_invoke("inventory.DeductStock",…)` → 拒绝，且模块执行计数不变
  （用 `inventory.ListStock` 前后同值 + 模块审计无新增行双重确认）。

**依赖**：T4。

---

## T6 `plan_preview`（三闸门预检，不落卡）

**改动**：`internal/mcpserver/tools_plan.go`。顺序：`policies:evaluate(triggerEventId)` →
`grants:evaluate(charge:false)` → 每个 write 的 `:dry-run`；**不写公告板**。

**判据**
- `qty=12`（DeductStock）→ `{policy:"approve", grant:true, dryRun:"verified"}`；
  库存不变、公告板条目数不变。
- `qty=5` → `{policy:"deny", reasons:["NO_POLICY_MATCH_DEFAULT_DENY"]}`（fail-closed 原样透出）。
- 预演 `unsafe` → 结论原样返回（不重试、不降级）。

**依赖**：T1（triggerEventId）。

---

## T7 `pending_list` / `pending_decide`（human token 从环境变量读）

**改动**：`internal/mcpserver/tools_pending.go`。token 只从 `TENON_MCP_HUMAN_TOKEN` 读，
**不进工具参数**（避免进模型上下文）。

**判据**
- 无 token → `pending_list`/`pending_decide` 返回未授权，且无任何副作用。
- 有 token（alice，试点口径）→ 卡 `done`、库存 −12、审计两段（`dispatched`/`result`）。
- **负向**：token 为 bob 之外的无角色账号 → 403，卡仍是 `awaiting_approval`。

**依赖**：T6（要有卡才能批——卡来自人的请求帧，不是 MCP）。

---

## T8 `audit_query`

**改动**：`internal/mcpserver/tools_audit.go`（`GET /tenon/audit?planId=`）。

**判据**：按 T7 的 planId 查得到 `dispatched` 与 `result` 两条，`approvedBy=user:alice`。

**依赖**：T7。

---

## T9 dsh 接线（依赖 T0 的结论）

**改动**
- `scripts/dsh-up.ps1`：`npx -y @deepseek-ai/dsh@latest web --port 3080`（仅 loopback），
  按 T0 记录的位置写入 MCP 条目：
  `{serverName:"tenon", transport:"stdio", command:"<repo>\out\dev\bin\tenon-mcp.exe",
    env:{TENON_HOST:"http://127.0.0.1:8080", TENON_MCP_HUMAN_TOKEN:"<alice token>"}}`
- `scripts/dev-up.ps1` 构建 `tenon-mcp.exe` 到 `out/dev/bin/`。

**判据**：`dsh` 侧工具清单出现 `mcp__tenon__registry_list` 等 8 个工具；
`Get-NetTCPConnection -LocalPort 3080` 绑定 `127.0.0.1`。

**依赖**：T0、T5–T8。

---

## T10 端到端验收（AC2–AC8）

**改动**：`out/dev/e2e-dsh.ps1`（能自动化的就自动化，人必须在 dsh 里说的话写成手工步骤清单）。

| AC | 判定 |
|---|---|
| AC2 | 对话问「有哪些服务注册了」→ 三模块 + 心跳年龄 <10s + 动作 kind |
| AC3 | 对话问「STD-1001 库存」→ 与业务页同值 |
| AC4 | 对话说「扣 12 件」→ 预检 approve/verified，**无卡、库存不变** |
| AC5 | 人发请求帧 → 落卡 → 对话里批 → 库存 −12 + 两段审计 |
| AC6 | 无 token 批 → 401 且无副作用 |
| AC7 | `lastSeen` 序列化断言绿 |
| AC8 | `action_invoke` 传写动作 → 拒绝且模块计数不变 |

**依赖**：T9。

---

## 顺序（可并行项已标注）

```
T0 ──────────────────────────────┐（只阻塞 T9）
T1 → T6 → T7 → T8 → T9 → T10
T2（独立）
T3（独立）
T4 → T5 ┘
```

## T11 对话入口收口（2026-11-01 追加）

**为什么追加**：T1–T10 完成时，对话入口有 5 处（共驾台、注册中心页、库存/备货/报价三页的
「发送给 agent」）。多入口最典型的故障是同一个决定被点两次，而审计里分不清是谁点的。

**改动**

- `ui/inventory|procurement|quote/index.html`：删掉 `/tenon/requests` 调用与登录表单，
  改成「复制话术 + 打开 dsh」；页面只保留模块自身的只读动作。
- `ui/registry.html`：摘掉 `chat-widget.js` 与 `#agent-chat`，只留注册中心视图 + 去 dsh 的链接。
- `ui/index.html` + `ui/app.js`：对话面板与发送区下线；待批卡改成**只读**（没有批准/驳回按钮，
  只给出「批准 pln_… / 驳回 pln_…」这句话）；agent 的帧改为 `#intent-log` 只读回放。
- `tenon-mcp` 新增第 7 个工具 `request_submit`（把人的原话投进入站队列）。**必须有它**：
  入站帧是唯一能开回合的入口，页面输入框收掉后，没有它就没有任何东西可批准。
- `pending_list` / `pending_decide` 的描述与回执里写明「先 ask_user_question 让用户选
  允许执行/驳回，再调 pending_decide」。
- `scripts/dev-up.ps1` 的种子卡改造：直连模块写（P0-1 之后必然 403 TICKET_MISSING）→ 改投入站帧，
  并且**失败要响**（原来把错误吞了，「已预置一张待批卡」是假的）。

**判据（已实测）**

- `go test ./...` 全绿；新增 3 个用例（pending_list 指令、request_submit 原话不变 / 无 token 拒绝）。
- 三张业务页 / 看板 / 注册中心：无 `#chat-log`、`#request-text`、`#send`、`#agent-chat`；
  待批卡内 button 数为 0；console 无错误（内置浏览器实测，截图在 `out/ui-check/`）。
- MCP 工具名 6 → 7，`request_submit` 在列；`pending_list` 回执含 ask_user_question 指令
  （直接驱动 `tenon-mcp.exe` 对活宿主实测）。

## 停止条件

- T0 失败（dsh 起不来）→ 报告并停在 T0，T4–T8 仍可独立完成并交付（MCP 层自证）；
- 任一步的**负向**判据不通过 → 停在那一步，不带着缺口往下做。
