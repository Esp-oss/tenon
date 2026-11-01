# 只在 dsh 对话里完成业务（P0+P1）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用户只在 dsh 对话里完成查、写、批/驳、审计；异步的待批与执行结果由一个新的 dsh 通知插件主动推回当前会话。页面保持只读，Go agent 继续做常驻执行者。

**Architecture:** 新增 `packages/dsh-plugin-tenon-notifier`：一个不注册模型工具的 cordis 插件，`inject: ['agents']`，轮询 host 的 `/tenon/intents/poll` 与 `/tenon/pending`，用 `ctx.agents.get(id)`/`roots()` 找到会话并 `agent.followup()`。写/批/审计仍由现有 `tenon-mcp` + Go agent + host 完成，本计划不改执行权与裁决层。

**Tech Stack:** TypeScript（Node 22，ESM）、`node:test`、cordis 插件、原生 `fetch`。不新增运行时依赖（消息形状用结构化对象，不引 `@deepseek-ai/dsh-llm`）。

**Scope:** 本计划只覆盖 spec 的 P0（通知通道）与 P1（对话闭环 MVP）。P2 amend、P3 补偿是独立计划，不在本文件里实现。

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `packages/dsh-plugin-tenon-notifier/package.json` | 包定义、build/test 脚本；无运行时依赖 |
| `packages/dsh-plugin-tenon-notifier/tsconfig.json` | 复用根 `tsconfig.base.json`，输出 `dist` |
| `packages/dsh-plugin-tenon-notifier/src/types.ts` | 传输/通知/消息的窄接口 |
| `packages/dsh-plugin-tenon-notifier/src/config.ts` | 配置校验与默认值 |
| `packages/dsh-plugin-tenon-notifier/src/host.ts` | host 登录、intents poll/ack、pending 列表 |
| `packages/dsh-plugin-tenon-notifier/src/format.ts` | 帧 → 人话、批量消息、UserMessage 形状 |
| `packages/dsh-plugin-tenon-notifier/src/notifier.ts` | 轮询、去重、节流、唤醒、游标持久化 |
| `packages/dsh-plugin-tenon-notifier/src/index.ts` | cordis 插件入口：`name`/`inject`/`apply` |
| `packages/dsh-plugin-tenon-notifier/src/*.test.ts` | 单测（node:test） |
| `scripts/install-dsh-notifier.ps1` | 建 profile junction、打印 YAML 片段、可导入性自检 |
| `scripts/notifier-smoke.ps1` | 往 host 投一条 intent，用于 dsh 里的肉眼冒烟 |
| `docs/dsh-chat-only.md` | 安装、配置、验收清单 |

根 `package.json` 的 `typecheck` 与 `test` 要加上这个包。

---

## Task 1: 包脚手架与工作区脚本

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/package.json`
- Create: `packages/dsh-plugin-tenon-notifier/tsconfig.json`
- Create: `packages/dsh-plugin-tenon-notifier/src/types.ts`
- Create: `packages/dsh-plugin-tenon-notifier/src/index.ts`（编译用入口桩，Task 6 替换）
- Modify: `package.json`（根；`typecheck`、`test`）

- [ ] **Step 1: 写包定义**

```json
{
  "name": "@tenon/dsh-plugin-tenon-notifier",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "description": "把 Tenon 的待批/执行结果主动推进 dsh 对话；不注册模型工具，不执行任何业务",
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
  outcome?: string;
  message?: string;
}

/** Host 公告板的一行（只声明本插件用到的字段）。 */
export interface PendingItem {
  planId: string;
  state?: string;
  summary?: string;
  requiredRoles?: string[];
}

/** 一条待推送的通知；id 用于跨来源去重。 */
export interface Notification {
  id: string;
  kind: 'pending' | 'terminal' | 'intent';
  planId?: string;
  text: string;
}

/**
 * 与 dsh-llm UserMessage 同形的最小子集。
 *
 * 本包不 import `@deepseek-ai/dsh-llm`（不为一条消息引整个运行时依赖），
 * 但形状是照 `UserMessage = { id, role:'user', content, source }` 抄的，不是猜的。
 */
export interface WakeMessage {
  id: string;
  role: 'user';
  content: Array<{ type: 'text'; text: string }>;
  source: { kind: 'user' };
}

