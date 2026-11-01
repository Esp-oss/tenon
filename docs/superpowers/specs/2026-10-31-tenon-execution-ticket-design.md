# 执行票据与模块侧对账（P0-1 / P0-2）设计

- 日期：2026-10-31
- 状态：待批准
- 范围：Tenon 0.2.0 增补——执行票据（P0-1）、模块侧审计链与日切对账（P0-2）
- 前置结论：当前「系统裁决」是咨询式的。`internal/agent/run.go:747` 由 agent 直连模块写端点，`internal/module/module.go:939` 只校验 `Tenon-Version` 与 `Idempotency-Key`，任何能连上端口的人都能扣库存。

---

## 1. 意图与为什么

目标不是"加一层校验"，而是把**副作用的产生条件**从"agent 自觉"改成"模块只认票"。

一句话判据：**没有宿主签发的有效票据，模块产生不了副作用；产生过的每一笔，两侧都留得下、对得上、改不了。**

为什么现在做：这是「AI 提议、系统裁决、人审批、全程可审」四条里唯一还空着的一条。审批链已经跑通（挂起/续跑/一次一用），但裁决结果到不了执行点，等于没有裁决。

---

## 2. 目标 / 非目标 / 反目标

**目标**

1. 写动作在模块侧强制验票；无票、过期、改参、重放、跨模块用票，全部拒绝且库存不变。
2. 票由宿主在「策略 + 授权」通过后签发，签发即写 `dispatched` 审计——审计前置不再依赖 agent 顺序。
3. 模块每笔写留本地记录，含 `auditId/planId/approvedBy`，并带前序哈希成链。
4. 宿主可按日增量拉取模块记录，做双向对账；删一条、改一条、绕过一笔，都能报出。

**非目标**

- 不做自动补偿（C7 未建；对账只出差异，不修数据）。
- 不改读路径：读动作无副作用，仍不验票、不入账（可审性覆盖副作用，不是覆盖全部请求）。
- 不做多租户、不做配额预扣与结算。
- 不做模块间互信：模块之间不共享任何密钥。

**反目标**（做到了也不算成功）

- 票只能证明"宿主同意过"，但模块仍可被直接调用成功 → 失败。
- 对账只能发现"一边有另一边没有"，发现不了单边改写 → 失败。
- 为通过测试而在模块里放一个开关，默认关闭验票 → 失败（生产档位必须默认强制）。

---

## 3. 约束

| 约束 | 来源 |
|---|---|
| 模块可实现任意语言，只能依赖公钥与 HTTP | 协议面向第三方 ERP |
| 宿主已有 Ed25519 能力需求之外的一切：策略、授权、审计链、幂等键 | 现有实现 |
| 两侧 `argsHash` 必须逐字节一致 | 现有实现都算 `sha256(raw body)` |
| 协议变更需向后兼容 0.1 | `spec/0.1` 已冻结 |
| 只有本机开发环境可放宽，生产必须 fail-closed | ADR-0001 的档位划分 |

---

## 4. 方案与取舍

**A. 票据强制 + 模块验签 + 链式模块账（已选）**

宿主 Ed25519 私钥签票，模块持公钥验；模块记录加链；宿主增量拉取对账。
取舍：模块要改代码（验签 + 链 + 读端点），要管公钥分发与轮换；换来的是 agent 仍直连、延迟不变、绕过在密码学上不可能。

**B. 宿主代理派发**

agent 不直连模块写，全部经宿主 + mTLS。
取舍：不用票据，授权与审计顺序天然正确；但宿主成为数据面单点，写全串行，`run.go` 与重试/幂等逻辑大改，"agent 驱动"叙事退化为"宿主驱动"。留作票据之后的加固选项。

**C. 票据 + 每条记录独立签名（不做链）**

取舍：省掉链，但只能证明单条未被改，证明不了中间少了一条；与验收冲突，单用不成立。

**为什么是 A**：B 解决的是"agent 不该有网络可达性"，A 解决的是"没有授权就写不进去"。当前架构里 agent 必须能读模块（dry-run/查询），把写也收归宿主会破坏分解并行的设计；而 A 用一层签名把"能不能写"与"能不能连"解耦，代价最小、判据最硬。

---

## 5. 票据设计

### 5.1 格式

```
Tenon-Ticket: v1.<base64url(payload)>.<base64url(sig)>
sig = Ed25519_sign(hostPrivateKey, "v1." || base64url(payload))
```

payload（JSON，字段短名以便放进头）：

