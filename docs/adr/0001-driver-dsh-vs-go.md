# ADR-0001：谁是驱动 —— DSH 做业务宿主，Go 做一致性参考

- 状态：**Accepted**（2026-10-27）
- 决策者：项目所有者
- 关联：[`docs/adr/`](.) 目录约定；设计文档 [`2026-10-27-tenon-ai-driven-system-design.md`](../superpowers/specs/2026-10-27-tenon-ai-driven-system-design.md)；POC 设计见本文 §5

---

## 1. 背景（Context）

**已发生的事实**

| 事实 | 证据 |
|---|---|
| Go 侧已写掉一个完整 agent 运行时 | `internal/agent/` 9 个非测试文件共 **149.2KB**（`run.go` 50KB、`planner.go` 30KB、`resume.go` 25KB、`suspend.go` 14KB、`board.go` 9.8KB、`state.go` 7.8KB、`hostapi.go` 7.1KB、`ledger.go` 2.9KB、`skillselect.go` 1.9KB）——loop / prompt / 规划 / 挂起 / 恢复 / 去重账本 / 公告板 |
| TS 侧**几乎为零** | `packages/` 下 6 个包，5 个只有 `README.md` + `tsconfig.json`（无 `src`）；唯一有代码的 `@tenon/core` 只有 `src/types.ts`（544 行类型） |
| 核心命题（D1：摘模块后规划自适应）**尚未被证明** | README「D1 判定结果」：三次真跑分别是「判定受阻（schema 撞墙）」「空洞通过（A 轮没引用被摘模块）」「B 轮非法计划（缺 referenceNo）」 |
| DSH 已具备本项目要手写的东西 | `dsh-user-approval`（ApprovalService：requests / cancel / audit / per-session）、`dsh-tools`（**pre-execute/guard 闸门** + `ask` 语义）、`dsh-skill` / `dsh-skill-filesystem` / `dsh-tool-skill`、`dsh-session-persistence`、`dsh-client-ui-chat` / `dsh-client-ui-approval` |
| 「谁是驱动」从未被写成决策 | README 只说「参考宿主是 Go（决策 A）」「TS 宿主未实现（D18 deferred）」，评审者需要从散句推断 |

**问题**：两个方向都在长。Go agent 继续加功能，就是在重复造 DSH 免费提供的运行时；而 DSH 路径若同时推进，两边都会半成品。**必须先定谁是驱动，再决定谁停手。**

## 2. 决策（Decision）

**DSH 是业务宿主（驱动）；Tenon 控制平面仍由 Go `cmd/host` 提供并保持权威；Go `cmd/agent` 降为一致性参考实现。**

| 层 | 归属 | 具体含义 |
|---|---|---|
| agent 运行时：回合循环 / prompt 组装 / 工具调用 / 会话 / 技能 / 审批 UI | **DSH** | 不再在 Go 侧重写这些 |
| 控制平面：策略判定 / 授权 / 两段审计与 `prevHash` 链 / 模块注册表 / 待批公告板 | **Go `cmd/host`（权威）** | 协议治理的单一真相；DSH 侧通过 HTTP 调用 |
| `cmd/agent` | **一致性参考实现** | 只服务 C1–C10 与 D1 的机械判定；**不再新增功能**，仅修 bug |
| `ui/`（自制静态页 + 公告板页面） | **冻结为 demo** | 只修 bug；DSH 的 chat / approval UI 是产品入口。M1 的一半会被顶掉——这笔账记在这里，不再追加投入 |

**为什么不让控制平面也迁 TS**：审计两段写 + 哈希链、策略合并（`deny > approve > auto`，无命中即 deny）、Grant 交集、公告板 durable 真相，是**唯一有完整测试覆盖**的部分（`internal/controlplane` + C4/C5 + D1）。TS 侧没有任何等价实现；DSH 的 authorization / sandbox 不覆盖 Tenon 九类契约。重写它是纯支出，没有收益。

## 3. 不变量（Invariants）

越线即视为回滚信号：

1. **九类契约不依赖宿主**：`spec/0.1/schemas/*` 是唯一真源，DSH 插件不得引入宿主专有字段。
2. **审计前置由控制平面强制**：`dispatched` 写失败 ⇒ 不许执行（I7）。若 DSH 的前置闸门无法 fail-closed（见 POC-1），这条必须在 Go host 侧强制，DSH 只上报。
3. **审批真相在 Tenon 公告板**：DSH 的 `ctx.get('approval')` 只是入口与呈现；`pending` 的权威副本在 agent 状态目录 / 公告板。

## 4. 后果（Consequences）

**接受**

- Go agent 停止增长（冻结），只为 D1/conformance 修 bug；
- 自制 UI 冻结为 demo；产品界面改用 DSH；
- 5 个 README-only 包（`dsh-plugin-governance`、`dsh-plugin-module-host`、`dsh-plugin-intent-ui`、`dsh-plugin-skill-biz`、`sidecar-http`）**删除**，只留 `packages/tenon-core`。

**删除的硬规则（防知识丢失）**：**删除动作发生在实施阶段**（不是本 ADR 提交时），且必须先 `git mv` 每个包的 `README.md` 到 `docs/designs/dsh-adapter-boundaries/<pkg>.md`，再删目录。职责边界是资产，不能随目录一起消失：