/** 会话里 agent 的最小结构：只用到 followup。 */
export interface WakeAgent {
  followup(message: WakeMessage): void;
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
  throw new Error('notifier 尚未实现（Task 6 接管）');
}
```

- [ ] **Step 5: 根脚本加上新包**

把根 `package.json` 的两行改成（注意 JSON 转义）：

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

- [ ] **Step 7: 提交**

```bash
git add package.json pnpm-lock.yaml packages/dsh-plugin-tenon-notifier
git commit -m "feat(notifier): 脚手架与工作区脚本"
```

---

## Task 2: 配置校验

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/config.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/config.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/config.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import { DEFAULT_BATCH, DEFAULT_INTERVAL_MS, DEFAULT_MIN_GAP_MS, normalizeConfig } from './config.js';

test('token 配置：只给 token 即可，默认值补全', () => {
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080/', token: 'tok' });
  assert.equal(config.hostURL, 'http://127.0.0.1:8080');
  assert.equal(config.token, 'tok');
  assert.equal(config.intervalMs, DEFAULT_INTERVAL_MS);
  assert.equal(config.batch, DEFAULT_BATCH);
  assert.equal(config.minGapMs, DEFAULT_MIN_GAP_MS);
  assert.equal(config.pendingScan, true);
  assert.equal(config.stateFile, 'out/dsh-tenon-notifier/state.json');
});

test('账号配置：user+password 也可；缺一组就拒绝', () => {
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080', user: 'alice', password: 'alice-dev' });
  assert.equal(config.user, 'alice');
  assert.equal(config.password, 'alice-dev');
  assert.throws(() => normalizeConfig({ hostURL: 'http://127.0.0.1:8080', user: 'alice' }), /token|user/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://127.0.0.1:8080' }), /token|user/);
});

test('坏配置：URL 协议、数字下限、类型都拒绝', () => {
  assert.throws(() => normalizeConfig({ hostURL: 'ftp://x', token: 't' }), /http/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', intervalMs: 100 }), /intervalMs/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', batch: 0 }), /batch/);
  assert.throws(() => normalizeConfig({ hostURL: 'http://x', token: 't', pendingScan: 'yes' }), /pendingScan/);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: FAIL，`Cannot find module './config.js'` 或 `normalizeConfig is not a function`。

- [ ] **Step 3: 实现配置**

`src/config.ts`：

```ts
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
}

export const DEFAULT_INTERVAL_MS = 2000;
export const DEFAULT_BATCH = 5;
export const DEFAULT_MIN_GAP_MS = 2000;
export const DEFAULT_TIMEOUT_MS = 10_000;
export const DEFAULT_STATE_FILE = 'out/dsh-tenon-notifier/state.json';

function optionalString(raw: Record<string, unknown>, key: string): string | undefined {
  const value = raw[key];
  if (value === undefined || value === null) return undefined;
  if (typeof value !== 'string') throw new Error(`notifier 配置 ${key} 必须是字符串`);
  const trimmed = value.trim();
  return trimmed.length === 0 ? undefined : trimmed;
}

function numberField(raw: Record<string, unknown>, key: string, fallback: number, min: number): number {
  const value = raw[key];
  if (value === undefined || value === null) return fallback;
  if (typeof value !== 'number' || !Number.isFinite(value) || value < min) {
    throw new Error(`notifier 配置 ${key} 必须是 >= ${min} 的数字`);
  }
  return value;
}

