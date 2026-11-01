// 确认轮询的用例（设计 §10.2 选项 A：只读不 ack）。
//
// 每条用例都盯两件事：
//   1. confirm 帧真的能把业务推动到底（或按角色拒绝）；
//   2. **从不 ack** —— 队列所有权不变，是这套方案能落地的全部理由。
import assert from 'node:assert/strict';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { createConfirmPoller } from './confirm.js';
import type { ModuleHostConfig } from './config.js';
import { startFakeHost, type FakeHandler } from './fake-host.js';
import { resetTokenCache } from './host-auth.js';
import { loadArchived, loadPending, savePending, type PendingPlan } from './pending.js';
import type { WakeEvent } from './wake.js';

const PLAN_ID = 'pln_AAAAAAAAAAAAAAAA';
const APPROVERS = ['role:warehouse_manager'];

const freshState = (): string => mkdtempSync(path.join(tmpdir(), 'tenon-confirm-'));

const config = (hostURL: string, stateDir: string): ModuleHostConfig => ({
  hostURL,
  agentID: 'ag_dsh_test',
  onBehalfOf: 'user:alice',
  timeoutMs: 3_000,
  stateDir,
  agentUser: 'agent',
  agentPassword: 'agent-dev',
});

function seedPending(stateDir: string, planId = PLAN_ID): PendingPlan {
  const plan: PendingPlan = {
    planId,
    module: 'inventory',
    action: 'DeductStock',
    args: { sku: 'STD-1001', qty: 12, referenceNo: 'REF-1' },
    inputHash: 'sha256:abc',
    idempotencyKey: `tenon:${planId}/s1`,
    requiredRoles: APPROVERS,
    policyId: 'pol-big',
    createdAt: '2026-10-29T00:00:00Z',
    expiresAt: '2099-01-01T00:00:00Z',
    state: 'awaiting_approval',
    auditSettled: true,
    dispatchedAuditId: 'aud_BBBBBBBBBBBBBBBBBBBB',
  };
  savePending(stateDir, plan);
  return plan;
}

/** 可变帧队列 + 记录所有请求的假宿主。 */
interface Harness {
  host: Awaited<ReturnType<typeof startFakeHost>>;
  frames: Array<{ seq: number; frame: Record<string, unknown> }>;
  actions: number;
}

