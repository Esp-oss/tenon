// `tenon_action` 的注册面与信任边界用例（计划 Task 2）。
//
// 这一组用例守的是安全性质，不是功能性质：工具的参数里**不许**出现身份字段，而且这条不许
// 只写在文档里——模型的入参必须真的到不了「自称别人 / 伪造计划号」那一步。
import assert from 'node:assert/strict';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import type { ToolDefinition, ToolRunContext } from '@deepseek-ai/dsh-tools';
import { TenonCallError } from './bridge.js';
import type { ModuleHostConfig } from './config.js';
import { startFakeHost, type FakeHandler } from './fake-host.js';
import { apply, inject, name as pluginName, parameters, readActionArgs, readResumeArgs } from './index.js';
import type { ApprovalPort } from './orchestrate.js';

const CONFIG: ModuleHostConfig = {
  hostURL: 'http://127.0.0.1:9',
  agentID: 'ag_dsh_test',
  onBehalfOf: 'user:alice',
  timeoutMs: 2_000,
};

/** 待批落盘目录：用例之间不共用（否则上一轮留下的卡会喂出假通过）。 */
const freshState = (): string => mkdtempSync(path.join(tmpdir(), 'tenon-tool-'));

/** 装一次插件，交出全部被注册的工具定义。 */
function registerTools(config: ModuleHostConfig = CONFIG, approval?: ApprovalPort): ToolDefinition[] {
  const registered: ToolDefinition[] = [];
  apply(
    {
      tools: {
        register(definition) {
          registered.push(definition);
          return () => undefined;
        },
      },
      ...(approval === undefined ? {} : { approval }),
    },
    config,
  );
  assert.equal(registered.length, 3, '本包注册三个工具：action / pending / resume（设计 §6）');
  return registered;
}

/** 取出某一个工具定义。 */
function toolNamed(
  name: string,
  config: ModuleHostConfig = CONFIG,
  approval?: ApprovalPort,
): ToolDefinition {
  const tool = registerTools(config, approval).find((definition) => definition.name === name);
  assert.ok(tool, `没有注册 ${name}`);
  return tool;
}

/** 最小可用的执行上下文：本工具只用 signal 与 agent。 */
function runContext(agent: unknown = { id: 'ag_test' }): ToolRunContext {
  return { signal: new AbortController().signal, agent } as unknown as ToolRunContext;
}

/** 假审批端口：记录问了几次，回固定结局。 */
function fakeApproval(outcome: string): ApprovalPort & { calls: number } {
  const port = {
    calls: 0,
    async request(): Promise<'allowed-once' | 'rejected' | 'cancelled' | 'unavailable'> {
      port.calls += 1;
      return outcome as 'allowed-once' | 'rejected' | 'cancelled' | 'unavailable';
    },
  };
  return port;
}

test('注册面：三个工具、inject 只要 tools、description 写死「未执行」', () => {
  assert.equal(pluginName, 'tenon-module-host');
  assert.deepEqual([...inject], ['tools']);

  assert.deepEqual(registerTools().map((definition) => definition.name).sort(), [
    'tenon_action',
    'tenon_pending',
    'tenon_resume',
  ]);

  const tool = toolNamed('tenon_action');
  assert.equal(tool.name, 'tenon_action');
  // 模型必须被提前告知「needs_approval 不是完成」——这是设计 §8 点名的风险。
  assert.match(tool.description, /needs_approval/);
  assert.match(tool.description, /未执行/);
  assert.match(tool.description, /module/);
  assert.match(tool.description, /action/);

  // 续跑的描述必须同样说清「needs_approval 仍然是未执行」，且不把批准说成模型能做的事。
  const resume = toolNamed('tenon_resume');
  assert.match(resume.description, /未执行/);
  assert.match(resume.description, /approvedBy/);
  assert.match(toolNamed('tenon_pending').description, /只读/);
});

