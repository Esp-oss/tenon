// `runAction` 的六步编排用例（计划 Task 1 Step 2 的 6 条 + 失败路径的 result 审计）。
//
// 每条用例都断言**请求序列**而不只是最终返回值：闸门的价值在于「哪一步没发生」，
// 只看返回值会把「跳过了预演」这种缺陷读成成功。
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import test from 'node:test';
import { TenonCallError } from './bridge.js';
import type { ModuleHostConfig } from './config.js';
import { startFakeHost, type FakeHandler } from './fake-host.js';
import { runAction } from './orchestrate.js';

const ARGS = { sku: 'STD-1001', qty: 5 };

const config = (hostURL: string): ModuleHostConfig => ({
  hostURL,
  agentID: 'ag_dsh_test',
  onBehalfOf: 'user:alice',
  timeoutMs: 5_000,
});

/** 期望的 sha256 摘要写法（与 `@tenon/core` 的 sha256Digest 同形，此处按字节自算以便独立核对）。 */
const digestOf = (text: string): string =>
  `sha256:${createHash('sha256').update(text, 'utf8').digest('hex')}`;

/** 跑一次必然失败的调用，把错误交出来。 */
async function failure(promise: Promise<unknown>): Promise<TenonCallError> {
  try {
    await promise;
  } catch (error) {
    assert.ok(error instanceof TenonCallError, `期望 TenonCallError，实际 ${String(error)}`);
    return error;
  }
  throw new Error('期望抛错，但调用成功了');
}

test('1. auto：六步顺序、审计两段与执行请求的头部/摘要', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return {
          status: 200,
          body: { outcome: 'auto', decidedBy: ['pol-stock-auto'], approvers: [], reasons: ['RISK_LOW'] },
        };
      case 'POST /tenon/actions/inventory.DeductStock:dry-run':
        return { status: 200, body: { dryRun: { status: 'ok', diff: [] } } };
      case 'POST /tenon/audit':
        return { status: 200, body: { ok: true } };
      case 'POST /tenon/actions/inventory.DeductStock':
        return { status: 200, body: { ok: true, remaining: 115 } };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const outcome = await runAction({
      config: config(host.origin),
      module: 'inventory',
      action: 'DeductStock',
      args: ARGS,
    });

    assert.equal(outcome.status, 'executed');
    assert.match(outcome.planId, /^pln_[A-Za-z0-9_-]{16}$/);
    assert.deepEqual(outcome.result, { ok: true, remaining: 115 });
    assert.match(outcome.detail, /已执行 inventory\.DeductStock/);

    // 六步顺序，一步不多一步不少。
    assert.deepEqual(host.sequence(), [
      'GET /tenon/modules',
      'POST /tenon/policies:evaluate',
      'POST /tenon/actions/inventory.DeductStock:dry-run',
      'POST /tenon/audit',
      'POST /tenon/actions/inventory.DeductStock',
      'POST /tenon/audit',
    ]);

    // 策略请求的形状：宿主收的是整份 plan，不是 {action,args,origin}。
    const policyBody = host.bodies('/tenon/policies:evaluate')[0] as {
      plan: { planId: string; steps: Array<{ stepId: string; action: string; args: unknown }> };
    };
    assert.equal(policyBody.plan.planId, outcome.planId);
    assert.deepEqual(policyBody.plan.steps, [
      { stepId: 's1', action: 'inventory.DeductStock', args: ARGS },
    ]);
    const policyText = host.requests[1]?.body ?? '';
    assert.ok(!policyText.includes('"origin"'), 'origin 不得自报：宿主自己判定来源');
    assert.ok(!policyText.includes('triggerEventId'), '本轮没有触发事件，不该伪造 triggerEventId');

    // 执行请求：Tenon-Version 与 Idempotency-Key。
    const actionRequest = host.requests[4];
    assert.ok(actionRequest, '缺少执行请求');
    assert.equal(actionRequest.headers['tenon-version'], '0.1');
    assert.equal(actionRequest.headers['idempotency-key'], `tenon:${outcome.planId}/s1`);
    assert.equal(actionRequest.headers['content-type'], 'application/json');

    // 两段审计：共用 auditId、dispatched 先于执行、result 带出参摘要。
    const audits = host.bodies('/tenon/audit') as unknown as AuditRecord[];
    assert.equal(audits.length, 2);
    const dispatched = audits[0] as AuditRecord;
    const settled = audits[1] as AuditRecord;
    assert.equal(dispatched.stage, 'dispatched');
    assert.equal(settled.stage, 'result');
    assert.equal(dispatched.auditId, settled.auditId);
    assert.match(dispatched.auditId, /^aud_[A-Za-z0-9_-]{4,}$/);
    assert.equal(dispatched.planId, outcome.planId);
    assert.equal(dispatched.stepId, 's1');
    assert.equal(dispatched.action, 'inventory.DeductStock');
    assert.equal(dispatched.idempotencyKey, `tenon:${outcome.planId}/s1`);
    assert.equal(dispatched.inputHash, digestOf(actionRequest.body));
    assert.equal(dispatched.actor.type, 'agent');
    assert.equal(dispatched.actor.id, 'ag_dsh_test');
    assert.equal(dispatched.actor.onBehalfOf, 'user:alice');
    assert.equal(dispatched.decision.outcome, 'auto');
    assert.equal(dispatched.decision.policyId, 'pol-stock-auto');
    assert.equal(dispatched.decision.grantId, null, '本轮不调 grant，不得伪造 grantId');
    assert.equal(dispatched.decision.approvedBy, null, 'auto 路径没有人批');
    assert.equal(dispatched.result.status, 'unknown');
    assert.deepEqual(dispatched.trigger, { eventId: null, correlationId: null });
    assert.deepEqual(dispatched.effects, { entities: [] });
    assert.deepEqual(dispatched.tokenUsage, { in: 0, out: 0 });
    assert.equal(settled.result.status, 'ok');
    // outputHash 是出参**原始字节**的摘要（Go 的 hashPayload 同一意图）。
    assert.equal(settled.result.outputHash, digestOf(JSON.stringify({ ok: true, remaining: 115 })));
    assert.equal(settled.result.errorClass, null);
  } finally {
    await host.close();
  }
});