async function harness(state: { policy?: 'approve' | 'deny' } = {}): Promise<Harness & { close: () => Promise<void> }> {
  const local = {
    frames: [] as Array<{ seq: number; frame: Record<string, unknown> }>,
    actions: 0,
  };
  const handler: FakeHandler = (request, origin) => {
    // 假宿主记录的是原始 url，带查询串（`/tenon/requests/poll?after=0`），
    // 所以按 pathname 路由，而不是整串比较。
    const route = `${request.method} ${new URL(request.path, 'http://placeholder').pathname}`;
    switch (route) {
      case 'POST /tenon/session':
        return { status: 200, body: { token: 'tok-agent', user: 'user:agent', roles: ['role:agent'], exp: 4_000_000_000 } };
      case 'GET /tenon/requests/poll':
        return { status: 200, body: { frames: local.frames, nextCursor: local.frames.length, dropped: 0 } };
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return state.policy === 'deny'
          ? { status: 200, body: { outcome: 'deny', decidedBy: ['pol-no'], approvers: [], reasons: ['BLOCKED'] } }
          : {
              status: 200,
              body: { outcome: 'approve', decidedBy: ['pol-big'], approvers: APPROVERS, reasons: [] },
            };
      case 'POST /tenon/actions/inventory.DeductStock:dry-run':
        return { status: 200, body: { dryRun: { status: 'ok' } } };
      case 'POST /tenon/audit':
        return { status: 200, body: { ok: true } };
      case 'POST /tenon/pending':
        return { status: 200, body: { ok: true } };
      case 'POST /tenon/actions/inventory.DeductStock':
        local.actions += 1;
        return { status: 200, body: { ok: true, onHand: 103 } };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  return {
    host,
    get frames() {
      return local.frames;
    },
    get actions() {
      return local.actions;
    },
    close: () => host.close(),
  } as Harness & { close: () => Promise<void> };
}

/** 造一条 confirm 帧（`by`/`roles` 在真实宿主里由 token 覆写，这里直接给定）。 */
function confirmFrame(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    type: 'tenon.confirm',
    confirmId: 'cfm_1',
    planId: PLAN_ID,
    decision: 'approve',
    by: 'user:bob',
    roles: APPROVERS,
    at: '2026-10-29T00:05:00Z',
    ...overrides,
  };
}

test('confirm approve + 有角色 → 真执行，审计 approvedBy 来自帧，并发出唤醒事件', async () => {
  resetTokenCache();
  const h = await harness();
  const stateDir = freshState();
  try {
    seedPending(stateDir);
    h.frames.push({ seq: 1, frame: confirmFrame() });

    const events: WakeEvent[] = [];
    const poller = createConfirmPoller({
      config: config(h.host.origin, stateDir),
      onExecuted: (event) => events.push(event),
    });
    const result = await poller.tick();

    assert.equal(result.confirms, 1);
    assert.equal(result.applied, 1);
    assert.equal(h.actions, 1);
    assert.equal(loadArchived(stateDir, PLAN_ID)?.state, 'done');
    assert.equal(loadArchived(stateDir, PLAN_ID)?.approvedBy, 'user:bob');

    // 唤醒事件只发一次，且带「谁批的」与执行结论——它是后台会话唯一能拿到的上下文。
    assert.equal(events.length, 1);
    assert.equal(events[0]!.planId, PLAN_ID);
    assert.equal(events[0]!.by, 'user:bob');
    assert.equal(events[0]!.decision, 'approve');
    assert.ok(events[0]!.detail.length > 0, '执行结论要带进唤醒消息');

    const audits = h.host.bodies('/tenon/audit') as Array<Record<string, unknown>>;
    // 步骤级 dispatched 由宿主在签票时写；插件只写 result，审批人必须落在 result 上。
    const stepResult = audits.find((record) => record['stepId'] === 's1' && record['stage'] === 'result');
    assert.equal((stepResult?.['decision'] as Record<string, unknown>)['approvedBy'], 'user:bob');
  } finally {
    await h.close();
  }
});

test('confirm approve 但角色不够 → 不执行，待批仍在（宿主验角色，但仍要校验够不够格）', async () => {
  resetTokenCache();
  const h = await harness();
  const stateDir = freshState();
  try {
    seedPending(stateDir);
    h.frames.push({ seq: 1, frame: confirmFrame({ by: 'user:carol', roles: ['role:sales'] }) });

    const events: WakeEvent[] = [];
    const poller = createConfirmPoller({
      config: config(h.host.origin, stateDir),
      onExecuted: (event) => events.push(event),
    });
    const result = await poller.tick();

    assert.equal(result.confirms, 1);
    assert.equal(result.applied, 0);
    assert.equal(events.length, 0, '没执行就没有唤醒事件');
    assert.equal(h.actions, 0, '角色不够就不该有动作请求');
    assert.equal(loadPending(stateDir, PLAN_ID)?.state, 'awaiting_approval');
    assert.ok(result.notes.some((note) => /不持有/.test(note)), `notes 应说明原因：${result.notes.join('｜')}`);
  } finally {
    await h.close();
  }
});

test('confirm reject → 归档 rejected，绝不执行', async () => {
  resetTokenCache();
  const h = await harness();
  const stateDir = freshState();
  try {
    seedPending(stateDir);
    h.frames.push({ seq: 1, frame: confirmFrame({ decision: 'reject' }) });

    const events: WakeEvent[] = [];
    const poller = createConfirmPoller({
      config: config(h.host.origin, stateDir),
      onExecuted: (event) => events.push(event),
    });
    const result = await poller.tick();

    assert.equal(result.applied, 1);
    assert.equal(events.length, 1, '驳回同样要唤醒：会话该知道「没办」');
    assert.equal(events[0]!.decision, 'reject');
    assert.equal(h.actions, 0);
    assert.equal(loadArchived(stateDir, PLAN_ID)?.state, 'rejected');
    assert.equal(loadPending(stateDir, PLAN_ID), null);

    const cards = h.host.bodies('/tenon/pending') as Array<Record<string, unknown>>;
    assert.equal(cards.at(-1)?.['state'], 'rejected', '公告板要覆盖成终态');
  } finally {
    await h.close();
  }
});

test('tenon.request 帧被忽略：不执行、不 ack，但游标照样前进', async () => {
  resetTokenCache();
  const h = await harness();
  const stateDir = freshState();
  try {
    seedPending(stateDir);
    h.frames.push({ seq: 1, frame: { type: 'tenon.request', text: '扣 12 件', by: 'user:alice' } });
    h.frames.push({ seq: 2, frame: { type: 'tenon.request', text: '顺便看看库存', by: 'user:alice' } });

    const poller = createConfirmPoller({ config: config(h.host.origin, stateDir) });
    const result = await poller.tick();

    assert.equal(result.confirms, 0);
    assert.equal(poller.cursor(), 2, '被忽略的帧也要推进游标，否则会一直重读');
    assert.equal(h.actions, 0);
    assert.equal(
      h.host.sequence().some((entry) => entry.includes('/tenon/requests/ack')),
      false,
      '绝不 ack：队列所有权不变',
    );
  } finally {
    await h.close();
  }
});

test('重放：重启后从头读旧帧，已归档就幂等返回，不重复执行', async () => {
  resetTokenCache();
  const h = await harness();
  const stateDir = freshState();
  try {
    seedPending(stateDir);
    h.frames.push({ seq: 1, frame: confirmFrame() });

    const events: WakeEvent[] = [];
    const first = createConfirmPoller({
      config: config(h.host.origin, stateDir),
      onExecuted: (event) => events.push(event),
    });
    await first.tick();
    assert.equal(h.actions, 1);
    assert.equal(events.length, 1, '首次执行发一次唤醒事件');

    // 新轮询器 = 新进程：游标归零，会重新读到同一帧。
    const replayEvents: WakeEvent[] = [];
    const replay = createConfirmPoller({
      config: config(h.host.origin, stateDir),
      onExecuted: (event) => replayEvents.push(event),
    });
    const again = await replay.tick();

    assert.equal(again.confirms, 1);
    assert.equal(again.applied, 0, '已归档的计划不该再产生动作');
    assert.equal(replayEvents.length, 0, '重放不该再唤醒一次（否则每次重启都刷一句）');
    assert.equal(events.length, 1, '首轮的唤醒事件仍是那一条');
    assert.equal(h.actions, 1, '动作只发生过一次');
    assert.equal(
      h.host.sequence().some((entry) => entry.includes('/tenon/requests/ack')),
      false,
    );
  } finally {
    await h.close();
  }
});

test('轮询失败：tick 不抛错，只把原因写进 notes', async () => {
  resetTokenCache();
  const host = await startFakeHost((request) => {
    const route = `${request.method} ${new URL(request.path, 'http://placeholder').pathname}`;
    if (route === 'POST /tenon/session') {
      return { status: 200, body: { token: 'tok-agent', exp: 4_000_000_000 } };
    }
    if (route === 'GET /tenon/requests/poll') {
      return { status: 503, body: { tenonError: { code: 'UNAVAILABLE', message: '宿主忙' } } };
    }
    return undefined;
  });
  const stateDir = freshState();
  try {
    const poller = createConfirmPoller({ config: config(host.origin, stateDir) });
    const result = await poller.tick();
    assert.equal(result.confirms, 0);
    assert.ok(result.notes.some((note) => /轮询失败/.test(note)));
  } finally {
    await host.close();
  }
});

test('未配置 agent 凭据：不轮询、不报错（功能关闭要真的是关闭）', async () => {
  resetTokenCache();
  const host = await startFakeHost(() => undefined);
  const stateDir = freshState();
  try {
    const poller = createConfirmPoller({
      config: { hostURL: host.origin, agentID: 'ag', onBehalfOf: 'user:alice', stateDir },
    });
    const result = await poller.tick();
    assert.equal(result.confirms, 0);
    assert.match(result.notes.join('｜'), /未配置 agent 凭据/);
    assert.deepEqual(host.sequence(), []);
  } finally {
    await host.close();
  }
});
