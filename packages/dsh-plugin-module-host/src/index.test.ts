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
import { apply, inject, name as pluginName, parameters, readActionArgs } from './index.js';

const CONFIG: ModuleHostConfig = {
  hostURL: 'http://127.0.0.1:9',
  agentID: 'ag_dsh_test',
  onBehalfOf: 'user:alice',
  timeoutMs: 2_000,
};

/** 待批落盘目录：用例之间不共用（否则上一轮留下的卡会喂出假通过）。 */
const freshState = (): string => mkdtempSync(path.join(tmpdir(), 'tenon-tool-'));

/** 装一次插件，交出被注册的那一个工具定义。 */
function registerTool(config: ModuleHostConfig = CONFIG): ToolDefinition {
  const registered: ToolDefinition[] = [];
  apply(
    {
      tools: {
        register(definition) {
          registered.push(definition);
          return () => undefined;
        },
      },
    },
    config,
  );
  assert.equal(registered.length, 1, '本包只注册一个工具（设计 §6）');
  const tool = registered[0];
  assert.ok(tool);
  return tool;
}

/** 最小可用的执行上下文：本工具只用它的 signal。 */
function runContext(): ToolRunContext {
  return { signal: new AbortController().signal } as unknown as ToolRunContext;
}

test('注册面：只注册 tenon_action，inject 只要 tools，description 写死「未执行」', () => {
  assert.equal(pluginName, 'tenon-module-host');
  assert.deepEqual([...inject], ['tools']);

  const tool = registerTool();
  assert.equal(tool.name, 'tenon_action');
  // 模型必须被提前告知「needs_approval 不是完成」——这是设计 §8 点名的风险。
  assert.match(tool.description, /needs_approval/);
  assert.match(tool.description, /未执行/);
  assert.match(tool.description, /module/);
  assert.match(tool.description, /action/);
});

test('参数形状：只有 module/action/args 三个必填项，没有任何身份字段', () => {
  const tool = registerTool();
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
    const tool = registerTool({ ...CONFIG, hostURL: host.origin });
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
    const tool = registerTool({ ...CONFIG, hostURL: host.origin, stateDir: freshState() });
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
    const tool = registerTool({ ...CONFIG, hostURL: host.origin });
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