| 字段 | 含义 | 约束 |
|---|---|---|
| `v` | 版本 | `1` |
| `tid` | 票据 id | `tkt_` + 随机 |
| `kid` | 签名密钥 id | 公钥 sha256 前 8 字节 hex |
| `mod` | 目标模块 id | 必须等于模块自身 id |
| `act` | 动作标识 | 必须等于 URL 里的 action |
| `ah` | 入参哈希 | `sha256:` + hex，对**原始请求体字节**求值 |
| `idem` | 幂等键 | 写动作必填 |
| `pid` / `sid` | 计划 / 步骤 | 审计与对账用 |
| `aud` | 宿主审计 id | 与 `dispatched` 记录一致 |
| `by` | 审批人 | 自动档可为 `null` |
| `gid` | 命中的授权 id | 无授权时为 `null` |
| `iat` / `exp` | 签发 / 过期（Unix 秒） | `exp - iat ≤ 60`；建议 30 |

### 5.2 为什么绑定原始字节

`ah` 对 agent 将要发出的**那串字节**求值，而不是对解析后的对象重新序列化。两侧现有实现都是 `sha256(raw body)`：agent 在 `run.go` 里算 `inputHash`，模块在 `record()` 里算 `argsHash`。任何"解析再序列化"都会引入键序/空白漂移，让票在同一次调用里验不过。因此：

- agent 把 args 作为**原始字符串**提交给签发端点；
- 宿主用原始字符串算 `ah`，另解析一份副本做策略判定（解析不改变字节）；
- agent 发送时使用**同一串字节**。

### 5.3 校验顺序（模块侧，任一失败即拒，全部 non-retryable）

1. 头缺失 → `TICKET_MISSING`（403）
2. 解码/版本/签名失败 → `TICKET_INVALID`（403）
3. `kid` 不在信任集 → `TICKET_UNKNOWN_KEY`（403）
4. `mod` 不等于自身 → `TICKET_WRONG_MODULE`（403）
5. `act` 与 URL 不符 → `TICKET_ACTION_MISMATCH`（403）
6. `now > exp` 或 `iat > now + 300` → `TICKET_EXPIRED`（403）
7. `ah` 与请求体哈希不符 → `TICKET_ARGS_MISMATCH`（403）
8. `tid` 已用过且本次 `idem` 不同 → `TICKET_REUSED`（403）
9. `tid` 已用过且 `idem` 相同 → 走幂等缓存，返回首次结果（200，不是拒绝）

> 第 9 条是刻意的：重试必须幂等，不能因为"票只能用一次"把正常的传输层重试打成失败。真正的"重放攻击"是换一个幂等键想再扣一次，那由第 8 条挡。

### 5.4 错误分类

全部 `permanent`。票据失败意味着计划已过期或有人改过参数，重试无意义，正确反应是**重新规划**而不是重发。

### 5.5 票必须绑定"人批准的那份参数"

只绑 `action` 是不够的：审批人看到的是"采购 8 件"，agent 完全可以拿同一动作申请一张 `qty=800` 的票，只要策略对两者都放行。

现状是 `pendingItem`（`internal/controlplane/pending.go:20`）**只存展示信息，不存 plan、不存 args**，宿主没有任何"批准了什么参数"的记录。因此本设计加一步：

- `pendingItem` 增加 `steps: [{stepId, action, argsHash}]`（只存哈希，不存业务数据）；
- agent 挂起时随公告板一起推送；宿主在**批准那一刻冻结**这份哈希清单；
- `tickets:issue` 必须命中 `(planId, stepId, argsHash)` 三元组，否则 `TICKET_NOT_APPROVED`（403）；
- 自动档（policy=auto）无公告板，改由 `policies:evaluate` 时落一份同样的步骤哈希快照。

残留风险（写进文档不隐瞒）：如果公告板上展示的 `summary` 与 `steps` 的实际参数不符，人可能批准了自己没看见的参数。这属于审批 UI 的诚实性，**票据管不了**；缓解办法是公告板逐行展开参数，另立任务。

### 5.6 一票一次尝试，不是一个步骤一张票

`executeStep` 每次尝试都新生成 `auditId` 并写一条 `dispatched`（`run.go:758`，在重试循环内）。若一个步骤只发一张票，`aud` 只能对上第一次尝试，后续尝试的 `dispatched` 会在对账里变成假阳性。

定：**每次尝试签发一张票**（新 `tid` + 新 `aud`），同一 `idem` 键贯穿所有尝试。对账单位是 `auditId`；`result=unknown` 且模块无记录的尝试归为 `host_only_unknown`（预期内，不计入篡改），只有 `result=ok` 却无模块记录才算 `host_only`。

