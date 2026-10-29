# module-host 最小版：DSH 的 `tenon_action` 工具

- 日期：2026-10-27
- 状态：待复核
- 关联：[ADR-0001 §6 第 2 步](../../adr/0001-driver-dsh-vs-go.md)（`module-host`）、[威胁模型](../specs/2026-10-27-tenon-security-threat-model.md)（档位 A）、[R2 验收](2026-10-27-tenon-acceptance-and-module-registration.md)

---

## 1. 目标与边界

**目标**：让 DSH 第一次真正驱动 Tenon 业务——模型的工具调用经 host 闸门落到真实模块，产出两段审计。

**本轮做**：一个工具 `tenon_action(module, action, args)`；`auto` 走完全部闸门并真实执行；`approve` 只**报告待批**（不执行、不挂起）；`deny` 直接拒。

**本轮不做**：挂起/续跑、DSH 审批 UI 接线、`restrict` 收窄脚本、工作区与操作员绑定（Q1/Q2 未决）。

## 2. 现状事实（决定了为什么这么设计）

| 事实 | 证据 | 含义 |
|---|---|---|
| 工具注册 API | `ctx.tools.register(defineTool({name,description,parameters,output,execute}))`（参考 `dsh-tool-todo`） | 一个 cordis 插件即可 |
| 模块 action 端点**不验签** | 验签只在 `POST /tenon/events`（`internal/module/module.go` 的 `deliverEvent`） | 调 action 不需要 HMAC |
| host 闸门端点**无鉴权** | `internal/controlplane/http.go` 无 `requireAuth`/`Bearer` | M0.5 没有 token，身份只进审计 |
| grant 校验未实现 | README「模块侧 `X-Tenon-Grant` 真校验（M0.5 没有 grant 签发）」 | 本轮**不调** grant 端点，也不假装做了 |
| 闸门顺序来自 Go agent | `internal/agent/run.go`：policy → dry-run → 审计 dispatched → action → 审计 result | TS 侧按同序重写，审计字段同形 |

## 3. 包形态

```
packages/dsh-plugin-module-host/
  package.json      # type module, deps: @tenon/core, @deepseek-ai/dsh-tools
  src/index.ts      # cordis 插件：inject=['tools']；apply 注册 tenon_action
  src/orchestrate.ts# 六步编排（纯函数式，便于用假 host 单测）
  src/config.ts     # Schemastery 配置
```

配置：`{ hostURL, agentID, onBehalfOf, timeoutMs? }`
- `agentID` / `onBehalfOf` **只写进审计的 `actor`**，不是鉴权凭据（见 §2）；
- `onBehalfOf` 必填：没有操作员身份的审计等于没有审计（Q2 未决前用配置值，**不从模型参数取**）。

## 4. 六步编排（`orchestrate.ts`）

```
1. 发现：GET /tenon/modules → 找 module.baseURL（命中缓存；未找到 → 抛 MODULE_NOT_FOUND）
2. 策略：POST /tenon/policies:evaluate {action, args, origin:'request'}
      outcome=deny    → 抛 TENON_POLICY_DENIED（带 decidedBy/reasons），不写审计、不执行
      outcome=approve → 返回 {status:'needs_approval', planId, requiredRoles}，不执行
3. 预演：POST {module}/tenon/actions/{id}:dry-run
      非 2xx → 抛（class 取自响应）
4. I7：POST /tenon/audit {stage:'dispatched', ...}
      非 2xx → **立即终止**，绝不执行（审计前置是本项目的铁律）
5. 执行：POST {module}/tenon/actions/{id}（头：Tenon-Version、Idempotency-Key）
6. 收尾：POST /tenon/audit {stage:'result', ...}（无论成功失败都写；失败与否不改业务结局）
```

标识生成：`planId = pln_<base32 随机 16>`、`stepId = s1`、`idempotencyKey = "tenon:<planId>/s1"`、`inputHash = sha256(CanonicalJSON(args))`（复用 `@tenon/core` 的 `sha256Digest` 与校验器）。

## 5. 结果与错误

| 结局 | 返回 |
|---|---|
| 执行成功 | `{ status:'executed', planId, module, action, result }` |
| 需人批 | `{ status:'needs_approval', planId, requiredRoles:[] }`，**detail 里明写「未执行」** |
| 策略拒绝 | 抛 `ToolArgsError`-兼容错误：`code='TENON_POLICY_DENIED'`，附 `decidedBy/reasons` |
| 契约/网络 | `code` 取自 host 的 `tenonError.code`；`retryable` 决定是否建议重试 |

**为什么不把 `needs_approval` 也当错误**：模型需要知道「事情停在哪」，而不是「调用失败」——后者会诱发它换路重试，那是绕过闸门。

## 6. 安全（档位 A）

- 本包只注册 `tenon_action` 一个工具，**不 import** 任何 shell/fs/web 能力；
- 生产 profile 仍须 `node scripts/check-profile-capabilities.mjs <profile>` exit 0；
- 工具参数只有 `module/action/args`：**没有** `onBehalfOf`、`planId`、`userId`，模型无法自称别人或伪造计划号。

## 7. 测试与验收

**单测**（假 host，`node --test`）：
1. auto 全链路：六次请求的顺序与报文形状（含 `Tenon-Version`、`Idempotency-Key`、`inputHash`）；
2. deny：只发到 policy 就抛错，**没有** dry-run/审计/action；
3. approve：返回 `needs_approval`，**没有** dry-run/审计/action；
4. **I7**：审计 dispatched 返回 500 → 不执行 action，返回错误；
5. 模块未注册 → `MODULE_NOT_FOUND`；
6. dry-run 失败 → 不写 dispatched、不执行。

**端到端（真 host + 真 inventory，验收重点）**：
1. `DeductStock {sku:'STD-1001', qty:5}` → 库存真减、审计两段齐全、`approvedBy` 为空；
2. `qty:12` → 返回 `needs_approval`，库存**不变**；
3. 未登记动作 → `TENON_POLICY_DENIED`；
4. 关掉 host → `retryable:true`。

**机械门禁**：`pnpm check`（含 TS 测试）、`gofmt`/`go test`（未改 Go，仅防回归）。

## 8. 风险

| 风险 | 处理 |
|---|---|
| TS 侧重写闸门顺序与 Go 侧漂移 | 编排是纯函数 + 表驱动测试（请求序列断言）；端到端用真 host 复核 |
| `removeAdditional`/校验器差异 | 只对 `args` 做**发送前**的自检（用 `validatePlan`? 不——用模块 manifest 的 `requiredArgs` 提示），但不阻断：真校验在模块侧 |
| 审计写失败导致业务静默 | I7 用例专门锁这条 |
| 模型把 `needs_approval` 当完成 | 返回体里 `detail` 明写「未执行」，并在工具 description 里写死这条 |
