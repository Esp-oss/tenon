# R1 设计：模块动态注册（全 push）+ 声明哈希校验 + 注册事件链

- 日期：2026-10-27
- 状态：待复核
- 关联：[需求 R1](2026-10-27-tenon-acceptance-and-module-registration.md)（动态注册 + 健康检查）、[ADR-0001](../../adr/0001-driver-dsh-vs-go.md) §3 不变量、[威胁模型](2026-10-27-tenon-security-threat-model.md)（注册端点属「新增写入口」，必须鉴权）

---

## 1. 现状取证（为什么要做）

| 事实 | 证据 | 后果 |
|---|---|---|
| 生产注册靠启动环境变量 | `cmd/host/main.go`：`TENON_MODULES=id=url,id=url`，解析失败即退出 | 新模块上线 = 改 env + **重启 host** |
| 动态注册端点仅测试态 | `POST /tenon/admin/modules` 只在 `TENON_TEST_MODE=1` 挂载 | 生产没有注册入口 |
| 注册表纯内存 | `internal/controlplane/registry/registry.go` 无任何落盘 | host 重启即丢 |
| 没有健康判定 | host 从不探测模块；模块的 `/healthz`、`/readyz` 无人消费 | 模块崩溃后 host 仍以为它在场，回合打到死地址 |
| 模块已有共享密钥 | `TENON_HMAC_SECRET_FILE`（事件投递验签） | 注册鉴权可复用，不必新造凭据 |

## 2. 决策（四条，均已与所有者确认）

| # | 决策 | 取舍理由 |
|---|---|---|
| D1 | **全 push**：模块注册 + 心跳，host 不做主动探活 | 与「模块主动申报」的需求一致；host 不需要出网探测；失效判定单点在 TTL |
| D2 | **人工 disable 粘性**：心跳只影响 `alive/lost`，不覆盖人工状态 | 否则 D1 的 B 轮（`SetEnabled(false)` 摘模块）会被心跳自动救回，测试随机变红 |
| D3 | **注册/心跳分离 + 声明哈希**：心跳只带 `declarationHash`，不匹配则要求重申报 | 省流量；又不出现「心跳到了但声明是旧的」而无人察觉。`409`（冲突）而非 `200+reRegister`：「必须重申报」是契约冲突，不是正常回执 |
| D4 | **独立 append-only 注册事件链**，不进业务审计 | `audit-event.schema.json` 根 required 有 15 个字段（`planId`/`stepId`/`action`/`inputHash`/`idempotencyKey`…），模块生命周期与它们无关；硬塞等于造假数据或破坏性改 schema |
| D5 | **无 static 豁免**：所有模块一律注册 + 心跳；`TENON_MODULES` 环境变量**移除** | 开发阶段不需要兼容旧部署（所有者 2026-10-27 明确）；两套注册路径会长期分叉，删掉更干净 |
| D6 | **不做持久化**：注册表只在内存 | D5 之后恢复路径已闭合——host 重启 → 模块心跳拿到 `409` → 重申报 → `alive`，一个心跳周期内完成。落盘只会多一份可能与事件链冲突的真相 |
| D7 | **缺 HMAC 密钥则启动失败**，不是「不挂端点」 | 静默不可用比拒绝启动更危险：运维会以为注册在工作，实际模块永远上不了线 |

## 3. 端点与报文

### 3.1 注册

```
POST /tenon/registry/register
X-Tenon-Signature: t=<unix>,v1=<hex>      # HMAC-SHA256(secret, "<t>." + body)
Content-Type: application/json

{
  "module": { "id": "inventory", "baseURL": "http://127.0.0.1:8081",
              "manifest": { ...ModuleManifest 摘要，与 /.well-known/tenon 同形... } },
  "declarationHash": "sha256:<64 hex>"
}
→ 200 { "accepted": true, "ttlSec": 15 }
→ 401 签名/时间戳问题（见 §5）
→ 409 declarationHash 与 manifest 不符（拒绝半真半假的申报）
```

`declarationHash` = `sha256:` + `sha256(canonicalJSON(manifest))`，复用包内 `sha256Digest`；`canonicalJSON` 复用审计链已有的 JCS 规范化（`internal/controlplane/audit`），**两侧用同一份实现**，否则同一个 manifest 会算出两个哈希。

### 3.2 心跳

```
POST /tenon/registry/heartbeat
X-Tenon-Signature: t=<unix>,v1=<hex>

{ "moduleId": "inventory", "declarationHash": "sha256:<64 hex>" }
→ 200 { "alive": true }
→ 409 { "reRegister": true, "reason": "unknown-module" | "declaration-changed" }
→ 401 签名问题
```

### 3.3 可见性（既有端点语义收紧）

`GET /tenon/modules` 只返回**可见**条目：`enabled == true && state == "alive"`。
**没有 static 条目可以绕过这条**（D5）：不心跳的模块就是不可见，没有例外。

## 4. 状态机

三组正交状态：

| 维度 | 取值 | 谁决定 | 说明 |
|---|---|---|---|
| 存活 | `alive` / `lost` | 心跳 TTL（默认 15s = 3× 5s） | `lost` 不参与可见性 |
| 人工 | `enabled` / `disabled` | 运维（`POST /tenon/admin/modules` 的 SetEnabled） | **粘性**，心跳不能覆盖 |

跃迁与事件（只有跃迁写 §7 的日志，心跳本身不写）：