> 澄清一处容易误读的地方：**删掉的是「占位包」**（README + tsconfig，无 src）。§6 的落地顺序是**届时按 M2 重建**这些插件——重建的是新包、有实现；不是保留占位包。占位包留着只会让人以为 DSH 路径在推进。

| 包（已搬迁） | 原本承诺什么 |
|---|---|
| `dsh-plugin-governance` | 治理控制平面：注册 / Grant 矩阵 / Policy 引擎 / Approval / 审计存储与查询 / 对账，与运行时无关 |
| `dsh-plugin-module-host` | `defineModule` 宿主侧接线：action → tool、events → 可订阅、manifest 能力发现、两段审计、执行语义（幂等/超时/补偿） |
| `dsh-plugin-intent-ui` | 共驾三帧前端通道：WS 主通道 + REST 降级 + 前端插件（可选增强） |
| `dsh-plugin-skill-biz` | 业务流程 skill 仓库：`SKILL.md` + `tenon:` 元数据、发布治理、版本可追溯 |
| `sidecar-http` | 任意语言模块接入的 6 个 HTTP 端点（M2） |

**新增的负担**：多一层 HTTP 调用（DSH → Go host），延迟与失败面增加；由 §3 的 fail-closed 与既有重试语义吸收。

## 5. POC（决定 adapter 形状，不决定去留）

两个半天，各自有明确判据。**方向不因结果改变**；结果只决定「闸门放哪一侧」。

### POC-1：前置闸门里抛错是否 fail-closed

- **实验**：最小 DSH 插件注册一个 `pre-execute/guard`，在闸门中**抛异常**；用一个「副作用 = 写标记文件」的工具，经 headless 回合调用它。
- **判据**：标记文件**不存在**且调用返回可解释的拒绝 ⇒ fail-closed；文件出现 ⇒ fail-open。
- **映射**：fail-closed ⇒ I7 可在 DSH 侧强制（先写 dispatched，失败即抛）；fail-open ⇒ I7 必须由 Go host 强制（adapter 先 `POST /tenon/audit`，失败就不调工具），DSH 闸门只做展示与策略提示。

### POC-2：审批跨重启是否存活

- **实验**：开一个回合 → 工具 `ask` → 审批 pending 时**重启 DSH 进程** → 再回答该审批。
- **判据**：重启后仍能拿到 `allowed-once` ⇒ 存活；变 `unavailable` / `cancelled` / 丢失 ⇒ 不存活。
- **前提**：`ApprovalService.ask` 在**没有开着的回合时会抛错**（`throws when no turn is open`），实验必须走真实回合，不能裸调服务。
- **映射**：不存活 ⇒ 公告板为唯一真相（预期结论）；存活 ⇒ 仍以公告板为准，但 DSH 的审批 UI 可直接复用。

## 6. 落地顺序（每步带出口判据）

0. **先做搬迁与清理**：`git mv` 5 个 README → `docs/designs/dsh-adapter-boundaries/`；删 5 个占位包目录；README 指向本 ADR → 出口：`pnpm check` 仍绿（`@tenon/core` 不受影响）
1. **`tenon-core` 真实现**：类型 → 校验器 / 实体寻址 / 幂等键 / HMAC → 出口：spec 用例在 TS 侧通过
2. **`module-host`**（register + 闸门 + 两段审计 + `restrict` 收窄工具面）→ 出口：C1–C5 在 DSH 宿主下通过
3. **approval 接 `ctx.get('approval')`，公告板为 durable 真相** → 出口：POC-2 结论对应的形态落地
4. **skills 挂 `dsh-skill-filesystem`** → 出口：每次运行固定 skill 版本并进审计
5. **`intent-ui` 最后** → 出口：共驾帧可回放

每步的「不做」：不提前做 UI、不改 Go 参考实现的判据、不为了演示放宽 fail-closed。

## 7. 被否的方案

| 方案 | 否决理由 |
|---|---|
| 全量迁 TS（含控制平面） | 要重写唯一有测试覆盖的治理实现；收益为零 |
| 维持 Go 为驱动，DSH 只做 UI | 继续手写 DSH 免费的运行时；D1 的根因（规划输入契约）与「谁跑 agent」无关，换宿主也不会自动变好——但长期会把运行时越写越厚 |
| 双轨并行、暂不决策 | 正是当前状态：两边都长、文档含糊、评审误判 |

## 8. 风险

| 风险 | 处理 |
|---|---|
| 删包丢知识 | §4 的 relocation 硬规则 + `git mv` 保留历史 |
| DSH 版本漂移（当前 `0.2.0-rc.2`） | POC 记录版本；adapter 只依赖 `ctx.get('approval')` 与 `tools/execute` 这两个窄接口 |
| 冻结纪律失守（有人继续给 Go 加功能） | 本 ADR 是唯一依据；新增功能必须先在 ADR 里改状态 |
| 两条线并行期的文档歧义 | README 加一行指向本 ADR；`docs/designs/dsh-adapter-boundaries/` 标明「M2 之后再实现」 |
