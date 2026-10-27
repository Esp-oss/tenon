# Tenon（榫卯）

> 榫卯：不用胶水，靠结构本身严丝合缝。

**让 AI 直接操作业务系统的可插拔协议。** 业务模块按协议接入后，agent 自己发现它能做什么、自己决定怎么做——你不需要为每条业务流程写 `on 事件 → 调接口` 的胶水代码。

[![spec](https://img.shields.io/badge/spec-0.1-blue)](spec/0.1/README.md)
[![license](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)

---

## 它解决什么问题

大多数"AI + 业务系统"的做法退化成两种：要么 agent 只会聊天，要么工程师把业务流写成规则——**那还是程序驱动，只是调用方换成了 LLM**。

Tenon 划了一条可机械检验的界线：

> **接入方为了跑通业务流程，必须写「某事件/某错误 → 调某 action」的映射代码 —— 此时系统已退化为程序驱动，Tenon 在该接入点失效。**

支撑"AI 驱动"的三个机制，缺一即退化：

| 机制 | 作用 |
|---|---|
| **能力发现** | 模块插进来，agent 通过 `/.well-known/tenon` 获得新能力，不改代码 |
| **技能即知识** | 业务流程写成可审阅、可版本化的 skill 文件，教 agent 新流程只改 skill |
| **治理闸门** | 正因为不确定性是固有的，才用策略/审批/审计控风险，而不是硬编码 |

订阅事件只决定「是否思考」，绝不决定「做什么」。

## 这套界线怎么被验证

不是靠自觉，靠 **D1 差异测试**：

```
冻结同一输入（事件 + 数据快照 + 固定 skill 版本）
  跑 A：三模块全在        → Plan A 的 action 序列
  跑 B：摘掉 procurement   → Plan B 的 action 序列
断言：A ≠ B，且 B 仍达成可接受结果（改预留 / 转人工），不抛错
```

硬编码映射的实现，摘掉模块后行为**要么不变**（说明它根本没在用那个模块）、**要么崩溃**（说明映射写死了）。只有真在按能力清单推理，行为才会自适应地改变。

辅助手段是 `tools/lint` 的静态扫描（事件订阅回调里的 action 字面量、错误码→action 映射、skill 正文里的动作标识符）。

## 架构

```mermaid
flowchart TB
    subgraph FE["前端（可选增强）"]
        UI["业务页面 + 共驾 SDK<br/>消费 Intent / 产出 Context·Confirm"]
    end
    subgraph RT["Agent 运行时（本轮参考实现：cmd/agent）"]
        AG["事件→回合→技能→能力发现→规划<br/>产出 Plan"]
    end
    subgraph MOD["业务模块（任意语言/框架）"]
        M1["quote"]
        M2["inventory"]
        M3["procurement"]
    end
    subgraph L1["L1 传输"]
        T["HTTP/JSON · Webhook(HMAC) · MCP(双向，M2) · 事件信封"]
    end
    subgraph X["L3 执行语义 + L4 控制平面（不依赖运行时）"]
        X1["幂等 · dry-run · 重试/熔断 · saga 补偿 · 审计两段写"]
        X2["注册 · 授权矩阵 · 策略分级 · 审批 · 审计查询 · skill 仓库"]
    end

    UI <-->|"Intent / Context / Confirm"| AG
    AG <-->|"Plan / Action"| L1
    L1 <--> MOD
    X -.横切.-> RT
    X -.横切.-> MOD
```

**协议住在规范里，宿主与运行时都可替换。** 协议层不 import 任何宿主类型：本轮的参考宿主是 Go 的 `cmd/host`，参考运行时是 `cmd/agent`；dsh 插件的进程内宿主是 M1 的另一种接入形态，不是前提。

## 语言选型：按层选，不按偏好选

| 层 | 语言 | 为什么 |
|---|---|---|
| 协议线格式 | JSON Schema | 语言无关是**设计要求**，不是实现偏好 |
| dsh 宿主 | TypeScript | 硬约束：dsh 是 cordis 插件树，原生插件入口就是 JS |
| 一致性套件 + lint | **Go** | 交付形态是单文件二进制——第三方下载即自证合规，不必先装 Node/Python |
| HTTP 侧车 / demo 模块 | Go | 与套件共用一套工具链；跨进程强制走真实报文，堵住"绕过协议直接 import"的偷懒 |
| 协议类型定义 | TypeScript | 由 schema 派生，供 dsh 侧编译期使用 |

> M0.5 的**参考宿主是 Go**（`cmd/host`，决策 A）：控制平面只做策略/授权/审计/注册表，没有一行 LLM 调用，用 Go 与模块、套件共用工具链。TypeScript 的 dsh 插件宿主仍未实现（D18 的进程内路径 deferred）——上表的「dsh 宿主」说的是 M1 的接入形态，不是本轮的 `host`。

**为什么不用 Rust / C++**：本项目是 I/O 与编排密集型，零 GC 与零开销带来的收益接近于零，而 JSON Schema draft 2020-12 的生态成熟度与开发速度是实质收益。真出现热点（HMAC 验签、批量 schema 校验）再局部换语言，那是实现细节，不是选型。

**为什么 Python 不是首发**：`jsonschema` 4.x 原生支持 2020-12，生态与可读性都最好，但交付一个需要解释器的 CLI 会显著抬高接入门槛。Python 的正确位置是 M2 的 **SDK 接入示例**，不是工具链。

> 一处诚实的不确定：各语言对 draft 2020-12 的支持矩阵我没有逐条核实（本机连不上 raw.githubusercontent）。所以 Go 侧的 JSON Schema 库选型是 M0 的阻塞项，验收方式是**正反对拍**：同一份 `action.schema.json`，一条合法实例必须过、一条已知应失败的实例必须挂。

## 九类契约

| 契约 | 方向 | 关键内容 |
|---|---|---|
| [Module Manifest](spec/0.1/schemas/module-manifest.schema.json) | 模块→系统 | 摘要级自描述，不含 inputSchema（按需拉取，控上下文） |
| [Action](spec/0.1/schemas/action.schema.json) | agent→模块 | `kind`(read/write)、risk、幂等键、补偿、dry-run、配额单位 |
| [Domain Event](spec/0.1/schemas/domain-event.schema.json) | 模块→agent | 至少一次投递、可重放、causation/correlation 链 |
| [Plan](spec/0.1/schemas/plan.schema.json) | agent→系统 | DAG + `expectedDiff` + `dryRun.status` 四态，人能读懂、可回放 |
| [Intent / Context / Confirm](spec/0.1/schemas/frames.schema.json) | 双向 | 人机共驾三帧，**不含 DOM 选择器** |
| [Grant](spec/0.1/schemas/grant.schema.json) | 控制平面 | agent 能力白名单 + 配额；effective = agent ∩ 操作人 |
| [Policy](spec/0.1/schemas/policy.schema.json) | 控制平面 | `auto\|approve\|deny`；合并 `deny > approve > auto`；无命中即 deny |
| [Audit](spec/0.1/schemas/audit-event.schema.json) | 系统→存储 | 两段写（`dispatched` 先落盘才允许执行）+ `prevHash` 哈希链 |
| [Error](spec/0.1/schemas/error.schema.json) | 模块→agent | 四类：retryable / business_rejection / policy_denied / contract_violation |

## 健壮性：fail-closed

组件缺失时行为**预先定义**，且全部朝安全方向倒：

| 缺失 | 行为 |
|---|---|
| 无前端 SDK | 丢 Intent 帧，业务结果不变 |
| 无 dry-run 或 dry-run 报 unsafe | 强制审批 |
| 策略/授权服务不可用 | **deny**（不是 auto） |
| 审计 `dispatched` 写失败 | **拒绝执行** |
| 模块超时/熔断 | 从能力清单摘掉 |

## 仓库结构

```
spec/0.1/         规范：schema 是唯一真源，md 只写语义（JSON Schema）
conformance/      可执行规范：C1–C10 一致性用例（D1 的 runner 在 internal/d1，经 `tenon d1 run` 调用）
tools/lint/       C10 反模式扫描器（Go）
internal/         协议实现对外的全部内核：protocol / spec / module / controlplane / llm / agent / d1
cmd/              host（控制平面）、agent（AI 运行时）、quote / inventory / procurement（模块）、tenon（工具）
skills/           业务流程知识：每个子目录一份 SKILL.md（订阅声明 + 业务语义正文）
packages/         @tenon/core 类型 + dsh 插件（TypeScript，M1 的进程内路径，本轮未接）
demo/mini-erp/    demo 的场景说明与 D1 期望形态
docs/             设计文档、接口文档与架构图
```

## 当前状态

**M0.5 已落地三个进程**：宿主 `cmd/host`（控制平面：策略 / 授权 / 审计链 / 注册表 / 共驾帧 REST）、agent 运行时 `cmd/agent`（轮询发件箱 → 读 skill → 能力发现 → LLM 规划 → 闸门 → 两段审计 → 执行）、触发器模块 `cmd/quote`；M0 的 `cmd/inventory` / `cmd/procurement` 继续可用。协议侧的三处扩展（`POST /tenon/audit`、`GET /tenon/modules`、模块发件箱 `direction=out`）见 [spec/0.1/CHANGELOG.md](spec/0.1/CHANGELOG.md) 的 `0.2.0` 提案段与 [docs/interfaces.md](docs/interfaces.md)。

### 对话入口（M1 切片 1）：后端就绪，链路未通

`cmd/host` 已具备「人 → agent」的入站面：

- 入站队列**落盘**（`requests.jsonl` + `.meta` 序号水位），独立于出站内存队列——两者的 `ack` 都是累积游标，共用一个会互相吞帧；
- `POST /tenon/session` 口令换**无状态 token**（dev 账号表 `config/users.dev.json`：`alice` / `bob` / `agent`）；
- 入站三端点 + 待批公告板 + `GET /` 同源静态页；**消费侧**（`poll` / `ack` / 公告板写入）要求 `role:agent`，普通用户 token 一律 403；
- `origin` 进策略：宿主签发合成 `evt_` 并**独立落盘登记**（这张表是治理状态，不是缓存——单删它会把在途请求静默降级成事件驱动），据此判定「对话发起 / 事件驱动」；缺 `triggerEventId` 的行为由 `TENON_REQUIRE_TRIGGER` 过渡控制（默认放行为 `event`，P4 收口时改强制）；
- 业务级规则从 `skills/<id>/approval.json` 加载，加载期校验 `arg.*` / `risk.*` 路径并试编译，按 skill 粒度 fail-closed；与平台规则同池合并、**取更严**，所以业务规则只能收紧不能放宽。

**链路已通（M1 切片 2）**：agent 消费入站队列、命中 `approve` 时挂起落盘（I7 三次写）、审批人批准后按原计划恢复执行，审计 `result` 段带 `approvedBy`。端到端真跑过：投一句话 → 挂起 → bob 批准 → 库存 120→108 → 归档 `done`。

> **启动顺序：模块先起，host 后起**（`inventory` → `procurement` → `host` → `agent`）。
> host 在**加载期**要拉每个模块的 `GET /tenon/actions/{id}` 来校验并加载 `skills/*/approval.json` 里的业务级规则。模块不在时那两条规则会**静默不加载**——日志里只是少了「业务级规则已加载 2 条（来自 skills）」这一行，不报错。**看到这行才算规则真的生效**。

现在能验证到的最小链路（先起模块，再起 host）：

```bash
# 1) 先起模块（各自一个终端）
TENON_MODULE_ID=inventory PORT=8081 go run ./cmd/inventory
# 2) 再起 host —— 顺序反了业务规则会静默不加载
TENON_USERS_FILE=config/users.dev.json TENON_SESSION_SECRET=dev \
TENON_INBOUND_DIR=out/inbound TENON_MODULES='inventory=http://127.0.0.1:8081' \
TENON_SKILLS_DIR=skills go run ./cmd/host
# 3) agent（需要 LLM 凭据；无凭据时用 stub，见 .githooks 旁的说明或 compose 的 smoke profile）
TENON_HOST_URL=http://127.0.0.1:8080 TENON_ON_BEHALF_OF=user:alice \
TENON_AGENT_ID=ag_fulfillment TENON_AGENT_STATE_DIR=out/agent-state \
TENON_AGENT_USER=agent TENON_AGENT_PASSWORD=agent-dev go run ./cmd/agent
```

```bash
# 换 token，再推一句人话；返回 {"ok":true,"seq":1,"durable":true}
curl -s -X POST localhost:8080/tenon/session -d '{"user":"alice","password":"alice-dev"}'
curl -s -X POST localhost:8080/tenon/requests -H "Authorization: Bearer <token>" \
  -d '{"type":"tenon.request","text":"扣 12 件"}'
# 浏览器打开 http://127.0.0.1:8080/ （需 TENON_UI_DIR=ui）即可用页面走同一条链路
```

### 能判定的用例（宿主模式，真进程实测）

```bash
# 宿主 + inventory 先起在 8080/8081
go run ./cmd/tenon conformance run --target http://127.0.0.1:8081 --host http://127.0.0.1:8080 \
  --module inventory --fixtures conformance/fixtures/mini-erp.json --source-dir . --report out/report.json
```

2026-10-27 10:59 实测输出：`小计：pass 8 / fail 0 / skip 1`，`exit=0`。

| 用例 | 结果 | 断言的是什么 |
|---|---|---|
| C1 | pass | manifest 合法、摘要不含 `inputSchema`、每个 write 都有幂等声明 |
| C2 | pass | 同 `Idempotency-Key` 两次 → 响应一致、副作用一次 |
| C3 | pass | 注入未知字段 → 200 而非 400（前向兼容） |
| C8 | pass | 不支持 dry-run 的 action → 404/501 优雅降级（agent 侧据此强制转人工） |
| C9 | pass | 库存不足 → 422 `business_rejection`，`retryable=false` |
| C10 | pass | 全仓反模式扫描 0 命中 |
| **C4** | **pass** | 审计两段写齐备 + `prevHash` 链连续（给 `--host` 才判定） |
| **C5** | **pass** | 4 个探针（空 plan / 无规则命中 / 未知动作 / 未登记模块）一律 `deny` |
| D1 | **skip** | 需要一个会推理的规划器；**SKIP 不等于 PASS**，报告会明说不能视为整体合规 |

不给 `--host` 时 C4/C5 仍记 SKIP 并写明原因——模块模式没有宿主侧端点可查。

### D1 差异测试：runner 已实现，判定见下

```bash
go run ./cmd/tenon d1 run --host http://127.0.0.1:8080 \
  --quote http://127.0.0.1:8083 --inventory http://127.0.0.1:8081 \
  --procurement http://127.0.0.1:8082 \
  --agent-result out/agent-result.jsonl --report out/d1.json
```

`internal/d1` 会做两轮：A 轮三个模块全在 → B 轮 `POST /tenon/admin/modules` 把 procurement 置 `enabled=false`，两轮之间 reset 所有模块、换一个报价单号（SKU 与数量不变），然后比对 agent 结果文件里两轮**规划出来的**动作序列（判据：序列不同、B 轮不碰被摘模块、B 轮转人工、两轮都没崩）。

**一键复现**（前置条件全部写成启动断言）：`powershell -NoProfile -File scripts/d1-e2e.ps1`（`-Stub` 只验编排，不出判定）。

### D1 判定结果（2026-10-27，真模型 A/B，三次尝试）

模型 `deepseek-flash`，报告 `out/d1/report.json`。
（`deepseek-v4-pro` 实测不可用：推理模型，同一条规划请求 600 token 全花在 reasoning 上、可见 `content` 为空，真实链路里会把轮次拖过超时——所以脚本缺省用 flash。）

| 次 | 条件 | 机械结论 | 实质 |
|---|---|---|---|
| #1 | 修 prompt 前 | `fail` | **判定受阻**：A 轮模型输出连续 2 次没过 `plan.schema.json`，两轮无可比 |
| #2 | 修 prompt 后 | 当时记 `pass` | **空洞通过**：A 轮只有 `inventory.GetStock`，本就没引用被摘的 `procurement`，两轮差异不是摘除造成的 |
| #3 | 判据收紧后 | `fail` | B 轮规划出 `inventory.DeductStock` 但**缺 `referenceNo`** → 预演 400 `SCHEMA_MISMATCH` → fail-closed，B 轮 `failed` |

**结论：D1 至今没有被真正判定过一次。** 三种形态各不相同，但都不是「agent 自适应」的证据。

**根因链（三层，前两层已修，第三层需单独评审）**

1. **`expectedDiff` 的形状没写给模型**（已修）：prompt 没说元素必须含 `field`/`from`/`to`，模型自造 `{op, note}`；prompt 说 entity 可写「字符串简写」却没给格式，模型写了裸主键 `"STD-1002"`，而 `EntityRefString` 要求三段 `模块:类型/主键`（`oneOf` 两侧都不匹配时报错文本会误导成「只收对象」）。修法：第 7 条给出两种等价写法的占位示例 + 明说「只写主键是错的」，`TestPromptStatesExpectedDiffShape` 锁住。
2. **判据会被计划质量架空**（已修）：A 轮若不含被摘模块的动作，「B 轮不碰它」是白送的。现在这种空洞通过记 **`inconclusive`**（新增第三态）而不是 `pass`，用例 `TestD1InconclusiveWhenADidNotUseRemovedModule` 锁住；退出码仍为非 0，不会被 CI 当成合格。
3. **prompt 不带入参契约**（未修，需评审）：动作清单只列 action id（`internal/agent/planner.go` 的「可调动作」块），模型不知道 `DeductStock` 需要 `referenceNo`，写动作必然靠猜——#3 的 400 就是它。要让 D1 可判定，得把每个动作的**最小入参契约**（`required` 字段名与类型）带进 prompt，或让规划器按需拉 `GET /tenon/actions/{id}`。这是设计变更（上下文成本 vs 计划合法率），不在本轮边界内。

> 本轮边界是「只判不调」：已修的两层都是**让判定成为可能**的缺陷（不是为了让结果变绿），第三层会让判定继续受阻，但它改变的是 prompt 的信息量与上下文成本，必须单独评审。用规则式规划器让 D1「通过」就是把核心命题做成假命题——宁可不判定。

### 其它检查

```bash
pnpm check                    # 规范 schema 校验 + TS 类型检查
go test ./... -count=1
docker compose -f compose.m0.yaml config --quiet
```

### 还没做的

dsh/cordis 原生宿主与进程内接入路径（D18 的 deferred 部分）、共驾 WS（Go 标准库无 WS 服务端，目前只有 REST 轮询）、**前端页面**（`GET /` 只是路由，`TENON_UI_DIR` 不配就回 503；对话入口有后端、没有界面）、模块侧 `X-Tenon-Grant` 真校验（M0.5 没有 grant 签发）、模块侧审计对账端点、`requests-index.jsonl` 的 GC/归档策略（它是治理状态，不能随便删，但也没有上限）、入站帧的 `frames.schema.json` 全量校验（现在只校验 `type`）、`durable` 的粘性（一次落盘失败后会一直报 `false` 直到重启）、撤销与补偿（`ReleaseStock` 未接）、`amend`（JSON Patch 改 step 后再批）、`-race` 并发验证（本机 `CGO_ENABLED=0` 且无 gcc/clang）、容器端到端 `up`。

> **审批人角色只有一条来路**：`tenon.confirm` 帧里的 `roles` **由宿主写入**（取提交者 token 的 `claims.roles`，先删客户端自报再写）。agent 侧不读登录身份、不做兜底——它若读自己的登录角色，等于用服务账号的身份给人授权。也因此：**策略必须真的给出 `approvers`**，否则 `requiredRoles` 为空、`rolesIntersect` 对空集 fail-closed，会出现"板上有卡、谁点都 403"。这是 fail-closed 的必然结果，不是 bug。

> **审计形状的一处已知偏离（加严校验时会撞上）**：**挂起那一刻**写的计划级 `dispatched`（`stepId=null`、无 `action` / `inputHash`）与恢复收尾的计划级 `result`，**不满足** `audit-event.schema.json` 的完整 required 集——人工批准的对象是整份 plan，不是某一步。宿主当前的 `POST /tenon/audit` 只校验 `auditId` 非空与 `stage ∈ {dispatched, result}`，所以这些记录能落盘。恢复执行的 **step 级**两段（`stepId` 非空）是 schema 完整的，且带 `decision.approvedBy`；`effects.entities` 已统一写 `[]`（不是 `null`）。哪天宿主把校验加严到 schema 完整，**计划级那两条会全线失败**——这是刻意接受的取舍（P2 设计 §4），不是疏漏。

里程碑：

| 阶段 | 内容 | 出口判据 |
|---|---|---|
| M0 | schema + conformance C1–C3/C8/C9/C10 + lint + `inventory` / `procurement` 两个侧车 | 模块侧用例全绿 |
| **M0.5（已完成）** | `host` + `agent` + `quote` + 协议扩展三处（写审计 / 注册表 / 发件箱） | C4/C5 判 PASS（**已达成**）；D1 需真 LLM 跑一次 A/B（**未跑**，接线已就位） |
| **M1 切片 1（已完成）** | host 入站面：落盘入站队列 / dev 身份 / `origin` 进策略 / 业务级规则 / 待批公告板 / 同源静态页 | 入站与策略侧用例全绿 |
| **M1 切片 2（已完成）** | agent 侧：消费入站队列 / 挂起落盘（I7 三次写）/ 审批人角色通道 / 恢复执行 / 去重账本 / 公告板终态 | 端到端真跑通：投一句话 → 挂起 → bob 批准 → 真扣库存 → 归档 done；缺界面（P3） |
| M1（剩余） | agent 挂起/恢复 + `pendingPlanId` + 补偿 UI 的 amend + 共驾 WS + grant 真校验 + 审计对账 | 高风险路径必须人工；对账能发现篡改 |
| M2 | HTTP 侧车 SDK + MCP 双向 + 非 TS 示例 | 非 TS 模块跑通全套用例 |
| M3 | skill 仓库 + 轨迹蒸馏 + 发布审计/回滚 | 新流程只改 skill 即可上线 |

## 容器

三层（细节与判据见 [docker/README.md](docker/README.md)）：检查/文档层现在就能跑；服务镜像的 **build 阶段**四个 Go 入口都已实测通过，完整镜像受运行时基础镜像与卷权限限制仍未验；拓扑层 `compose.m0.yaml` 语法可验、端到端 `up` 未验。

```bash
docker compose up -d docs      # 文档站 → http://localhost:8088（规范 + 架构图 + 设计文档）
docker compose run --rm spec   # 规范校验（与 CI 的 spec job 同源）
docker compose run --rm go     # Go 工具链 gofmt/vet/build（与 CI 的 go job 同源）
```

M0.5 的目标拓扑是 7 个服务（`host`、`agent`、`quote`、`inventory`、`procurement` + 一次性的 `conformance`、`lint`）、2 个网络、2 个 secret、5 个命名卷，见 [compose.m0.yaml](compose.m0.yaml)：

```bash
docker compose -f compose.m0.yaml config --quiet   # 现在就能过
docker compose -f compose.m0.yaml build            # 逐服务构建；运行时阶段仍未验
```

对外接口（端口分配、模块侧 6 端点、宿主侧 **13 个协议端点 + 2 个探针 + 静态页**、请求头与 token、游标与方向、健康探针、调用矩阵、G1 的 fail-closed 语义、已知缺口）：
[**docs/interfaces.md**](docs/interfaces.md)（§4 有逐条清单）。

## 与相邻协议的关系

| | Tenon | MCP | A2A |
|---|---|---|---|
| 层次 | 业务治理与执行语义 | 工具暴露与传输 | agent 间委派 |
| 关系 | — | 互补：模块可同时以 MCP server 暴露 | v2 适配：Agent Card → Module Manifest |

## 深入

- 设计文档：[docs/superpowers/specs/2026-10-27-tenon-ai-driven-system-design.md](docs/superpowers/specs/2026-10-27-tenon-ai-driven-system-design.md)
- 规范索引：[spec/0.1/README.md](spec/0.1/README.md)
- 架构图（浏览器打开）：[docs/design-screens/](docs/design-screens/architecture-approaches.html)
- 参与贡献：[CONTRIBUTING.md](CONTRIBUTING.md)

## 许可

[Apache-2.0](LICENSE)
