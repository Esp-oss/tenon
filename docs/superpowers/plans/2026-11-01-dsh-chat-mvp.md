# 只在 dsh 对话里完成业务（P0+P1）Implementation Plan · v2

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用户只在 dsh 对话里完成查、写、批/驳、审计；异步的待批与执行结果由一个 dsh 通知插件**按 planId 订阅、按忙闲分流**推回**原来那个会话**，且不打断正在进行的对话、不冒充人类发言。

**Architecture:** 新增 `packages/dsh-plugin-tenon-notifier`：一个不注册模型工具的 cordis 插件，`inject: ['agents']`，轮询 host 的 `/tenon/intents/poll` 与 `/tenon/pending`，通过 planId 订阅表定位会话，按 `agent.status` 选 `followup`（空闲）或 `inject`（忙），并受唤醒预算约束。写/批/审计仍由现有 `tenon-mcp` + Go agent + host 完成，本计划不改执行权与裁决层。

**Tech Stack:** TypeScript（Node 22，ESM）、`node:test`、cordis 插件、原生 `fetch`。不新增运行时依赖（消息形状按 `@deepseek-ai/dsh-llm` 的 `UserMessage` 手抄，不 import 整个运行时）。

**Scope:** 本计划只覆盖 spec v1.1 的 **P0（通知通道）+ P1（对话闭环 MVP）**。P1.5（确认令牌）、P2 amend、P3 补偿、P4（事件 Fast Path + Watchdog）是独立计划。**Task 0 是本计划的前置 spike**：先用一次性插件验证 dsh 插件加载、`followup` 和插件工具里的原生审批；Task 0 不通过，后面的 notifier 与 P1.5 都不要开工。

**相对 v1 计划的变化（对应 spec v1.1）:**

| # | 变化 | 落在哪个任务 |
|---|---|---|
| 1 | 通知来源从 `{kind:'user'}` 改为 `{kind:'plugin', form:'notice'}` | Task 4、Task 5 |
| 2 | 新增投递适配器：忙 `inject` / 闲 `followup` + 唤醒预算 + 人类输入回填 | Task 5 |
| 3 | 会话选择改为 planId 订阅表（持久化，支持 migrate） | Task 6 |
| 4 | 轮询器改写：只 ack 已投递的连续前缀、不折叠帧、ack 重试、单 tick 互斥 | Task 7 |
| 5 | 文案与 `pending_list` 按 Inline & Spill 收窄 | Task 4（`pending_list` 在后续计划） |
| 6 | 新增 8 条针对"会静默丢消息"的边界测试 | Task 7 |

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `packages/dsh-plugin-tenon-notifier/package.json` | 包定义、build/test 脚本；无运行时依赖 |
| `packages/dsh-plugin-tenon-notifier/tsconfig.json` | 复用根 `tsconfig.base.json`，输出 `dist` |
| `.../src/types.ts` | 传输/通知/消息的窄接口（`NoticeMessage` 的来源是 plugin） |
| `.../src/config.ts` | 配置校验与默认值（`delivery`/`wakeBudget`/绝对路径 `stateFile`） |
| `.../src/host.ts` | host 登录、intents poll/ack（含 `dropped`）、pending 列表 |
| `.../src/format.ts` | 帧 → 人话（Inline & Spill）、批量文案、notice 消息 |
| `.../src/delivery.ts` | **投递规则**：来源、忙/闲分流、唤醒预算、人类输入回填 |
| `.../src/subscription.ts` | **订阅表**：planId → sessionId（有界、可持久化、可迁移） |
| `.../src/notifier.ts` | 轮询、去重、节流、游标前缀推进、ack 重试、单 tick 互斥 |
| `.../src/index.ts` | cordis 插件入口：`name`/`inject`/`apply`（配置坏不拖垮 dsh） |
| `.../src/*.test.ts` | 单测（node:test） |
| `scripts/install-dsh-notifier.ps1` | 建 profile junction（安全删除）、打印 YAML 片段、可导入性自检 |
| `scripts/notifier-smoke.ps1` | 往 host 投一条 intent，用于 dsh 里的肉眼冒烟 |
| `docs/dsh-chat-only.md` | 安装、配置、验收清单 |

根 `package.json` 的 `typecheck` 与 `test` 要加上这个包。

---

## Task 0: DSH 插件加载 + followup + 原生审批 spike（前置，不提交）

**目的**：在写完整 notifier 之前，先用一个一次性插件证明三件事——

1. dsh web 能加载本地 cordis 插件；
2. 插件能拿到 `ctx.agents.roots()`，并用 `agent.followup()` 把消息送进当前会话；
3. 插件注册的工具里能调 `ctx.approval.request()`，拿到 `allowed-once` / `rejected`。

第 3 条是 P1.5 确认令牌的地基：如果原生审批在插件工具里出不来，token 方案要换，不能继续写 P1.5。

**Files（临时，验证后删除，不提交）:**
- Create: `out/dsh-plugin-tenon-spike/package.json`
- Create: `out/dsh-plugin-tenon-spike/index.js`

- [ ] **Step 1: 写 spike 包**

`out/dsh-plugin-tenon-spike/package.json`：

```json
{
  "name": "@tenon/dsh-plugin-tenon-spike",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "main": "index.js"
}
```

`out/dsh-plugin-tenon-spike/index.js`：

```js
import { randomUUID } from 'node:crypto';
import { defineTool } from '@deepseek-ai/dsh-tools';

export const name = 'tenon-spike';
export const inject = ['agents', 'tools'];

export function apply(ctx) {
  const roots = ctx.agents.roots();
  console.warn(`[tenon-spike] applied, roots=${roots.length}`);
  const agent = roots[0];
  if (!agent) {
    console.warn('[tenon-spike] no root agent; followup 未验证');
  } else {
    agent.followup({
      id: randomUUID(),
      role: 'user',
      content: [{ type: 'text', text: '【Tenon spike】插件加载成功；followup 可用' }],
      source: { kind: 'user' },
    });
  }

  ctx.tools.register(
    defineTool({
      name: 'tenon_spike_confirm',
      description: 'spike：弹一次 DSH 原生审批，返回结局。不执行任何业务。',
      parameters: {},
      output: {
        schema: {
          type: 'object',
          properties: { outcome: { type: 'string' } },
          required: ['outcome'],
        },
        render: (_args, value) => [{ type: 'text', text: String(value.outcome) }],
      },
      async execute(_args, exec) {
        if (!ctx.approval || typeof ctx.approval.request !== 'function') {
          return { outcome: 'unavailable:no-approval-service' };
        }
        if (!exec?.agent) return { outcome: 'unavailable:no-agent' };
        const outcome = await ctx.approval.request({
          agent: exec.agent,
          toolName: 'tenon_spike_confirm',
          reason: 'spike：这是原生审批测试；允许一次即可，不会执行任何业务。',
        });
        return { outcome };
      },
    }),
  );
}
```

- [ ] **Step 2: 装进 profile 并重启**

```powershell
$spike = Join-Path (Get-Location) 'out\dsh-plugin-tenon-spike'
$profile = Join-Path $HOME '.dsh\profiles\web'
$scope = Join-Path $profile 'node_modules\@tenon'
New-Item -ItemType Directory -Force -Path $scope | Out-Null
$link = Join-Path $scope 'dsh-plugin-tenon-spike'
if (Test-Path $link) { Remove-Item $link -Force -Recurse }
New-Item -ItemType Junction -Path $link -Target $spike | Out-Null
```

在 `~/.dsh/profiles/web/cordis.patch.yml` 顶层数组加：

```yaml
- insert:
    - id: tenon-spike
      name: '@tenon/dsh-plugin-tenon-spike'
      config: {}
```

重启 dsh web（`pwsh -File scripts/dsh-up.ps1`）。

- [ ] **Step 3: 验三条**

1. 启动日志出现 `[tenon-spike] applied, roots=1`；
2. 当前对话出现 `【Tenon spike】插件加载成功；followup 可用`；
3. 在对话里让模型调用 `tenon_spike_confirm`，dsh 弹出原生审批；点允许后工具返回 `allowed-once`，点拒绝后返回 `rejected`。

Expected: 三条都成立。若第 2 条失败，先修插件加载/会话句柄；若第 3 条失败，P1.5 的确认令牌必须改用 `ctx.userQuestions.ask` 或别的人工确认面，**不要继续写 token 代码**。

- [ ] **Step 4: 清理 spike**

从 `cordis.patch.yml` 删掉 `tenon-spike` 条目，删除 junction，重启 dsh。`out/dsh-plugin-tenon-spike` 留在本地作证据，不入库。

---

## Task 1: 包脚手架与工作区脚本

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/package.json`
- Create: `packages/dsh-plugin-tenon-notifier/tsconfig.json`
- Create: `packages/dsh-plugin-tenon-notifier/src/types.ts`
- Create: `packages/dsh-plugin-tenon-notifier/src/index.ts`（编译用入口桩，Task 8 替换）
- Modify: `package.json`（根；`typecheck`、`test`）

- [ ] **Step 1: 写包定义**（与 `dsh-plugin-module-host` 保持同一套脚本口径）

```json
{
  "name": "@tenon/dsh-plugin-tenon-notifier",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "description": "把 Tenon 的待批/执行结果推进 dsh 对话；不注册模型工具，不执行任何业务",
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "exports": {
    ".": {
      "types": "./dist/index.d.ts",
      "default": "./dist/index.js"
    },
    "./package.json": "./package.json"
  },
  "files": ["dist"],
  "scripts": {
    "build": "tsc -p tsconfig.json",
    "test": "npm run build && node --test \"dist/**/*.test.js\""
  },
  "engines": {
    "node": ">=22"
  }
}
```

- [ ] **Step 2: 写 tsconfig**

```json
{
  "extends": "../../tsconfig.base.json",
  "compilerOptions": {
    "outDir": "dist",
    "rootDir": "src",
    "types": ["node"]
  },
  "include": ["src"]
}
```

- [ ] **Step 3: 写窄接口**

`src/types.ts`：

```ts
/** Host 的 intent 帧里本插件用到的字段。 */
export interface IntentFrame {
  intentId?: string;
  planId?: string;
  requestId?: string;
  outcome?: string;
  message?: string;
}

/** Host 公告板的一行（只声明本插件用到的字段）。 */
export interface PendingItem {
  planId: string;
  state?: string;
  summary?: string;
  requiredRoles?: string[];
  /** 宿主按 token 角色算出的「当前账号是不是能干批这张卡的人」。 */
  mine?: boolean;
}

export type NotificationKind = 'pending' | 'terminal' | 'intent';

/** 一条待推送的通知；id 用于跨来源去重，seq 只有 intent 帧才有。 */
export interface Notification {
  id: string;
  kind: NotificationKind;
  planId?: string;
  seq?: number;
  text: string;
}

/**
 * 通知消息的来源。
 *
 * `{kind:'user'}` 在 dsh 里是**宿主对「这句话是人说的」的证明**，不是文案标签：
 * 它会让该回合具备「包含人类直接输入」的资格，并让 dsh 的唤醒预算规则把它当成
 * 人类输入而回填预算（预算因此永远不会耗尽，防自激失效）。
 *
 * 所以通知必须标 `plugin`。`form:'notice'` + `summary` 让客户端把它渲染成一条
 * 可折叠的通知行，而不是用户气泡。
 */
export interface NoticeSource {
  kind: 'plugin';
  plugin: string;
  form: 'notice';
  summary: string;
}

/** 与 dsh-llm `UserMessage` 同形的最小子集。 */
export interface NoticeMessage {
  id: string;
  role: 'user';
  content: Array<{ type: 'text'; text: string }>;
  source: NoticeSource;
}

/** 会话里 agent 的最小结构：本插件只用到 id/status/followup/inject。 */
export interface WakeAgent {
  readonly id: string;
  /** dsh 的生命周期状态；不可得时为 undefined（按 idle 处理，见 delivery.ts）。 */
  readonly status?: string;
  followup(message: NoticeMessage): void;
  /** 进 next-step 收件箱、不唤醒；老版本 dsh 可能没有。 */
  inject?(message: NoticeMessage): void;
}