test('2. deny：只发到 policies 就抛错，没有预演/审计/执行', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return {
          status: 200,
          body: {
            outcome: 'deny',
            decidedBy: ['pol-x'],
            approvers: [],
            reasons: ['NO_POLICY_MATCH_DEFAULT_DENY'],
          },
        };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const error = await failure(
      runAction({ config: config(host.origin), module: 'inventory', action: 'DeductStock', args: ARGS }),
    );
    assert.equal(error.code, 'TENON_POLICY_DENIED');
    assert.equal(error.errorClass, 'policy_denied');
    assert.equal(error.retryable, false, '策略拒绝不得被当成可重试——重试就是绕过闸门');
    assert.deepEqual(error.details, {
      decidedBy: ['pol-x'],
      reasons: ['NO_POLICY_MATCH_DEFAULT_DENY'],
    });
    assert.deepEqual(host.sequence(), ['GET /tenon/modules', 'POST /tenon/policies:evaluate']);
  } finally {
    await host.close();
  }
});

test('3. approve：返回 needs_approval 且 detail 明写「未执行」，序列停在 policy', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return {
          status: 200,
          body: {
            outcome: 'approve',
            decidedBy: ['pol-big-deduction'],
            approvers: ['role:warehouse_manager'],
            reasons: ['QUANTITY_ABOVE_AUTO_LIMIT'],
          },
        };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const outcome = await runAction({
      config: config(host.origin),
      module: 'inventory',
      action: 'DeductStock',
      args: { sku: 'STD-1001', qty: 12 },
    });
    assert.equal(outcome.status, 'needs_approval');
    assert.match(outcome.planId, /^pln_/);
    assert.match(outcome.detail, /未执行/, 'detail 必须明写「未执行」');
    assert.match(outcome.detail, /pol-big-deduction/);
    assert.deepEqual(outcome.requiredRoles, ['role:warehouse_manager']);
    assert.equal(outcome.result, null);
    // 停在 policy：没有预演、没有审计、没有执行。
    assert.deepEqual(host.sequence(), ['GET /tenon/modules', 'POST /tenon/policies:evaluate']);
  } finally {
    await host.close();
  }
});

