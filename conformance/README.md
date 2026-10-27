# conformance（Go）

可执行规范：把 §10 的 C1–C10 与 D1 差异测试做成 runner。规范的可信度来自「第三方跑测即可自证合规」，而不是文档措辞（§1.1 目标 3）。

**实现语言是 Go**，交付形态是单二进制：第三方下载一个文件就能自证合规，不必先装 Node 或 Python（D17）。

## 职责边界

- 负责：
  - 用例集 C1–C10 + D1（§10.1），每条用例对应一个可执行断言。
  - 命令 `tenon conformance run`（**计划提供**，见下）与机器可读报告输出（每条用例的通过 / 失败与证据：请求、响应、审计片段）。
  - 通过全部用例后签发 badge `tenon-conformant-0.1`（§10.2；签发流程属 M2）。
- 不负责：
  - 不规定协议：用例只是 §4 / §5 规范条款的投影。按 §2.1 判据，写不出用例的条款就不该进协议。
  - 不做反模式静态扫描本身：C10 的扫描器在 `tools/lint`（Go），本包调用它并汇总结果。
  - 不替代人工验收：一致性通过不等于业务正确，更不等于体验合格。
  - 不做性能压测与安全渗透测试。

## 依赖

- 对 dsh：**无依赖**。任何语言的实现都应能跑这套用例（§10、§12 M2 出口判据），runner 绑死 dsh 就失去意义。
- 对 Node：**无依赖**。套件自身不装 Node；被测目标是 TS 实现时，只是把它的地址交给 `--target`。
- 仓内依赖：→ `tenon.dev/tools/lint`（C10 扫描器，同仓 Go 包）。
- schema 校验（C1 需要）：用 Go 的 JSON Schema 库解析 `spec/0.1/schemas/`。**库选型是 M0 的阻塞项**，必须是支持 draft 2020-12 的实现，且用「一份已知应失败的实例」做正反对拍（设计文档 §15 未决问题 6）。
- 方向：conformance → lint；被测目标通过 `--target` / `--host` 以 HTTP 交互，不被 import。
  - 例外在测试侧：`host_test.go` import `internal/controlplane` 起**真实宿主**来证明 C4/C5 判 PASS。它只是测试脚手架，非测试包不引用它，runner 仍是黑盒。

## 用例表

| 用例 | 断言 | 里程碑 |
|---|---|---|
| C1 | manifest 合法：符合 schema；每个 action 有 `kind`、`risk`；`kind=write` 有 `idempotency` | M0 |
| C2 | 幂等：同 `Idempotency-Key` 提交两次 → 副作用一次、返回首次结果 | M0 |
| C3 | 未知字段：注入未知字段 → 忽略而非报错 | M0 |
| C4 | 审计可查：写调用后可查，`dispatched` 与 `result` 两段齐备，`prevHash` 链连续（**宿主模式**；模块模式 SKIP 并写明原因） | M0.5 |
| C5 | fail-closed：策略评估在空 plan / 无命中 / 未知动作上一律 `deny`（**宿主模式**；模块模式 SKIP 并写明原因） | M0.5 |
| C6 | 事件重放：`since=cursor` 补齐丢失事件，消费侧不重复生效 | M1 |
| C7 | 补偿：多步 Plan 中途失败 → 已成功步骤被补偿，审计标 `partially_failed` | M1 |
| C8 | 可选能力容忍：未实现 dry-run → 用例 pass，但运行时强制审批 | M0 |
| C9 | 错误分类：四类错误码与 `retryable` 标志正确 | M0 |
| C10 | 反模式 lint：§10.2 三条扫描规则，任一命中即失败 | M0 |
| D1 | 差异测试：摘掉 `procurement` 后 Plan 必须改变且不抛错 | M0 |

## 命令形态

单二进制 `tenon`，子命令区分工具（`cmd/tenon`）：

```
tenon conformance run --target http://localhost:8081 --report out.json
tenon lint --path .                                 # C10 扫描，见 tools/lint
```

- `--target`：被测**模块**的基地址（`/.well-known/tenon`、`/tenon/actions/{id}` 等六个模块侧端点）。
- `--host`：被测**宿主**的基地址（`/tenon/policies:evaluate`、`/tenon/audit` 等控制平面端点）。**这个开关还没接到 CLI 上**：宿主模式已在库层可用（`conformance.Options.HostTarget`，用例见 `host_test.go`），把 `--host` 接进 `tenon conformance run` 是紧跟着的一处改动。在那之前从 CLI 跑出来的 C4/C5 仍是 SKIP——报告会如实写「未判定」，不会假装通过。
- `--report`：机器可读报告输出路径，内容为逐用例结果与证据。
- 退出码：`0` 全绿；`1` 有 FAIL；`2` 有 SKIP 且带 `--strict`（或用法 / 环境错误）。SKIP 默认不算失败，但报告会写明「不能视为整体合规」。
- 分发：单文件二进制 `tenon`，`go install tenon.dev/cmd/tenon` 或直接下载 release 产物；无运行时依赖。

