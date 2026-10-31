# 执行票据与模块侧对账：实施计划

- 依据：`docs/superpowers/specs/2026-10-31-tenon-execution-ticket-design.md`（已批准）
- 日期：2026-10-31
- 总量：11.5 人日 / 7 个任务
- 纪律：每个任务**先写失败测试**，再写实现；每个任务结束时 `go test ./...` 必须全绿，且不留 TODO

---

## 0. 前置

```bash
git switch -c feat/execution-ticket
```

生成宿主签名密钥（T1 会提供 `tenon keygen`，此前可用临时脚本）：

```bash
tenon keygen --out ./secrets/host-ticket.key --pub ./secrets/host-ticket.pub.json
# host-ticket.key 权限 0600，不入库；.gitignore 加 secrets/
```

环境变量（新增）：

| 变量 | 作用 | 缺省 |
|---|---|---|
| `TENON_HOST_SIGNING_KEY` | 宿主私钥路径 | 无 → 宿主拒绝启动 |
| `TENON_TRUSTED_KEYS` | 模块信任的公钥集 JSON | 无 → 模块拒绝启动（enforce 档） |
| `TENON_TICKET_MODE` | `enforce` / `warn` | `enforce` |

---

## 1. 任务图

```
T1 协议与密钥工具
 ├── T2 宿主签发（依赖 T1）
 │    └── T6 批准参数快照（依赖 T2，改 pending/policies）
 ├── T3 模块验签（依赖 T1）
 │    └── T4 模块记录+链+读端点（依赖 T3）
 │         └── T5 对账（依赖 T4）
 └── T7 一致性用例 C11/C12（依赖 T2+T3+T4+T5）
```

T2 与 T3/T4 可并行；T5 必须等 T4；T6 独立于 T3/T4，但端到端验收在 T7。

---

## 2. T1 协议与密钥工具（1 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `internal/protocol/protocol.go` | 增 `HeaderTicket = "Tenon-Ticket"`；错误码常量 `TICKET_MISSING/INVALID/UNKNOWN_KEY/WRONG_MODULE/ACTION_MISMATCH/EXPIRED/ARGS_MISMATCH/REUSED/NOT_APPROVED`，全部 `ClassPermanent` |
| `internal/protocol/ticket.go`（新） | `Ticket` 结构、`SignTicket(priv, t)`、`VerifyTicket(pub, token, now)`、`ParseTicket` |
| `cmd/tenon/keygen.go`（新） | `tenon keygen --out --pub`：Ed25519 私钥 PKCS#8 + 公钥 JSON `{kid, publicKey}` |
| `spec/0.1/CHANGELOG.md` | 0.2.0 提案段落补票据条目；`spec/0.1/schemas/` 增 `ticket.schema.json` |

**测试先行**（`internal/protocol/ticket_test.go`）

1. 正确票据 → 验签通过，字段还原一致
2. 改 payload 任一字段 → 验签失败
3. 换密钥签 → 失败
4. `exp` 已过 → `TICKET_EXPIRED`
5. `iat` 超前 301s → `TICKET_EXPIRED`
6. `exp-iat > 60` → 签票时直接拒绝（不签发超长票）
7. `kid` 不在信任集 → `TICKET_UNKNOWN_KEY`

**验收**

```bash
go test ./internal/protocol -run Ticket -v
tenon keygen --out /tmp/k.key --pub /tmp/k.pub.json && cat /tmp/k.pub.json
```

**完成判据**：七个用例全绿；`keygen` 产出的公钥能被 `VerifyTicket` 直接用；票据字符串长度 < 1KB（能进 HTTP 头）。

---

## 3. T2 宿主签发（2 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `internal/controlplane/ticket.go`（新） | 密钥加载（缺失即 `os.Exit`）、`serveTicketIssue`、签发逻辑 |
| `internal/controlplane/http.go` | 路由 `case "/tenon/tickets:issue"`；`serveAuditAppend` 增加：`stage==dispatched && stepId!=""` → `AUDIT_STAGE_FORBIDDEN` |
| `internal/controlplane/grant/grant.go` | 抽出 `EvaluateNoCharge`（或给 `Evaluate` 加 `charge bool`），签发走扣减版 |
| `internal/agent/run.go` | 写动作：先调 `tickets:issue` 拿票；不再自己写步骤级 dispatched；请求头带 `Tenon-Ticket` |
| `internal/agent/suspend.go` | 计划级 dispatched 保持原样（不写票据），仅确认未被 T2 的拒绝逻辑波及 |