test('4. I7：dispatched 审计写失败 → 绝不执行 action', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return { status: 200, body: { outcome: 'auto', decidedBy: ['pol-auto'], reasons: [] } };
      case 'POST /tenon/actions/inventory.DeductStock:dry-run':
        return { status: 200, body: { dryRun: { status: 'ok' } } };
      case 'POST /tenon/audit':
        return {
          status: 500,
          body: {
            tenonError: {
              class: 'retryable',
              code: 'AUDIT_WRITE_FAILED',
              message: '审计存储不可写',
              retryable: true,
            },
          },
        };
      case 'POST /tenon/actions/inventory.DeductStock':
        return { status: 200, body: { ok: true } };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const error = await failure(
      runAction({ config: config(host.origin), module: 'inventory', action: 'DeductStock', args: ARGS }),
    );
    assert.equal(error.code, 'AUDIT_WRITE_FAILED');
    assert.match(error.message, /未执行任何动作/);
    // 关键断言：序列里没有执行请求。审计没落盘，副作用就不许发生。
    assert.deepEqual(host.sequence(), [
      'GET /tenon/modules',
      'POST /tenon/policies:evaluate',
      'POST /tenon/actions/inventory.DeductStock:dry-run',
      'POST /tenon/audit',
    ]);
    assert.equal(
      host.sequence().filter((entry) => entry === 'POST /tenon/actions/inventory.DeductStock').length,
      0,
      'I7 被违反：审计失败后仍然发出了执行请求',
    );
  } finally {
    await host.close();
  }
});

test('5. 预演失败：不写 dispatched 审计、不执行', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return { status: 200, body: { outcome: 'auto', decidedBy: ['pol-auto'], reasons: [] } };
      case 'POST /tenon/actions/inventory.DeductStock:dry-run':
        return {
          status: 422,
          body: {
            tenonError: {
              class: 'business_rejection',
              code: 'STOCK_INSUFFICIENT',
              message: '可用 3，需求 12',
              retryable: false,
            },
          },
        };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const error = await failure(
      runAction({ config: config(host.origin), module: 'inventory', action: 'DeductStock', args: ARGS }),
    );
    assert.equal(error.code, 'STOCK_INSUFFICIENT');
    assert.equal(error.errorClass, 'business_rejection');
    assert.equal(error.retryable, false);
    assert.deepEqual(host.sequence(), [
      'GET /tenon/modules',
      'POST /tenon/policies:evaluate',
      'POST /tenon/actions/inventory.DeductStock:dry-run',
    ]);
  } finally {
    await host.close();
  }
});

test('6. 模块未注册：MODULE_NOT_FOUND，且不调 policy', async () => {
  const host = await startFakeHost((request) =>
    request.path === '/tenon/modules' ? { status: 200, body: { modules: [] } } : undefined,
  );
  try {
    const error = await failure(
      runAction({ config: config(host.origin), module: 'inventory', action: 'DeductStock', args: ARGS }),
    );
    assert.equal(error.code, 'MODULE_NOT_FOUND');
    assert.equal(error.errorClass, 'contract_violation');
    assert.deepEqual(host.sequence(), ['GET /tenon/modules']);
  } finally {
    await host.close();
  }
});

test('7. 执行失败：result 审计仍写（status=error），错误码取自 host', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return { status: 200, body: { outcome: 'auto', decidedBy: ['pol-auto'], reasons: [] } };
      case 'POST /tenon/actions/inventory.DeductStock:dry-run':
        return { status: 200, body: { dryRun: { status: 'ok' } } };
      case 'POST /tenon/audit':
        return { status: 200, body: { ok: true } };
      case 'POST /tenon/actions/inventory.DeductStock':
        return {
          status: 409,
          body: {
            tenonError: {
              class: 'business_rejection',
              code: 'STOCK_RESERVED',
              message: '该批次已被其它单据占用',
              retryable: false,
            },
          },
        };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const error = await failure(
      runAction({ config: config(host.origin), module: 'inventory', action: 'DeductStock', args: ARGS }),
    );
    assert.equal(error.code, 'STOCK_RESERVED');
    assert.equal(error.errorClass, 'business_rejection');

    const audits = host.bodies('/tenon/audit') as unknown as AuditRecord[];
    assert.equal(audits.length, 2, '失败路径也必须写 result 段');
    assert.equal(audits[0]?.stage, 'dispatched');
    assert.equal(audits[1]?.stage, 'result');
    assert.equal(audits[0]?.auditId, audits[1]?.auditId);
    assert.equal(audits[1]?.result.status, 'error');
    assert.equal(audits[1]?.result.errorClass, 'business_rejection');
    assert.equal(audits[1]?.result.outputHash, null);
  } finally {
    await host.close();
  }
});

/** 审计记录里断言用到的那部分字段。 */
interface AuditRecord {
  auditId: string;
  stage: string;
  planId: string;
  stepId: string;
  action: string;
  inputHash: string;
  idempotencyKey: string;
  actor: { type: string; id: string; onBehalfOf: string | null };
  trigger: { eventId: string | null; correlationId: string | null };
  decision: { outcome: string; policyId: string | null; grantId: string | null; approvedBy: string | null };
  result: { status: string; outputHash: string | null; errorClass: string | null };
  effects: { entities: unknown[] };
  tokenUsage: { in: number; out: number };
}
