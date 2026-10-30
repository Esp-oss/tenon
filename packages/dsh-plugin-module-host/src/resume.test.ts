// `resumePlan` 的用例：续跑是「再确认一次」而不是「照着旧结论执行」。
//
// 每条用例都盯同一件事：**没执行的时候，一次动作请求都不许发出去**。续跑涉及过期、策略改判、
// 无资格、拒批、已归档五种「不该执行」的路径，它们全都必须在 dry-run 之前就停下。
import assert from 'node:assert/strict';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { TenonCallError } from './bridge.js';
import type { ModuleHostConfig } from './config.js';
import { startFakeHost, type FakeHandler } from './fake-host.js';
import { resumePlan, runAction, type ApprovalOutcome, type ApprovalPort } from './orchestrate.js';
import { listPending, loadArchived, loadPending, savePending, type PendingPlan } from './pending.js';

const freshState = (): string => mkdtempSync(path.join(tmpdir(), 'tenon-resume-'));
const APPROVERS = ['role:warehouse_manager'];

const config = (hostURL: string, stateDir: string, extra: Partial<ModuleHostConfig> = {}): ModuleHostConfig => ({
  hostURL,
  agentID: 'ag_dsh_test',
  onBehalfOf: 'user:bob',
  timeoutMs: 5_000,
  stateDir,
  ...extra,
});

function fakeApproval(outcome: ApprovalOutcome): ApprovalPort & { calls: number } {
  const port = {
    calls: 0,
    async request(): Promise<ApprovalOutcome> {
      port.calls += 1;
      return outcome;
    },
  };
  return port;
}

function failWith(error: unknown, code: string): boolean {
  return error instanceof TenonCallError && error.code === code;
}

/** 走一次真挂起，交出 planId（续跑用例的前置状态就是它）。 */
async function suspendOnce(
  hostURL: string,
  stateDir: string,
  qty = 12,
): Promise<string> {
  const outcome = await runAction({
    config: config(hostURL, stateDir, { operatorRoles: [] }),
    module: 'inventory',
    action: 'DeductStock',
    args: { sku: 'STD-1001', qty },
  });
  assert.equal(outcome.status, 'needs_approval');
  return outcome.planId;
}

/** 一个能同时回答发现/策略/预演/审计/动作的宿主；`policy` 可变，用来演「挂起后策略改了」。 */
function handlerWith(state: { policy: 'approve' | 'deny' | 'auto' }): FakeHandler {
  return (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return state.policy === 'deny'
          ? { status: 200, body: { outcome: 'deny', decidedBy: ['pol-no'], approvers: [], reasons: ['BLOCKED'] } }
          : state.policy === 'auto'
            ? { status: 200, body: { outcome: 'auto', decidedBy: ['pol-auto'], approvers: [], reasons: [] } }
            : {
                status: 200,
                body: { outcome: 'approve', decidedBy: ['pol-big'], approvers: APPROVERS, reasons: [] },
              };
      case 'POST /tenon/actions/inventory.DeductStock:dry-run':
        return { status: 200, body: { dryRun: { status: 'ok' } } };
      case 'POST /tenon/audit':
        return { status: 200, body: { ok: true } };
      case 'POST /tenon/actions/inventory.DeductStock':
        return { status: 200, body: { ok: true, onHand: 108 } };
      default:
        return undefined;
    }
  };
}