test('参数形状：只有 module/action/args 三个必填项，没有任何身份字段', () => {
  const tool = toolNamed('tenon_action');
  const schema = tool.parameters as {
    type?: string;
    properties?: Record<string, unknown>;
    required?: string[];
    additionalProperties?: boolean;
  };

  assert.equal(schema.type, 'object');
  assert.deepEqual(Object.keys(schema.properties ?? {}).sort(), ['action', 'args', 'module']);
  assert.deepEqual([...(schema.required ?? [])].sort(), ['action', 'args', 'module']);

  // 模型看不到、也传不进这四个名字。
  const surface = JSON.stringify(tool.parameters);
  for (const forbidden of ['onBehalfOf', 'planId', 'userId', 'agentID']) {
    assert.ok(!surface.includes(forbidden), `参数面不得出现 ${forbidden}`);
  }

  // 记录一处与计划书的真实差异（不是断言 dsh-tools 有 bug，而是把现状钉住）：
  // `defineTool` 的参数根是**隐式开放对象**，编译产物里没有 additionalProperties，
  // DefineToolOptions 也没有关闭它的入口。所以「只允许三项」最终由 readActionArgs 的白名单
  // 执行——下面两条用例证明它在**任何 HTTP 请求之前**生效。哪天真能声明封闭根，
  // 这条断言会变红，正好提醒把白名单保留为纵深防御。
  assert.equal(
    'additionalProperties' in schema,
    false,
    'dsh-tools 的参数根不声明 additionalProperties：闭合由 readActionArgs 保证',
  );
  assert.deepEqual(Object.keys(parameters).sort(), ['action', 'args', 'module']);

  // args 必须是对象：模型看到的形状要与「只接受 JsonObject」的执行面一致，
  // 但键集开放（由各模块的 inputSchema 决定）。
  const argsSchema = (schema.properties ?? {})['args'] as {
    type?: string;
    additionalProperties?: boolean;
  };
  assert.equal(argsSchema.type, 'object');
  assert.equal(argsSchema.additionalProperties, true);
});

test('参数白名单：多出的身份字段在发出任何请求之前就被拒', async () => {
  const host = await startFakeHost(() => undefined);
  try {
    const tool = toolNamed('tenon_action', { ...CONFIG, hostURL: host.origin });
    await assert.rejects(
      () =>
        tool.execute(
          {
            module: 'inventory',
            action: 'DeductStock',
            args: { sku: 'STD-1001', qty: 5 },
            onBehalfOf: 'user:mallory',
          },
          runContext(),
        ),
      (error: unknown) => {
        assert.ok(error instanceof Error);
        assert.match(error.message, /INVALID_ARGUMENTS/);
        assert.match(error.message, /onBehalfOf/);
        return true;
      },
    );
    assert.deepEqual(host.requests, [], '被拒的调用连模块发现都不该发生');
  } finally {
    await host.close();
  }
});

test('参数白名单：planId / userId 同样进不来（readActionArgs 直测）', () => {
  for (const extra of [{ planId: 'pln_FAKE' }, { userId: 'user:mallory' }, { token: 'x' }]) {
    assert.throws(
      () => readActionArgs({ module: 'inventory', action: 'DeductStock', args: {}, ...extra }),
      (error: unknown) => {
        // 直接调编排入口时错误还没被工具边界加前缀，判定看 code 而不是消息文本。
        assert.ok(error instanceof TenonCallError);
        assert.equal(error.code, 'INVALID_ARGUMENTS');
        assert.match(error.message, /多余字段/);
        return true;
      },
      `多余字段 ${Object.keys(extra)[0]} 必须被拒`,
    );
  }
  assert.deepEqual(readActionArgs({ module: 'inventory', action: 'DeductStock', args: {} }), {
    module: 'inventory',
    action: 'DeductStock',
    args: {},
  });
  // args 省略时按空对象处理（schema 要求必填，但这里对模型宽一点不会造成安全问题）。
  assert.deepEqual(readActionArgs({ module: 'inventory', action: 'DeductStock' }).args, {});
});