/** 校验并补全配置；坏值直接抛错（插件加载失败比静默跑错好）。 */
export function normalizeConfig(raw: unknown): NotifierConfig {
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
  const hostURL = rawHost.replace(/\/+$/, '');

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

  return {
    hostURL,
    ...(token === undefined ? {} : { token }),
    ...(user === undefined ? {} : { user }),
    ...(password === undefined ? {} : { password }),
    sessionId: optionalString(record, 'sessionId'),
    intervalMs: numberField(record, 'intervalMs', DEFAULT_INTERVAL_MS, 500),
    batch: numberField(record, 'batch', DEFAULT_BATCH, 1),
    minGapMs: numberField(record, 'minGapMs', DEFAULT_MIN_GAP_MS, 0),
    timeoutMs: numberField(record, 'timeoutMs', DEFAULT_TIMEOUT_MS, 500),
    stateFile: optionalString(record, 'stateFile') ?? DEFAULT_STATE_FILE,
    pendingScan: pendingRaw === undefined ? true : pendingRaw,
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: `# pass 3`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/config.ts packages/dsh-plugin-tenon-notifier/src/config.test.ts
git commit -m "feat(notifier): 配置校验与默认值"
```

---

## Task 3: Host 客户端（登录、poll、ack、pending）

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

function fakeFetch(
  handler: (url: string, init: { method: string; headers: Record<string, string>; body?: string } | undefined) => { status: number; body: unknown },
): { fetch: FetchLike; calls: Array<{ url: string; init: { method: string; headers: Record<string, string>; body?: string } | undefined }> } {
  const calls: Array<{ url: string; init: { method: string; headers: Record<string, string>; body?: string } | undefined }> = [];
  const fetch: FetchLike = async (url, init) => {
    calls.push({ url, init });
    const reply = handler(url, init);
    return { status: reply.status, text: async () => JSON.stringify(reply.body) };
  };
  return { fetch, calls };
}

test('账号登录只发生一次；poll/ack/pending 带 Bearer 与 Tenon-Version', async () => {
  const { fetch, calls } = fakeFetch((url) => {
    if (url.endsWith('/tenon/session')) {
      return { status: 200, body: { token: 'tok-agent', exp: 4_000_000_000 } };
    }
    if (url.includes('/tenon/intents/poll')) {
      return { status: 200, body: { frames: [{ seq: 7, frame: { intentId: 'int_1', planId: 'pln_1', outcome: 'completed', message: 'ok' } }], nextCursor: 7 } };
    }
    if (url.endsWith('/tenon/intents/ack')) return { status: 200, body: { ok: true } };
    if (url.endsWith('/tenon/pending')) {
      return { status: 200, body: { items: [{ planId: 'pln_1', state: 'awaiting_approval', summary: '扣 12 件' }] } };
    }
    return { status: 404, body: { tenonError: { message: 'no route' } } };
  });
  const client = createHostClient(
    normalizeConfig({ hostURL: 'http://127.0.0.1:8080', user: 'alice', password: 'alice-dev' }),
    fetch,
  );

  const polled = await client.pollIntents(0);
  assert.equal(polled.nextCursor, 7);
  assert.equal(polled.frames[0]?.frame.planId, 'pln_1');
  await client.ackIntents(7);
  const pending = await client.listPending();
  assert.equal(pending[0]?.planId, 'pln_1');

  const logins = calls.filter((call) => call.url.endsWith('/tenon/session'));
  assert.equal(logins.length, 1, '同一实例只登录一次');
  const pollCall = calls.find((call) => call.url.includes('/tenon/intents/poll'));
  const headers = pollCall?.init?.headers ?? {};
  assert.equal(headers['authorization'], 'Bearer tok-agent');
  assert.equal(headers['tenon-version'], '0.1');
});

test('静态 token：不登录；HTTP 非 2xx 抛错并带响应片段', async () => {
  const { fetch, calls } = fakeFetch(() => ({ status: 403, body: { tenonError: { code: 'DENIED', message: 'no' } } }));
  const client = createHostClient(normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 'tok-static' }), fetch);
  await assert.rejects(() => client.pollIntents(0), /403/);
  assert.equal(calls.some((call) => call.url.endsWith('/tenon/session')), false);
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

export interface HostClient {
  pollIntents(after: number): Promise<{ frames: IntentEntry[]; nextCursor: number }>;
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
    async pollIntents(after: number): Promise<{ frames: IntentEntry[]; nextCursor: number }> {
      const reply = await request('GET', `/tenon/intents/poll?after=${after}`);
      expectOK(reply, 'GET /tenon/intents/poll');
      const body = JSON.parse(reply.text) as {
        frames?: Array<{ seq?: unknown; frame?: unknown }>;
        nextCursor?: unknown;
      };
      const frames: IntentEntry[] = [];
      for (const entry of body.frames ?? []) {
        if (typeof entry.seq !== 'number' || typeof entry.frame !== 'object' || entry.frame === null) continue;
        frames.push({ seq: entry.seq, frame: entry.frame as IntentFrame });
      }
      const nextCursor = typeof body.nextCursor === 'number' ? body.nextCursor : after;
      return { frames, nextCursor };
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
Expected: `# pass 5`（3 条配置 + 2 条 host）、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/host.ts packages/dsh-plugin-tenon-notifier/src/host.test.ts
git commit -m "feat(notifier): host 登录、intent 轮询/ack 与 pending 列表"
```

---

## Task 4: 人话格式化与唤醒消息

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/format.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/format.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/format.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import {
  composeWakeText,
  makeWakeMessage,
  notificationFromIntent,
  notificationFromPending,
  notificationFromTerminal,
} from './format.js';

test('待批卡：人话 + 角色；非 awaiting 不生成', () => {
  const item = { planId: 'pln_1', state: 'awaiting_approval', summary: '扣减 STD-1001 库存 12 件', requiredRoles: ['role:warehouse_manager'] };
  const notification = notificationFromPending(item);
  assert.ok(notification);
  assert.match(notification.text, /pln_1 待批/);
  assert.match(notification.text, /12 件/);
  assert.match(notification.text, /warehouse_manager/);
  assert.equal(notificationFromPending({ ...item, state: 'done' }), null);
});

test('终态与 intent：已执行/驳回/未执行文案', () => {
  assert.match(notificationFromTerminal({ planId: 'pln_1', state: 'done', summary: '扣减 12 件' })?.text ?? '', /已执行/);
  assert.match(notificationFromTerminal({ planId: 'pln_1', state: 'rejected' })?.text ?? '', /已驳回/);
  assert.match(
    notificationFromIntent({ intentId: 'int_1', planId: 'pln_1', outcome: 'needs_human', message: '策略要求审批' })?.text ?? '',
    /未执行.*策略要求审批/,
  );
  assert.equal(notificationFromIntent({ intentId: 'int_2', outcome: 'ignored' }), null);
});

test('批量消息：带免责声明、编号与后续提示；UserMessage 形状正确', () => {
  const pending = notificationFromPending({ planId: 'pln_1', state: 'awaiting_approval', summary: '扣 12 件' })!;
  const done = notificationFromIntent({ intentId: 'int_1', planId: 'pln_2', outcome: 'completed', message: '库存 108→96' })!;
  const text = composeWakeText([pending, done], 5);
  assert.match(text, /仅通知，不要自动批准/);
  assert.match(text, /1\. pln_1/);
  assert.match(text, /2\. pln_2/);
  assert.match(text, /列待批/);

  const message = makeWakeMessage(text);
  assert.equal(message.role, 'user');
  assert.equal(message.source.kind, 'user');
  assert.equal(message.content[0]?.type, 'text');
  assert.match(message.content[0]?.text ?? '', /pln_1/);
  assert.ok(message.id.length > 0);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: FAIL，`format.js` 不存在。

- [ ] **Step 3: 实现格式化**

`src/format.ts`：

```ts
import { randomUUID } from 'node:crypto';
import type { IntentFrame, Notification, PendingItem, WakeMessage } from './types.js';

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

export function notificationFromPending(item: PendingItem): Notification | null {
  if (item.state !== 'awaiting_approval') return null;
  const roles = item.requiredRoles && item.requiredRoles.length > 0
    ? `（需要角色：${item.requiredRoles.join(' / ')}）`
    : '';
  return {
    id: `pending:${item.planId}`,
    kind: 'pending',
    planId: item.planId,
    text: `${item.planId} 待批：${item.summary ?? '（无摘要）'}${roles}`,
  };
}

export function notificationFromTerminal(item: PendingItem): Notification | null {
  if (!item.state || item.state === 'awaiting_approval') return null;
  const summary = item.summary ? `：${item.summary}` : '';
  return {
    id: `terminal:${item.planId}:${item.state}`,
    kind: 'terminal',
    planId: item.planId,
    text: `${item.planId} ${terminalWord(item.state)}${summary}`,
  };
}

export function notificationFromIntent(frame: IntentFrame): Notification | null {
  if (!frame.outcome) return null;
  const plan = frame.planId ?? '（无计划号）';
  const detail = frame.message && frame.message.trim().length > 0 ? `：${frame.message.trim()}` : '';
  let text: string;
  switch (frame.outcome) {
    case 'completed':
      text = `${plan} 已执行${detail}`;
      break;
    case 'needs_human':
      text = `${plan} 未执行${detail}`;
      break;
    case 'failed':
      text = `${plan} 执行失败${detail}`;
      break;
    case 'rejected':
      text = `${plan} 已驳回${detail}`;
      break;
    default:
      return null;
  }
  return {
    id: `intent:${frame.intentId ?? `${plan}:${frame.outcome}`}`,
    kind: 'intent',
    planId: frame.planId,
    text,
  };
}

export function composeWakeText(notifications: Notification[], batch: number): string {
  const picked = notifications.slice(0, Math.max(1, batch));
  const lines = picked.map((notification, index) => `${index + 1}. ${notification.text}`);
  const more = notifications.length > picked.length ? `（还有 ${notifications.length - picked.length} 条，稍后补推）` : '';
  return [
    '【Tenon 通知】仅通知，不要自动批准。',
    `共 ${notifications.length} 条，先看这 ${picked.length} 条${more}：`,
    ...lines,
    '要看详情可以说「列待批」或「查审计 <planId>」。',
  ].join('\n');
}

export function makeWakeMessage(text: string): WakeMessage {
  return {
    id: randomUUID(),
    role: 'user',
    content: [{ type: 'text', text }],
    source: { kind: 'user' },
  };
}
```

- [ ] **Step 4: 跑测试，确认通过**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: `# pass 8`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/format.ts packages/dsh-plugin-tenon-notifier/src/format.test.ts
git commit -m "feat(notifier): 待批/结果格式化与唤醒消息"
```

---

## Task 5: 轮询器（去重、节流、会话解析、游标）

**Files:**
- Create: `packages/dsh-plugin-tenon-notifier/src/notifier.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/notifier.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/notifier.test.ts`：

```ts
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { normalizeConfig } from './config.js';
import type { HostClient } from './host.js';
import { createNotifier } from './notifier.js';
import type { WakeAgent, WakeMessage } from './types.js';

function fakeAgent(sink: WakeMessage[], fail = false): WakeAgent {
  return {
    followup(message: WakeMessage): void {
      if (fail) throw new Error('agent disposed');
      sink.push(message);
    },
  };
}

function hostWith(entries: {
  intents?: Array<{ seq: number; frame: Record<string, unknown> }>;
  nextCursor?: number;
  pending?: Array<Record<string, unknown>>;
}): HostClient & { acked: number[]; listCalls: number } {
  let acked: number[] = [];
  let listCalls = 0;
  return {
    get acked() {
      return acked;
    },
    get listCalls() {
      return listCalls;
    },
    async pollIntents() {
      return {
        frames: (entries.intents ?? []).map((entry) => ({ seq: entry.seq, frame: entry.frame })),
        nextCursor: entries.nextCursor ?? 0,
      };
    },
    async ackIntents(upto: number) {
      acked.push(upto);
    },
    async listPending() {
      listCalls += 1;
      return (entries.pending ?? []).map((item) => ({
        planId: String(item['planId']),
        state: typeof item['state'] === 'string' ? item['state'] : undefined,
        summary: typeof item['summary'] === 'string' ? item['summary'] : undefined,
        requiredRoles: Array.isArray(item['requiredRoles']) ? item['requiredRoles'].filter((role): role is string => typeof role === 'string') : undefined,
      }));
    },
  };
}

test('待批 + 结果：一次唤醒、批量消息、ack 与游标落盘', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'tenon-notifier-'));
  const stateFile = path.join(dir, 'state.json');
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 'tok', sessionId: 'sess-1', stateFile, minGapMs: 0 });
  const woken: WakeMessage[] = [];
  const host = hostWith({
    intents: [{ seq: 3, frame: { intentId: 'int_1', planId: 'pln_1', outcome: 'completed', message: '库存 108→96' } }],
    nextCursor: 3,
    pending: [{ planId: 'pln_2', state: 'awaiting_approval', summary: '扣 8 件', requiredRoles: ['role:warehouse_manager'] }],
  });
  const notifier = createNotifier({
    host,
    agents: { get: (id) => (id === 'sess-1' ? fakeAgent(woken) : undefined), roots: () => [] },
    config,
  });

  const result = await notifier.tick();
  assert.equal(result.woken, true);
  assert.equal(result.events, 2);
  assert.equal(woken.length, 1);
  assert.match(woken[0]?.content[0]?.text ?? '', /pln_1/);
  assert.match(woken[0]?.content[0]?.text ?? '', /pln_2/);
  assert.deepEqual(host.acked, [3]);
  assert.deepEqual(JSON.parse(readFileSync(stateFile, 'utf8')), { cursor: 3 });

  const again = await notifier.tick();
  assert.equal(again.woken, false, '同一批 pending 不应重复唤醒');
});

