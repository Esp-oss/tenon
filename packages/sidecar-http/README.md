# @tenon/sidecar-http

参考实现#2：让任意语言实现的模块以 **6 个 HTTP 端点**接入 Tenon（§4.10 模块侧）。M2 交付。

## 职责边界

- 负责（模块侧 6 端点，§4.10）：
  - `GET /.well-known/tenon` → manifest（摘要级，§4.1）
  - `GET /tenon/actions/{id}` → 完整 Action（含 `inputSchema`）
  - `POST /tenon/actions/{id}` → 执行（头：`Idempotency-Key`、`X-Tenon-Grant`）
  - `POST /tenon/actions/{id}:dry-run` → 预演（可选；缺失即降级为强制审批，§8）
  - `POST /tenon/events` → 系统 → 模块投递（Webhook，HMAC-SHA256，时钟容差 ±300s）
  - `GET /tenon/events?since=cursor` → 补课 / 重放
- 负责：把语言无关的模块能力描述转成 manifest；维护事件游标；按 `eventId` 去重；`Idempotency-Key` 透传与首次结果返回。
- 不负责：
  - 不实现宿主侧 4 端点（`/tenon/audit`、`grants:evaluate`、`policies:evaluate`、三帧 WS）——那些属控制平面（§4.10）。
  - 不做 UI / DOM 自动化（§1.2 非目标）。
  - 不替模块实现业务逻辑，也不替模块校验前置条件（§3.2 #2）。
  - 不引入 dsh 依赖：接入方的语言与框架都不该被协议层约束。

## 依赖

- 对 dsh：**无依赖**。M2 的出口判据就是「非 TS 模块接入跑通全套用例」（§12），若侧车依赖 dsh 就证明不了这一点。
- 包间依赖：→ `@tenon/core`（HMAC 签名、`sha256:` 哈希、`EntityRef`、错误分类的语义对齐）；不依赖任何 dsh 插件。
- 方向：模块（任意语言）→ 侧车 → 6 端点；侧车只按端点契约与控制平面交互，不调用其内部实现。

## 阶段状态

M0 与 M1（当前）：只有骨架，未实现，也不应被使用——协议的服务端参照此时只有 dsh 内嵌模块一种。

M2 计划实现全部 6 端点、manifest 生成、HMAC-SHA256 验签（±300s）、事件游标与 `eventId` 去重、`Idempotency-Key` 透传，并提供 Python 示例模块跑通 C1–C10 + D1（§12 M2 出口判据）。

M3 计划不新增端点，只补事件信封的 broker 适配器（Kafka / NATS / Redis，§9 兼容性表）与多语言接入模板，使侧车可嵌进已有消息基础设施。
