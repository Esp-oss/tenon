# @tenon/conformance

可执行规范：把 §10 的 C1–C10 与 D1 差异测试做成 runner。规范的可信度来自「第三方跑测即可自证合规」，而不是文档措辞（§1.1 目标 3）。

## 职责边界

- 负责：
  - 用例集 C1–C10 + D1（§10.1），每条用例对应一个可执行断言。
  - 命令 `tenon conformance run`（**计划提供**，见下）与机器可读报告输出（每条用例的通过 / 失败与证据：请求、响应、审计片段）。
  - 通过全部用例后签发 badge `tenon-conformant-0.1`（§10.2；签发流程属 M2）。
- 不负责：
  - 不规定协议：用例只是 §4 / §5 规范条款的投影。按 §2.1 判据，写不出用例的条款就不该进协议。
  - 不做反模式静态扫描本身：C10 的扫描器在 `@tenon/lint`，本包只调用它并汇总结果。
  - 不替代人工验收：一致性通过不等于业务正确，更不等于体验合格。
  - 不做性能压测与安全渗透测试。

## 依赖

- 对 dsh：**无依赖**。任何语言的实现都应能跑这套用例（§10、§12 M2 出口判据），runner 绑死 dsh 就失去意义。
- 包间依赖：→ `@tenon/lint`（C10 扫描器）、→ `@tenon/core`（manifest / Action / Audit 的 schema 校验，C1 与 C4 需要）。
- 方向：conformance → lint / tenon-core；被测目标通过 `--target` 以 HTTP / MCP 交互，不被 import。

## 用例表

| 用例 | 断言 | 里程碑 |
|---|---|---|
| C1 | manifest 合法：符合 schema；每个 action 有 `kind`、`risk`；`kind=write` 有 `idempotency` | M0 |
| C2 | 幂等：同 `Idempotency-Key` 提交两次 → 副作用一次、返回首次结果 | M0 |
| C3 | 未知字段：注入未知字段 → 忽略而非报错 | M0 |
| C4 | 审计可查：写调用后可查，`dispatched` 与 `result` 两段齐备，`prevHash` 链连续 | M0 |
| C5 | fail-closed：策略 / 授权评估端点不可达时，写操作被拒 | M0（桩）/ M1（真引擎） |
| C6 | 事件重放：`since=cursor` 补齐丢失事件，消费侧不重复生效 | M1 |
| C7 | 补偿：多步 Plan 中途失败 → 已成功步骤被补偿，审计标 `partially_failed` | M1 |
| C8 | 可选能力容忍：未实现 dry-run → 用例 pass，但运行时强制审批 | M0 |
| C9 | 错误分类：四类错误码与 `retryable` 标志正确 | M0 |
| C10 | 反模式 lint：§10.2 三条扫描规则，任一命中即失败 | M0 |
| D1 | 差异测试：摘掉 `procurement` 后 Plan 必须改变且不抛错 | M0 |

## 命令形态（计划提供）

本阶段**不发布 `bin`**：`package.json` 里没有 `bin` 字段，因为 `dist/cli.js` 尚不存在，先声明就会骗人。计划提供的形态是：

```
tenon conformance run --target http://localhost:8080 --report out.json
```

- `--target`：被测实现的基地址（模块侧 6 端点 + 宿主侧 4 端点所在的同一 origin）。
- `--report`：机器可读报告输出路径，内容为逐用例结果与证据。
- 退出码：全绿 `0`，任一用例失败非 `0`（便于直接进 CI）。

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

M0（当前）：只有骨架，未实现。M0 计划实现 C1–C5 / C8 / C9 / C10 与 D1 的 runner 及报告输出，达成 M0 出口判据「D1 与 C10 全绿」。

M1 计划实现 C6（事件重放）与 C7（补偿），并把 C5 从桩切到真引擎对真引擎。

M2 计划实现 badge `tenon-conformant-0.1` 的签发，并让非 TS 模块（HTTP 侧车、MCP 服务）跑通全套用例（§12 M2 出口判据）。

M3 计划把 D1 扩展到 skill 版本变更场景，验证「新业务流程只改 skill 即可上线」（§12 M3 出口判据）。