test('工具层 needs_approval：返回体 detail 明写「未执行」，且没有执行请求', async () => {
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
            reasons: [],
          },
        };
      case 'POST /tenon/audit':
        return { status: 200, body: { ok: true } };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const tool = toolNamed('tenon_action', { ...CONFIG, hostURL: host.origin, stateDir: freshState() });
    const value = (await tool.execute(
      { module: 'inventory', action: 'DeductStock', args: { sku: 'STD-1001', qty: 12 } },
      runContext(),
    )) as { status: string; detail: string; requiredRoles: string[]; planId: string; result: unknown };

    assert.equal(value.status, 'needs_approval');
    assert.match(value.detail, /未执行/);
    assert.deepEqual(value.requiredRoles, ['role:warehouse_manager']);
    assert.match(value.planId, /^pln_/);
    assert.equal(value.result, null);
    // 挂起要落盘并写计划级审计：序列比「只报告」那版多一次 /tenon/audit，仍没有动作调用。
    assert.deepEqual(host.sequence(), [
      'GET /tenon/modules',
      'POST /tenon/policies:evaluate',
      'POST /tenon/audit',
    ]);
  } finally {
    await host.close();
  }
});

test('工具层错误：消息以协议码开头，且带上可判定的属性', async () => {
  const handler: FakeHandler = (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return {
          status: 200,
          body: { outcome: 'deny', decidedBy: ['pol-x'], approvers: [], reasons: ['NO_POLICY_MATCH'] },
        };
      default:
        return undefined;
    }
  };
  const host = await startFakeHost(handler);
  try {
    const tool = toolNamed('tenon_action', { ...CONFIG, hostURL: host.origin });
    await assert.rejects(
      () =>
        tool.execute(
          { module: 'inventory', action: 'DeductStock', args: { sku: 'STD-1001', qty: 5 } },
          runContext(),
        ),
      (error: unknown) => {
        assert.ok(error instanceof Error);
        assert.match(error.message, /^TENON_POLICY_DENIED: /);
        const annotated = error as Error & { code?: string; retryable?: boolean };
        assert.equal(annotated.code, 'TENON_POLICY_DENIED');
        assert.equal(annotated.retryable, false);
        return true;
      },
    );
  } finally {
    await host.close();
  }
});