test('没有会话句柄：不 ack、不落游标；会话出现后重试成功', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'tenon-notifier-'));
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 'tok', stateFile: path.join(dir, 'state.json'), minGapMs: 0 });
  const woken: WakeMessage[] = [];
  const host = hostWith({ intents: [{ seq: 1, frame: { intentId: 'int_1', planId: 'pln_1', outcome: 'completed' } }], nextCursor: 1 });
  let agent: WakeAgent | undefined;
  const notifier = createNotifier({
    host,
    agents: { get: () => agent, roots: () => (agent ? [agent] : []) },
    config,
  });

  assert.equal((await notifier.tick()).woken, false);
  assert.deepEqual(host.acked, []);
  agent = fakeAgent(woken);
  assert.equal((await notifier.tick()).woken, true);
  assert.deepEqual(host.acked, [1]);
});

test('新实例重新提醒仍待批的卡（重启后不丢）', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'tenon-notifier-'));
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 'tok', sessionId: 'sess-1', stateFile: path.join(dir, 'state.json'), minGapMs: 0 });
  const host = hostWith({ pending: [{ planId: 'pln_1', state: 'awaiting_approval', summary: '扣 12 件' }] });
  const firstWoken: WakeMessage[] = [];
  const first = createNotifier({
    host,
    agents: { get: () => fakeAgent(firstWoken), roots: () => [] },
    config,
  });
  assert.equal((await first.tick()).woken, true);

  const secondWoken: WakeMessage[] = [];
  const second = createNotifier({
    host,
    agents: { get: () => fakeAgent(secondWoken), roots: () => [] },
    config,
  });
  assert.equal((await second.tick()).woken, true, '新进程/新实例要重新提醒仍待批的卡');
  assert.match(secondWoken[0]?.content[0]?.text ?? '', /pln_1/);
});

