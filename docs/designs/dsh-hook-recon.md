# DSH 钩子协议侦察（0.2.0-rc.2）

- 日期：2026-10-27
- 目的：为 ADR-0001 的两个 POC 提供事实基线。**只记录源码与库函数实测，不臆测。**

## 1. 钩子点与桥接

- 钩子点枚举里有 `PreToolUse`（`dsh-hook-protocol/lib/types/events.d.ts`："The hook point (`PreToolUse`, `Stop`, …)"）。
- 桥接实现是 `@deepseek-ai/dsh-hooks-claude-code`，它的 `lib/index.js` **调用** `parseHookOutput` / `mergeHookOutputs`——所以下面的语义就是网关真实使用的语义，不是文档推测。

## 2. 退出码语义（`lib/index.js` 的 `parseHookOutput`）

```js
const BLOCKING_EXIT_CODE = 2;
if (exitCode === BLOCKING_EXIT_CODE) { output.decision = "block"; ... }
if (exitCode === 0) { /* 解析 stdout JSON（仅当以 '{' 开头） */ }
return output;            // 其它退出码：不设任何 decision
```

| 退出码 | 结论字段 |
|---|---|
| `2` | `decision = "block"`，`stderr` 成为 `reason` |
| `0` | 可能带 JSON 决策（见 §3）；纯文本输出不构成决策 |
| **其它（1 / 124 / undefined）** | **不设 decision** ⇒ 合并结果为 `none` ⇒ 视为「钩子没意见」 |

## 3. 什么才能真的拦住（`mergeHookOutputs` 的优先级：`deny > ask > allow`）

实测（`scripts/poc-hook-codec.mjs`，直接调用上述两个库函数）：

| 钩子行为 | 单钩子决策 | 合并决策 | 工具是否被拦 |
|---|---|---|---|
| `exit 0`，无输出 | 无 | `none` | 否（放行） |
| `exit 0` + `{"decision":"allow"}` | 无 | `none` | 否（放行） |
| `exit 0` + `{"decision":"deny"}` | 无 | `none` | **否（放行）** ← 方言不认这个取值 |
| `exit 2` + stderr | `block` | `deny` | **是** |
| `exit 0` + `{"decision":"block","reason":…}` | `block` | `deny` | **是** |
| `exit 0` + `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",…}}` | `deny` | `deny` | **是** |
| `exit 0` + `{"continue":false}` | 无 | `none` | 否（只置 stop，不是权限决策） |
| `exit 1`（脚本抛异常） | 无 | `none` | **否（放行）** |
| `exit 124`（超时被杀） | 无 | `none` | **否（放行）** |
| spawn 失败（`exitCode === undefined`） | 无 | `none` | **否（放行）** |

## 4. 结论

**闸门在故障时是 fail-open 的**：

- 唯一可靠的阻断方式是**显式** `exit 2`（或方言认得的 JSON block/deny）；
- 钩子脚本崩溃、被杀、根本没起来，一律**放行**——「闸门写错了」与「闸门说可以」在工具侧无法区分。

这条直接决定 ADR-0001 §3 不变量 2 的落点：**I7（先写 dispatched 审计、写失败即拒绝执行）不能挂在这种钩子上当作安全边界**，只能由控制平面（Go host 的 `/tenon/audit`）强制；钩子可以留作「尽力而为的提示」，但脚本必须把任何失败路径都收敛成 `exit 2`，否则它静默失效。

## 5. 证据层级（诚实声明）

- ✅ **源码级**：退出码 → decision 的映射、合并优先级（读 `dsh-hook-protocol/lib/index.js`）。
- ✅ **库函数级实测**：上表每行都由 `scripts/poc-hook-codec.mjs` 实跑产出。
- ❌ **未做 live 会话验证**：没有起一个真实 DSH 会话 + 配置真实钩子去验证「桥接把 deny 落到工具调用上」。理由：需要独立实例与项目级钩子配置，成本远高于收益；而桥接调用点已由源码确认。如需补验，见 ADR §5 的「待补」。