```
(void) --register--> alive
alive --TTL 过期--> lost
lost  --heartbeat(200)--> alive
alive/lost --SetEnabled(false)--> disabled（粘性）
disabled --SetEnabled(true)--> enabled（若 TTL 已过则仍为 lost，等心跳）
```

## 5. 鉴权与防重放

- 复用 `TENON_HMAC_SECRET_FILE` 的共享密钥；
- 签名覆盖 `"<t>." + rawBody`，与既有 `X-Tenon-Signature` 完全同形；
- 容差 **±300s**（与 `internal/protocol.SignatureTolerance` 一致），超差返回 401 且理由区分 `格式错` / `过期` / `不匹配`；
- 未配置密钥时**host 启动直接失败**（D7）：打印明确原因（缺哪个环境变量、注册端点会因此不可用），退出码非零。静默「不挂端点」会让运维以为注册在工作、实际模块永远上不了线；
- 同一 `moduleId` 的 `baseURL` 变化视为**新声明**（`declarationHash` 变），走重申报路径；
- **注册不改变人工状态**：对一个 `disabled` 的模块注册，只更新声明与存活状态，仍保持 `disabled`（D2 粘性）。

## 6. 持久化：**不做**（D6）

注册表只在内存。恢复路径靠协议本身闭合：

```
host 重启 → 注册表为空
模块下一次心跳（≤5s）→ host 不认识这个 moduleId → 409 {reRegister}
模块收到 409 → 立即 POST /register（带完整声明与哈希）→ alive
```

**代价**：重启后最多有一个心跳周期（≤5s）的「不可见窗口」，期间 agent 看不到这些模块。这比「落盘可能引入第二份真相」更可接受；窗口长度由心跳间隔决定，验收口径里要显式断言它。

## 7. 注册事件日志

`registry-events.jsonl`，append-only，每行：

```json
{ "at": "2026-10-27T00:00:00Z", "event": "registered", "moduleId": "inventory",
  "declarationHash": "sha256:...", "prevHash": "sha256:...", "hash": "sha256:..." }
```

- `hash = "sha256:" + sha256(canonicalJSON(去掉 hash 字段))`，`prevHash` 指向上一条；
- 事件类型：`registered` / `re-registered`（哈希变）/ `lost` / `recovered` / `manual-disabled` / `manual-enabled`；
- 提供 `--verify` 校验（或测试断言）：篡改任一行即链断。

## 8. 模块侧改动

`internal/module` 增加 registry client：

1. 启动时 `register`（带 manifest 与哈希）；
2. 每 5s `heartbeat`；
3. 收到 `409` 立即重申报；
4. **注册失败不阻塞模块启动**——业务照常，只是 host 看不见它（fail-closed 在 host 侧）；
5. 心跳间隔与 TTL 可通过环境变量覆盖（测试用短间隔）。

## 9. 兼容与不变量

| 项 | 处理 |
|---|---|
| D1 测试 | `SetEnabled(false)` 仍粘性——但 **`TENON_MODULES` 移除后脚本要改**：三模块改为自注册（`scripts/d1-e2e.ps1` / `dev-up.ps1` 不再传该 env），B 轮摘除仍用 admin 端点 |
| 业务审计 schema | **一字不改**（决策 D4） |
| `TENON_MODULES` | **移除**（D5）：无 static 豁免、无兼容路径；不心跳的模块即不可见 |
| 失联期间的回合 | 沿用既有 fail-closed（step 失败 → 回合 `failed`），不新增语义 |
| ADR §3 三条不变量 | 不变；注册端点属新增写入口，按 §5 鉴权 |

## 10. 验收口径（可机械验证）

1. 注册后 `GET /tenon/modules` **立即**出现该模块；
2. 停心跳 → **≤15s** 内从 agent 视图消失，`registry-events` 出现 `lost`；
3. 心跳带错哈希 → `409`；模块重申报后恢复 `alive`（`registry-events` 出现 `re-registered`）；
4. **host 重启 → 注册表为空，模块在 ≤1 个心跳周期内收到 409 并重申报 → `alive`**（断言这个窗口存在且不超过 5s，而不是「重启后立刻可见」）；
5. 人工 `disable` 后即使心跳正常也不可见；`enable` 后立即可见；
6. 篡改 `registry-events.jsonl` 任一行 → 链校验失败；
7. 未配置 HMAC 密钥 → **host 启动失败**（退出码非零、错误信息点名缺哪个环境变量）。

## 11. 风险

| 风险 | 处理 |
|---|---|
| 心跳把 host 打爆（模块多、间隔短） | 默认 5s；端点无状态、只做哈希比对与 TTL 刷新，单条 O(1)；后续可加限流 |
| 注册端点成为攻击面 | HMAC + ±300s 容差；缺密钥拒绝启动；注册只增改自己 id 的条目 |
| **重启窗口**：host 重启后 ≤1 个心跳周期内所有模块不可见 | D6 的直接代价，已写进验收口径（断言窗口 ≤5s）；若将来不可接受，再引入落盘或「模块侧感知 host 重启立即重申报」 |
| 时钟漂移导致 TTL 误判 | TTL 用 host 单调时钟（`time.Since`），不依赖模块时间 |

## 12. 非目标

- 不做服务发现（模块地址仍由模块自报 baseURL）；
- 不做多副本/负载均衡（一个 moduleId 一个 baseURL，后注册覆盖先注册）；
- 不做灰度/版本协商（manifest 的 `version` 已有，但本轮不据此路由）；
- 不改 `audit-event.schema.json`。