## 宿主模式：C4 / C5 怎么判

模块模式下这两条**没有判定**，报告记 SKIP 并写明原因。给了宿主地址（`Options.HostTarget`）后才跑，判据如下。

### C4 审计可查（判定面：`POST` / `GET /tenon/audit`）

1. 写两段：先 `POST /tenon/audit` 写 `stage=dispatched`，再用**同一个** `auditId` 写 `stage=result`。写入口是 M0.5 新增的（设计文档 §2.1）；0.1 只定义了查询，所以旧实现连写都写不进去。
2. `GET /tenon/audit?planId=<pln>` 断言：两条都在、`dispatched` 的写入顺序在 `result` 之前、两条 `hash` 都以 `sha256:` 开头、`result.prevHash == dispatched.hash`。
3. `GET /tenon/audit?limit=1` 断言**全链第一条**的 `prevHash` 为 `null`（interfaces.md §4.3「链首为 null」）。本次写入若排在历史之后，则断言它接在历史之后而不是 `null`，并额外排除「中段 `prevHash` 为 null」这种把链断成两条的形态。

证据字段：两段的写入状态码与 `auditId`、按 `planId` 命中的条数与两段的位次、`hash` 前缀与头部字符、`prevHash` 相等关系、链首 `auditId`、本次写入是「链首」还是「接在历史之后」。

写入的记录带 `auditId` / `planId` / `stepId` / `action` / `actor` / `decision` / `inputHash` / `idempotencyKey`，`stage=result` 那条再带 `result{status,outputHash,errorClass}`；`actor`、`decision`、`result` 都是封闭对象，键按 `audit-event.schema.json` 写全（缺键与给 `null` 在哈希链里是不同的字节）。`at` 由宿主盖时间戳，`trigger` / `effects` / `tokenUsage` 不写——写入口是设计文档 §2.1 冻结的**子集**，不是完整 `AuditEvent`，所以整条记录不宣称 schema 完整。

### C5 fail-closed（判定面：`POST /tenon/policies:evaluate`）

四个探针必须全部 `outcome=deny`：

| 探针 | 请求里的动作 | 额外要求 |
|---|---|---|
| ① 空 plan（零步） | `{"plan":{}}` | `reasons` 必须含 `NO_POLICY_MATCH_DEFAULT_DENY` |
| ② 合法 action 但无规则命中 | `tenonconformance.ProbeNoRuleMatch` | 同上 |
| ③ 未知动作（不满足 `ActionId` 词法） | `UnknownAction` | 只锁 `deny` |
| ④ 未登记模块的动作 | `ghostmod.GhostAction` | 只锁 `deny` |

- ①② 锁理由码：`NO_POLICY_MATCH_DEFAULT_DENY` 是规范给的示例值（policy.schema.json#/$defs/PolicyEvaluationResponse），它把「无命中默认拒绝」与「策略本意拒绝」区分开——没有它，运维看不出这是配置缺口。
- ③④ 只锁 `outcome`：会先查注册表的宿主完全可以用别的理由码拒绝，那是更强的实现，不该被判失败。
- 四个探针的共同点是宿主**不可能有正当理由放行**：零步计划没有可执行语义；无规则命中是配置缺口；不认识的动作连语义都不成立。放行其中任何一个，都等于「没证据也执行」——这正是 fail-closed 要堵的口子。

证据字段：每个探针的 HTTP 状态、`outcome`、`decidedBy`、`reasons`，外加一条汇总（`NO_POLICY_MATCH_DEFAULT_DENY` 出现在哪几个探针里）。

### 前提与边界（不满足时结论不能外推）

