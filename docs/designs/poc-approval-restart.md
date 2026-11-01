# POC-2：审批跨重启是否存活（DSH 0.2.0-rc.2）

- 日期：2026-10-27
- 问题：一个 pending 的审批，在 DSH 进程重启后还能不能被回答？
- 结论：**不能存活——而且是设计如此，不是缺陷。**

## 1. 证据（源码级）

`@deepseek-ai/dsh-user-approval/lib/index.js` 的 `request(req)`：

```js
async request(req) {
  const session = req.agent.session;
  if (!hasOpenTurn(session)) throw new Error("approval.request() outside an open turn: …");
  const id = ApprovalRequestId(randomUUID());
  session.append("approval/asked", { id, toolName, callId?, reason? });
  const outcome = await this.decide(req, session);   // ← pending 只存在于这个 await 里
  session.append("approval/decided", { id, outcome });
  return outcome;
}
```

三处关键语义（都来自源码注释与实现）：

1. **必须有开着的回合**：`hasOpenTurn` 不满足直接抛错——「回合外裸调用」被明确拒绝；
2. **pending 是进程内的一个 promise**：没有 pending 表、没有持久化查询接口；
3. **重启后孤立的 `approval/asked` 被当作垃圾**：注释原文「a bare event between turns is **crash-tail garbage on reload**」——重载时不会重建这个请求。

另外：answerer 缺失或抛错 → `'unavailable'`（**fail closed**，即拒绝）；`allowed-once` 是唯一放行值。

## 2. 对 Tenon 的含义

1. **审批真相必须在 Tenon 公告板**（durable），DSH 的 `ctx.get('approval')` 只是一次性入口。这正好是 ADR-0001 §3 不变量 3 的说法，现在有源码依据。
2. **重启后的「续批」必须由 adapter 主动重建**：从公告板读出 `awaiting_approval` 的条目（幂等重推已经具备），在新回合里重新发起一次 `ask`——而不是指望 DSH 记住上次那个问题。
3. **不要在回合外调 `request()`**：会直接抛错；续批只能在「用户下次触发的回合」里做，或由 adapter 在回合内挂载。

## 3. 证据层级

- ✅ **源码级**：`request()` 的实现与注释（含 crash-tail garbage 的措辞）。
- ❌ **未做 live 重启实验**：需要独立 DSH 实例 + 配置一个需要审批的工具 + 杀进程重启。源码已经把「pending 是否持久化」这个问题回答死了（没有存储层），再做实验只能重复验证内存语义。