---

## 6. 密钥管理

| 项 | 做法 |
|---|---|
| 算法 | Ed25519（签发快、公钥短、无参数歧义） |
| 宿主私钥 | 文件 `TENON_HOST_SIGNING_KEY`（PKCS#8 PEM 或 32 字节种子），权限 0600；缺失则**拒绝启动**（不降级） |
| 公钥分发 | 模块配置 `TENON_TRUSTED_KEYS`：JSON `{"<kid>": "<base64 raw pubkey>"}`，可含多个以支持轮换 |
| 轮换 | 先给模块加新公钥 → 宿主切新私钥 → 旧 `kid` 保留一个票据 TTL 周期后移除 |
| 泄露处置 | 移除该 `kid` 公钥即全线失效；票据 TTL ≤60s 保证暴露窗口短 |

---

## 7. 签发流程（宿主）

新增 `POST /tenon/tickets:issue`：

```json
{
  "planId": "plan_...", "stepId": "s2", "action": "inventory.ReserveStock",
  "argsRaw": "{\"sku\":\"M8\",\"qty\":8}",
  "approvedBy": null, "grantId": "gr-dev-fulfillment"
}
```

宿主按序执行，任一步失败即返回，不签票：

1. 策略判定（现有 `rules.go`：deny > approve > auto，无命中即 deny）
2. 授权判定（现有 `grants:evaluate` 逻辑，含过期与配额）
3. 写 `dispatched` 审计（**宿主自己写**，拿回 `auditId`）
4. 签票，返回 `{ticket, auditId, exp}`

```json
{ "ticket": "v1...", "auditId": "aud_...", "exp": 1793400000 }
```

**这一步同时收紧现有漏洞**：`POST /tenon/audit` 不再接受 agent 写 **步骤级** `stage=dispatched`（`stepId != ""` 时返回 `AUDIT_STAGE_FORBIDDEN`），dispatched 只能由签发流程产生，result 仍由 agent 在调用后写。否则"审计前置"仍是 agent 自觉。

> 为什么要限定"步骤级"：`suspend()` 会写一条计划级 `dispatched`（`stepId` 为空，`auditId` 由 agent 生成，批准后 `resume.go:386` 还要拿同一个 `auditId` 写计划级 `result`）。一刀切拒绝会把整条批准—续跑链路打断。计划级 dispatched 的收归宿主留到 P1，与本设计不冲突。

### 7.1 签发端点谁来调

现有宿主控制端点多数只做 `preflight`（无身份），`/tenon/pending`、`/tenon/requests` 才走 `bearerClaims`。如果 `tickets:issue` 也只做 preflight，那"运维拿配置 curl 扣库存"只是从模块挪到了宿主——威胁没消失，只换了一站。

定：

- `tickets:issue` **必须**带 agent 角色的 bearer；`approvedBy` 与 `gid` **从 token claims / 冻结的批准记录取，不从 body 取**（body 里同名字段一律忽略）。
- `TENON_TEST_MODE=1` 下允许 preflight 直通，但响应里带 `"devBypass": true`，并在审计里记 `actor=dev`。
- 生产档位缺失 bearer → `401 AUTH_REQUIRED`，不签票。用 401 而不是 403：这是「你是谁」的问题，与宿主既有端点（pending/inbound）的约定一致，混进权限判定会让对端往错的方向修。

### 7.2 配额的扣减点

`grant.Evaluate` 在"放行"时就扣配额（`internal/controlplane/grant/grant.go:242`，追加 usageRecord）。如果 agent 先 evaluate 一次、签发又 evaluate 一次，同一笔会被扣两次。

定：

- **只在签发时扣一次**；agent 侧的 `grants:evaluate` 改为不扣减的预检（`charge:false`），仅供计划阶段估算。
- 票签出但模块永久拒绝（票据校验失败、幂等冲突）时，配额**不退**，但 `dispatched` 审计记 `quotaCharged: true` 与 `gid`，让超扣可见可审。
- 自动退款（release）不在 v1，列入待定。

### 7.3 步骤参数快照

`policies:evaluate` 时落一份 `(planId, stepId, argsHash)` 快照（无公告板场景的批准依据）；`tickets:issue` 用它做 §5.5 的命中校验。快照随计划结束清理，保留期与审计一致。

---

## 8. 模块侧改动

### 8.1 记录增强

`auditRecord` 增加：`auditId`、`planId`、`stepId`、`approvedBy`、`ticketId`、`prevHash`、`hash`。

```
hash = sha256(prevHash || canonical_json(record 去掉 hash))   // 首条 prevHash = ""
```