/** dsh 的 agent 注册表（`ctx.agents`）最小接口。 */
export interface AgentRegistry {
  get(id: string): WakeAgent | undefined;
  roots(): WakeAgent[];
}
```

- [ ] **Step 4: 写编译用入口桩（让包能先编译）**

`src/index.ts`：

```ts
export const name = 'tenon-notifier';
export const inject = ['agents'] as const;

export function apply(_ctx: unknown, _config: unknown): void {
  throw new Error('notifier 尚未实现（Task 8 接管）');
}
```

- [ ] **Step 5: 根脚本加上新包**

把根 `package.json` 的这两行改成：

```json
"typecheck": "pnpm --filter @tenon/core run build && tsc -p packages/dsh-plugin-module-host/tsconfig.json --noEmit && tsc -p packages/dsh-plugin-tenon-notifier/tsconfig.json --noEmit",
"test": "pnpm --filter @tenon/core run test && pnpm --filter @tenon/dsh-plugin-module-host run test && pnpm --filter @tenon/dsh-plugin-tenon-notifier run test",
```

- [ ] **Step 6: 安装工作区并编译**

Run:
```bash
pnpm install
pnpm --filter @tenon/dsh-plugin-tenon-notifier run build
```

Expected: `pnpm install` 成功；`tsc` 退出 0，生成 `dist/index.js`。

> 这一步**不要**跑根 `pnpm run test`：此时包里还没有测试文件，`node --test` 匹配不到文件会非零退出。Task 2 加进第一条测试后才有意义。

- [ ] **Step 7: 提交**

```bash
git add package.json pnpm-lock.yaml packages/dsh-plugin-tenon-notifier
git commit -m "feat(notifier): 脚手架、窄接口与工作区脚本"
```

---

## Task 2: 配置校验（含投递开关与绝对路径状态文件）

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/config.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/config.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/config.test.ts`：

```ts
import assert from 'node:assert/strict';
import { isAbsolute } from 'node:path';
import test from 'node:test';
import {
  DEFAULT_BATCH,
  DEFAULT_DELIVERY,
  DEFAULT_INTERVAL_MS,
  DEFAULT_MIN_GAP_MS,
  DEFAULT_WAKE_BUDGET,
  normalizeConfig,
} from './config.js';

const HOME = 'C:\\Users\\tester';

test('token 配置：默认值补全，stateFile 落在 home 下且是绝对路径', () => {
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080/', token: 'tok' }, HOME);
  assert.equal(config.hostURL, 'http://127.0.0.1:8080');
  assert.equal(config.intervalMs, DEFAULT_INTERVAL_MS);
  assert.equal(config.batch, DEFAULT_BATCH);
  assert.equal(config.minGapMs, DEFAULT_MIN_GAP_MS);
  assert.equal(config.delivery, DEFAULT_DELIVERY);
  assert.equal(config.wakeBudget, DEFAULT_WAKE_BUDGET);
  assert.equal(config.pendingScan, true);
  assert.ok(isAbsolute(config.stateFile), 'stateFile 必须是绝对路径');
  assert.match(config.stateFile, /\.dsh[\\/]tenon-notifier[\\/]state\.json$/);
});

test('账号配置：user+password 也可；缺一组就拒绝', () => {
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080', user: 'alice', password: 'alice-dev' }, HOME);
  assert.equal(config.user, 'alice');
  assert.throws(() => normalizeConfig({ hostURL: 'http://127.0.0.1:8080', user: 'alice' }, HOME), /token|user/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://127.0.0.1:8080' }, HOME), /token|user/);
});

test('相对 stateFile 会被解析成绝对路径（dsh 的 CWD 不可预测）', () => {
  const config = normalizeConfig({ hostURL: 'http://x', token: 't', stateFile: 'out/x.json' }, HOME);
  assert.ok(isAbsolute(config.stateFile), `应绝对化，得到 ${config.stateFile}`);
});

test('坏配置：URL 协议、数字下限、布尔、枚举都拒绝', () => {
  assert.throws(() => normalizeConfig({ hostURL: 'ftp://x', token: 't' }, HOME), /http/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', intervalMs: 100 }, HOME), /intervalMs/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', batch: 0 }, HOME), /batch/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', pendingScan: 'yes' }, HOME), /pendingScan/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', delivery: 'loud' }, HOME), /delivery/);
});

test('wakeBudget 必须是非负整数（0 = 只注入不唤醒）', () => {
  assert.equal(normalizeConfig({ hostURL: 'http://x', token: 't', wakeBudget: 0 }, HOME).wakeBudget, 0);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', wakeBudget: -1 }, HOME), /wakeBudget/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', wakeBudget: 1.5 }, HOME), /wakeBudget/);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```

Expected: FAIL，`Cannot find module './config.js'`。

- [ ] **Step 3: 实现配置**

`src/config.ts`：

```ts
import { homedir } from 'node:os';
import { join, resolve } from 'node:path';

export type DeliveryMode = 'wakeup' | 'quiet';

export interface NotifierConfig {
  hostURL: string;
  token?: string;
  user?: string;
  password?: string;
  sessionId?: string;
  intervalMs: number;
  batch: number;
  minGapMs: number;
  timeoutMs: number;
  stateFile: string;
  pendingScan: boolean;
  /** wakeup：空闲时开一个通知回合；quiet：只注入（不对 transcript 产生新回合）。 */
  delivery: DeliveryMode;
  /** 同一会话连续自行唤醒的上限；0 表示只注入不唤醒。 */
  wakeBudget: number;
}

export const DEFAULT_INTERVAL_MS = 2000;
export const DEFAULT_BATCH = 5;
export const DEFAULT_MIN_GAP_MS = 2000;
export const DEFAULT_TIMEOUT_MS = 10_000;
export const DEFAULT_DELIVERY: DeliveryMode = 'wakeup';
export const DEFAULT_WAKE_BUDGET = 3;

/** 状态文件的缺省位置：**绝对路径**，因为 dsh 进程的 CWD 取决于启动方式。 */
export function defaultStateFile(home: string): string {
  return join(home, '.dsh', 'tenon-notifier', 'state.json');
}

function optionalString(raw: Record<string, unknown>, key: string): string | undefined {
  const value = raw[key];
  if (value === undefined || value === null) return undefined;
  if (typeof value !== 'string') throw new Error(`notifier 配置 ${key} 必须是字符串`);
  const trimmed = value.trim();
  return trimmed.length === 0 ? undefined : trimmed;
}

function numberField(
  raw: Record<string, unknown>,
  key: string,
  fallback: number,
  min: number,
  max = Number.MAX_SAFE_INTEGER,
): number {
  const value = raw[key];
  if (value === undefined || value === null) return fallback;
  if (typeof value !== 'number' || !Number.isFinite(value) || value < min || value > max) {
    throw new Error(`notifier 配置 ${key} 必须是 ${min}..${max} 之间的数字`);
  }
  return value;
}

function integerField(
  raw: Record<string, unknown>,
  key: string,
  fallback: number,
  min: number,
  max = Number.MAX_SAFE_INTEGER,
): number {
  const value = numberField(raw, key, fallback, min, max);
  if (!Number.isSafeInteger(value)) throw new Error(`notifier 配置 ${key} 必须是整数`);
  return value;
}

/**
 * 校验并补全配置；坏值直接抛错。
 *
 * @param raw 插件 config 段。
 * @param home 用于默认 stateFile；测试注入，生产用 homedir()。
 */
export function normalizeConfig(raw: unknown, home: string = homedir()): NotifierConfig {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error('notifier 配置必须是对象');
  }
  const record = raw as Record<string, unknown>;

  const rawHost = optionalString(record, 'hostURL');
  if (!rawHost) throw new Error('notifier 配置缺少 hostURL');
  let parsed: URL;
  try {
    parsed = new URL(rawHost);
  } catch {
    throw new Error(`notifier 配置 hostURL 不是合法 URL：${rawHost}`);
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error(`notifier 配置 hostURL 只支持 http/https：${rawHost}`);
  }

  const token = optionalString(record, 'token');
  const user = optionalString(record, 'user');
  const password = optionalString(record, 'password');
  if (!token && (!user || !password)) {
    throw new Error('notifier 配置需要 token，或 user + password');
  }

  const pendingRaw = record['pendingScan'];
  if (pendingRaw !== undefined && typeof pendingRaw !== 'boolean') {
    throw new Error('notifier 配置 pendingScan 必须是布尔值');
  }

  const deliveryRaw = record['delivery'];
  if (deliveryRaw !== undefined && deliveryRaw !== 'wakeup' && deliveryRaw !== 'quiet') {
    throw new Error('notifier 配置 delivery 只能是 wakeup 或 quiet');
  }

  const stateRaw = optionalString(record, 'stateFile');
  const stateFile = stateRaw === undefined ? defaultStateFile(home) : resolve(stateRaw);

  return {
    hostURL: rawHost.replace(/\/+$/, ''),
    ...(token === undefined ? {} : { token }),
    ...(user === undefined ? {} : { user }),
    ...(password === undefined ? {} : { password }),
    sessionId: optionalString(record, 'sessionId'),
    intervalMs: numberField(record, 'intervalMs', DEFAULT_INTERVAL_MS, 500),
    batch: integerField(record, 'batch', DEFAULT_BATCH, 1, 50),
    minGapMs: numberField(record, 'minGapMs', DEFAULT_MIN_GAP_MS, 0),
    timeoutMs: numberField(record, 'timeoutMs', DEFAULT_TIMEOUT_MS, 500),
    stateFile,
    pendingScan: pendingRaw === undefined ? true : pendingRaw,
    delivery: deliveryRaw === undefined ? DEFAULT_DELIVERY : deliveryRaw,
    wakeBudget: integerField(record, 'wakeBudget', DEFAULT_WAKE_BUDGET, 0),
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```

Expected: `# pass 5`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/config.ts packages/dsh-plugin-tenon-notifier/src/config.test.ts
git commit -m "feat(notifier): 配置校验、投递开关与绝对路径状态文件"
```

---

## Task 3: Host 客户端（登录、poll 含 dropped、ack、pending）

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/host.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/host.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/host.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizeConfig } from './config.js';
import { createHostClient, type FetchLike } from './host.js';

interface Init {
  method: string;
  headers: Record<string, string>;
  body?: string;
}

function fakeFetch(handler: (url: string, init: Init) => { status: number; body: unknown }): {
  fetch: FetchLike;
  calls: Array<{ url: string; init: Init }>;
} {
  const calls: Array<{ url: string; init: Init }> = [];
  const fetch: FetchLike = async (url, init) => {
    calls.push({ url, init });
    const reply = handler(url, init);
    return { status: reply.status, text: async () => JSON.stringify(reply.body) };
  };
  return { fetch, calls };
}

const HOME = 'C:\\Users\\tester';

test('账号登录只发生一次；poll/ack/pending 带 Bearer 与 Tenon-Version；dropped 被解析', async () => {
  const { fetch, calls } = fakeFetch((url) => {
    if (url.endsWith('/tenon/session')) return { status: 200, body: { token: 'tok-agent', exp: 4_000_000_000 } };
    if (url.includes('/tenon/intents/poll')) {
      return {
        status: 200,
        body: { frames: [{ seq: 7, frame: { intentId: 'int_1', planId: 'pln_1', outcome: 'completed', message: 'ok' } }], nextCursor: 7, dropped: 3 },
      };
    }
    if (url.endsWith('/tenon/intents/ack')) return { status: 200, body: { ok: true } };
    if (url.endsWith('/tenon/pending')) {
      return { status: 200, body: { items: [{ planId: 'pln_1', state: 'awaiting_approval', summary: '扣 12 件', mine: true }] } };
    }
    return { status: 404, body: { tenonError: { message: 'no route' } } };
  });
  const client = createHostClient(
    normalizeConfig({ hostURL: 'http://127.0.0.1:8080', user: 'alice', password: 'alice-dev' }, HOME),
    fetch,
  );

  const polled = await client.pollIntents(0);
  assert.equal(polled.nextCursor, 7);
  assert.equal(polled.dropped, 3, '宿主队列丢帧数必须透出，不能静默');
  assert.equal(polled.frames[0]?.frame.planId, 'pln_1');
  await client.ackIntents(7);
  const pending = await client.listPending();
  assert.equal(pending[0]?.planId, 'pln_1');
  assert.equal(pending[0]?.mine, true);

  const logins = calls.filter((call) => call.url.endsWith('/tenon/session'));
  assert.equal(logins.length, 1, '同一实例只登录一次');
  const pollCall = calls.find((call) => call.url.includes('/tenon/intents/poll'));
  assert.equal(pollCall?.init.headers['authorization'], 'Bearer tok-agent');
  assert.equal(pollCall?.init.headers['tenon-version'], '0.1');
});

test('静态 token：不登录；HTTP 非 2xx 抛错并带响应片段', async () => {
  const { fetch, calls } = fakeFetch(() => ({ status: 403, body: { tenonError: { code: 'DENIED', message: 'no' } } }));
  const client = createHostClient(normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 'tok-static' }, HOME), fetch);
  await assert.rejects(() => client.pollIntents(0), /403/);
  assert.equal(calls.some((call) => call.url.endsWith('/tenon/session')), false);
});

test('脏条目被丢弃而不是整批失败', async () => {
  const { fetch } = fakeFetch((url) => {
    if (url.endsWith('/tenon/pending')) {
      return { status: 200, body: { items: [{ planId: '' }, { planId: 'pln_ok', state: 'awaiting_approval' }, null, 'junk'] } };
    }
    return { status: 200, body: { frames: [{ seq: 'x' }, { frame: null }, { seq: 2, frame: { planId: 'pln_1' } }], nextCursor: 2 } };
  });
  const client = createHostClient(normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 't' }, HOME), fetch);
  const polled = await client.pollIntents(0);
  assert.equal(polled.frames.length, 1);
  assert.equal(polled.frames[0]?.seq, 2);
  const pending = await client.listPending();
  assert.equal(pending.length, 1);
  assert.equal(pending[0]?.planId, 'pln_ok');
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```