**测试先行**（`internal/controlplane/ticket_test.go`）

1. 无 bearer → 401 `AUTH_REQUIRED`，审计无新增
2. 策略 deny → 不签票，返回 policy 拒绝原因
3. 授权不足 → 不签票，`SUBJECT_NOT_PERMITTED`
4. 放行 → 返回 `{ticket, auditId, exp}`，且审计里**已有一条** `stage=dispatched`
5. 同一请求调两次 → 配额只扣 1（读 usage 记录）
6. `POST /tenon/audit` 写步骤级 dispatched → 403 `AUDIT_STAGE_FORBIDDEN`
7. `POST /tenon/audit` 写计划级 dispatched（`stepId==""`）→ 仍允许（保住挂起链路）

**验收**

```bash
go test ./internal/controlplane -run 'Ticket|AuditAppend' -v
go test ./internal/agent ./internal/controlplane
```

**完成判据**：七个用例全绿；`go test ./...` 不回归；`run.go` 里不再有"先写 dispatched 再调模块"的自造顺序。

---

## 4. T3 模块验签（1.5 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `internal/module/ticket.go`（新） | 信任集加载、`verifyTicket(r, body, action)`，按设计 §5.3 的 9 步顺序 |
| `internal/module/module.go` | `serveActionExecute` 入口先验票；票据 id 存入幂等记录 |
| `internal/module/store.go`（或幂等表结构处） | 幂等记录增 `ticketId`，用于第 8/9 条判定 |

**测试先行**（`internal/module/ticket_test.go`）

对设计 §12 的 1–6 条逐条覆盖：无票 / 改参 / 换幂等键重放 / 过期 / 跨模块用票 / 同键重放返回首次结果。每条都断言**库存未变**（读模块状态，不看响应码）。

**验收**

```bash
go test ./internal/module -run Ticket -v
```

**完成判据**：六条全绿；`warn` 模式下同样的请求返回 200 但在日志里留 `ticket_rejected` 记录（供本地调试，生产禁开）。

---

## 5. T4 模块记录、链与读端点（2 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `internal/module/module.go` | `auditRecord` 增 `auditId/planId/stepId/approvedBy/ticketId/prevHash/hash/legacy`；写入时算链 |
| `internal/module/chain.go`（新） | 尾行读取、校验、创世行追加 |
| `internal/module/module.go` | 新端点 `GET /tenon/audit/local?since=&limit=` |
| `internal/module/module.go` | 启动时：文件非空且末行无 `hash` → 追加创世行并标旧记录 `legacy` |

**测试先行**（`internal/module/chain_test.go`）

1. 连续写 3 行 → 链可校验，`headHash` = 第 3 行 hash
2. 中间删一行 → 校验失败，报出断点位置
3. 改一行 `result` → 校验失败
4. 旧格式文件（无 hash）启动 → 追加创世行，旧记录 `legacy:true`，新记录从创世后起链
5. `?since=` 分页 → 不重不漏，`headSeq` 正确
6. 空文件启动 → 直接写创世行，无 legacy

**验收**

```bash
go test ./internal/module -run 'Chain|AuditLocal' -v
curl -s "localhost:8081/tenon/audit/local?since=0&limit=5" | jq .headSeq
```

**完成判据**：六条全绿；手工删一行后 `tail` 校验能指出是哪一行。

---

## 6. T5 对账（2 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `internal/reconcile/`（新包） | 拉取器、join、六类差异判定、报告写出 |
| `cmd/tenon/reconcile.go`（新） | `tenon reconcile --module --host --since --out --anchor` |
| `internal/controlplane/` | 锚存储：`(module, headSeq, headHash, at)` 追加到 `out/reconcile/anchor.jsonl` |

**测试先行**（`internal/reconcile/reconcile_test.go`，用临时目录造两侧日志）

1. 完全一致 → 零差异，退出码 0
2. 模块少一条 → `host_only` + `chain_broken`
3. 模块多一条 → `module_only`，退出码非 0
4. `result` 不一致 → `field_mismatch`
5. 宿主 `result=unknown` 且模块无 → `host_only_unknown`，退出码 0
6. 旧记录（`legacy`）在创世前缺失 → `legacy_gap`，退出码 0
7. 锚被改 → 下次拉取 `chain_broken`
8. 无 `auditId` 的旧记录 → 按 `idem` 对齐，报告标 `joinedBy: idem`