链按文件顺序，追加写；启动时校验尾部，不合法则拒绝启动（宁可不可用，不可静默）。

### 8.2 读端点

```
GET /tenon/audit/local?since=<seq>&limit=200
→ { "module": "inventory", "records": [...], "headSeq": 812, "headHash": "sha256:..." }
```

只读、只返回本模块、按 `seq` 升序。宿主记录每次拉取的 `(module, headSeq, headHash)` 作为外锚。

### 8.3 历史数据怎么进链

现存的 `audit-<module>.jsonl` 是无链、且混着 `kind=event` 的旧格式。直接"启动时校验尾部，不合法则拒绝启动"会让**每个升级过的部署第一次启动就起不来**。

定：链从创世行开始，旧数据处理成三段共存：

1. 文件里已有记录时，首次以新版本启动先追加一行创世标记：
   `{"kind":"genesis","at":"...","prevHash":"","hash":"sha256:...","legacyCount":N}`
2. 创世行之前的记录标 `legacy: true`，**不参与链校验**，只参与按 `idem` 的字段对账。
3. 创世行之后严格校验；`headSeq` 从创世行起算，`GET /tenon/audit/local?since=` 的游标语义以新序号为准。

对账里 `legacy` 记录若出现 `field_mismatch` 照报；若出现"宿主有、模块无"且发生在创世之前，标 `legacy_gap` 而不是 `chain_broken`——升级前的账不追究，升级后的账必须严。

### 8.4 为什么不做模块签名

模块给自己的链签名，只在"模块主机未被攻破"时有意义；主机被攻破时它可以用自己私钥重签一条假链。真正有效的是**宿主增量拉取并留存**——拉走的部分模块改不了（改了 next 拉取就对不上）。v1 只做增量锚定，模块密钥留作后续加固。

---

## 9. 对账

**触发**：日切定时 + 手动 `tenon reconcile --module inventory --since <headSeq>`。

**join 键**：优先 `auditId`；模块记录无 `auditId`（历史数据）时退化到 `idempotencyKey`，并在报告里标 `joinedBy: idem`。

**四类差异**

| 类 | 含义 | 处置 |
|---|---|---|
| `host_only` | 宿主 `result=ok`，模块无记录 | 调用未达或模块丢账；查网络与模块日志 |
| `host_only_unknown` | 宿主 `result=unknown`（传输层失败），模块无记录 | **预期内**：重试尝试未落地；不计入篡改 |
| `module_only` | 模块有记录，宿主无 | **绕过**（或宿主审计丢失）；最高优先级 |
| `field_mismatch` | 两侧都有，`argsHash`/`result` 不一致 | 有人在中间改过；冻结该模块写权限 |
| `chain_broken` | 链断、`prevHash` 不接、`headHash` 与上次锚不符 | 记录被删/改/插；冻结并人工取证 |
| `legacy_gap` | 创世行之前的"宿主有、模块无" | 升级前的账，不追究；只记录 |

**输出**：`out/reconcile/<date>-<module>.json`，含四类计数与逐条明细；退出码非 0 表示存在 `module_only` 或 `chain_broken`。

**不做**：不自动补偿、不自动回滚、不改任一侧数据。对账是取证，不是修复。

---

## 10. 威胁模型

**能防**

- agent 代码 bug / 模型编造动作 → 无票，403。
- 运维拿着 agent 的配置直接 `curl` 扣库存 → 无票，403。
- 拿到 A 模块公钥的人替 B 模块造票 → `mod` 不匹配 + 私钥不在他手上。
- 票被截获后改参数或换动作重放 → `ah`/`act` 不匹配。
- 有人事后删改模块账 → 链与外锚对不上。

**不能防（必须在文档里承认）**

- 拿到宿主私钥的人：他就是宿主。
- 同时控制模块主机与磁盘、且在宿主第一次拉取之前就重写历史的人。
- 直接改数据库、绕过模块接口的人（这属于模块自身的账实一致性，票据管不到）。
- 读路径：数据泄露不在本设计范围内。

---

## 11. 兼容与配置

| 项 | 值 |
|---|---|
| 协议版本 | 0.2.0 增补 `ticket` 头与 `tickets:issue`；0.1 模块不认识该头，行为不变 |
| 模块强制模式 | `TENON_TICKET_MODE`：`enforce`（默认）/ `warn`（只记日志，仅限本机开发） |
| 宿主私钥缺失 | 拒绝启动 |
| 一致性套件 | 新增 C11（票据六类负向）、C12（链与对账四类），进 `tenon-conformance` |

---