Expected: FAIL，`createHostClient` 不存在。

- [ ] **Step 3: 实现 host 客户端**

`src/host.ts`：

```ts
import type { NotifierConfig } from './config.js';
import type { IntentFrame, PendingItem } from './types.js';

/** 一次传输的最小形状：测试可注入，不依赖 DOM/undici 类型。 */
export interface FetchReply {
  status: number;
  text(): Promise<string>;
}

export interface FetchInit {
  method: string;
  headers: Record<string, string>;
  body?: string;
  signal?: AbortSignal;
}

export type FetchLike = (url: string, init: FetchInit) => Promise<FetchReply>;

export interface IntentEntry {
  seq: number;
  frame: IntentFrame;
}

export interface PollResult {
  frames: IntentEntry[];
  nextCursor: number;
  /** 宿主队列因容量上限丢掉的累计帧数；变多说明通知已经丢过。 */
  dropped: number;
}

export interface HostClient {
  pollIntents(after: number): Promise<PollResult>;
  ackIntents(upto: number): Promise<void>;
  listPending(): Promise<PendingItem[]>;
}

interface Reply {
  status: number;
  text: string;
}

const defaultFetch: FetchLike = (url, init) => fetch(url, init);

export function createHostClient(config: NotifierConfig, fetchImpl: FetchLike = defaultFetch): HostClient {
  let cached: { token: string; expiresAt: number } | null = null;

  async function token(): Promise<string> {
    if (config.token) return config.token;
    if (cached && cached.expiresAt - 60_000 > Date.now()) return cached.token;

    const response = await fetchImpl(`${config.hostURL}/tenon/session`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', accept: 'application/json' },
      body: JSON.stringify({ user: config.user, password: config.password }),
      signal: AbortSignal.timeout(config.timeoutMs),
    });
    const text = await response.text();
    if (response.status !== 200) {
      throw new Error(`notifier 登录失败（HTTP ${response.status}）：${text.slice(0, 300)}`);
    }
    const body = JSON.parse(text) as { token?: unknown; exp?: unknown };
    if (typeof body.token !== 'string' || body.token.length === 0) {
      throw new Error(`notifier 登录响应里没有 token：${text.slice(0, 300)}`);
    }
    const expiresAt = typeof body.exp === 'number' ? body.exp * 1000 : Date.now() + 5 * 60_000;
    cached = { token: body.token, expiresAt };
    return body.token;
  }

  async function request(method: string, path: string, body?: unknown): Promise<Reply> {
    const headers: Record<string, string> = {
      accept: 'application/json',
      'tenon-version': '0.1',
      authorization: `Bearer ${await token()}`,
    };
    if (body !== undefined) headers['content-type'] = 'application/json';
    const response = await fetchImpl(`${config.hostURL}${path}`, {
      method,
      headers,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      signal: AbortSignal.timeout(config.timeoutMs),
    });
    return { status: response.status, text: await response.text() };
  }

  function expectOK(reply: Reply, what: string): void {
    if (reply.status < 200 || reply.status > 299) {
      throw new Error(`${what} 返回 HTTP ${reply.status}：${reply.text.slice(0, 300)}`);
    }
  }

  return {
    async pollIntents(after: number): Promise<PollResult> {
      const reply = await request('GET', `/tenon/intents/poll?after=${after}`);
      expectOK(reply, 'GET /tenon/intents/poll');
      const body = JSON.parse(reply.text) as {
        frames?: Array<{ seq?: unknown; frame?: unknown }>;
        nextCursor?: unknown;
        dropped?: unknown;
      };
      const frames: IntentEntry[] = [];
      for (const entry of body.frames ?? []) {
        if (typeof entry?.seq !== 'number' || typeof entry.frame !== 'object' || entry.frame === null) continue;
        frames.push({ seq: entry.seq, frame: entry.frame as IntentFrame });
      }
      return {
        frames,
        nextCursor: typeof body.nextCursor === 'number' ? body.nextCursor : after,
        dropped: typeof body.dropped === 'number' ? body.dropped : 0,
      };
    },

    async ackIntents(upto: number): Promise<void> {
      expectOK(await request('POST', '/tenon/intents/ack', { upto }), 'POST /tenon/intents/ack');
    },

    async listPending(): Promise<PendingItem[]> {
      const reply = await request('GET', '/tenon/pending');
      expectOK(reply, 'GET /tenon/pending');
      const body = JSON.parse(reply.text) as { items?: unknown[] };
      const items: PendingItem[] = [];
      for (const raw of body.items ?? []) {
        if (typeof raw !== 'object' || raw === null) continue;
        const item = raw as Record<string, unknown>;
        if (typeof item['planId'] !== 'string' || item['planId'].length === 0) continue;
        items.push({
          planId: item['planId'],
          state: typeof item['state'] === 'string' ? item['state'] : undefined,
          summary: typeof item['summary'] === 'string' ? item['summary'] : undefined,
          requiredRoles: Array.isArray(item['requiredRoles'])
            ? item['requiredRoles'].filter((role): role is string => typeof role === 'string')
            : undefined,
          mine: typeof item['mine'] === 'boolean' ? item['mine'] : undefined,
        });
      }
      return items;
    },
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```

Expected: `# pass 8`（5 配置 + 3 host）、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/host.ts packages/dsh-plugin-tenon-notifier/src/host.test.ts
git commit -m "feat(notifier): host 登录、intent 轮询（含 dropped）与 pending 列表"
```

---

## Task 4: 人话格式化（Inline & Spill）与通知消息

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/format.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/format.test.ts`

> 设计要点（spec §6.6）：通知里**只放 `planId｜一句结论｜角色`**，全文留给 `pending_list` / `audit_query`。
> 一屏 5 条整段 summary 会挤占窗口、稀释注意力；而模型真需要细节时，一次工具调用比一次唤醒便宜。

- [ ] **Step 1: 先写失败测试**

`src/format.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import {
  NOTICE_SUMMARY_MAX,
  SUMMARY_MAX,
  clip,
  composeWakeText,
  makeNoticeMessage,
  notificationFromIntent,
  notificationFromPending,
  notificationFromTerminal,
} from './format.js';

test('待批卡：planId + 一句结论 + 角色；mine=false 要提示没权限', () => {
  const base = { planId: 'pln_1', state: 'awaiting_approval', summary: '扣减 STD-1001 库存 12 件', requiredRoles: ['role:warehouse_manager'] };
  const notification = notificationFromPending(base);
  assert.ok(notification);
  assert.equal(notification.id, 'pending:pln_1');
  assert.match(notification.text, /^pln_1｜待批/);
  assert.match(notification.text, /12 件/);
  assert.match(notification.text, /warehouse_manager/);
  assert.doesNotMatch(notification.text, /你可能无审批角色/);

  const noRole = notificationFromPending({ ...base, mine: false });
  assert.match(noRole?.text ?? '', /你可能无审批角色/);

  assert.equal(notificationFromPending({ ...base, state: 'done' }), null);
});

test('终态与 intent：词表、seq 保留、长文截断、未知 outcome 丢弃', () => {
  assert.match(notificationFromTerminal({ planId: 'pln_1', state: 'done', summary: '扣 12 件' })?.text ?? '', /pln_1｜已执行：扣 12 件/);
  assert.match(notificationFromTerminal({ planId: 'pln_1', state: 'rejected' })?.text ?? '', /已驳回/);
  assert.equal(notificationFromTerminal({ planId: 'pln_1', state: 'awaiting_approval' }), null);

  const intent = notificationFromIntent({ intentId: 'int_1', planId: 'pln_2', outcome: 'completed', message: '库存 108→96' }, 7);
  assert.equal(intent?.seq, 7, 'seq 必须带上，轮询器靠它推进游标');
  assert.match(intent?.text ?? '', /pln_2｜已执行：库存 108→96/);
  assert.match(notificationFromIntent({ intentId: 'int_2', planId: 'pln_3', outcome: 'needs_human', message: '策略要求审批' }, 8)?.text ?? '', /未执行/);
  assert.equal(notificationFromIntent({ intentId: 'int_3', outcome: 'ignored' }, 9), null);

  const long = notificationFromIntent({ intentId: 'int_4', planId: 'pln_4', outcome: 'failed', message: 'x'.repeat(500) }, 10);
  const body = (long?.text ?? '').split('：')[1] ?? '';
  assert.ok(body.length <= SUMMARY_MAX, `结论应被截断到 ${SUMMARY_MAX}，得到 ${body.length}`);
});

test('批量文案只取 batch 条，并如实说明还剩几条', () => {
  const items = [
    notificationFromIntent({ intentId: 'i1', planId: 'pln_1', outcome: 'completed' }, 1)!,
    notificationFromIntent({ intentId: 'i2', planId: 'pln_2', outcome: 'completed' }, 2)!,
    notificationFromIntent({ intentId: 'i3', planId: 'pln_3', outcome: 'completed' }, 3)!,
  ];
  const { text, picked } = composeWakeText(items, 2);
  assert.equal(picked.length, 2, 'picked 决定 ack 到哪，绝不能多报');
  assert.equal(picked[1]?.planId, 'pln_2');
  assert.match(text, /仅通知，不要自动批准/);
  assert.match(text, /还有 1 条/);
  assert.match(text, /^1\. pln_1/m);
  assert.match(text, /^2\. pln_2/m);
  assert.doesNotMatch(text, /pln_3/);
});

test('通知消息：来源是 plugin + notice，不是人类', () => {
  const message = makeNoticeMessage('【Tenon 通知】仅通知，不要自动批准。\n1. pln_1｜待批：扣 12 件｜需 role:warehouse_manager');
  assert.equal(message.role, 'user');
  assert.equal(message.source.kind, 'plugin');
  assert.equal(message.source.plugin, 'tenon-notifier');
  assert.equal(message.source.form, 'notice');
  assert.ok(message.source.summary.length > 0);
  assert.ok(message.source.summary.length <= NOTICE_SUMMARY_MAX);
  assert.ok(!message.source.summary.includes('\n'), 'summary 必须单行');
  assert.equal(message.content[0]?.type, 'text');
  assert.equal(clip('  a   b  ', 10), 'a b');
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: FAIL，`./format.js` 不存在。

- [ ] **Step 3: 实现格式化**

`src/format.ts`：

```ts
import { randomUUID } from 'node:crypto';
import type { IntentFrame, NoticeMessage, Notification, PendingItem } from './types.js';