test('续跑：有资格 + allowed-once → 按原 planId/幂等键执行，归档 done', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  try {
    const planId = await suspendOnce(host.origin, stateDir);
    const before = host.requests.length;

    const approval = fakeApproval('allowed-once');
    const outcome = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId,
      approval,
      agent: { id: 'ag_test' },
    });

    assert.equal(outcome.status, 'executed');
    assert.deepEqual(outcome.result, { ok: true, onHand: 108 });
    assert.equal(approval.calls, 1, '续跑必须问人，且只问一次');

    // 续跑这一段：重跑策略 → 重新发现 → 预演 → step dispatched → 动作 → step result → 计划级 result
    assert.deepEqual(
      host.sequence().slice(before),
      [
        'POST /tenon/policies:evaluate',
        'GET /tenon/modules',
        'POST /tenon/actions/inventory.DeductStock:dry-run',
        'POST /tenon/audit',
        'POST /tenon/actions/inventory.DeductStock',
        'POST /tenon/audit',
        'POST /tenon/audit',
      ],
    );

    // 幂等键与挂起时一致（模块据此回放首次结果而不是重复扣减）。
    const action = host.requests.find((request) => request.path === '/tenon/actions/inventory.DeductStock');
    assert.equal(action?.headers['idempotency-key'], `tenon:${planId}/s1`);

    // 四段审计：计划级 dispatched（挂起时）+ step 两段 + 计划级 result。
    const audits = host.bodies('/tenon/audit') as Array<Record<string, unknown>>;
    assert.deepEqual(
      audits.map((record) => `${record['stage']}/${String(record['stepId'])}`),
      ['dispatched/null', 'dispatched/s1', 'result/s1', 'result/null'],
    );
    assert.equal((audits[2]!['decision'] as Record<string, unknown>)['approvedBy'], 'user:bob');
    assert.equal(audits[3]!['auditId'], audits[0]!['auditId'], '计划级两段共用挂起时的 auditId');

    assert.equal(loadArchived(stateDir, planId)?.state, 'done');
    assert.equal(loadArchived(stateDir, planId)?.approvedBy, 'user:bob');
    assert.deepEqual(listPending(stateDir), [], '归档后待批目录必须清空');
  } finally {
    await host.close();
  }
});

test('续跑：已归档 done 再续 → 幂等返回，一个请求都不发', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  try {
    const planId = await suspendOnce(host.origin, stateDir);
    await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId,
      approval: fakeApproval('allowed-once'),
      agent: { id: 'ag_test' },
    });
    const before = host.requests.length;

    const again = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId,
      approval: fakeApproval('allowed-once'),
      agent: { id: 'ag_test' },
    });

    assert.equal(again.status, 'executed');
    assert.match(again.detail, /不重复执行/);
    assert.equal(host.requests.length, before, '已归档的计划不该再产生任何请求');
  } finally {
    await host.close();
  }
});

test('续跑：过期 → 归档 expired，绝不执行', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  try {
    const planId = await suspendOnce(host.origin, stateDir);
    const pending = loadPending(stateDir, planId)!;
    savePending(stateDir, { ...pending, expiresAt: '2020-01-01T00:00:00Z' });
    const before = host.requests.length;

    const error = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId,
      approval: fakeApproval('allowed-once'),
      agent: { id: 'ag_test' },
    }).catch((thrown: unknown) => thrown);

    assert.ok(failWith(error, 'PENDING_EXPIRED'), `期望 PENDING_EXPIRED，实际 ${String(error)}`);
    const after = host.sequence().slice(before);
    assert.equal(after.includes('POST /tenon/actions/inventory.DeductStock'), false, '过期绝不执行');
    assert.equal(after.includes('POST /tenon/actions/inventory.DeductStock:dry-run'), false);
    assert.equal(loadArchived(stateDir, planId)?.state, 'expired');
  } finally {
    await host.close();
  }
});

test('续跑：策略在挂起后改判 deny → 归档 stale，绝不执行', async () => {
  const state = { policy: 'approve' as 'approve' | 'deny' | 'auto' };
  const host = await startFakeHost(handlerWith(state));
  const stateDir = freshState();
  try {
    const planId = await suspendOnce(host.origin, stateDir);
    state.policy = 'deny';
    const before = host.requests.length;

    const error = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId,
      approval: fakeApproval('allowed-once'),
      agent: { id: 'ag_test' },
    }).catch((thrown: unknown) => thrown);

    assert.ok(failWith(error, 'TENON_POLICY_DENIED'), `期望 TENON_POLICY_DENIED，实际 ${String(error)}`);
    const after = host.sequence().slice(before);
    assert.equal(after.includes('POST /tenon/actions/inventory.DeductStock'), false, '改判拒绝后绝不执行');
    assert.equal(loadArchived(stateDir, planId)?.state, 'stale');
  } finally {
    await host.close();
  }
});

