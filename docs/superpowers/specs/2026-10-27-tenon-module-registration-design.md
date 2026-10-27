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
| D3 | **注册/心跳分离 + 声明哈希**：心跳只带 `declarationHash`，不匹配则要求重申报 | 省流量；又不出现「心跳到了但声明是旧的」而无人察觉 |
| D4 | **独立 append-only 注册事件链**，不进业务审计 | `audit-event.schema.json` 根 required 有 15 个字段（`planId`/`stepId`/`action`/`inputHash`/`idempotencyKey`…），模块生命周期与它们无关；硬塞等于造假数据或破坏性改 schema |

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

`GET /tenon/modules` 只返回**可见**条目：`enabled == true && (source == "static" || state == "alive")`。

## 4. 状态机

三组正交状态：

| 维度 | 取值 | 谁决定 | 说明 |
|---|---|---|---|
| 存活 | `alive` / `lost` | 心跳 TTL（默认 15s = 3× 5s） | `lost` 不参与可见性 |
| 人工 | `enabled` / `disabled` | 运维（`POST /tenon/admin/modules` 的 SetEnabled） | **粘性**，心跳不能覆盖 |
| 来源 | `static` / `dynamic` | `TENON_MODULES` / 注册 | `static` **不参与 TTL**（否则老部署一上线就全掉） |

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
- 未配置密钥时**注册端点不挂载**（fail-closed：宁可没有动态注册，也不要匿名注册）；
- 同一 `moduleId` 的 `baseURL` 变化视为**新声明**（`declarationHash` 变），走重申报路径；
- **注册不改变人工状态**：对一个 `disabled` 的模块注册，只更新声明与存活状态，仍保持 `disabled`（D2 粘性）。

## 6. 持久化

- `registry.json`：**原子写**（临时文件 + `os.Rename`），boot 时加载；
- `dynamic` 条目恢复为 **`lost`**，等首个心跳转 `alive`；
- `static` 条目由 env 重新构建，不写入 `registry.json`（避免两份真源）；
- 写失败**不改内存态**，只记日志（与既有「观测文件可用性不决定业务处置」一致）。

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
| D1 测试 | 不受影响：`SetEnabled(false)` 仍粘性；`static` 条目行为不变 |
| 业务审计 schema | **一字不改**（决策 D4） |
| `TENON_MODULES` | 保留为 bootstrap 与「不实现心跳的模块」的兼容路径，标 `source=static` |
| 失联期间的回合 | 沿用既有 fail-closed（step 失败 → 回合 `failed`），不新增语义 |
| ADR §3 三条不变量 | 不变；注册端点属新增写入口，按 §5 鉴权 |

## 10. 验收口径（可机械验证）

1. 注册后 `GET /tenon/modules` **立即**出现该模块；
2. 停心跳 → **≤15s** 内从 agent 视图消失，`registry-events` 出现 `lost`；
3. 心跳带错哈希 → `409`；模块重申报后恢复 `alive`（`registry-events` 出现 `re-registered`）；
4. host 重启 → `registry.json` 恢复、dynamic 条目为 `lost`，首心跳后 `alive`（期间 agent 视图不含它）；
5. 人工 `disable` 后即使心跳正常也不可见；`enable` 后立即可见；
6. 篡改 `registry-events.jsonl` 任一行 → 链校验失败；
7. 未配置 HMAC 密钥时注册端点不挂载（404/未注册），且 host 启动日志有明确警告。

## 11. 风险

| 风险 | 处理 |
|---|---|
| 心跳把 host 打爆（模块多、间隔短） | 默认 5s；端点无状态、只做哈希比对与 TTL 刷新，单条 O(1)；后续可加限流 |
| 注册端点成为攻击面 | HMAC + ±300s 容差；未配密钥不挂载；注册只增改自己 id 的条目 |
| `registry.json` 与 `registry-events.jsonl` 两份状态 | 前者是当前视图、后者是历史；以事件链为准做校验，冲突时**以事件链回放结果覆盖**并在日志里报冲突 |
| 时钟漂移导致 TTL 误判 | TTL 用 host 单调时钟（`time.Since`），不依赖模块时间 |

## 12. 非目标

- 不做服务发现（模块地址仍由模块自报 baseURL）；
- 不做多副本/负载均衡（一个 moduleId 一个 baseURL，后注册覆盖先注册）；
- 不做灰度/版本协商（manifest 的 `version` 已有，但本轮不据此路由）；
- 不改 `audit-event.schema.json`。