- **C4 假定本轮没有别的审计写入者**。两段之间若有并发写入插队，`result.prevHash` 会指向插队那条（链本身没断），本用例会判 FAIL。并发写入下的链连续性校验留到 M1。
- **C4 只验宿主侧审计链**。模块侧没有对账端点（interfaces.md §9 缺口 1），「审计双写 + 日切对账」仍无法机械验证。
- **C5 只覆盖策略闸门**。`grants:evaluate` 的 fail-closed 由 `internal/controlplane` 的单测覆盖，本用例不断言它；`decision` / `result` 等审计字段的 schema 完整性也不在 C4 的断言里。
- **C5 对探针 ①③ 是严格判定**：`{"plan":{}}` 与非法 `ActionId` 都不满足 `plan.schema.json`，若宿主对评估请求做 schema 严格校验并回 `400`，本条会判 FAIL。当前实现（与 interfaces.md §4.2 的形态一致）是 `200 + outcome=deny`；本轮没有按实现差异放宽，因为「宿主不认识这个动作时是否仍给出拒绝判定」正是要测的东西。
- **C5 的 ② 依赖一个前提**：用一个合成模块 id（`tenonconformance`）构造「无规则命中」。若被测宿主的策略文件恰好给这个模块写了规则，② 会判 FAIL——那是运行环境的配置问题，不是宿主缺陷，detail 里会打出命中的 `decidedBy`。
- **宿主不可达判 FAIL，不判 SKIP**：给了宿主地址就是要求判定，宿主不应答属于失败，不能退化成「这条没判」。

## badge

通过者获 badge **`tenon-conformant-0.1`**（含协议版本号，§10.2）。签发与校验流程属 M2；在 M2 之前没有可用的 badge 服务，任何「已合规」的宣称都不成立。

## D1 差异测试（主判据）

三步（§10.2）：

1. **冻结输入**：同一事件 + 同一数据快照 + 固定 skill 版本。
2. **跑 A**：三模块全在 → 记录 Plan A（含 chosen actions 序列）。
3. **跑 B**：摘掉 `procurement` 模块 → 记录 Plan B。

断言：**Plan A 的 action 序列 ≠ Plan B 的 action 序列**（比结构，不比参数字符串），且 B 达成可接受结果（预留 / 转人工）、**不抛错**。演示载体是 `demo/mini-erp`（§6.3），对应出口判据是 M0 的「D1 与 C10 全绿」（§12）。

**为什么它能证明 AI 驱动**：如果实现是硬编码映射，摘掉 procurement 后行为要么不变（说明它根本没在用能力清单）、要么崩溃（说明映射写死了）。**只有真正在按能力清单推理，行为才会自适应地改变。** 这也是 D1 作为主判据、C10 静态 lint 只作辅助的原因（D13）。

**受 LLM 非确定性影响的处理**（R7）：只比对 action 序列的结构差异，不比对参数文本；同一 skill 版本 + `temperature=0` + 重试 2 次取多数。

## 阶段状态

M0.5（当前）：模块侧 **C1 / C2 / C3 / C8 / C9 / C10 已实现**；宿主侧 **C4 / C5 在宿主模式下可判定**（`Options.HostTarget` 非空时），模块模式仍**显式记为 SKIP 并写明原因**——SKIP 不等于 PASS，报告会明说「不能视为整体合规」，`--strict` 下 SKIP 让退出码变 2。**D1 永远是 SKIP**：它需要会推理的规划器（LLM），规则式规划器会让它变成假命题。

实测（`go test ./conformance/ -count=1 -v`）：

- 模块模式（合规假模块，无 fixtures / `LintRoot`）：`C1`、`C8` PASS；`C2`、`C3`、`C9` SKIP（缺 fixtures）；`C10` SKIP（未给源码目录）；`C4`、`C5`、`D1` SKIP。`exit=0`，`--strict` 下 `2`。
- 宿主模式（同一假模块 + `internal/controlplane` 起的真实宿主）：`C1`、`C8`、`C4`、`C5` PASS；`D1` 与其余 SKIP；`exit=0`，`--strict` 下 `2`（D1 仍是 SKIP）。
- 反向用例（桩）：第二段没落盘 → `C4` FAIL 且 detail 指名 `stage=result`；策略一律放行 → `C5` FAIL 且 detail 指名被放行的探针；宿主不可达 → `C4`/`C5` FAIL（不退化成 SKIP）。这三条证明 C4/C5 不是「永远 PASS 的摆设」。

历史口径（C4/C5 落地之前，对真实 `cmd/inventory` + `cmd/procurement` 服务并带 fixtures 与 `--source-dir .`）：`exit=0`，pass 6 / fail 0 / skip 3——当时的 3 个 SKIP 正是 C4、C5、D1。

M1 计划实现 C6（事件重放）与 C7（补偿），并把 C5 从「只测策略闸门」扩到「策略 + 授权双闸门」，同时补上 C4 在并发写入下的链连续性校验。

M2 计划实现 badge `tenon-conformant-0.1` 的签发，并让非 TS 模块（HTTP 侧车、MCP 服务）跑通全套用例（§12 M2 出口判据）。

M3 计划把 D1 扩展到 skill 版本变更场景，验证「新业务流程只改 skill 即可上线」（§12 M3 出口判据）。