**验收**

```bash
tenon reconcile --module inventory --host http://127.0.0.1:8080 --out out/reconcile/
echo $?   # 有人为差异时非 0
```

**完成判据**：八条全绿；报告 JSON 能直接被人读懂（含每类的 `auditId/idemKey/说明`）。

---

## 7. T6 批准参数快照（1 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `internal/controlplane/pending.go` | `pendingItem` 增 `steps []{stepId, action, argsHash}`；批准时写入 `frozenAt` |
| `internal/controlplane/http.go` | `policies:evaluate` 落步骤哈希快照（自动档用）；`tickets:issue` 校验 `(planId, stepId, argsHash)` 命中，否则 `TICKET_NOT_APPROVED` |
| `internal/agent/suspend.go` | 推公告板时带上 `steps` |

**测试先行**（`internal/controlplane/pending_test.go` 扩展）

1. 批准 `qty=8` 后申请 `qty=800` → 403 `TICKET_NOT_APPROVED`
2. 批准 `qty=8` 后申请 `qty=8` → 签票成功
3. 未挂起（自动档）但未跑过 `policies:evaluate` → 无快照 → 403
4. 自动档跑过 evaluate → 快照命中 → 签票成功
5. 计划结束后快照被清理 → 再申请 → 403

**验收**

```bash
go test ./internal/controlplane -run 'Pending|Ticket' -v
```

**完成判据**：五条全绿；公告板 JSON 里能看到 `steps` 但看不到业务参数原文（只有 hash）。

---

## 8. T7 一致性用例与端到端（2 人日）

**改动**

| 文件 | 内容 |
|---|---|
| `conformance/conformance.go` | 新用例 `C11`（票据六类负向）、`C12`（链与对账六类） |
| `conformance/README.md` | 用例说明与前置（需要 `TENON_HOST_SIGNING_KEY`） |
| `conformance/host_test.go` | 无密钥时 SKIP 并给出可操作提示，不假通过 |

**验收**

```bash
go test ./...
pnpm check
tenon conformance run --target http://localhost:8081 --module inventory \
  --fixtures conformance/fixtures/mini-erp.json --source-dir . --report out/report.json --strict
```

**完成判据**：C1–C10 不回归；C11/C12 全绿；用设计 §12 的 13 条手工过一遍，逐条留输出。

---

## 9. 全局门槛

| 检查 | 命令 | 门槛 |
|---|---|---|
| 单测 | `go vet ./... && go test ./... -count=1` | 全绿，无跳过（除显式 SKIP 用例） |
| 前端/规范 | `pnpm check` | 全绿 |
| 反模式 | `tenon lint --path .` | 0 违规 |
| 一致性 | `tenon conformance run --strict` | 全绿 |
| 竞态 | `CGO_ENABLED=1 go test -race ./internal/...` | 无竞态（此前未跑过，本计划补上） |

---

## 10. 风险与回滚

| 风险 | 缓解 |
|---|---|
| 模块时钟偏移导致票据误过期 | TTL 30s + `iat` 允许 +300s；文档写明需 NTP |
| 密钥丢失导致全线不可写 | 私钥文件纳入备份清单；`kid` 轮换流程写进 runbook（T2 产出） |
| 旧部署升级首次启动失败 | T4 的创世行逻辑必须有 "空文件 / 旧文件 / 新文件" 三态测试 |
| `warn` 被误用于生产 | `warn` 仅在 `TENON_TEST_MODE=1` 时允许，否则拒绝启动 |

回滚：`TENON_TICKET_MODE=warn` 只是开发期缓冲；生产回滚靠**回退版本 + 移除 `Tenon-Ticket` 头校验**，不需要迁移数据（链只追加，旧版本读新文件会因未知字段忽略而不崩，需在 T4 加一条兼容测试）。

---

## 11. 不做

- 计划级 dispatched 收归宿主（P1）
- 配额退款 / release（P1）
- 事件记录对账（只对 `kind=action`）
- 模块自签名（链锚定已够）
- 网关代验票（仅当存在不可改模块时另立设计）