/** 插件名同时是通知来源标识；测试与文档都引用这一处。 */
export const PLUGIN_NAME = 'tenon-notifier';
/** 单条通知里「一句结论」的上限。 */
export const SUMMARY_MAX = 60;
/** notice 行摘要上限（客户端折叠行不换行）。 */
export const NOTICE_SUMMARY_MAX = 80;

/** 压平成单行并截断；通知是给人快速判断的，不是日志搬运。 */
export function clip(text: string, max: number): string {
  const flat = text.replace(/\s+/g, ' ').trim();
  if (flat.length <= max) return flat;
  return `${flat.slice(0, Math.max(0, max - 1))}…`;
}

function terminalWord(state: string): string {
  switch (state) {
    case 'done':
      return '已执行';
    case 'rejected':
      return '已驳回';
    case 'expired':
      return '已过期';
    case 'stale':
      return '已失效';
    case 'amended':
      return '已修改，等待重新批准';
    default:
      return `状态 ${state}`;
  }
}

/**
 * 公告板一行 → 待批通知。
 *
 * `mine === false` 时补一句「你可能无审批角色」：宿主按 token 角色算了 mine，
 * 不告诉用户就会出现「收到待批、点了批准却 403」。
 */
export function notificationFromPending(item: PendingItem): Notification | null {
  if (item.state !== 'awaiting_approval') return null;
  const summary = item.summary ? clip(item.summary, SUMMARY_MAX) : '（无摘要）';
  const roles = item.requiredRoles && item.requiredRoles.length > 0 ? `需 ${item.requiredRoles.join('/')}` : '未指定角色';
  const warn = item.mine === false ? '；你可能无审批角色' : '';
  return {
    id: `pending:${item.planId}`,
    kind: 'pending',
    planId: item.planId,
    text: `${item.planId}｜待批：${summary}｜${roles}${warn}`,
  };
}

export function notificationFromTerminal(item: PendingItem): Notification | null {
  if (!item.state || item.state === 'awaiting_approval') return null;
  const summary = item.summary ? `：${clip(item.summary, SUMMARY_MAX)}` : '';
  return {
    id: `terminal:${item.planId}:${item.state}`,
    kind: 'terminal',
    planId: item.planId,
    text: `${item.planId}｜${terminalWord(item.state)}${summary}`,
  };
}

export function notificationFromIntent(frame: IntentFrame, seq: number): Notification | null {
  if (!frame.outcome) return null;
  const plan = frame.planId ?? '（无计划号）';
  const detail = frame.message && frame.message.trim().length > 0 ? `：${clip(frame.message, SUMMARY_MAX)}` : '';
  let head: string;
  switch (frame.outcome) {
    case 'completed':
      head = '已执行';
      break;
    case 'needs_human':
      head = '未执行';
      break;
    case 'failed':
      head = '执行失败';
      break;
    case 'rejected':
      head = '已驳回';
      break;
    default:
      return null;
  }
  return {
    id: `intent:${frame.intentId ?? `${plan}:${frame.outcome}`}`,
    kind: 'intent',
    planId: frame.planId,
    seq,
    text: `${plan}｜${head}${detail}`,
  };
}

export interface WakeText {
  /** 真正投递的正文。 */
  text: string;
  /** 真正进了正文的通知——轮询器只对这批负责（标记已推 + ack）。 */
  picked: Notification[];
}

/**
 * 组装一次唤醒/注入的正文。
 *
 * `picked` 是契约的一部分：超出的条目**不能**被标记已推、也不能被 ack，
 * 否则"还有 N 条，稍后继续"就是一句假话（v1 计划在这里丢过消息）。
 */
export function composeWakeText(notifications: Notification[], batch: number): WakeText {
  const limit = Math.max(1, batch);
  const picked = notifications.slice(0, limit);
  const rest = notifications.length - picked.length;
  const lines = picked.map((notification, index) => `${index + 1}. ${notification.text}`);
  const head = rest > 0 ? `共 ${notifications.length} 条，本轮 ${picked.length} 条（还有 ${rest} 条，稍后继续）` : `共 ${picked.length} 条`;
  const hint = picked.some((notification) => notification.planId !== undefined) ? '要看全文就说「列待批」或「查审计 <planId>」。' : '';
  const text = ['【Tenon 通知】仅通知，不要自动批准。', head, ...lines, hint]
    .filter((line) => line.length > 0)
    .join('\n');
  return { text, picked };
}