test('followup 抛错：不 ack；恢复后重试', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'tenon-notifier-'));
  const config = normalizeConfig({ hostURL: 'http://127.0.0.1:8080', token: 'tok', sessionId: 'sess-1', stateFile: path.join(dir, 'state.json'), minGapMs: 0 });
  const host = hostWith({ intents: [{ seq: 2, frame: { intentId: 'int_1', planId: 'pln_1', outcome: 'completed' } }], nextCursor: 2 });
  let fail = true;
  const notifier = createNotifier({
    host,
    agents: { get: () => ({ followup: () => { if (fail) throw new Error('boom'); } }), roots: () => [] },
    config,
  });
  const first = await notifier.tick();
  assert.equal(first.woken, false);
  assert.match(first.notes.join('\n'), /唤醒失败/);
  assert.deepEqual(host.acked, []);
  fail = false;
  assert.equal((await notifier.tick()).woken, true);
  assert.deepEqual(host.acked, [2]);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: FAIL，`createNotifier` 不存在。

- [ ] **Step 3: 实现轮询器**

`src/notifier.ts`：

```ts
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { NotifierConfig } from './config.js';
import {
  composeWakeText,
  makeWakeMessage,
  notificationFromIntent,
  notificationFromPending,
  notificationFromTerminal,
} from './format.js';
import type { HostClient } from './host.js';
import type { AgentRegistry, Notification, PendingItem, WakeAgent } from './types.js';

export interface NotifierOptions {
  host: HostClient;
  agents: AgentRegistry;
  config: NotifierConfig;
  log?: (line: string) => void;
  now?: () => number;
}

export interface TickResult {
  woken: boolean;
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

function readCursor(path: string): number {
  try {
    const raw = JSON.parse(readFileSync(path, 'utf8')) as { cursor?: unknown };
    return typeof raw.cursor === 'number' && Number.isFinite(raw.cursor) ? raw.cursor : 0;
  } catch {
    return 0;
  }
}

function writeCursor(path: string, cursor: number): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify({ cursor }), 'utf8');
}

export function createNotifier(options: NotifierOptions): Notifier {
  const { host, agents, config } = options;
  const log = options.log ?? (() => undefined);
  const now = options.now ?? (() => Date.now());

  let cursor = readCursor(config.stateFile);
  let lastWake = 0;
  let timer: ReturnType<typeof setInterval> | null = null;
  const notified = new Set<string>();
  const terminalSeen = new Map<string, string>();

  function resolveAgent(): WakeAgent | undefined {
    if (config.sessionId) return agents.get(config.sessionId);
    const roots = agents.roots();
    return roots.length === 1 ? roots[0] : undefined;
  }

  async function tick(): Promise<TickResult> {
    const notes: string[] = [];
    const candidates: Notification[] = [];
    let pendingStates: Array<{ id: string; state: string }> = [];
    let nextCursor = cursor;

    try {
      const polled = await host.pollIntents(cursor);
      nextCursor = polled.nextCursor;
      for (const entry of polled.frames) {
        const notification = notificationFromIntent(entry.frame);
        if (notification && !notified.has(notification.id)) candidates.push(notification);
      }
    } catch (error) {
      notes.push(`poll intents 失败：${error instanceof Error ? error.message : String(error)}`);
      return { woken: false, events: 0, notes };
    }

    if (config.pendingScan) {
      try {
        const items: PendingItem[] = await host.listPending();
        for (const item of items) {
          const state = item.state ?? '';
          if (state === 'awaiting_approval') {
            const notification = notificationFromPending(item);
            if (notification && !notified.has(notification.id)) candidates.push(notification);
            pendingStates.push({ id: item.planId, state });
            continue;
          }
          const previous = terminalSeen.get(item.planId);
          if (previous === undefined || previous === state) {
            terminalSeen.set(item.planId, state);
            continue;
          }
          const notification = notificationFromTerminal(item);
          if (notification && !notified.has(notification.id)) candidates.push(notification);
          pendingStates.push({ id: item.planId, state });
        }
      } catch (error) {
        notes.push(`pending 扫描失败：${error instanceof Error ? error.message : String(error)}`);
      }
    }

    // 同一 plan 的 intent 与 pending 可能同时到；按 planId 去重，先到的先用。
    const seenPlans = new Set<string>();
    const unique = candidates.filter((notification) => {
      if (!notification.planId) return true;
      if (seenPlans.has(notification.planId)) return false;
      seenPlans.add(notification.planId);
      return true;
    });

    if (unique.length === 0) return { woken: false, events: 0, notes };

    if (now() - lastWake < config.minGapMs) {
      notes.push(`节流：距上次唤醒不足 ${config.minGapMs}ms，本轮不推送`);
      return { woken: false, events: unique.length, notes };
    }

    const agent = resolveAgent();
    if (!agent) {
      notes.push('没有可唤醒的会话（配置 sessionId，或让 roots() 恰好一个）');
      return { woken: false, events: unique.length, notes };
    }

    const text = composeWakeText(unique, config.batch);
    try {
      agent.followup(makeWakeMessage(text));
    } catch (error) {
      notes.push(`唤醒失败：${error instanceof Error ? error.message : String(error)}`);
      return { woken: false, events: unique.length, notes, text };
    }

    for (const notification of unique) notified.add(notification.id);
    for (const entry of pendingStates) terminalSeen.set(entry.id, entry.state);

    let acked = true;
    try {
      await host.ackIntents(nextCursor);
    } catch (error) {
      acked = false;
      notes.push(`ack 失败（下轮会重复唤醒，不丢帧）：${error instanceof Error ? error.message : String(error)}`);
    }
    if (acked && nextCursor > cursor) {
      cursor = nextCursor;
      try {
        writeCursor(config.stateFile, cursor);
      } catch (error) {
        notes.push(`游标落盘失败：${error instanceof Error ? error.message : String(error)}`);
      }
    }
    lastWake = now();
    log(`notifier 已推送 ${unique.length} 条（cursor=${cursor}）`);
    return { woken: true, events: unique.length, notes, text };
  }

  return {
    tick,
    start(): void {
      if (timer) return;
      const run = (): void => {
        void tick().catch((error: unknown) => {
          log(`notifier tick 异常：${error instanceof Error ? error.message : String(error)}`);
        });
      };
      timer = setInterval(run, config.intervalMs);
      run();
    },
    stop(): void {
      if (timer) {
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

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: `# pass 12`、`# fail 0`。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/notifier.ts packages/dsh-plugin-tenon-notifier/src/notifier.test.ts
git commit -m "feat(notifier): 轮询、去重、节流、会话唤醒与游标"
```

---

## Task 6: cordis 插件入口与整体构建

**Files:**
- Modify: `packages/dsh-plugin-tenon-notifier/src/index.ts`
- Test: `packages/dsh-plugin-tenon-notifier/src/index.test.ts`

- [ ] **Step 1: 先写失败测试**

`src/index.test.ts`：

```ts
import assert from 'node:assert/strict';
import test from 'node:test';
import { apply, inject, name } from './index.js';

