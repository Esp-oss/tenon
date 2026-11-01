// 9 类契约的最小合法样例。
//
// 这些样例的用途不只是「让测试过」——它们是**跨实现的共识样本**：Go 侧的 conformance
// 用例将来会拿同一批 JSON 去校验。所以每一条都必须真的符合 spec/0.1/schemas 的
// required 与 additionalProperties（严格视图也过），而不是「够填满必填项」。
//
// 字段清单来源：各 schema 的 root required + $defs required + allOf/then 的条件必填。
// 改 schema 时这些样例必须跟着改；`pnpm --filter @tenon/core test` 会替你把关。
import type { ContractName } from './types.js';

export const FIXTURES: Record<ContractName, unknown> = {
  'module-manifest': {
    tenon: '0.1',
    module: { id: 'inventory', version: '1.4.0', title: '库存' },
    actions: [],
    events: [],
    transports: ['http'],
    capabilities: ['dry-run'],
  },

  // read 动作不触发 allOf[0].then 的 idempotency 必填。
  action: {
    id: 'inventory.GetStock',
    version: '1.0.0',
    title: '查询在库量',
    kind: 'read',
    inputSchema: { type: 'object', properties: { sku: { type: 'string' } }, required: ['sku'] },
    outputSchema: { type: 'object' },
    risk: { level: 'low', reversible: true },
    timeoutMs: 1000,
    dryRun: { supported: false },
  },

  plan: {
    planId: 'pln_0001',
    supersedes: null,
    trigger: { eventId: 'evt_0001', correlationId: 'cor:quote/Q-1' },
    steps: [
      {
        stepId: 's1',
        action: 'inventory.GetStock',
        args: { sku: 'STD-1001' },
        dependsOn: [],
        kind: 'read',
      },
    ],
    expectedDiff: [],
    dryRun: { status: 'verified' },
    estimatedRisk: 'low',
    rationale: '先查在库量再决定是否扣减',
  },

  policy: {
    policyId: 'pol-inventory-getstock-auto',
    priority: 10,
    match: { module: 'inventory' },
    when: { all: [] },
    outcome: 'auto',
  },

  grant: {
    grantId: 'gr-0001',
    agent: 'agt_0001',
    allow: [{ module: 'inventory', actions: ['inventory.GetStock'] }],
    expiresAt: '2026-12-31T00:00:00Z',
  },

  // stage=dispatched 时 allOf[1].then 要求 result.status 恰为 'unknown'；
  // 而 status=unknown 又要求 outputHash / errorClass 为 null（allOf[4].then）。
  'audit-event': {
    auditId: 'aud_0001',
    stage: 'dispatched',
    at: '2026-10-27T00:00:00Z',
    actor: { type: 'agent', id: 'agt_0001', onBehalfOf: 'user:alice' },
    trigger: { eventId: 'evt_0001', correlationId: 'cor:quote/Q-1' },
    planId: 'pln_0001',
    stepId: 's1',
    action: 'inventory.GetStock',
    inputHash: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    idempotencyKey: 'tenon:pln_0001/s1',
    decision: { outcome: 'auto', policyId: 'pol-inventory-getstock-auto', grantId: 'gr-0001', approvedBy: null },
    result: { status: 'unknown', outputHash: null, errorClass: null },
    effects: { entities: [] },
    tokenUsage: { in: 0, out: 0 },
    prevHash: null,
  },

  'domain-event': {
    eventId: 'evt_0001',
    type: 'quote.completed',
    version: '0.1.0',
    subject: { module: 'quote', type: 'Quote', id: 'Q-1' },
    occurredAt: '2026-10-27T00:00:00Z',
    causationId: null,
    correlationId: 'cor:quote/Q-1',
    payload: {},
  },

  // 三帧取 Intent 帧（其余两帧由同一 schema 的 oneOf 覆盖，测试用「类型错」用例兜底）。
  frames: {
    type: 'tenon.intent',
    intentId: 'int_0001',
    planId: 'pln_0001',
    actions: [{ kind: 'open', target: { module: 'inventory', type: 'SKU', id: 'STD-1001' } }],
  },

  error: {
    tenonError: {
      class: 'contract_violation',
      code: 'SCHEMA_MISMATCH',
      message: '入参不符合 inputSchema',
      retryable: false,
    },
  },
};