## 12. 验收

对应你给的三条，加两条对账：

| # | 场景 | 期望 |
|---|---|---|
| 1 | 无票直接 `curl` 写端点 | 403 `TICKET_MISSING`，库存不变 |
| 2 | 同一张票改 `qty` 后重发 | 403 `TICKET_ARGS_MISMATCH`，库存不变 |
| 3 | 同一张票换 `Idempotency-Key` 重发 | 403 `TICKET_REUSED`，库存只减一次 |
| 4 | 票据 `exp` 过期 | 403 `TICKET_EXPIRED` |
| 5 | 拿 inventory 的票打 procurement | 403 `TICKET_WRONG_MODULE` |
| 6 | 同一张票同 key 重发（传输层重试） | 200，返回首次结果，库存只减一次 |
| 7 | 删模块日志其中一行 | 对账报 `chain_broken` + 该笔 `host_only` |
| 8 | 改一行 `result` | 对账报 `field_mismatch` + `chain_broken` |
| 9 | 绕过宿主直接写成功 | 对账报 `module_only`，退出码非 0 |
| 10 | 批准 `qty=8`，申请 `qty=800` 的票 | 403 `TICKET_NOT_APPROVED` |
| 11 | 无 bearer 调 `tickets:issue` | 401 `AUTH_REQUIRED`，不签票 |
| 12 | 一次写动作重试 3 次（前两次传输失败） | 配额只扣 1；对账不把两次失败尝试报成篡改 |
| 13 | 带旧 `audit-*.jsonl` 启动新版本 | 正常启动，写创世行，旧记录标 `legacy` |

---

## 13. 拆分与工作量

| 任务 | 内容 | 人日 |
|---|---|---|
| T1 | 协议增补：`ticket` 头、错误码、`tickets:issue` 契约、schemas | 1 |
| T2 | 宿主：密钥加载、签发端点（含 bearer 鉴权）、dispatched 收归宿主（仅步骤级） | 2 |
| T3 | 模块：验签中间件 + 9 条校验 + 错误码 | 1.5 |
| T4 | 模块：记录增强 + 链 + 创世行 + `GET /tenon/audit/local` | 2 |
| T5 | 宿主：增量拉取 + 对账六类 + 报告 + 退出码 | 2 |
| T6 | 宿主：步骤参数快照与 `TICKET_NOT_APPROVED` 校验（含 `pendingItem.steps`） | 1 |
| T7 | 一致性用例 C11/C12 + 端到端演练（含重试与 legacy 场景） | 2 |

合计 **11.5 人日**（原估 4–6；增量来自密钥管理、批准参数绑定、链创世与重试对齐——不做这些，验收里的第 3、10、12、13 条过不了）。

---

## 14. 待定（需要你定，或我先按默认走）

1. **改不动的遗留模块**：本设计假定模块可加验签与读端点。若存在不能改的系统，只能走"网关代验票 + 代记账"的旁路（另一份 P1 设计），本设计不覆盖。
2. **TTL 取值**：默认 30s。若模块在慢网络后面（跨机房），需要 60s 或本地时钟同步。
3. **事件记录是否入对账**：模块日志含 `kind=event`，v1 只对 `kind=action`；事件对账另议。

我的默认：1 按"全部可改"推进并在文档标注；2 取 30s；3 只对 action。

---

## 15. 我替你做的六个决定（请核对）

构建者审查提出、文档原本没写清、我按最保守可实施的方式定下的六条：

| # | 问题 | 我的决定 | 影响到 |
|---|---|---|---|
| 1 | 配额被 evaluate 和签发各扣一次 | 只在签发扣；agent 侧改预检不扣 | 验收 12、T2 |
| 2 | 签发端点的身份，`approvedBy` 从哪来 | 必须 bearer；`by`/`gid` 取 claims，不信 body | 验收 11、T2 |
| 3 | 票据只绑动作、绑不上"人批准的那份参数" | `pendingItem` 增步骤哈希并在批准时冻结，签票必须命中 | 验收 10、T6 |
| 4 | 一次步骤多次重试，一票对不上多个 auditId | 一次尝试一票；`result=unknown` 单独归类 | 验收 12、§5.6 |
| 5 | 一刀切拒绝 dispatched 会打断挂起/续跑 | 只拒步骤级；计划级留 P1 | §7、T2 |
| 6 | 旧的无链日志会让升级后首次启动失败 | 创世行 + `legacy` 标记，旧账不追 | 验收 13、T4 |

这六条如果你有不同意见，改哪条都行——但它们不是细节，改一条会动对应任务的拆法。