test('插件身份：name/inject 固定，不注册模型工具', () => {
  assert.equal(name, 'tenon-notifier');
  assert.deepEqual([...inject], ['agents']);
});

test('apply：无效配置抛错；有效配置注册 effect 并可停', () => {
  let disposed = false;
  let started = false;
  const ctx = {
    agents: { get: () => undefined, roots: () => [] },
    effect(fn: () => () => void): unknown {
      const stop = fn();
      started = true;
      assert.equal(typeof stop, 'function');
      stop();
      disposed = true;
      return undefined;
    },
  };
  assert.throws(() => apply(ctx, { hostURL: 'ftp://x', token: 't' }), /http/);

  apply(ctx, {
    hostURL: 'http://127.0.0.1:9',
    token: 'tok',
    intervalMs: 600_000,
    stateFile: 'out/dsh-tenon-notifier/test-state.json',
  });
  assert.equal(started, true);
  assert.equal(disposed, true);
});
```

- [ ] **Step 2: 跑测试，确认失败**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
```
Expected: FAIL，入口桩的 `apply` 抛“尚未实现”。

- [ ] **Step 3: 实现入口**

`src/index.ts`：

```ts
import { normalizeConfig } from './config.js';
import { createHostClient } from './host.js';
import { createNotifier } from './notifier.js';
import type { AgentRegistry } from './types.js';

export const name = 'tenon-notifier';
export const inject = ['agents'] as const;

export interface NotifierContext {
  agents: AgentRegistry;
  effect?: (fn: () => () => void) => unknown;
}

/**
 * cordis 插件入口。
 *
 * 注意：本插件**不注册任何模型工具**。它只读 host、唤醒会话；
 * 写/批/审计仍由 `tenon-mcp` 与 Go agent 完成。
 */
export function apply(ctx: NotifierContext, raw: unknown): void {
  const config = normalizeConfig(raw);
  const host = createHostClient(config);
  const notifier = createNotifier({
    host,
    agents: ctx.agents,
    config,
    log: (line) => console.warn(line),
  });

  const start = (): void => notifier.start();
  const stop = (): void => notifier.stop();
  if (typeof ctx.effect === 'function') {
    ctx.effect(() => {
      start();
      return stop;
    });
  } else {
    start();
  }
}
```

