# module-host 最小版实施计划（`tenon_action`）

> **For agentic workers:** 本计划由子代理逐任务执行。每任务自带上下文；先读设计 spec。

**Goal:** 在 `packages/dsh-plugin-module-host` 里实现一个 cordis 插件，向 DSH 注册 `tenon_action(module, action, args)`，经 host 闸门执行真实业务，并产出两段审计。

**设计依据：** [`2026-10-27-tenon-module-host-minimal-design.md`](../specs/2026-10-27-tenon-module-host-minimal-design.md)。**先读它**。

**全局约束：**
- **不要 `git push`**（GitHub 不通）；只本地提交。
- 不用第三方依赖（`@tenon/core`、`@deepseek-ai/dsh-tools`、`schemastery` 除外）。
- 每任务结束：`pnpm check` 通过（含 TS 测试）；改到 Go 才需要 `go test ./...`。
- 提交信息 conventional commits，正文中文。

---

## Task 1: 包骨架 + 配置 + 六步编排（纯函数）+ 单测

**Files:**
- Create: `packages/dsh-plugin-module-host/package.json`、`tsconfig.json`
- Create: `src/config.ts`、`src/orchestrate.ts`、`src/bridge.ts`（HTTP 与签名）
- Test: `src/orchestrate.test.ts`

- [ ] **Step 1: 建包**

`package.json`：`{ "name": "@tenon/dsh-plugin-module-host", "private": true, "type": "module", "scripts": { "build": "tsc -p tsconfig.json", "test": "npm run build && node --test \"dist/**/*.test.js\"" }, "dependencies": { "@tenon/core": "workspace:*", "@deepseek-ai/dsh-tools": "workspace:*" } }`

`tsconfig.json` 继承 `../../tsconfig.base.json`，`outDir dist`、`rootDir src`、`types: ["node"]`。

> 若 `@tenon/core` 的 workspace 解析不通，用 `link:` 指向 `../tenon-core`。

- [ ] **Step 2: 写失败用例（先定义行为）**

`src/orchestrate.test.ts` 用 `node:http` 起假宿主，记录 `{method, path, headers, body}` 序列，断言：

1. **auto**：请求序列恰为 `GET /tenon/modules` → `POST /tenon/policies:evaluate` → `POST /tenon/actions/inventory.DeductStock:dry-run` → `POST /tenon/audit`(dispatched) → `POST /tenon/actions/inventory.DeductStock` → `POST /tenon/audit`(result)；返回 `{status:'executed'}`；action 请求带 `Tenon-Version` 与 `Idempotency-Key: tenon:<planId>/s1`。
2. **deny**：policy 回 `{outcome:'deny',decidedBy:['pol-x'],reasons:['NO_POLICY_MATCH_DEFAULT_DENY']}` → 抛错，`code='TENON_POLICY_DENIED'`，且序列里**没有** dry-run/audit/action。
3. **approve**：policy 回 `{outcome:'approve',approvers:['role:warehouse_manager']}` → 返回 `{status:'needs_approval'}`，detail 含「未执行」，序列**停在 policy**。
4. **I7**：dispatched 审计回 500 → 抛错，序列里**没有** action 请求。
5. **dry-run 失败**（回 422）→ 抛错，序列里**没有** dispatched 审计与 action。
6. **模块未注册**：`GET /tenon/modules` 回空数组 → 抛 `MODULE_NOT_FOUND`，不调 policy。

- [ ] **Step 3: 跑用例确认失败**（模块不存在）

- [ ] **Step 4: 实现**

`src/config.ts`：Schemastery 配置 `{ hostURL: string, agentID: string, onBehalfOf: string, timeoutMs?: number }`（`onBehalfOf` 必填，注释写「身份只进审计 actor，不是鉴权」）。

`src/bridge.ts`：`fetchJSON(url, init)` 封装超时与错误归一化（把 host 的 `tenonError` 转成 `TenonCallError{code, class, retryable}`）。

`src/orchestrate.ts`：`runAction({config, module, action, args, fetchImpl?})` 按 spec §4 六步执行；标识生成用 `@tenon/core` 的 `sha256Digest` + 本地 `pln_` 随机；`canonicalJSON` 用 `@tenon/core` 已有实现（若未导出，改用它导出的能力，**不要**复制一份）。

- [ ] **Step 5: 跑用例确认通过** → `pnpm --filter @tenon/dsh-plugin-module-host test`

- [ ] **Step 6: Commit**

---

## Task 2: cordis 插件注册 `tenon_action`

**Files:**
- Create: `src/index.ts`
- Test: `src/index.test.ts`

- [ ] **Step 1: 失败用例**：用假 `ctx.tools.register` 记录 `defineTool` 的产物，断言：`name==='tenon_action'`；`parameters` 只有 `module/action/args` 三个必填项且 `additionalProperties:false`（**不许有** `onBehalfOf`/`planId`）；description 含「needs_approval 表示未执行」。

- [ ] **Step 2: 实现**：`export const name='tenon-module-host'`、`inject=['tools']`、`apply(ctx,config)` 注册工具；`execute` 调 `runAction` 并把异常转成模型可读的失败（含 `code`）。

- [ ] **Step 3: 跑通并 Commit**

---

## Task 3: 端到端验收（真 host + 真 inventory）

**Files:**
- Create: `scripts/verify-module-host.mjs`

- [ ] **Step 1**: 脚本自己起 host（8094）+ inventory（8095），用 `runAction` 真跑三条：
  1. `DeductStock {qty:5}` → 库存 120→115、审计两段齐全；2. `qty:12` → `needs_approval` 且库存不变；3. 未登记动作 → `TENON_POLICY_DENIED`。
- [ ] **Step 2**: 另跑一次「工具层」：直接 import `dist/index.js`，用假 ctx 注册后调 `execute` 两次（qty=5 / qty=12），断言 `status`。
- [ ] **Step 3**: Commit。

---

## Task 4: 门禁与文档

- [ ] **Step 1**: `node scripts/check-profile-capabilities.mjs web` 仍报违规（证明门禁有效，与本包无关）；在 spec 里记「生产 profile 需只挂本插件」。
- [ ] **Step 2**: README 的「DSH 还不是业务驱动」段改为「已有 `tenon_action` 最小工具面（`approve` 仍只报告）」，并把剩余缺口（挂起/审批 UI/Q1/Q2）列清。
- [ ] **Step 3**: `pnpm check` + `go test ./...` 全绿，Commit。

---

## 验收口径（整份计划）

1. `pnpm --filter @tenon/dsh-plugin-module-host test` 全绿（Task 1 六条 + Task 2）；
2. `node scripts/verify-module-host.mjs` 三条端到端全过；
3. `pnpm check` 全绿；
4. 工具的 `parameters` 无身份字段（`additionalProperties:false` 且只有三项）——安全可由单测机械验证。
