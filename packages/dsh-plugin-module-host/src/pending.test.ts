// 待批存储的用例：原子性、幂等 upsert、归档顺序、路径安全、损坏判定。
//
// 这些断言服务的都是续跑：只要「读回来的东西和写进去的不是同一份」，续跑就会用错的入参
// 重放一次业务调用——那是本项目最不能接受的失败形态（做了，但做错了）。
import assert from 'node:assert/strict';
import { mkdtempSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { TenonCallError } from './bridge.js';
import {
  archiveDir,
  archivePlan,
  isExpired,
  listPending,
  loadArchived,
  loadPending,
  pendingDir,
  rfc3339,
  savePending,
  type PendingPlan,
} from './pending.js';

/** 每个用例一个独立目录：存储测试最怕的就是上一个用例的残留把下一个用例喂成假通过。 */
function freshState(): string {
  return mkdtempSync(path.join(tmpdir(), 'tenon-pending-'));
}

function planOf(overrides: Partial<PendingPlan> = {}): PendingPlan {
  return {
    planId: 'pln_AAAAAAAAAAAAAAAA',
    module: 'inventory',
    action: 'DeductStock',
    args: { sku: 'STD-1001', qty: 12 },
    inputHash: 'sha256:abc',
    idempotencyKey: 'tenon:pln_AAAAAAAAAAAAAAAA/s1',
    requiredRoles: ['role:warehouse_manager'],
    policyId: 'pol-deduct-large',
    createdAt: '2026-10-29T00:00:00Z',
    expiresAt: '2026-10-29T00:15:00Z',
    state: 'awaiting_approval',
    auditSettled: false,
    dispatchedAuditId: 'aud_BBBBBBBBBBBBBBBBBBBB',
    ...overrides,
  };
}

test('save → load 往返：读回来的就是写进去的那一份', () => {
  const stateDir = freshState();
  const plan = planOf();
  savePending(stateDir, plan);
  assert.deepEqual(loadPending(stateDir, plan.planId), plan);
});

test('upsert：同 planId 再写一次是覆盖，不是追加第二份', () => {
  const stateDir = freshState();
  savePending(stateDir, planOf());
  savePending(stateDir, planOf({ auditSettled: true }));
  assert.equal(loadPending(stateDir, 'pln_AAAAAAAAAAAAAAAA')?.auditSettled, true);
  assert.deepEqual(readdirSync(pendingDir(stateDir)), ['pln_AAAAAAAAAAAAAAAA.json']);
});

test('原子写：成功后目录里只有目标文件，没有 .tmp 残留', () => {
  const stateDir = freshState();
  savePending(stateDir, planOf());
  assert.deepEqual(
    readdirSync(pendingDir(stateDir)).filter((name) => name.endsWith('.tmp')),
    [],
  );
});

test('原子写：目标路径不可写时抛错，且不留半截文件', () => {
  const stateDir = freshState();
  // 把「待批目录」先占成一个普通文件：mkdir 会失败，写入必须整体失败。
  writeFileSync(pendingDir(stateDir), 'not a directory', 'utf8');
  assert.throws(
    () => savePending(stateDir, planOf()),
    (error: unknown) => error instanceof TenonCallError && error.code === 'PENDING_WRITE_FAILED',
  );
  assert.equal(loadPending(stateDir, 'pln_AAAAAAAAAAAAAAAA'), null);
});

test('planId 词法：路径穿越在任何文件操作之前被拒', async () => {
  const stateDir = freshState();
  for (const bad of ['../../etc/passwd', 'pln_', 'pln_AA/BB', 'aud_AAAAAAAAAAAAAAAA']) {
    assert.throws(
      () => loadPending(stateDir, bad),
      (error: unknown) => error instanceof TenonCallError && error.code === 'INVALID_PLAN_ID',
      `应拒绝 planId=${bad}`,
    );
    assert.throws(() => {
      savePending(stateDir, planOf({ planId: bad }));
    }, /INVALID_PLAN_ID|不是合法的计划标识/);
  }
});

test('损坏判定：读不懂的记录不会被当成「没有这一单」', () => {
  const stateDir = freshState();
  savePending(stateDir, planOf());
  writeFileSync(path.join(pendingDir(stateDir), 'pln_AAAAAAAAAAAAAAAA.json'), '{ 坏掉的 JSON', 'utf8');
  assert.throws(
    () => loadPending(stateDir, 'pln_AAAAAAAAAAAAAAAA'),
    (error: unknown) => error instanceof TenonCallError && error.code === 'PENDING_CORRUPT',
  );
  assert.throws(() => listPending(stateDir), /PENDING_CORRUPT|不可用/);
});

test('归档：先写 archived 再删 pending，两步之后各只有一份', () => {
  const stateDir = freshState();
  const plan = planOf();
  savePending(stateDir, plan);

  const archived = archivePlan(stateDir, plan, 'done', {
    approvedBy: 'user:bob',
    result: { status: 'executed' },
  });

  assert.equal(archived.state, 'done');
  assert.equal(archived.approvedBy, 'user:bob');
  assert.deepEqual(archived.result, { status: 'executed' });
  assert.equal(loadPending(stateDir, plan.planId), null, '待批原件必须已被删除');
  assert.deepEqual(loadArchived(stateDir, plan.planId)?.state, 'done');
  assert.deepEqual(readdirSync(archiveDir(stateDir)), ['pln_AAAAAAAAAAAAAAAA.json']);
});

test('归档：终态记录不会出现在待批列表里（不然会被二次批准）', () => {
  const stateDir = freshState();
  const plan = planOf();
  savePending(stateDir, plan);
  archivePlan(stateDir, plan, 'rejected');
  assert.deepEqual(listPending(stateDir), []);
});

test('列表：按 expiresAt 升序，过期条目照样返回并标记', () => {
  const stateDir = freshState();
  const now = Date.parse('2026-10-29T00:30:00Z');
  savePending(stateDir, planOf({ planId: 'pln_LATE00000000000', expiresAt: '2026-10-29T01:00:00Z' }));
  savePending(stateDir, planOf({ planId: 'pln_SOON00000000000', expiresAt: '2026-10-29T00:10:00Z' }));

  const entries = listPending(stateDir, now);
  assert.deepEqual(
    entries.map((entry) => entry.plan.planId),
    ['pln_SOON00000000000', 'pln_LATE00000000000'],
  );
  assert.deepEqual(
    entries.map((entry) => entry.expired),
    [true, false],
  );
});

test('过期判定：解析不出的 expiresAt 按过期处理（fail-closed）', () => {
  const now = Date.parse('2026-10-29T00:30:00Z');
  assert.equal(isExpired(planOf({ expiresAt: '2026-10-29T00:29:59Z' }), now), true);
  assert.equal(isExpired(planOf({ expiresAt: '2026-10-29T00:30:01Z' }), now), false);
  assert.equal(isExpired(planOf({ expiresAt: '不是时间' }), now), true);
});

test('rfc3339：与 Go 同形（无小数秒）', () => {
  assert.equal(rfc3339(new Date('2026-10-29T00:00:00.123Z')), '2026-10-29T00:00:00Z');
});

test('目录不存在时列表为空，不抛错（第一次运行就是这种情况）', () => {
  const stateDir = path.join(freshState(), 'never-created');
  assert.deepEqual(listPending(stateDir), []);
  rmSync(stateDir, { recursive: true, force: true });
});