test('续跑：本会话无资格 → 不提问、不执行，待批仍在', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  try {
    const planId = await suspendOnce(host.origin, stateDir);
    const approval = fakeApproval('allowed-once');
    const before = host.requests.length;

    const outcome = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: ['role:sales'] }),
      planId,
      approval,
      agent: { id: 'ag_test' },
    });

    assert.equal(outcome.status, 'needs_approval');
    assert.match(outcome.detail, /不持有/);
    assert.equal(approval.calls, 0, '没资格就不该问人');
    assert.equal(
      host.sequence().slice(before).includes('POST /tenon/actions/inventory.DeductStock'),
      false,
    );
    assert.ok(loadPending(stateDir, planId) !== null, '待批必须还在');
  } finally {
    await host.close();
  }
});

test('续跑：人拒了 → 不执行，状态从 resuming 还原成 awaiting_approval', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  try {
    const planId = await suspendOnce(host.origin, stateDir);
    const outcome = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId,
      approval: fakeApproval('rejected'),
      agent: { id: 'ag_test' },
    });

    assert.equal(outcome.status, 'needs_approval');
    assert.match(outcome.detail, /rejected/);
    assert.equal(loadPending(stateDir, planId)?.state, 'awaiting_approval', '拒批不能把计划卡成 resuming');
    assert.equal(host.sequence().includes('POST /tenon/actions/inventory.DeductStock'), false);
  } finally {
    await host.close();
  }
});

test('续跑：查无此单 → PENDING_NOT_FOUND（不是「已执行」）', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  try {
    const error = await resumePlan({
      config: config(host.origin, stateDir, { operatorRoles: APPROVERS }),
      planId: 'pln_NOTEXIST0000000',
    }).catch((thrown: unknown) => thrown);
    assert.ok(failWith(error, 'PENDING_NOT_FOUND'), `期望 PENDING_NOT_FOUND，实际 ${String(error)}`);
    assert.deepEqual(host.sequence(), [], '查无此单不该发任何请求');
  } finally {
    await host.close();
  }
});

test('续跑：终态非 done（如 expired）再续 → PLAN_ALREADY_FINISHED', async () => {
  const host = await startFakeHost(handlerWith({ policy: 'approve' }));
  const stateDir = freshState();
  const plan: PendingPlan = {
    planId: 'pln_EXPIRED0000000',
    module: 'inventory',
    action: 'DeductStock',
    args: { sku: 'STD-1001', qty: 12 },
    inputHash: 'sha256:abc',
    idempotencyKey: 'tenon:pln_EXPIRED0000000/s1',
    requiredRoles: APPROVERS,
    policyId: 'pol-big',
    createdAt: '2026-10-29T00:00:00Z',
    expiresAt: '2026-10-29T00:15:00Z',
    state: 'awaiting_approval',
    auditSettled: true,
    dispatchedAuditId: 'aud_EXPIRED0000000000',
  };
  try {
    savePending(stateDir, { ...plan, expiresAt: '2020-01-01T00:00:00Z' });
    await resumePlan({ config: config(host.origin, stateDir), planId: plan.planId }).catch(() => undefined);

    const error = await resumePlan({
      config: config(host.origin, stateDir),
      planId: plan.planId,
    }).catch((thrown: unknown) => thrown);
    assert.ok(failWith(error, 'PLAN_ALREADY_FINISHED'), `期望 PLAN_ALREADY_FINISHED，实际 ${String(error)}`);
  } finally {
    await host.close();
  }
});
