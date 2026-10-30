// 公告板上推的用例。
//
// 这一组守的是一条边界：**公告板是展示层，它的失败不许把业务拖下水**。所以每个失败用例都断言
// 「返回了一句告警」而不是「抛了错」，同时断言「没有配置凭据时一个请求都不发」——功能关闭要真的
// 是关闭，不是悄悄发了请求再吞掉 401。
import assert from 'node:assert/strict';
import test from 'node:test';
import { pendingCard, pushBoardCard, terminalCard } from './board.js';
import type { ModuleHostConfig } from './config.js';
import { startFakeHost, type FakeHandler } from './fake-host.js';
import { resetTokenCache } from './host-auth.js';
import type { PendingPlan } from './pending.js';

const PLAN: PendingPlan = {
  planId: 'pln_AAAAAAAAAAAAAAAA',
  module: 'inventory',
  action: 'DeductStock',
  args: { sku: 'STD-1001', qty: 12 },
  inputHash: 'sha256:abc',
  idempotencyKey: 'tenon:pln_AAAAAAAAAAAAAAAA/s1',
  requiredRoles: ['role:warehouse_manager'],
  policyId: 'pol-big',
  createdAt: '2026-10-29T00:00:00Z',
  expiresAt: '2026-10-29T00:15:00Z',
  state: 'awaiting_approval',
  auditSettled: true,
  dispatchedAuditId: 'aud_BBBBBBBBBBBBBBBBBBBB',
};

/** 会签发 token 并接受公告板上推的假宿主。 */
function hostHandler(state: { logins: number; pushes: number; pushStatus?: number }): FakeHandler {
  return (request) => {
    switch (`${request.method} ${request.path}`) {
      case 'POST /tenon/session':
        state.logins += 1;
        return {
          status: 200,
          body: { token: 'tok-agent', user: 'user:agent', roles: ['role:agent'], exp: 4_000_000_000 },
        };
      case 'POST /tenon/pending':
        state.pushes += 1;
        return { status: state.pushStatus ?? 200, body: { ok: true } };
      default:
        return undefined;
    }
  };
}

const withCreds = (hostURL: string): ModuleHostConfig => ({
  hostURL,
  agentID: 'ag_dsh_test',
  onBehalfOf: 'user:alice',
  timeoutMs: 3_000,
  agentUser: 'agent',
  agentPassword: 'agent-dev',
});

test('未配置 agent 凭据：一次请求都不发，只回一句「未更新」', async () => {
  resetTokenCache();
  const state = { logins: 0, pushes: 0 };
  const host = await startFakeHost(hostHandler(state));
  try {
    const note = await pushBoardCard(
      { hostURL: host.origin, agentID: 'ag', onBehalfOf: 'user:alice' },
      pendingCard(PLAN),
    );
    assert.match(String(note), /未配置 agent 凭据/);
    assert.deepEqual(host.sequence(), [], '功能关闭就必须真的不发请求');
    assert.equal(state.pushes, 0);
  } finally {
    await host.close();
  }
});

test('配了账号口令：先换 token，再带上 Authorization 推卡片', async () => {
  resetTokenCache();
  const state = { logins: 0, pushes: 0 };
  const host = await startFakeHost(hostHandler(state));
  try {
    const note = await pushBoardCard(withCreds(host.origin), pendingCard(PLAN));
    assert.equal(note, null, '成功时返回 null');
    assert.deepEqual(host.sequence(), ['POST /tenon/session', 'POST /tenon/pending']);

    const push = host.requests.find((request) => request.path === '/tenon/pending');
    assert.equal(push?.headers['authorization'], 'Bearer tok-agent');
    const body = JSON.parse(push?.body ?? '{}') as Record<string, unknown>;
    assert.equal(body['planId'], PLAN.planId);
    assert.equal(body['state'], 'awaiting_approval');
    assert.deepEqual(body['requiredRoles'], ['role:warehouse_manager']);
    assert.equal(body['expiresAt'], PLAN.expiresAt);
    assert.match(String(body['summary']), /inventory\.DeductStock/);
  } finally {
    await host.close();
  }
});

test('token 缓存：两次推送只登录一次', async () => {
  resetTokenCache();
  const state = { logins: 0, pushes: 0 };
  const host = await startFakeHost(hostHandler(state));
  try {
    const config = withCreds(host.origin);
    await pushBoardCard(config, pendingCard(PLAN));
    await pushBoardCard(config, terminalCard(PLAN, 'done', null));
    assert.equal(state.logins, 1);
    assert.equal(state.pushes, 2);
  } finally {
    await host.close();
  }
});

test('静态 token：不调登录端点，直接用', async () => {
  resetTokenCache();
  const state = { logins: 0, pushes: 0 };
  const host = await startFakeHost(hostHandler(state));
  try {
    const note = await pushBoardCard(
      { hostURL: host.origin, agentID: 'ag', onBehalfOf: 'user:alice', agentToken: 'tok-static' },
      pendingCard(PLAN),
    );
    assert.equal(note, null);
    assert.deepEqual(host.sequence(), ['POST /tenon/pending']);
    assert.equal(host.requests[0]?.headers['authorization'], 'Bearer tok-static');
    assert.equal(state.logins, 0);
  } finally {
    await host.close();
  }
});

test('推送失败只回告警，不抛错（业务已经发生了，不该被展示层拖成失败）', async () => {
  resetTokenCache();
  const state = { logins: 0, pushes: 0, pushStatus: 500 };
  const host = await startFakeHost(hostHandler(state));
  try {
    const note = await pushBoardCard(withCreds(host.origin), pendingCard(PLAN));
    assert.match(String(note), /公告板未更新（HTTP 500/);
  } finally {
    await host.close();
  }
});

test('登录被拒也只回告警，且不伪装成已推送', async () => {
  resetTokenCache();
  const host = await startFakeHost((request) => {
    if (`${request.method} ${request.path}` === 'POST /tenon/session') {
      return { status: 401, body: { tenonError: { code: 'AUTH_INVALID', message: '账号或口令不正确' } } };
    }
    return undefined;
  });
  try {
    const note = await pushBoardCard(withCreds(host.origin), pendingCard(PLAN));
    assert.match(String(note), /公告板未更新/);
    assert.match(String(note), /AGENT_LOGIN_FAILED|登录被拒绝/);
    assert.equal(
      host.sequence().includes('POST /tenon/pending'),
      false,
      '没换到 token 就不该尝试写公告板',
    );
  } finally {
    await host.close();
  }
});

test('终态卡：同 planId 覆盖，state 与摘要反映结局', () => {
  const card = terminalCard(PLAN, 'done', null);
  assert.equal(card.planId, PLAN.planId);
  assert.equal(card.state, 'done');
  assert.match(card.summary, /已执行/);

  const stale = terminalCard(PLAN, 'stale', '续跑时策略拒绝');
  assert.equal(stale.state, 'stale');
  assert.match(stale.summary, /已终结（stale）/);
  assert.match(stale.summary, /策略拒绝/);
});
