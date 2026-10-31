// 批准后唤醒的用例：形状、失败处理、句柄上限。
//
// 这些用例守的是一条行为：公告板批准**已经完成业务**，唤醒只是「把结论送回会话」。
// 所以唤醒失败绝不能抛回轮询器，也不能重试到刷屏；没有句柄要能说清为什么没有。
import assert from 'node:assert/strict';
import test from 'node:test';
import { createWakeRegistry, wakeMessage, type WakeEvent, type WakeMessage } from './wake.js';

const APPROVE: WakeEvent = {
  planId: 'pln_AAAAAAAAAAAAAAAA',
  by: 'user:bob',
  decision: 'approve',
  detail: '库存 108 → 96',
};

test('有句柄：唤醒一次，消息是与 dsh-llm UserMessage 同形的对象', () => {
  const woken: WakeMessage[] = [];
  const wake = createWakeRegistry();
  wake.remember(APPROVE.planId, {
    followup(message: WakeMessage) {
      woken.push(message);
    },
  });

  assert.equal(wake.notify(APPROVE), true);
  assert.equal(woken.length, 1);
  const message = woken[0]!;
  assert.equal(message.role, 'user');
  assert.equal(message.source.kind, 'user');
  assert.equal(message.content[0]!.type, 'text');
  assert.match(message.content[0]!.text, /pln_AAAAAAAAAAAAAAAA/);
  assert.match(message.content[0]!.text, /user:bob/);
  assert.match(message.content[0]!.text, /批准并执行/);
  assert.match(message.content[0]!.text, /108 → 96/);
  assert.ok(message.id.length > 0);

  // 唤醒后就摘掉句柄：同一条事件重放不会在聊天里再刷一句。
  assert.equal(wake.size(), 0);
  assert.equal(wake.notify(APPROVE), false);
});

test('没有句柄：返回 false 并说明原因（重启/跨进程是已知限制）', () => {
  const lines: string[] = [];
  const wake = createWakeRegistry((line) => lines.push(line));
  assert.equal(wake.notify(APPROVE), false);
  assert.match(lines.join('\n'), /没有 .* 的会话句柄/);
  assert.match(lines.join('\n'), /业务已完成/);
});

test('followup 抛错：不把异常抛给轮询器，且句柄不残留（避免重复唤醒）', () => {
  const lines: string[] = [];
  const wake = createWakeRegistry((line) => lines.push(line));
  wake.remember(APPROVE.planId, {
    followup() {
      throw new Error('agent disposed');
    },
  });
  assert.equal(wake.notify(APPROVE), false);
  assert.match(lines.join('\n'), /唤醒失败/);
  assert.equal(wake.size(), 0);
});

test('驳回也唤醒，措辞明确「未执行」', () => {
  const message = wakeMessage({ ...APPROVE, decision: 'reject', detail: '' });
  assert.match(message.content[0]!.text, /驳回/);
  assert.match(message.content[0]!.text, /未执行/);
});

test('形状不对的句柄被忽略；句柄数有上限，丢最旧', () => {
  const lines: string[] = [];
  const wake = createWakeRegistry((line) => lines.push(line));
  wake.remember('pln_shape', { notFollowup: 1 });
  assert.equal(wake.size(), 0);

  const noop = { followup() {} };
  for (let i = 0; i < 101; i += 1) wake.remember(`pln_${i}`, noop);
  assert.equal(wake.size(), 100);
  assert.equal(wake.notify({ ...APPROVE, planId: 'pln_0' }), false, '最旧的句柄应已被丢弃');
  assert.equal(wake.notify({ ...APPROVE, planId: 'pln_100' }), true, '最新的句柄应仍在');
  assert.match(lines.join('\n'), /句柄超过 100 个/);
});