- [ ] **Step 4: 跑测试与全仓检查**

Run:
```bash
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
pnpm run typecheck
pnpm run test
```
Expected: 新包 `# pass 14`、`# fail 0`；`typecheck` 退出 0；全仓 test 退出 0。

- [ ] **Step 5: 提交**

```bash
git add packages/dsh-plugin-tenon-notifier/src/index.ts packages/dsh-plugin-tenon-notifier/src/index.test.ts
git commit -m "feat(notifier): cordis 插件入口"
```

---

## Task 7: profile 安装脚本与冒烟脚本

**Files:**
- Create: `scripts/install-dsh-notifier.ps1`
- Create: `scripts/notifier-smoke.ps1`

- [ ] **Step 1: 写安装脚本**

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
if (Test-Path $link) {
    $target = @((Get-Item $link).Target)[0]
    if ($target -and ((Resolve-Path -LiteralPath $target).Path -ne (Resolve-Path -LiteralPath $plugin).Path)) {
        throw "已存在指向其他目录的 junction：$link -> $target"
    }
    Remove-Item $link -Force -Recurse
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
'@
Write-Host ''
Write-Host '然后重启 dsh web；日志里应看到 notifier 的轮询/推送行。' -ForegroundColor DarkGray
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
    type      = 'tenon.intent'
    intentId  = $intentId
    planId    = 'pln_notifier_smoke'
    outcome   = 'completed'
    message   = 'notifier smoke：如果你在 dsh 里看到这条，通知链路是通的'
} | ConvertTo-Json -Compress