/** 组装 dsh 消息：来源是 plugin/notice，摘要单行。 */
export function makeNoticeMessage(text: string, plugin: string = PLUGIN_NAME): NoticeMessage {
  const firstLine = text
    .split('\n')
    .map((line) => line.trim())
    .find((line) => line.length > 0) ?? text;
  return {
    id: randomUUID(),
    role: 'user',
    content: [{ type: 'text', text }],
    source: { kind: 'plugin', plugin, form: 'notice', summary: clip(firstLine, NOTICE_SUMMARY_MAX) },
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: `# pass 12`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/format.ts packages/dsh-plugin-tenon-notifier/src/format.test.ts
git commit -m "feat(notifier): 通知文案（Inline & Spill）与 plugin 来源消息"
```

---

## Task 5: 投递适配器（忙 inject / 闲 followup + 唤醒预算）

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/delivery.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/delivery.test.ts`

> 这是 v2 的核心新增。规则抄自 dsh `tool-jobs`（`packages/jobs/tool-jobs/src/index.ts:281-300`），
> 但**不接管它的 job 语义**：host 事件不是模型启动的后台工作，套进 job 会让模型以为能 `job_kill` 一条待批。

- [ ] **Step 1: 先写失败测试**

`src/delivery.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import { createDelivery } from './delivery.js';
import { makeNoticeMessage } from './format.js';
import type { NoticeMessage, WakeAgent } from './types.js';

interface Spy extends WakeAgent {
  woken: NoticeMessage[];
  injected: NoticeMessage[];
}

function spy(status?: string, withInject = true): Spy {
  const spyAgent: Spy = {
    id: 'sess-1',
    status,
    woken: [],
    injected: [],
    followup(message: NoticeMessage): void {
      spyAgent.woken.push(message);
    },
  };
  if (withInject) {
    spyAgent.inject = (message: NoticeMessage): void => {
      spyAgent.injected.push(message);
    };
  }
  return spyAgent;
}

const message = (): NoticeMessage => makeNoticeMessage('【Tenon 通知】1. pln_1｜待批');

test('空闲且预算未用尽：开一个通知回合', () => {
  const delivery = createDelivery({ delivery: 'wakeup', wakeBudget: 3 });
  const agent = spy('idle');
  assert.equal(delivery.deliver(agent, message()).lane, 'wakeup');
  assert.equal(agent.woken.length, 1);
  assert.equal(agent.injected.length, 0);
  assert.equal(delivery.spent(agent), 1);
});

test('会话忙：注入到下一步，不插回合', () => {
  const delivery = createDelivery({ delivery: 'wakeup', wakeBudget: 3 });
  const agent = spy('running');
  const result = delivery.deliver(agent, message());
  assert.equal(result.lane, 'inject');
  assert.equal(agent.woken.length, 0);
  assert.equal(agent.injected.length, 1);
  assert.match(result.reason, /running/);
});

test('预算用尽后降级为注入（防自激链）', () => {
  const delivery = createDelivery({ delivery: 'wakeup', wakeBudget: 1 });
  const agent = spy('idle');
  assert.equal(delivery.deliver(agent, message()).lane, 'wakeup');
  const second = delivery.deliver(agent, message());
  assert.equal(second.lane, 'inject');
  assert.match(second.reason, /预算/);
  assert.equal(delivery.spent(agent), 1);
});

test('人类输入回填预算：下一个回合可以再次唤醒', () => {
  const delivery = createDelivery({ delivery: 'wakeup', wakeBudget: 1 });
  const agent = spy('idle');
  delivery.deliver(agent, message());
  assert.equal(delivery.deliver(agent, message()).lane, 'inject');
  delivery.noteHumanInput(agent);
  assert.equal(delivery.spent(agent), 0);
  assert.equal(delivery.deliver(agent, message()).lane, 'wakeup');
});

test('quiet 只注入；agent 没有 inject 时退回 followup 并说明原因', () => {
  const quiet = createDelivery({ delivery: 'quiet', wakeBudget: 3 });
  const idleAgent = spy('idle');
  assert.equal(quiet.deliver(idleAgent, message()).lane, 'inject');
  assert.equal(idleAgent.woken.length, 0);

  const legacy = createDelivery({ delivery: 'wakeup', wakeBudget: 3 });
  const busyNoInject = spy('running', false);
  const result = legacy.deliver(busyNoInject, message());
  assert.equal(result.lane, 'wakeup');
  assert.match(result.reason, /inject/);
  assert.equal(busyNoInject.woken.length, 1);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: FAIL，`./delivery.js` 不存在。

- [ ] **Step 3: 实现投递适配器**

`src/delivery.ts`：

```ts
import type { DeliveryMode } from './config.js';
import type { NoticeMessage, WakeAgent } from './types.js';

/** 实际走的通道：wakeup 开了回合，inject 只进了下一步收件箱。 */
export type Lane = 'wakeup' | 'inject';

export interface DeliveryOptions {
  delivery: DeliveryMode;
  /** 同一会话连续自行唤醒的上限；0 = 只注入。 */
  wakeBudget: number;
  log?: (line: string) => void;
}

export interface DeliveryResult {
  lane: Lane;
  /** 为什么走这条通道（写日志用，也用于测试断言）。 */
  reason: string;
}

export interface Delivery {
  deliver(agent: WakeAgent, message: NoticeMessage): DeliveryResult;
  /** 人类真的说话了：回填该会话的唤醒预算。 */
  noteHumanInput(agent: WakeAgent): void;
  spent(agent: WakeAgent): number;
}

/**
 * 投递规则（与 dsh `tool-jobs` 同构）：
 *
 * 1. 会话忙 → `inject`（进 next-step，不打断正在进行的回合）；
 * 2. 会话空闲且预算未用尽 → `followup`（开一个通知回合，用掉一次预算）；
 * 3. 预算**只在人类输入被领取时**回填。
 *
 * status 不可得时按 idle 处理：待批"必须被看见"优先于"绝不插回合"。
 * 代价是可能插一个回合——这条写在 spec §9，而不是藏在这里。
 */
export function createDelivery(options: DeliveryOptions): Delivery {
  const log = options.log ?? (() => undefined);
  const spent = new WeakMap<WakeAgent, number>();

  return {
    deliver(agent: WakeAgent, message: NoticeMessage): DeliveryResult {
      if (options.delivery === 'quiet') {
        if (typeof agent.inject === 'function') {
          agent.inject(message);
          return { lane: 'inject', reason: 'quiet 配置：只注入不唤醒' };
        }
        agent.followup(message);
        return { lane: 'wakeup', reason: 'quiet 配置但 agent 没有 inject，退回 followup' };
      }

      const idle = agent.status === undefined || agent.status === 'idle';
      const used = spent.get(agent) ?? 0;
      if (idle && used < options.wakeBudget) {
        spent.set(agent, used + 1);
        agent.followup(message);
        return { lane: 'wakeup', reason: `idle 且预算未用尽（${used + 1}/${options.wakeBudget}）` };
      }

      if (typeof agent.inject === 'function') {
        agent.inject(message);
        return {
          lane: 'inject',
          reason: idle ? `唤醒预算已用尽（${used}/${options.wakeBudget}），降级注入` : `会话忙（status=${agent.status}），不插回合`,
        };
      }

      log(`tenon-notifier: agent ${agent.id} 没有 inject，只能 followup（可能打断当前回合）`);
      spent.set(agent, used + 1);
      agent.followup(message);
      return { lane: 'wakeup', reason: 'agent 没有 inject，退回 followup' };
    },

    noteHumanInput(agent: WakeAgent): void {
      if (spent.delete(agent)) log(`tenon-notifier: agent ${agent.id} 有人类输入，唤醒预算回填`);
    },

    spent(agent: WakeAgent): number {
      return spent.get(agent) ?? 0;
    },
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: `# pass 17`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/delivery.ts packages/dsh-plugin-tenon-notifier/src/delivery.test.ts
git commit -m "feat(notifier): 投递适配器（忙 inject / 闲 followup + 唤醒预算）"
```

---

## Task 6: 订阅表（planId → 会话）

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/subscription.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/subscription.test.ts`

> 替代 v1 的"唯一 root / 钉死 sessionId"。`planId` 是贯穿审计链、公告板、intent 帧的现成 ID，
> 把它当订阅单元，多会话就各收各的，不需要"恰好一个根会话"这个脆弱前提（spec §6.4）。

- [ ] **Step 1: 先写失败测试**

`src/subscription.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import { MAX_SUBSCRIPTIONS, createSubscriptions } from './subscription.js';

test('绑定后可查；未绑定返回 undefined', () => {
  const table = createSubscriptions();
  assert.equal(table.get('pln_1'), undefined);
  table.bind('pln_1', 'sess-a');
  assert.equal(table.get('pln_1'), 'sess-a');
  assert.equal(table.size(), 1);
});

test('重复绑定以最新为准（同一计划换了会话）', () => {
  const table = createSubscriptions();
  table.bind('pln_1', 'sess-a');
  table.bind('pln_1', 'sess-b');
  assert.equal(table.get('pln_1'), 'sess-b');
  assert.equal(table.size(), 1, '同 key 不应产生第二条');
});

test('超过上限淘汰最旧的绑定，不会无限增长', () => {
  const table = createSubscriptions();
  for (let index = 0; index < MAX_SUBSCRIPTIONS + 5; index += 1) table.bind(`pln_${index}`, 'sess');
  assert.equal(table.size(), MAX_SUBSCRIPTIONS);
  assert.equal(table.get('pln_0'), undefined, '最旧的应被淘汰');
  assert.equal(table.get(`pln_${MAX_SUBSCRIPTIONS + 4}`), 'sess');
});

test('amend 迁移：新 planId 继承旧订阅；快照可 JSON 往返', () => {
  const table = createSubscriptions();
  table.bind('pln_old', 'sess-a');
  table.migrate('pln_old', 'pln_new');
  assert.equal(table.get('pln_new'), 'sess-a');

  const restored = createSubscriptions(JSON.parse(JSON.stringify(table.snapshot())) as Record<string, string>);
  assert.equal(restored.get('pln_new'), 'sess-a');
  assert.notEqual(table.snapshot(), table.snapshot(), '每次返回新对象，调用方改不到内部状态');
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: FAIL，`./subscription.js` 不存在。

- [ ] **Step 3: 实现订阅表**

`src/subscription.ts`：

```ts
/** 订阅表上限：够覆盖"同时挂着的审批"，又不至于把状态文件写成日志。 */
export const MAX_SUBSCRIPTIONS = 200;

export interface SubscriptionTable {
  get(planId: string): string | undefined;
  bind(planId: string, sessionId: string): void;
  /** amend：新计划号继承旧计划的会话绑定。 */
  migrate(fromPlanId: string, toPlanId: string): void;
  snapshot(): Record<string, string>;
  size(): number;
}

/**
 * planId → sessionId 的订阅表（spec §6.4「会话选择」）。
 *
 * Map 的插入序即淘汰序：超过上限丢最旧的。这条有界性不是洁癖——计划号只增不减，
 * 无界增长会让状态文件每轮重写越来越慢，最终把 tick 拖到超时。
 */
export function createSubscriptions(initial?: Record<string, string>): SubscriptionTable {
  const entries = new Map<string, string>();
  for (const [planId, sessionId] of Object.entries(initial ?? {})) {
    if (typeof planId === 'string' && planId.length > 0 && typeof sessionId === 'string' && sessionId.length > 0) {
      entries.set(planId, sessionId);
    }
  }

  return {
    get(planId: string): string | undefined {
      return entries.get(planId);
    },

    bind(planId: string, sessionId: string): void {
      if (planId.length === 0 || sessionId.length === 0) return;
      // 先删再插：让"刚更新过"的绑定排到队尾，不被下一轮淘汰掉。
      entries.delete(planId);
      entries.set(planId, sessionId);
      while (entries.size > MAX_SUBSCRIPTIONS) {
        const oldest = entries.keys().next().value as string | undefined;
        if (oldest === undefined) break;
        entries.delete(oldest);
      }
    },

    migrate(fromPlanId: string, toPlanId: string): void {
      const sessionId = entries.get(fromPlanId);
      if (sessionId === undefined || toPlanId.length === 0) return;
      this.bind(toPlanId, sessionId);
    },

    snapshot(): Record<string, string> {
      return Object.fromEntries(entries);
    },

    size(): number {
      return entries.size;
    },
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: `# pass 21`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/subscription.ts packages/dsh-plugin-tenon-notifier/src/subscription.test.ts
git commit -m "feat(notifier): planId 订阅表（有界、可持久化、可迁移）"
```

---

## Task 7: 轮询器（订阅解析、忙闲投递、游标前缀、ack 重试、单 tick 互斥）

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/notifier.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/notifier.test.ts`

**三条不变量（spec §6.4），本任务的测试就是它们的守卫：**

1. 只 ack **已投递的连续前缀**——遇到本轮因 `batch` 推不动的帧就停止推进（不丢消息）。
2. **不按 planId 折叠帧**——同一计划的多条结论都要送达（折叠只用于文案合并）。
3. **ack 与"本轮有没有新内容"解耦**——`delivered > cursor` 就重试 ack。

- [ ] **Step 1: 先写失败测试**

`src/notifier.test.ts`：

```ts
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { normalizeConfig } from './config.js';
import { createDelivery } from './delivery.js';
import type { HostClient } from './host.js';
import { createNotifier } from './notifier.js';
import { createSubscriptions } from './subscription.js';
import type { NoticeMessage, PendingItem, WakeAgent } from './types.js';

interface Spy extends WakeAgent {
  woken: NoticeMessage[];
  injected: NoticeMessage[];
}

function spy(id: string, status?: string): Spy {
  const agent: Spy = {
    id,
    status,
    woken: [],
    injected: [],
    followup(message: NoticeMessage): void {
      agent.woken.push(message);
    },
    inject(message: NoticeMessage): void {
      agent.injected.push(message);
    },
  };
  return agent;
}

interface HostState {
  frames?: Array<{ seq: number; frame: Record<string, unknown> }>;
  pending?: Array<Record<string, unknown>>;
  dropped?: number;
  ackFailures?: number;
}

function fakeHost(state: HostState): { client: HostClient; acked: number[] } {
  const acked: number[] = [];
  let ackFailures = state.ackFailures ?? 0;
  const client: HostClient = {
    async pollIntents(after: number) {
      const frames = (state.frames ?? []).filter((entry) => entry.seq > after);
      const nextCursor = frames.length > 0 ? Math.max(...frames.map((entry) => entry.seq)) : after;
      return { frames, nextCursor, dropped: state.dropped ?? 0 };
    },
    async ackIntents(upto: number) {
      if (ackFailures > 0) {
        ackFailures -= 1;
        throw new Error('ack boom');
      }
      acked.push(upto);
    },
    async listPending(): Promise<PendingItem[]> {
      return (state.pending ?? []).flatMap((raw) => {
        if (typeof raw['planId'] !== 'string' || raw['planId'].length === 0) return [];
        return [{
          planId: raw['planId'],
          state: typeof raw['state'] === 'string' ? raw['state'] : undefined,
          summary: typeof raw['summary'] === 'string' ? raw['summary'] : undefined,
          requiredRoles: Array.isArray(raw['requiredRoles']) ? (raw['requiredRoles'] as string[]) : undefined,
          mine: typeof raw['mine'] === 'boolean' ? raw['mine'] : undefined,
        }];
      });
    },
  };
  return { client, acked };
}

function build(options: {
  frames?: HostState['frames'];
  pending?: HostState['pending'];
  dropped?: number;
  ackFailures?: number;
  batch?: number;
  sessionId?: string;
  agents: WakeAgent[];
  subscriptions?: ReturnType<typeof createSubscriptions>;
}) {
  const dir = mkdtempSync(path.join(tmpdir(), 'tenon-notifier-'));
  const stateFile = path.join(dir, 'state.json');
  const config = normalizeConfig(
    {
      hostURL: 'http://127.0.0.1:8080',
      token: 'tok',
      stateFile,
      minGapMs: 0,
      ...(options.batch === undefined ? {} : { batch: options.batch }),
      ...(options.sessionId === undefined ? {} : { sessionId: options.sessionId }),
    },
    dir,
  );
  const host = fakeHost({ frames: options.frames, pending: options.pending, dropped: options.dropped, ackFailures: options.ackFailures });
  const agents = options.agents;
  const notifier = createNotifier({
    host: host.client,
    agents: { get: (id) => agents.find((agent) => agent.id === id), roots: () => agents },
    delivery: createDelivery({ delivery: 'wakeup', wakeBudget: 3 }),
    config,
    ...(options.subscriptions === undefined ? {} : { subscriptions: options.subscriptions }),
  });
  return { notifier, acked: host.acked, stateFile };
}

test('待批 + 结果同批：一次投递、ack 到最大序号、状态落盘含订阅', async () => {
  const agent = spy('sess-1', 'idle');
  const { notifier, acked, stateFile } = build({
    frames: [{ seq: 3, frame: { intentId: 'int_1', planId: 'pln_1', outcome: 'completed', message: '库存 108→96' } }],
    pending: [{ planId: 'pln_2', state: 'awaiting_approval', summary: '扣 8 件', requiredRoles: ['role:warehouse_manager'] }],
    agents: [agent],
  });

  const result = await notifier.tick();
  assert.equal(result.woken, true);
  assert.equal(result.lane, 'wakeup');
  assert.equal(result.events, 2);
  assert.equal(agent.woken.length, 1);
  const text = agent.woken[0]?.content[0]?.text ?? '';
  assert.match(text, /pln_1/);
  assert.match(text, /pln_2/);
  assert.deepEqual(acked, [3]);

  const state = JSON.parse(readFileSync(stateFile, 'utf8')) as { cursor: number; subscriptions: Record<string, string> };
  assert.equal(state.cursor, 3);
  assert.equal(state.subscriptions['pln_1'], 'sess-1');
  assert.equal(state.subscriptions['pln_2'], 'sess-1');

  const again = await notifier.tick();
  assert.equal(again.woken, false, '同一批 pending 不应重复唤醒');
});

test('超 batch：只推 batch 条，ack 只到已推的那一帧，下一轮继续（不丢）', async () => {
  const agent = spy('sess-1', 'idle');
  const { notifier, acked } = build({
    batch: 1,
    frames: [
      { seq: 1, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } },
      { seq: 2, frame: { intentId: 'i2', planId: 'pln_2', outcome: 'completed' } },
      { seq: 3, frame: { intentId: 'i3', planId: 'pln_3', outcome: 'completed' } },
    ],
    agents: [agent],
  });

  assert.equal((await notifier.tick()).woken, true);
  assert.match(agent.woken[0]?.content[0]?.text ?? '', /pln_1/);
  assert.doesNotMatch(agent.woken[0]?.content[0]?.text ?? '', /pln_2/);
  assert.deepEqual(acked, [1], '不能 ack 到 3：2/3 还没推');

  assert.equal((await notifier.tick()).woken, true);
  assert.match(agent.woken[1]?.content[0]?.text ?? '', /pln_2/);
  assert.deepEqual(acked, [1, 2]);

  assert.equal((await notifier.tick()).woken, true);
  assert.match(agent.woken[2]?.content[0]?.text ?? '', /pln_3/);
  assert.deepEqual(acked, [1, 2, 3]);
});

test('同一计划两条结论同批到达：两条都送达，不折叠', async () => {
  const agent = spy('sess-1', 'idle');
  const { notifier, acked } = build({
    frames: [
      { seq: 1, frame: { intentId: 'i1', planId: 'pln_x', outcome: 'needs_human' } },
      { seq: 2, frame: { intentId: 'i2', planId: 'pln_x', outcome: 'completed' } },
    ],
    agents: [agent],
  });
  assert.equal((await notifier.tick()).woken, true);
  const text = agent.woken[0]?.content[0]?.text ?? '';
  assert.match(text, /未执行/);
  assert.match(text, /已执行/);
  assert.deepEqual(acked, [2]);
});

test('会话忙：走 inject 而不是插一个回合', async () => {
  const agent = spy('sess-1', 'running');
  const { notifier, acked } = build({
    frames: [{ seq: 1, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } }],
    agents: [agent],
  });
  const result = await notifier.tick();
  assert.equal(result.lane, 'inject');
  assert.equal(agent.woken.length, 0);
  assert.equal(agent.injected.length, 1);
  assert.deepEqual(acked, [1]);
});

test('没有会话：不 ack、不落盘（下轮再来）', async () => {
  const { notifier, acked } = build({
    frames: [{ seq: 1, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } }],
    agents: [],
  });
  assert.equal((await notifier.tick()).woken, false);
  assert.deepEqual(acked, [], '没人收就不能 ack');
  assert.equal(notifier.cursor(), 0);
});

test('投递抛错：不 ack、不标记已推；恢复后重试一次成功', async () => {
  let fail = true;
  const agent: Spy = spy('sess-1', 'idle');
  agent.followup = (message: NoticeMessage): void => {
    if (fail) throw new Error('agent disposed');
    agent.woken.push(message);
  };
  const { notifier, acked } = build({
    frames: [{ seq: 4, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } }],
    agents: [agent],
  });

  const first = await notifier.tick();
  assert.equal(first.woken, false);
  assert.match(first.notes.join('\n'), /投递失败/);
  assert.deepEqual(acked, []);

  fail = false;
  assert.equal((await notifier.tick()).woken, true);
  assert.equal(agent.woken.length, 1, '只成功一次');
  assert.deepEqual(acked, [4]);
});

test('ack 失败：保留 delivered，下一轮即使无新内容也重试 ack 并前进游标', async () => {
  const agent = spy('sess-1', 'idle');
  const { notifier, acked } = build({
    ackFailures: 1,
    frames: [{ seq: 3, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } }],
    agents: [agent],
  });

  await notifier.tick();
  assert.deepEqual(acked, [], '第一次 ack 失败');
  assert.equal(notifier.cursor(), 0);

  const second = await notifier.tick();
  assert.equal(second.woken, false, '没有新内容');
  assert.deepEqual(acked, [3], '但必须重试 ack');
  assert.equal(notifier.cursor(), 3);
});

test('订阅优先：两个根会话时推给订阅的那个', async () => {
  const wrong = spy('sess-1', 'idle');
  const right = spy('sess-2', 'idle');
  const { notifier } = build({
    frames: [{ seq: 1, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } }],
    agents: [wrong, right],
    subscriptions: createSubscriptions({ pln_1: 'sess-2' }),
  });
  assert.equal((await notifier.tick()).woken, true);
  assert.equal(wrong.woken.length, 0);
  assert.equal(right.woken.length, 1);
});

test('宿主队列丢帧：写进 notes，不静默', async () => {
  const agent = spy('sess-1', 'idle');
  const { notifier } = build({
    dropped: 2,
    frames: [{ seq: 1, frame: { intentId: 'i1', planId: 'pln_1', outcome: 'completed' } }],
    agents: [agent],
  });
  const result = await notifier.tick();
  assert.match(result.notes.join('\n'), /丢弃 2 帧/);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: FAIL，`createNotifier` 不存在。

- [ ] **Step 3: 实现轮询器**

`src/notifier.ts`：

```ts
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { NotifierConfig } from './config.js';
import type { Delivery, DeliveryResult, Lane } from './delivery.js';
import {
  PLUGIN_NAME,
  composeWakeText,
  makeNoticeMessage,
  notificationFromIntent,
  notificationFromPending,
  notificationFromTerminal,
} from './format.js';
import type { HostClient, PollResult } from './host.js';
import { createSubscriptions, type SubscriptionTable } from './subscription.js';
import type { AgentRegistry, Notification, WakeAgent } from './types.js';

/** 内存去重集合上限：planId 只增不减，无界会让长时间运行慢慢吃内存。 */
export const NOTIFIED_CAP = 500;

export interface NotifierOptions {
  host: HostClient;
  agents: AgentRegistry;
  delivery: Delivery;
  config: NotifierConfig;
  /** 从状态文件恢复的订阅表；测试可注入。 */
  subscriptions?: SubscriptionTable;
  log?: (line: string) => void;
  now?: () => number;
}

export interface TickResult {
  woken: boolean;
  lane?: Lane;
  events: number;
  notes: string[];
  text?: string;
}

export interface Notifier {
  tick(): Promise<TickResult>;
  start(): void;
  stop(): void;
  cursor(): number;
}

interface Persisted {
  cursor: number;
  subscriptions: Record<string, string>;
}

function reason(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function readState(path: string): Persisted {
  try {
    const raw = JSON.parse(readFileSync(path, 'utf8')) as { cursor?: unknown; subscriptions?: unknown };
    const cursor = typeof raw.cursor === 'number' && Number.isFinite(raw.cursor) && raw.cursor >= 0 ? raw.cursor : 0;
    const subscriptions: Record<string, string> = {};
    if (typeof raw.subscriptions === 'object' && raw.subscriptions !== null) {
      for (const [planId, sessionId] of Object.entries(raw.subscriptions as Record<string, unknown>)) {
        if (typeof sessionId === 'string' && sessionId.length > 0) subscriptions[planId] = sessionId;
      }
    }
    return { cursor, subscriptions };
  } catch {
    return { cursor: 0, subscriptions: {} };
  }
}

function writeState(path: string, state: Persisted): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(state), 'utf8');
}

export function createNotifier(options: NotifierOptions): Notifier {
  const { host, agents, delivery, config } = options;
  const log = options.log ?? (() => undefined);
  const now = options.now ?? (() => Date.now());

  const initial = readState(config.stateFile);
  let cursor = initial.cursor;
  /**
   * 已投递（或有意丢弃）到的序号。与 `cursor`（已 ack）分开存，是为了让 ack 失败可重试：
   * 只推进 cursor 的话，ack 一失败就永远卡在旧值（v1 计划的缺陷 3）。
   */
  let delivered = initial.cursor;
  let lastWake = 0;
  let droppedSeen = 0;
  let lastSessionId: string | undefined;
  let timer: ReturnType<typeof setInterval> | null = null;
  let running = false;

  const subscriptions = options.subscriptions ?? createSubscriptions(initial.subscriptions);
  const notified = new Set<string>();
  const terminalSeen = new Map<string, string>();

  function remember(id: string): void {
    notified.add(id);
    while (notified.size > NOTIFIED_CAP) {
      const oldest = notified.values().next().value as string | undefined;
      if (oldest === undefined) break;
      notified.delete(oldest);
    }
  }

  function persist(): void {
    try {
      writeState(config.stateFile, { cursor, subscriptions: subscriptions.snapshot() });
    } catch (error) {
      log(`tenon-notifier: 状态落盘失败（游标只在内存，重启会重推）：${reason(error)}`);
    }
  }

  /** 会话选择：订阅 → 配置 → 上次投递过的会话 → 唯一根会话 → 放弃（不猜、不广播）。 */
  function resolveAgent(planId?: string): WakeAgent | undefined {
    if (planId !== undefined) {
      const bound = subscriptions.get(planId);
      if (bound !== undefined) {
        const agent = agents.get(bound);
        if (agent !== undefined) return agent;
      }
    }
    if (config.sessionId !== undefined) {
      const agent = agents.get(config.sessionId);
      if (agent !== undefined) return agent;
    }
    if (lastSessionId !== undefined) {
      const agent = agents.get(lastSessionId);
      if (agent !== undefined) return agent;
    }
    const roots = agents.roots();
    if (roots.length === 1) return roots[0];
    if (roots.length > 1) log(`tenon-notifier: ${roots.length} 个根会话且该通知无订阅，本轮跳过`);
    return undefined;
  }

  async function flushAck(notes: string[]): Promise<void> {
    if (delivered <= cursor) return;
    try {
      await host.ackIntents(delivered);
      cursor = delivered;
      persist();
    } catch (error) {
      notes.push(`ack 失败（保留 delivered=${delivered}，下轮重试）：${reason(error)}`);
    }
  }

  async function runTick(): Promise<TickResult> {
    const notes: string[] = [];
    const candidates: Notification[] = [];

    let polled: PollResult;
    try {
      polled = await host.pollIntents(cursor);
    } catch (error) {
      return { woken: false, events: 0, notes: [`poll intents 失败：${reason(error)}`] };
    }

    if (polled.dropped > droppedSeen) {
      notes.push(`宿主队列丢弃 ${polled.dropped - droppedSeen} 帧（积压超上限），期间的通知已不可补`);
      droppedSeen = polled.dropped;
    }

    // 游标只能推进到"已投递的连续前缀"：遇到第一条本轮推不动的候选就停在这里。
    let frontier = delivered;
    for (const entry of polled.frames) {
      if (entry.seq <= delivered) continue;
      const notification = notificationFromIntent(entry.frame, entry.seq);
      if (notification === null) {
        frontier = entry.seq; // outcome 不可读：有意丢弃，允许越过
        continue;
      }
      if (notified.has(notification.id)) {
        frontier = entry.seq; // 上轮已推过（ack 没成功），允许越过
        continue;
      }
      if (candidates.length >= config.batch) break; // 推不完：不许越过它 ack
      candidates.push(notification);
      frontier = entry.seq;
    }

    if (config.pendingScan) {
      try {
        const items = await host.listPending();
        for (const item of items) {
          const state = item.state ?? '';
          const previous = terminalSeen.get(item.planId);
          terminalSeen.set(item.planId, state);
          if (state === 'awaiting_approval') {
            const notification = notificationFromPending(item);
            if (notification !== null && !notified.has(notification.id) && candidates.length < config.batch) {
              candidates.push(notification);
            }
            continue;
          }
          if (previous === undefined || previous === state) continue; // 首次见到的终态不补推历史
          const notification = notificationFromTerminal(item);
          if (notification !== null && !notified.has(notification.id) && candidates.length < config.batch) {
            candidates.push(notification);
          }
        }
      } catch (error) {
        notes.push(`pending 扫描失败：${reason(error)}`);
      }
    }

    if (candidates.length === 0) {
      await flushAck(notes);
      return { woken: false, events: 0, notes };
    }

    if (now() - lastWake < config.minGapMs) {
      notes.push(`节流：距上次投递不足 ${config.minGapMs}ms，本轮不推（帧留在 host）`);
      await flushAck(notes);
      return { woken: false, events: candidates.length, notes };
    }

    const agent = resolveAgent(candidates.find((notification) => notification.planId !== undefined)?.planId);
    if (agent === undefined) {
      notes.push('没有可投递的会话（无订阅、无 sessionId、根会话不唯一）');
      await flushAck(notes);
      return { woken: false, events: candidates.length, notes };
    }

    const { text, picked } = composeWakeText(candidates, config.batch);
    let result: DeliveryResult;
    try {
      result = delivery.deliver(agent, makeNoticeMessage(text, PLUGIN_NAME));
    } catch (error) {
      notes.push(`投递失败（不 ack，下轮重试）：${reason(error)}`);
      await flushAck(notes);
      return { woken: false, events: candidates.length, notes, text };
    }

    for (const notification of picked) {
      remember(notification.id);
      if (notification.planId !== undefined) subscriptions.bind(notification.planId, agent.id);
    }
    lastSessionId = agent.id;
    delivered = Math.max(delivered, frontier);
    await flushAck(notes);
    lastWake = now();
    log(`tenon-notifier: 投递 ${picked.length} 条（${result.lane}：${result.reason}；cursor=${cursor}）`);
    return { woken: true, lane: result.lane, events: picked.length, notes, text };
  }

  async function tick(): Promise<TickResult> {
    // 慢响应时 tick 会叠跑（interval 2s / timeout 10s），叠跑的 tick 会把同一张卡推两次。
    if (running) return { woken: false, events: 0, notes: ['上一轮 tick 未结束，本轮跳过（in-flight）'] };
    running = true;
    try {
      return await runTick();
    } finally {
      running = false;
    }
  }

  return {
    tick,
    start(): void {
      if (timer !== null) return;
      const run = (): void => {
        void tick().catch((error: unknown) => log(`tenon-notifier tick 异常：${reason(error)}`));
      };
      timer = setInterval(run, config.intervalMs);
      run();
    },
    stop(): void {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    },
    cursor(): number {
      return cursor;
    },
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: `# pass 30`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/notifier.ts packages/dsh-plugin-tenon-notifier/src/notifier.test.ts
git commit -m "feat(notifier): 订阅解析、忙闲投递、游标前缀与 ack 重试"
```

---

## Task 8: cordis 插件入口与整体构建

**Files:**
- Modify: `packages/dsh-plugin-tenon-notifier/src/index.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/index.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/index.test.ts`：

```ts
import assert from 'node:assert/strict';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { apply, inject, name } from './index.js';

const stateFile = path.join(mkdtempSync(path.join(tmpdir(), 'tenon-notifier-index-')), 'state.json');

function context(options: { withEffect?: boolean; withOn?: boolean } = {}) {
  const record = { effects: 0, stopped: false, events: [] as string[], listener: undefined as ((payload: unknown) => void) | undefined };
  const ctx: Record<string, unknown> = { agents: { get: () => undefined, roots: () => [] } };
  if (options.withEffect !== false) {
    ctx['effect'] = (fn: () => (() => void) | void): unknown => {
      record.effects += 1;
      const stop = fn();
      // 必须真的调用 disposer：否则 notifier 的 setInterval 会一直挂着，
      // `node --test` 永远不退出（这不是洁癖，是踩过的坑）。
      if (typeof stop === 'function') {
        stop();
        record.stopped = true;
      }
      return undefined;
    };
  }
  if (options.withOn === true) {
    ctx['on'] = (event: string, listener: (payload: unknown) => void): unknown => {
      record.events.push(event);
      record.listener = listener;
      return undefined;
    };
  }
  return { ctx, record };
}

test('插件身份：name/inject 固定，不注册模型工具', () => {
  assert.equal(name, 'tenon-notifier');
  assert.deepEqual([...inject], ['agents']);
});

test('apply：坏配置不抛也不启动（不能拖垮 dsh）；好配置注册 effect 并可停', () => {
  const bad = context();
  assert.doesNotThrow(() => apply(bad.ctx as never, { hostURL: 'ftp://x', token: 't' }));
  assert.equal(bad.record.effects, 0, '配置无效时不得启动轮询');

  const good = context();
  apply(good.ctx as never, { hostURL: 'http://127.0.0.1:9', token: 'tok', intervalMs: 600_000, minGapMs: 0, stateFile });
  assert.equal(good.record.effects, 1);
  assert.equal(good.record.stopped, true);
});

test('有 agent/inbox/claimed 时注册回填回调；只有人类来源才回填', () => {
  const { ctx, record } = context({ withOn: true });
  apply(ctx as never, { hostURL: 'http://127.0.0.1:9', token: 'tok', intervalMs: 600_000, minGapMs: 0, stateFile });
  assert.deepEqual(record.events, ['agent/inbox/claimed']);
  assert.equal(typeof record.listener, 'function');
  // 通知自己的 plugin 来源不得回填；人的消息才回填。两条都只要求不抛。
  record.listener?.({ message: { source: { kind: 'plugin' } } });
  record.listener?.({ agent: { id: 'sess-1', followup: () => undefined }, message: { source: { kind: 'user' } } });
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run: `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test`
Expected: FAIL，入口桩的 `apply` 抛「尚未实现」。

- [ ] **Step 3: 实现入口**

`src/index.ts`：

```ts
import { normalizeConfig, type NotifierConfig } from './config.js';
import { createDelivery } from './delivery.js';
import { createHostClient } from './host.js';
import { createNotifier } from './notifier.js';
import type { AgentRegistry, WakeAgent } from './types.js';

export const name = 'tenon-notifier';
export const inject = ['agents'] as const;

/** `agent/inbox/claimed` 的载荷里本插件用到的字段。 */
interface ClaimedPayload {
  agent?: WakeAgent;
  message?: { source?: { kind?: string } };
}

export interface NotifierContext {
  agents: AgentRegistry;
  /** cordis 生命周期钩子：`ctx.effect(execute, label?)`，execute 返回 disposer。 */
  effect?: (execute: () => (() => void) | void, label?: string) => unknown;
  /** cordis 事件订阅；没有它就不做预算回填（预算于是永不回填，通知会逐渐降级为注入）。 */
  on?: (event: string, listener: (payload: unknown) => void) => unknown;
}

/**
 * cordis 插件入口。
 *
 * 两条刻意选择：
 * 1. **不注册任何模型工具**——它只读 host、只投递通知；写/批/审计仍由 `tenon-mcp` 与 Go agent 完成。
 * 2. **配置坏了不抛穿**（fail-soft）——通知通道坏掉不该让整个 dsh profile 起不来；错误留在日志，
 *    其余功能（对话、MCP 工具）照常可用。
 */
export function apply(ctx: NotifierContext, raw: unknown): void {
  let config: NotifierConfig;
  try {
    config = normalizeConfig(raw);
  } catch (error) {
    console.error(
      `tenon-notifier: 配置无效，通知通道不启动（dsh 其余功能不受影响）：${error instanceof Error ? error.message : String(error)}`,
    );
    return;
  }

  const delivery = createDelivery({
    delivery: config.delivery,
    wakeBudget: config.wakeBudget,
    log: (line) => console.warn(line),
  });

  const notifier = createNotifier({
    host: createHostClient(config),
    agents: ctx.agents,
    delivery,
    config,
    log: (line) => console.warn(line),
  });

  // 唤醒预算只在"人的话被领取"时回填。通知自己是 plugin 来源，永远不会回填自己用掉的预算——
  // 这就是防"通知→回合→新请求→通知"自激链的那道闸。
  if (typeof ctx.on === 'function') {
    ctx.on('agent/inbox/claimed', (payload) => {
      const claimed = payload as ClaimedPayload | null;
      if (claimed?.agent !== undefined && claimed.message?.source?.kind === 'user') {
        delivery.noteHumanInput(claimed.agent);
      }
    });
  }

  if (typeof ctx.effect === 'function') {
    ctx.effect(() => {
      notifier.start();
      return () => notifier.stop();
    }, 'tenon-notifier');
    return;
  }
  notifier.start();
}
```

- [ ] **Step 4: 跑测试与全仓检查**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
pnpm run typecheck
pnpm run test
```

Expected: 新包 `# pass 33`、`# fail 0`；`typecheck` 退出 0；全仓 test 退出 0。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/index.ts packages/dsh-plugin-tenon-notifier/src/index.test.ts
git commit -m "feat(notifier): cordis 插件入口（fail-soft、预算回填）"
```

---

## Task 9: profile 安装脚本与冒烟脚本

**Files:**
- Create: `scripts/install-dsh-notifier.ps1`
- Create: `scripts/notifier-smoke.ps1`

- [ ] **Step 1: 写安装脚本**

> junction 的删除必须走 `Directory.Delete(path, $false)`：`Remove-Item -Recurse` 在"链接指向真实目录"时的失败模式是**删到目标内容**——那是 `packages/dsh-plugin-tenon-notifier` 的源码。

`scripts/install-dsh-notifier.ps1`：

```powershell
# 把 notifier 装进 dsh 的 web profile（junction），并打印要手工加入的 YAML 片段。
# 不自动改 cordis.patch.yml：那是用户的配置，脚本只做可诊断的安装与提示。
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$plugin = Join-Path $repo 'packages\dsh-plugin-tenon-notifier'
$entry = Join-Path $plugin 'dist\index.js'
if (-not (Test-Path $entry)) {
    throw "缺少 $entry：先跑 pnpm --filter @tenon/dsh-plugin-tenon-notifier run build"
}

$profile = Join-Path $HOME '.dsh\profiles\web'
if (-not (Test-Path $profile)) { throw "找不到 profile：$profile" }
$scope = Join-Path $profile 'node_modules\@tenon'
New-Item -ItemType Directory -Force -Path $scope | Out-Null
$link = Join-Path $scope 'dsh-plugin-tenon-notifier'

if (Test-Path -LiteralPath $link) {
    $item = Get-Item -LiteralPath $link -Force
    $isLink = [bool]($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint)
    if (-not $isLink) {
        throw "拒绝删除 $link：它不是链接而是真实目录。请人工确认后再处理（可能是有人把源码复制到了 profile 里）"
    }
    $target = @($item.Target)[0]
    if ($target -and ((Resolve-Path -LiteralPath $target).Path -ne (Resolve-Path -LiteralPath $plugin).Path)) {
        Write-Host "已存在指向其他目录的链接：$link -> $target，改为指向本仓库" -ForegroundColor DarkYellow
    }
    # 只删链接本身，绝不递归——否则可能删到目标目录里的源码。
    [System.IO.Directory]::Delete($link, $false)
}
New-Item -ItemType Junction -Path $link -Target $plugin | Out-Null

Push-Location $profile
try {
    node -e "import('@tenon/dsh-plugin-tenon-notifier').then(m => { if (m.name !== 'tenon-notifier') process.exit(1); console.log('✓ 可导入', m.name) })"
    if ($LASTEXITCODE -ne 0) { throw 'profile 里无法导入 notifier' }
} finally { Pop-Location }

Write-Host ''
Write-Host '把下面这段加到 cordis.patch.yml 顶层数组（缩进与现有条目一致）：' -ForegroundColor Cyan
@'
- insert:
    - id: tenon-notifier
      name: '@tenon/dsh-plugin-tenon-notifier'
      config:
        hostURL: 'http://127.0.0.1:8080'
        user: 'alice'
        password: 'alice-dev'
        intervalMs: 2000
        batch: 5
        minGapMs: 2000
        delivery: wakeup
        wakeBudget: 3
'@
Write-Host ''
Write-Host '然后重启 dsh web；日志里应看到 notifier 的投递行（含 lane：wakeup 或 inject）。' -ForegroundColor DarkGray
Write-Host '注意：stateFile 缺省落在 ~/.dsh/tenon-notifier/state.json（绝对路径，不随 CWD 变）。' -ForegroundColor DarkGray
```

- [ ] **Step 2: 写冒烟脚本**

`scripts/notifier-smoke.ps1`：

```powershell
# 往 host 投一条 intent，验证 notifier 会把它推进当前 dsh 会话。
# 前置：dev-up 已起；dsh web 已加载 notifier 并打开对话。
$ErrorActionPreference = 'Stop'
$hostUrl = 'http://127.0.0.1:8080'

$agent = Invoke-RestMethod -Method Post -Uri "$hostUrl/tenon/session" -ContentType 'application/json' -Body '{"user":"agent","password":"agent-dev"}'
$intentId = 'int_notifier_smoke_' + (Get-Date -Format 'HHmmss')
$body = @{
    type     = 'tenon.intent'
    intentId = $intentId
    planId   = 'pln_notifier_smoke'
    outcome  = 'completed'
    message  = 'notifier smoke：如果你在 dsh 里看到这条，通知链路是通的'
} | ConvertTo-Json -Compress

$null = Invoke-RestMethod -Method Post -Uri "$hostUrl/tenon/intents" -ContentType 'application/json' -Headers @{
    Authorization   = "Bearer $($agent.token)"
    'Tenon-Version' = '0.1'
} -Body $body

Write-Host "已投递 intent $intentId；等 notifier 下一次轮询（<= 2s）。" -ForegroundColor Cyan
Write-Host '预期：dsh 里出现一条以「【Tenon 通知】」开头的通知行；'
Write-Host '      若当时会话空闲 → 开一个回合（日志 lane=wakeup）；若正忙 → 进下一步（lane=inject）。'
```

- [ ] **Step 3: 跑安装脚本（构建后）**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run build
pwsh -NoProfile -File scripts/install-dsh-notifier.ps1
```

Expected: `✓ 可导入 tenon-notifier`；打印 YAML 片段。

- [ ] **Step 4: 手工把 YAML 加入 profile 并重启 dsh**

把脚本打印的 `- insert:` 片段追加到 `~/.dsh/profiles/web/cordis.patch.yml` 顶层数组，然后重启 dsh web（`pwsh -File scripts/dsh-up.ps1`）。

> **前置阻塞**：本机 profile 里 `@deepseek-ai/*` 的 junction 目前指向已不存在的 `apps/cli/node_modules`（2026-11-01 核对）。先在 profile 里 `pnpm install` 把运行时链重建好，再重启 dsh，否则插件加载失败与 notifier 无关却被误判成 notifier 的问题。

- [ ] **Step 5: 跑冒烟脚本**

Run:
```bash
pwsh -NoProfile -File scripts/notifier-smoke.ps1
```

Expected: dsh 对话里出现 `【Tenon 通知】仅通知，不要自动批准。`，包含 `notifier smoke`。

- [ ] **Step 6: 提交**

```bash
git add scripts/install-dsh-notifier.ps1 scripts/notifier-smoke.ps1
git commit -m "feat(notifier): profile 安装（安全删除 junction）与 dsh 冒烟脚本"
```

---

## Task 10: 文档与 P1 验收

**Files:**
- Create: `docs/dsh-chat-only.md`
- Modify: `README.md`（DSH 段落加一行指向文档）

- [ ] **Step 1: 写使用文档**

`docs/dsh-chat-only.md`：

```markdown
# 只在 dsh 对话里完成业务（P0+P1）

## 组件

- `tenon-mcp`：读/预检/投请求/待批/审批/审计；
- `dsh-plugin-tenon-notifier`：轮询 host，按 planId 订阅把待批与结果推进会话；
- Go agent + host + modules：执行与裁决（不变）。

> 不要同时加载旧 `module-host` 插件：它有自己的 pending store 与 `tenon_action` 工具，会和 Go agent 形成两套状态。产品 profile 只挂 `tenon-mcp` + `tenon-notifier`。

## 通知的三条规矩

1. **来源是 plugin**（`{kind:'plugin', form:'notice'}`），不是`人说的话`——通知不会获得人类发言的资格，也不会污染审计。
2. **忙 inject / 闲 followup**：会话空闲才开通知回合；正在对话时通知进下一步，不打断你。
3. **唤醒预算**（缺省 3，只有真的有人说话才回填）：防止"通知→回合→新请求→通知"自激。

## 安装

```bash
pnpm install
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
pnpm --filter @tenon/dsh-plugin-tenon-notifier run build
pwsh -NoProfile -File scripts/install-dsh-notifier.ps1
```

把脚本打印的 `tenon-notifier` 条目追加到 `~/.dsh/profiles/web/cordis.patch.yml`，重启 dsh web。

配置项（`config` 段）：`hostURL`、`token` 或 `user+password`、`sessionId?`、
`intervalMs`(2000)、`batch`(5)、`minGapMs`(2000)、`delivery`(wakeup|quiet)、`wakeBudget`(3)、
`pendingScan`(true)、`stateFile`（缺省 `~/.dsh/tenon-notifier/state.json`）。

## 冒烟

```bash
pwsh -NoProfile -File scripts/dev-up.ps1 -NoDemoCard
pwsh -NoProfile -File scripts/dsh-up.ps1
pwsh -NoProfile -File scripts/notifier-smoke.ps1
```

对话里应出现 `【Tenon 通知】`。

## 已知限制（P1 接受的，别当成 bug）

- **首条通知的会话归属不保证**：订阅是「投递成功后」才写的。第一次收到某条计划的通知时，如果开了多个会话，可能落到最近投递过的那个。同一条计划的后续通知会稳定回到同一会话。P1.5 由 `request_submit` 回传发起会话解决。
- **预算耗尽后，通知要等你下次开口才出现**：唤醒预算（3 次）用完会降级为 `inject`，它进的是下一步收件箱；你不说话它就躺着。`pending_list` 是随时可查的权威路径。
- **通知是 best-effort**：宿主队列上限 1024 帧，溢出丢最旧的（notifier 会把丢帧数写进日志）。

## 验收（全部在 dsh 对话里）

1. 查：“STD-1001 还有多少” → 正确数值。
2. 写：“扣 12 件” → 待批通知 → 选“允许执行” → 库存 −12 → 完成通知 → 审计 `approvedBy=user:alice`。
3. 驳：同一请求选“驳回” → 库存不变 → 通知“未执行”。
4. 断线：发请求后关 dsh/重启 → 重开后**在原来那个会话**看到待批通知 → 批 → 执行。
5. 事件：建报价单触发 `quote.completed` → Go agent 自动开回合落卡 → 对话通知 → 批准 → 库存/采购变化。
6. 负向：`action_invoke` 传写动作被拒；页面无写/批入口；notifier 没有模型工具。
7. **不打搅**：让模型跑一个长回合，期间投一条 intent → 不插新回合，通知在该回合的下一步出现（日志 lane=inject）。
8. **不自激**：连投 5 条 intent（间隔 > 2s）→ 前 3 条 `lane=wakeup`，第 4 条起 `lane=inject`；随后你说一句话，预算回填。
```

- [ ] **Step 2: README 加一行**

在 README 的 DSH 段落末尾加：

```markdown
> **只在 dsh 对话里完成业务（P0+P1）**：安装 `dsh-plugin-tenon-notifier`，待批与执行结果会按 planId 订阅主动推回会话（忙时注入、闲时开回合，带唤醒预算）；步骤见 [`docs/dsh-chat-only.md`](docs/dsh-chat-only.md)。
```

- [ ] **Step 3: 跑门槛**

Run:
```bash
pnpm run check
go test ./... -count=1
pwsh -NoProfile -File scripts/e2e-dsh.ps1
```

Expected: `pnpm check` 退出 0；`go test` 全绿；`e2e-dsh` 18 条全过。

- [ ] **Step 4: 手工跑 P1 验收清单**

按 `docs/dsh-chat-only.md` 的 1–8 在 dsh 里走一遍；截图/日志留在 `out/dsh-chat-p1/`（不入库）。至少确认：通知出现、批准后库存变化、审计 `approvedBy`、驳回不执行、事件回合通知、**忙时不插回合**、**预算生效**。

- [ ] **Step 5: 提交**

```bash
git add docs/dsh-chat-only.md README.md
git commit -m "docs: dsh 对话闭环（P0+P1）使用与验收"
```

---

## 验收判据（P0+P1）

- `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test` 全绿（33 条：配置 5 / host 3 / 格式 4 / 投递 5 / 订阅 4 / 轮询 9 / 入口 3）。
- **三条不变量有专门测试**：超 batch 不丢（三 tick 分别推 1/2/3 且 ack 逐帧前进）、同 planId 两帧都送达、ack 失败后下轮重试并前进游标。
- **投递规矩有专门测试**：忙 → `inject`、预算耗尽 → `inject`、人类输入回填、`quiet` 只注入。
- **来源有专门测试**：`makeNoticeMessage` 的 `source.kind === 'plugin'` 且 `form === 'notice'`。
- `pnpm check` 全绿；`go test ./...` 全绿；`scripts/e2e-dsh.ps1` 18 条全过。
- 活 dsh：`scripts/notifier-smoke.ps1` 投递后对话出现通知；日志能看到 `lane=wakeup` 或 `lane=inject` 及原因。
- 手工场景 1–8 全过，全程不打开看板/业务页。
- **不把「多会话首条通知归属」当判据**：P1 只在单会话下验收；多会话的期望行为由 P1.5 的 `origin.sessionId` 回传定义。
- 负向：notifier 没有模型工具；页面无写/批入口；无 token 批准被拒；通知不是 `kind:'user'`。

## P1.5 确认令牌（下一份计划的任务边界）

Task 0 第 3 条通过后，P1.5 单独写计划，至少包含：

1. **Host challenge**：`POST /tenon/approvals:challenge`（human token）校验 pending + 角色，发一次性 token（绑定 `by/planId/decision`，TTL 60s）；单测覆盖缺失/过期/串单/重放。
2. **Host confirm**：confirm 帧必须带 `confirmationToken`；宿主校验、消费、剥离后再入队；缺/坏/重放 403；不改 agent 的 confirm 语义。
3. **MCP 收口**：删除 `pending_decide`，保留 `pending_list` 只读；更新 tools/list 与测试。
4. **新插件 `dsh-plugin-tenon-confirm`**：只注册 `tenon_confirm(planId)`；调 `ctx.approval.request`，`allowed-once`→approve、`rejected`→reject、其余不提交；换 token 后提交；单测覆盖四种审批结局。
5. **活体验收**：不弹原生审批/无 token 直接调决策 → 403；允许 → 执行；拒绝 → 卡驳回；token 重放/过期 → 403；模型工具面里没有 `pending_decide`。

## 本计划故意没做（避免顺手扩大范围）

| 项 | 为什么不在 P1 | 去哪儿 |
|---|---|---|
| `pending_decide` → 确认令牌（host challenge + `tenon_confirm` 插件工具） | MCP 拿不到 DSH 的 `ctx.approval`；审批入口必须搬到 dsh 插件，并改 host 控制平面 | P1.5；先过 Task 0 第 3 条 |
| `/tenon/intents` 的 poll/ack 鉴权 | 改 host 控制平面，牵动现有 `testmode` 表驱动用例 | P1.5（先作为部署前提） |
| `pending_list` / `audit_query` 的 Inline & Spill 收窄 | 属 MCP 返回契约，独立可测 | P1.5 |
| 事件入口 Fast Path 过滤 | 改 Go agent 事件管线，需要事件规则表 | P4 |
| Watchdog 只读巡检 | 新的只读组件 + 报告落盘格式 | P4 |
| `request_submit` 改为异步工具（`start/stop` 形态） | 会改 MCP 工具面的同步语义，且与 notifier 的职责边界重叠，需要先定契约 | 待评估（见 spec §11 决策 13 的取舍） |
| P2 amend / P3 补偿 | 见 spec 分期表 | 各自独立计划 |
| 首条通知的会话归属（`request_submit` 回传 `origin.sessionId`） | 要同时改 host 与 MCP 契约，单会话 P1 用不到 | P1.5 |
| `pending_list` 的 `planId`/`full` 参数 | 只有 amend 需要读 `steps[i].args` | P1.5 |
| `POST /tenon/notices` 端点与 `refId` 订阅键 | 为 Watchdog 报告与无 planId 的帧服务 | P4 |
| UI 未读徽标 | 前端改动，不影响链路正确性 | P1.5 |

## 与前两轮评审的对应关系

| 评审发现 | 本计划怎么处理 |
|---|---|
| 通知冒充人类来源（`{kind:'user'}`） | Task 4/5 改 `plugin` + `form:'notice'`；Task 8 的回填只看 `kind==='user'` |
| 无条件 `followup` 打断对话 | Task 5 忙/闲分流；Task 7 调用点；验收场景 7 |
| 无唤醒预算（可自激） | Task 5 预算 + Task 8 人类输入回填；验收场景 8 |
| `sessionId`/唯一 root 脆弱 | Task 6 订阅表 + Task 7 `resolveAgent`；验收场景 4 |
| 超 batch 丢消息 | Task 4 `picked` 契约 + Task 7 前缀推进；Task 7 第 2 条测试 |
| 同 planId 折叠丢帧 | Task 7 不折叠；第 3 条测试 |
| ack 失败游标不动 | Task 7 `delivered`/`cursor` 分离 + 第 7 条测试 |
| 无 in-flight 闸 | Task 7 `running` 守卫 |
| `stateFile` 相对路径 | Task 2 `resolve()` + 缺省放 `~/.dsh` |
| 安装脚本 `Remove-Item -Recurse` 删源码 | Task 9 改 `Directory.Delete(..., $false)` + 非链接拒删 |
