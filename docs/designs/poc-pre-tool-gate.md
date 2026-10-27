# POC-1：PreToolUse 闸门的失败语义（DSH 0.2.0-rc.2）

- 日期：2026-10-27
- 问题：闸门脚本**抛错/超时/没起来**时，工具调用会被拦下（fail-closed）还是照常执行（fail-open）？
- 结论：**fail-open**（除显式 `exit 2` / 方言认得的 JSON block 外，一律放行）

## 1. 实验

用 DSH 自带的两个库函数（`parseHookOutput` + `mergeHookOutputs`，桥接实现调用的就是它们）
对 10 种钩子行为求值，看合并决策是否为 `deny`/`ask`（= 拦下）。

复跑：`node scripts/poc-hook-codec.mjs`

| 场景 | 退出码 | 合并决策 | 工具被拦 |
|---|---|---|---|
| 正常退出、无输出 | 0 | `none` | 否 |
| `{"decision":"deny"}` | 0 | `none` | 否（方言不认） |
| 显式阻断 | 2 | `deny` | **是** |
| `{"decision":"block"}` | 0 | `deny` | **是** |
| `hookSpecificOutput.permissionDecision=deny` | 0 | `deny` | **是** |
| `{"continue":false}` | 0 | `none` | 否 |
| 脚本抛异常 | 1 | `none` | 否 |
| 超时被杀 | 124 | `none` | 否 |
| spawn 失败 | undefined | `none` | 否 |

## 2. 对 Tenon 的含义（决定 adapter 形状）

Tenon 的 I7 要求「**先写 `dispatched` 审计，写不进去就拒绝执行**」。既然钩子在故障时放行：

1. **I7 不能由钩子承担**：钩子崩溃 = 静默放行，审计前置就在这里断掉。
2. **I7 由控制平面强制**：adapter 在执行工具前**同步调用** `POST /tenon/audit`；非 200 就不调工具。钩子最多做「提前提示」，且脚本必须把所有失败路径收敛成 `exit 2`（否则等于没写）。
3. **钩子可用于「策略预检」这类可降级能力**（例如把「本动作需要人批」提前告诉你），但不能作为**安全边界**。

> 一句话：**把安全放在钩子里 = 把安全放在一个「出错就当没说过」的组件里。**
