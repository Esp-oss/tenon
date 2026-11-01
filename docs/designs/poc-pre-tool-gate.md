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

## 3. live 会话验证尝试（2026-10-27，未成功；记录卡点以免重复）

**结论先说：没验成，但也没推翻上面的判定。** 这次尝试的产出是「卡在哪」，不是「结论变了」。

做法（全部在 `out/poc/live/`，不进仓库）：

1. `guard.ps1` 读同目录 `mode.txt`：`block` → stderr + `exit 2`；`crash` → 抛异常；`allow` → `exit 0`。单独调用验证过 **exit code = 2**（`powershell -NoProfile -File guard.ps1`）。
2. `hooks.json`：`{"hooks":{"PreToolUse":[{"matcher":"pwsh|bash","hooks":[{"type":"command","command":"powershell -NoProfile -ExecutionPolicy Bypass -File …guard.ps1"}]}]}}`。
3. `patch.yml`：root 级 `insert` 挂 `@deepseek-ai/dsh-hooks-claude-code`，`configPath` 指向上面的 hooks.json。
4. 运行：`dsh headless --patch out/poc/live/patch.yml "用 pwsh 工具把 hello 写入 out/poc/live/side-effect.txt"`（带 `DSH_PERMISSION_MODE=danger-full-access`，否则子会话的 pwsh 会先被沙箱挡掉，那是另一个假阴性——第一次跑就踩到了）。

观察到的现象：

| 检查 | 结果 |
|---|---|
| 桥接是否进了组合配置 | ✅ `dsh --profile headless --patch … --dump-config` 末尾能看到该条目与 `configPath` |
| 闸门脚本本身 | ✅ 单独调用 exit code = 2、stderr 有理由 |
| 工具是否被执行 | ❌ **执行了**（`side-effect.txt` 出现），而 `mode.txt` 是 `block` |
| 会话日志是否有 `hook/invoked` / `hook/result` | ❌ **零条**（全盘搜 `~/.dsh/storages` 也没有任何会话记录过 `hook/invoked`） |
| 启动期 stderr 告警 | ❌ 空（无「插件解析失败」「configPath 读不到」之类的提示） |

**两种可能，尚不能区分**：

- **A：钩子根本没 fire**（桥接挂载了但没注册监听，或 `configPath` 的解析/匹配没生效）。证据倾向 A：会话日志里连 `hook/invoked` 都没有——而按 `events.d.ts`，只要 fire 过就该有这对事件。
- **B：钩子 fire 了但 spawn 失败**（例如钩子命令经 `sh -c` 执行、`powershell` 不在该 shell 的 PATH），`exitCode` 为 `undefined` → 按 §1 的语义 **fail-open 放行**。若如此，这次现象本身就是「故障时放行」的 live 例证。

**下一步（如果补验）**：优先查「桥接到底有没有注册监听」——`dsh plugin --profile headless add @deepseek-ai/dsh-hooks-claude-code`（让 profile 能解析该包）后用 `dsh --dump-config` 对比；或用 `--from-default-profile` 复制一个干净的 `hookpoc` profile 再挂，避免污染现有 profile。**不建议**在没有隔离的情况下改用户的 `headless` profile。

**这层补验的影响面**：只影响「显式 block 是否真能落到工具调用上」这一条；**不改变**「故障时放行」的判定（§1 已由库函数实测确定），因此 ADR-0001 §5 的落点决定（I7 在 Go host 强制）不依赖它。

> 一句话：**把安全放在钩子里 = 把安全放在一个「出错就当没说过」的组件里。**
