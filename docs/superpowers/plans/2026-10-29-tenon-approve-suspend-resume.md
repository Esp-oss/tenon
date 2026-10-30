# approve 挂起 / 续跑 实施计划

> **For agentic workers:** 本计划由子代理逐任务执行。每任务自带上下文；**先读设计 spec**：[`2026-10-29-tenon-approve-suspend-resume-design.md`](../specs/2026-10-29-tenon-approve-suspend-resume-design.md)（尤其 §3 三条裁决与 §6 编排顺序）。

**Goal:** 让 `approve` 从「只报告」变成「真挂起 + 可续跑」：计划落盘、计划级审计、回合内经 DSH 审批问人、按原幂等键执行、审计带 `approvedBy`。

**全局约束：**
- **不要 `git push`**；只本地提交。
- 不用第三方依赖（`@tenon/core`、`@deepseek-ai/dsh-tools`、`@deepseek-ai/schemastery` 除外）。
- **不改 Go**：控制平面已冻结（ADR §2）。本计划全部落在 `packages/dsh-plugin-module-host`。
- 每任务结束：`pnpm check` 通过；提交信息 conventional commits，正文中文。
- **D2 未裁决前不得开工**（`operatorRoles` 的授权语义见 spec §3.2）。

---

## Task 1: pending 存储（原子写 / 状态机 / TTL）+ 单测

**Files:** Create `src/pending.ts`、`src/pending.test.ts`；改 `src/config.ts`（`stateDir`、`pendingTTLMinutes`）

- [ ] **Step 1 失败用例**：`save` 后 `load` 得到同构对象；`save` 覆盖同 planId 为幂等 upsert；写入过程崩溃不留半个文件（临时文件 + rename 可断言：中途抛错时目标文件不存在或仍是旧内容）；`archive(planId,'done')` 后 `pending/` 无原件、`archived/` 有终态；`load` 过期条目返回 `expired` 而不是抛错。
- [ ] **Step 2 跑红**（模块不存在）。
- [ ] **Step 3 实现**：`save` / `load` / `list` / `archive` / `isExpired`；路径 `path.join(stateDir,'pending')`、`.../pending/archived`；TTL 缺省 15 分钟（与 `internal/agent/suspend.go:18` 一致）。
- [ ] **Step 4 跑绿 + Commit**。

## Task 2: `approve` 分支改为真挂起（计划级 dispatched + I7 三步）

**Files:** 改 `src/orchestrate.ts`；改 `src/orchestrate.test.ts`

- [ ] **Step 1 失败用例**（假 host + 假 approval 记录调用）：
  1. `approve` → 请求序列 = `GET /tenon/modules` → `POST /tenon/policies:evaluate` → `POST /tenon/audit`（**计划级**：`stepId:null`、`action:""`、`inputHash:""`、`decision.outcome:"approve"`、`decision.approvedBy:null`、`result.status:"unknown"`）→ 无 dry-run、无 action 请求；返回 `needs_approval`；
  2. I7：该审计 500 → 归档 `stale`、抛 `AUDIT_WRITE_FAILED`、`pending/` 不留残留、无 action 请求；
  3. `operatorRoles=[]` → **approval 一次都没被调用**（`request` 调用计数为 0）；
  4. `operatorRoles` 与 `requiredRoles` 无交集 → 同上，且 detail 里写明缺什么角色。
- [ ] **Step 2 跑红** → **Step 3 实现** → **Step 4 跑绿 + Commit**。

> 注意：计划级记录的形状**不满足** `audit-event.schema.json` 完整 required 集，是既有刻意取舍（README:265），不要「顺手补全」成 step 形状。

## Task 3: 执行已批准的计划（`resume` 与「当场批准」共用）

**Files:** Create `src/execute-approved.ts`、`src/execute-approved.test.ts`

- [ ] **Step 1 失败用例**：
  1. 正常：预演 → step 级 dispatched（`approvedBy=user:bob`）→ action（幂等键 `tenon:<planId>/s1` 与挂起时**一致**）→ step 级 result → 计划级 result → 归档 `done`；
  2. 过期：归档 `expired`、**零** action 请求、写计划级 result（`policy_denied`）；
  3. policy 重跑改判 `deny` → 归档 `stale`、零 action 请求；
  4. 已归档 `done` 再续 → 返回归档结论、零新请求；
  5. 执行失败（模块 500，retryable）→ step 级 result 写 `error`、归档 `stale`、不假装成功。
- [ ] **Step 2 跑红** → **Step 3 实现**（顺序严格按 spec §6.2）→ **Step 4 跑绿 + Commit**。

## Task 4: 工具面 `tenon_pending` / `tenon_resume` + 配置

**Files:** 改 `src/index.ts`、`src/config.ts`、`src/index.test.ts`

- [ ] **Step 1 失败用例**：注册三个工具；`tenon_pending` 无必填参数、返回里每条含 `canApprove`；`tenon_resume` 只有 `planId` 一个必填、`additionalProperties` 白名单拒 `onBehalfOf`/`approvedBy`/`roles`（在发请求前拒）；`tenon_resume` 在 `allowed-once` 下返回 `executed`、在 `rejected` 下返回 `needs_approval` 且 detail 含拒批原因。
- [ ] **Step 2 跑红** → **Step 3 实现** → **Step 4 跑绿 + Commit**。

## Task 5: 端到端验收（真 host + 真 inventory + 假 answerer）

**Files:** 改 `scripts/verify-module-host.mjs`

- [ ] **Step 1**：新增段落 `e) 挂起`：`qty=12` → `needs_approval`、库存不变、`out/verify-module-host/module-host-state/pending/` 有该 plan、计划级审计恰 1 条（字段形状按 Task 2 的断言）；
- [ ] **Step 2**：新增段落 `f) 续跑`：无资格（`operatorRoles=[]`）续跑被拒且无 action 请求 → 有资格（`operatorRoles:['role:warehouse_manager']` + answerer 返 `allowed-once`）续跑 → 库存 115→103、step 级两段 + 计划级两段齐全、`approvedBy='user:bob'`、重复续跑幂等（库存不再变）；
- [ ] **Step 3**：`node scripts/verify-module-host.mjs` 全过（既有 a–d 段落不得回退）+ Commit。

## Task 6: 文档与门禁

- [ ] **Step 1**：设计 spec 状态从「待裁决」改为「已实现」并补一行实测结果；
- [ ] **Step 2**：README 的「DSH 还不是业务驱动」段落改写为「已有 `tenon_action` + 挂起/续跑三个工具」，列清剩余缺口（公告板不可见、自动唤醒未做、Q2 未定、DSH UI 需真机点批）；
- [ ] **Step 3**：`pnpm check` + `go test ./...` 全绿 + `node scripts/check-profile-capabilities.mjs web` 仍报违规（预期）+ Commit。

---

## 验收口径（整份计划）

1. `pnpm --filter @tenon/dsh-plugin-module-host test` 全绿（Task 1–4）；
2. `node scripts/verify-module-host.mjs` 的 a–f 段全过；
3. `pnpm check` 全绿；
4. 工具面参数里没有身份字段（`operatorRoles`/`approvedBy`/`onBehalfOf` 均不可由模型传入）——单测机械保证；
5. **已归档 `done` 的计划不会被二次执行**——单测 + e2e 双证。