/** 一个能完成「挂起 → 续跑」全链路的假宿主。 */
function approveHost(): FakeHandler {
  return (request, origin) => {
    switch (`${request.method} ${request.path}`) {
      case 'GET /tenon/modules':
        return { status: 200, body: { modules: [{ id: 'inventory', baseURL: origin }] } };
      case 'POST /tenon/policies:evaluate':
        return {
          status: 200,
          body: { outcome: 'approve', decidedBy: ['pol-big'], approvers: ['role:warehouse_manager'], reasons: [] },
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

test('tenon_resume 参数白名单：身份与 approvedBy 进不来，且在发请求之前被拒', async () => {
  for (const extra of [
    { approvedBy: 'user:mallory' },
    { onBehalfOf: 'user:mallory' },
    { operatorRoles: ['role:warehouse_manager'] },
    { roles: ['role:warehouse_manager'] },
  ]) {
    assert.throws(
      () => readResumeArgs({ planId: 'pln_AAAAAAAAAAAAAAAA', ...extra }),
      (error: unknown) => error instanceof TenonCallError && error.code === 'INVALID_ARGUMENTS',
      `应拒绝多余字段 ${Object.keys(extra).join(',')}`,
    );
  }
  // planId 会被拼进文件路径：词法不对即拒绝。
  assert.throws(
    () => readResumeArgs({ planId: '../../etc/passwd' }),
    (error: unknown) => error instanceof TenonCallError && error.code === 'INVALID_PLAN_ID',
  );

  // 工具层同样在**任何请求之前**拒（假宿主的 handler 永远返回 undefined，一旦发出请求就会 500）。
  const host = await startFakeHost(() => undefined);
  try {
    const tool = toolNamed('tenon_resume', { ...CONFIG, hostURL: host.origin });
    await assert.rejects(
      () => tool.execute({ planId: 'pln_AAAAAAAAAAAAAAAA', approvedBy: 'user:mallory' }, runContext()),
      /approvedBy/,
    );
    assert.deepEqual(host.sequence(), []);
  } finally {
    await host.close();
  }
});

test('tenon_pending：清单里的 canApprove 由部署配置的角色决定，模型改不了', async () => {
  const host = await startFakeHost(approveHost());
  const stateDir = freshState();
  try {
    const action = toolNamed('tenon_action', { ...CONFIG, hostURL: host.origin, stateDir });
    await action.execute(
      { module: 'inventory', action: 'DeductStock', args: { sku: 'STD-1001', qty: 12 } },
      runContext(),
    );

    type Board = { summary: string; pending: Array<Record<string, unknown>> };
    const sales = (await toolNamed('tenon_pending', {
      ...CONFIG,
      hostURL: host.origin,
      stateDir,
      operatorRoles: ['role:sales'],
    }).execute({}, runContext())) as Board;
    assert.equal(sales.pending.length, 1);
    assert.equal(sales.pending[0]!['canApprove'], false, '销售不该能批超额扣减');
    assert.equal(sales.pending[0]!['expired'], false);
    assert.match(sales.summary, /1 条待批/);

    const manager = (await toolNamed('tenon_pending', {
      ...CONFIG,
      hostURL: host.origin,
      stateDir,
      operatorRoles: ['role:warehouse_manager'],
    }).execute({}, runContext())) as Board;
    assert.equal(manager.pending[0]!['canApprove'], true);
    assert.match(manager.summary, /1 条本会话操作员可批/);

    // 空目录是一个正常答案，不是错误。
    const empty = (await toolNamed('tenon_pending', {
      ...CONFIG,
      hostURL: host.origin,
      stateDir: freshState(),
    }).execute({}, runContext())) as Board;
    assert.deepEqual(empty.pending, []);
    assert.match(empty.summary, /没有待批/);
  } finally {
    await host.close();
  }
});

test('tenon_resume：挂起后经人批 → 工具层返回 executed，审计带 approvedBy', async () => {
  const host = await startFakeHost(approveHost());
  const stateDir = freshState();
  try {
    const config: ModuleHostConfig = {
      ...CONFIG,
      hostURL: host.origin,
      stateDir,
      operatorRoles: ['role:warehouse_manager'],
    };
    const suspended = (await toolNamed('tenon_action', config).execute(
      { module: 'inventory', action: 'DeductStock', args: { sku: 'STD-1001', qty: 12 } },
      runContext(),
    )) as { status: string; planId: string; detail: string };
    assert.equal(suspended.status, 'needs_approval');
    assert.match(suspended.detail, /未执行/);

    const approval = fakeApproval('allowed-once');
    const resumed = (await toolNamed('tenon_resume', config, approval).execute(
      { planId: suspended.planId },
      runContext(),
    )) as { status: string; detail: string; result: unknown };
    assert.equal(resumed.status, 'executed');
    assert.deepEqual(resumed.result, { ok: true, onHand: 108 });
    assert.equal(approval.calls, 1);

    // 工具层没有把身份/审批人的东西漏进返回值形状之外的地方：审计里的 approvedBy 来自配置。
    const audits = host.bodies('/tenon/audit') as Array<Record<string, unknown>>;
    const stepDispatched = audits.find(
      (record) => record['stage'] === 'dispatched' && record['stepId'] === 's1',
    );
    assert.equal((stepDispatched?.['decision'] as Record<string, unknown>)['approvedBy'], 'user:alice');
  } finally {
    await host.close();
  }
});