$reply = Invoke-RestMethod -Method Post -Uri "$hostUrl/tenon/intents" -ContentType 'application/json' -Headers @{
    Authorization  = "Bearer $($agent.token)"
    'Tenon-Version' = '0.1'
} -Body $body

Write-Host "已投递 intent $intentId；等 notifier 下一次轮询（<= 2s）。" -ForegroundColor Cyan
Write-Host '在 dsh 对话里应出现以「【Tenon 通知】」开头的消息。'
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

- [ ] **Step 5: 跑冒烟脚本**

Run:
```bash
pwsh -NoProfile -File scripts/notifier-smoke.ps1
```
Expected: dsh 对话里出现 `【Tenon 通知】仅通知，不要自动批准。`，包含 `notifier smoke`。

- [ ] **Step 6: 提交**

```bash
git add scripts/install-dsh-notifier.ps1 scripts/notifier-smoke.ps1
git commit -m "feat(notifier): profile 安装与 dsh 冒烟脚本"
```

---

## Task 8: 文档与 P1 验收

**Files:**
- Create: `docs/dsh-chat-only.md`
- Modify: `README.md`（DSH 段落加一行指向文档）

- [ ] **Step 1: 写使用文档**

`docs/dsh-chat-only.md`：

```markdown
# 只在 dsh 对话里完成业务（P0+P1）

## 组件

- `tenon-mcp`：读/预检/投请求/待批/审批/审计；
- `dsh-plugin-tenon-notifier`：轮询 host，把待批与结果推进当前会话；
- Go agent + host + modules：执行与裁决（不变）。

> 不要同时加载旧 `module-host` 插件：它有自己的 pending store 与 `tenon_action` 工具，会和 Go agent 形成两套状态。产品 profile 只挂 `tenon-mcp` + `tenon-notifier`。

## 安装

```bash
pnpm install
pnpm --filter @tenon/dsh-plugin-tenon-notifier run test
pnpm --filter @tenon/dsh-plugin-tenon-notifier run build
pwsh -NoProfile -File scripts/install-dsh-notifier.ps1
```

把脚本打印的 `tenon-notifier` 条目追加到 `~/.dsh/profiles/web/cordis.patch.yml`，重启 dsh web。

## 冒烟

```bash
pwsh -NoProfile -File scripts/dev-up.ps1 -NoDemoCard
pwsh -NoProfile -File scripts/dsh-up.ps1
pwsh -NoProfile -File scripts/notifier-smoke.ps1
```

对话里应出现 `【Tenon 通知】`。

## 验收（全部在 dsh 对话里）

1. 查：“STD-1001 还有多少” → 正确数值。
2. 写：“扣 12 件” → 待批通知 → 选“允许执行” → 库存 −12 → 完成通知 → 审计 `approvedBy=user:alice`。
3. 驳：同一请求选“驳回” → 库存不变 → 通知“未执行”。
4. 断线：发请求后关 dsh/重启 → 重开后看到待批通知 → 批 → 执行。
5. 事件：建报价单触发 `quote.completed` → Go agent 自动开回合落卡 → 对话通知 → 批准 → 库存/采购变化。
6. 负向：`action_invoke` 传写动作被拒；页面无写/批入口；notifier 没有模型工具。
```

- [ ] **Step 2: README 加一行**

在 README 的 DSH 段落末尾加：

```markdown
> **只在 dsh 对话里完成业务（P0+P1）**：安装 `dsh-plugin-tenon-notifier`，待批与执行结果会主动推回会话；步骤见 [`docs/dsh-chat-only.md`](docs/dsh-chat-only.md)。
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

按 `docs/dsh-chat-only.md` 的场景 1–6 在 dsh 里走一遍；截图/日志留在 `out/dsh-chat-p1/`（不入库）。至少确认：通知出现、批准后库存变化、审计 approvedBy、驳回不执行、事件回合通知。

- [ ] **Step 5: 提交**

```bash
git add docs/dsh-chat-only.md README.md
git commit -m "docs: dsh 对话闭环（P0+P1）使用与验收"
```

---

## 验收判据（P0+P1）

- `pnpm --filter @tenon/dsh-plugin-tenon-notifier run test` 全绿（配置/host/format/notifier/入口）。
- `pnpm check` 全绿；`go test ./...` 全绿；`scripts/e2e-dsh.ps1` 18 条全过。
- 活 dsh：`scripts/notifier-smoke.ps1` 投递后对话出现通知。
- 手工场景 1–6 全过，全程不打开看板/业务页。
- 负向：notifier 没有模型工具；页面无写/批入口；无 token 批准被拒。

## 不在本计划

- P2 amend（`pending_amend`、agent patch、强制重批）。
- P3 补偿（`request_compensation`、ReleaseStock 策略、逆计划）。
- P4 事件硬化、多用户 Q2、D1 迁移。
