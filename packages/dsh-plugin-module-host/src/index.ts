// cordis 插件：向 DSH 注册三个工具——
//   `tenon_action(module, action, args)`   走闸门执行一次业务动作
//   `tenon_pending()`                       列出本 adapter 的待批（只读）
//   `tenon_resume(planId)`                  续跑一条已获人批的待批计划
//
// 安全边界（设计 §6、§7，档位 A）：
//   - 本包只注册这三个工具，不 import 任何 shell / fs / web / subagent 能力；
//   - 工具参数里**没有** `onBehalfOf` / `operatorRoles` / `approvedBy`：身份与资格只来自部署配置，
//     模型既不能自称别人，也不能自报「我批准了」；
//   - `tenon_resume` 能指定的只有 `planId`（续哪一单），批准本身只能由 DSH 回合内的**人**给出。
import { DEFAULT_STATE_DIR } from './config.js';
import { defineTool } from '@deepseek-ai/dsh-tools';
import type { ToolDefinition } from '@deepseek-ai/dsh-tools';
import { invalidArguments, TenonCallError, type Json } from './bridge.js';
import { Config, type ModuleHostConfig } from './config.js';
import { resumePlan, rolesIntersect, runAction, type ApprovalPort } from './orchestrate.js';
import { assertPlanId, listPending } from './pending.js';

/** 插件名（cordis 的 `name` 导出）。 */
export const name = 'tenon-module-host';

/** 依赖的服务：只有工具注册表。 */
export const inject = ['tools'];

/** 工具面允许的入参键。**这就是 `additionalProperties` 的实际执行点**（见下面的注释）。 */
export const ALLOWED_ARGUMENT_KEYS = ['module', 'action', 'args'] as const;

/**
 * 模型可见的参数声明。
 *
 * ⚠️ 与计划书的一处真实差异：`defineTool` 的 `parameters` **不是**一份原始 JSON Schema，
 * 它是「参数对象根的 properties 映射」——根对象由 dsh-tools 隐式生成为
 * `{type:'object', properties, required}`，且**没有**在根上加 `additionalProperties:false`
 * 的入口（`parameterSchemaSpecToJsonSchema` 的输出里没有这个键，`DefineToolOptions` 也没有
 * 对应选项）。因此「只允许三项」不能只靠 schema 声明来保证：真正把它关上的是
 * {@link readActionArgs} 的白名单——多余的键在那里被拒，且在**任何网络请求之前**被拒。
 * 这两件事都有单测（index.test.ts）。
 */
export const parameters = {
  module: {
    type: 'string',
    required: true,
    description:
      '模块 id，小写（例 inventory）。动作契约里的动作 id 是 <module>.<Action>，本工具由 module + action 两段拼成。',
  },
  action: {
    type: 'string',
    required: true,
    description: '动作名，字母开头（例 DeductStock）。不是完整的 <module>.<Action>——模块段请放在 module 里。',
  },
  args: {
    // 声明成「开放对象」而不是 type:'json'：入参必须是 JSON 对象（协议里是 JsonObject），
    // 但键集由各模块的 inputSchema 决定，本工具不预先收紧——真校验在模块侧。
    type: 'object',
    additionalProperties: true,
    required: true,
    description:
      '动作的入参对象（无入参时传 {}）。原样发给模块，本工具只做 JSON 对象自检，不做业务校验——真校验在模块侧。',
  },
} as const;

/** 工具返回值的声明（两种结局同一形状，靠 status 区分）。 */
const outputSchema = {
  type: 'object',
  additionalProperties: false,
  properties: {
    status: {
      type: 'string',
      required: true,
      enum: ['executed', 'needs_approval'],
      description: 'executed 表示真的执行了；needs_approval 表示停在待批，**未执行**。',
    },
    planId: { type: 'string', required: true, description: '本次调用的计划标识（pln_…）。' },
    module: { type: 'string', required: true, description: '模块 id。' },
    action: { type: 'string', required: true, description: '动作名。' },
    detail: {
      type: 'string',
      required: true,
      description: '一句话交代结局；needs_approval 时明写「未执行」。',
    },
    result: { type: 'json', description: '模块出参原文；needs_approval 时为 null。' },
    requiredRoles: {
      type: 'array',
      items: { type: 'string' },
      description: 'needs_approval 时需要的审批角色；executed 时为空数组。',
    },
  },
} as const;

/**
 * 工具描述。末尾那条「needs_approval 表示未执行」是刻意的：模型把待批读成「做完了」是本设计
 * 点名的风险（§8），因此**工具描述与返回体两处都写死**这句话。
 */
export const DESCRIPTION =
  '执行一个 Tenon 业务动作：先过宿主闸门（发现模块 → 策略评估 → 预演 → dispatched 审计），' +
  '再调真实模块，最后写 result 审计。动作 id 由 module 与 action 拼成 <module>.<Action>。' +
  '两种结局：status=executed 表示已经真实执行；status=needs_approval 表示**未执行**——' +
  '策略要求人工批准，本轮只报告待批，不挂起、不重试；切勿把它当成完成，也不要换路径绕过闸门重试。' +
  '策略拒绝（TENON_POLICY_DENIED）与契约/网络错误会以错误形式返回，错误消息以错误码开头。';

/** 工具注册所需的宿主上下文（结构类型，免得为一个方法引一整个 cordis 依赖）。 */
export interface ToolHostContext {
  tools: {
    register(definition: ToolDefinition): () => void;
  };
  /**
   * DSH 的审批服务（`ctx.approval`，`dsh-user-approval`）。
   *
   * **声明成可选**：没装载审批插件的部署里它不存在，此时 approve 一律只挂起、不提问。
   * 也刻意不写进 `inject`：那会让整个插件在该服务缺失时**装不上**——待批与续跑并不需要它。
   */
  approval?: ApprovalPort;
}

/** 工具执行所需的入参形状（已是 JSON 对象）。 */
export interface ActionCall {
  module: string;
  action: string;
  args: unknown;
}

/**
 * 解析并**收窄**模型给的入参。
 *
 * 这是本工具的信任边界：dsh-tools 的隐式参数根是开放对象（多余的键能到达 `execute`），
 * 所以身份字段必须在进入编排前被挡掉。挡在这里还有一个额外好处——被拒的调用不会发出
 * 任何一次 HTTP 请求（连模块发现都不会发生）。
 *
 * @param raw 模型给的原始入参。
 * @returns 只含 module / action / args 的调用。
 * @throws TenonCallError 形状不对或多出字段（code=INVALID_ARGUMENTS）。
 */
export function readActionArgs(raw: unknown): ActionCall {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw invalidArguments('tenon_action 的入参必须是对象 {module, action, args}');
  }
  const record = raw as Record<string, unknown>;
  const unexpected = Object.keys(record).filter(
    (key) => !(ALLOWED_ARGUMENT_KEYS as readonly string[]).includes(key),
  );
  if (unexpected.length > 0) {
    throw invalidArguments(
      `tenon_action 只接受 module/action/args，收到多余字段：${unexpected.join(', ')}。` +
        '身份（onBehalfOf）与计划号（planId）由部署配置与宿主给出，模型不能自报',
    );
  }
  const module = record['module'];
  const action = record['action'];
  if (typeof module !== 'string' || module.trim().length === 0) {
    throw invalidArguments('module 必须是非空字符串');
  }
  if (typeof action !== 'string' || action.trim().length === 0) {
    throw invalidArguments('action 必须是非空字符串');
  }
  const args = record['args'];
  if (args !== undefined && args !== null && (typeof args !== 'object' || Array.isArray(args))) {
    throw invalidArguments('args 必须是 JSON 对象（无入参时传 {}）');
  }
  return { module, action, args: args ?? {} };
}

/**
 * 把内部错误翻成模型可读的形式：消息以 `code` 开头，并把 `code`/`retryable` 留在属性上
 * （宿主与 UI 可以据此提示，模型至少能读到码）。
 *
 * 不用 dsh-tools 的 `ToolArgsError`：那个类的入参只有 `violations: string[]`，装不下
 * `TENON_POLICY_DENIED` 这样的协议码；而本工具要表达的正是协议码。
 */
function toToolError(error: unknown): Error {
  if (error instanceof TenonCallError) {
    const wrapped = new Error(`${error.code}: ${error.message}`);
    wrapped.name = 'TenonActionError';
    Object.assign(wrapped, {
      code: error.code,
      errorClass: error.errorClass,
      retryable: error.retryable,
    });
    return wrapped;
  }
  return error instanceof Error ? error : new Error(String(error));
}

/** `tenon_resume` 的参数：只有「续哪一单」，没有「我是谁」「我批准了」。 */
export const RESUME_PARAMETERS = {
  planId: {
    type: 'string',
    required: true,
    description:
      '要续跑的计划号（pln_…），来自 tenon_action 的 needs_approval 返回或 tenon_pending 的清单。' +
      '续跑会重新过策略并**向人提问**，批准不是这个参数能表达的东西。',
  },
} as const;

/** `tenon_resume` 允许的入参键（白名单的实际执行点，见 {@link readResumeArgs}）。 */
export const RESUME_ARGUMENT_KEYS = ['planId'] as const;

/** `tenon_pending` 的返回声明：清单是自由形状，整体按 JSON 交给渲染器。 */
const pendingOutputSchema = {
  type: 'object',
  additionalProperties: false,
  properties: {
    summary: {
      type: 'string',
      required: true,
      description: '一句话概况（几条待批、几条已过期、本会话能批几条）。',
    },
    pending: {
      type: 'json',
      required: true,
      description: '待批清单数组：planId / module / action / args / requiredRoles / expiresAt / expired / canApprove。',
    },
  },
} as const;

/**
 * 解析并收窄 `tenon_resume` 的入参。
 *
 * 与 {@link readActionArgs} 同一个信任边界：多余字段在**任何文件或网络操作之前**被拒。
 * `planId` 走 {@link assertPlanId} 的词法（它会被拼进文件路径，不能有 `..`）。
 */
export function readResumeArgs(raw: unknown): { planId: string } {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw invalidArguments('tenon_resume 的入参必须是对象 {planId}');
  }
  const record = raw as Record<string, unknown>;
  const unexpected = Object.keys(record).filter(
    (key) => !(RESUME_ARGUMENT_KEYS as readonly string[]).includes(key),
  );
  if (unexpected.length > 0) {
    throw invalidArguments(
      `tenon_resume 只接受 planId，收到多余字段：${unexpected.join(', ')}。` +
        '审批人（approvedBy）与身份（onBehalfOf）由宿主与部署配置给出，模型不能自报',
    );
  }
  assertPlanId(record['planId']);
  return { planId: record['planId'] };
}

/** `tenon_pending` 的工具描述。 */
export const PENDING_DESCRIPTION =
  '列出本 adapter 上的待批计划（只读）：每条给出计划号、动作、入参、需要的角色、有效期，' +
  '以及「本会话操作员能不能批」（canApprove，按部署配置里声明的角色算，模型改不了它）。' +
  '要真的推进，用 tenon_resume(planId)——它会重新过策略并向人提问。';

/** `tenon_resume` 的工具描述。 */
export const RESUME_DESCRIPTION =
  '续跑一条挂起的 Tenon 计划：重跑策略 → 校验本会话是否持有审批角色 → 经 DSH 审批**问人** → ' +
  '批准后按原计划与同一幂等键执行，审计写明 approvedBy。' +
  '返回 status=executed 表示真的执行了；status=needs_approval 表示**仍未执行**' +
  '（没人批、没资格、或审批服务不可用），此时待批仍然有效，可换人再续。' +
  '已归档的计划会幂等返回结论而不重复执行。';

/**
 * 注册三个工具。
 *
 * @param ctx 携带工具注册表（以及可选的审批服务）的宿主上下文。
 * @param config 部署配置（身份与审批资格只来自这里，见 config.ts）。
 */
export function apply(ctx: ToolHostContext, config: ModuleHostConfig): void {
  ctx.tools.register(
    defineTool({
      name: 'tenon_action',
      description: DESCRIPTION,
      parameters,
      output: {
        schema: outputSchema,
        render: (_args, value) => [
          {
            type: 'text',
            text:
              `${value.status === 'needs_approval' ? '待人工批准（未执行）' : '已执行'}` +
              `｜${value.detail}`,
          },
        ],
      },
      async execute(rawArgs, exec) {
        try {
          const call = readActionArgs(rawArgs);
          const outcome = await runAction({
            config,
            module: call.module,
            action: call.action,
            args: call.args,
            signal: exec?.signal,
            approval: ctx.approval,
            agent: exec?.agent,
          });
          return {
            status: outcome.status,
            planId: outcome.planId,
            module: outcome.module,
            action: outcome.action,
            detail: outcome.detail,
            result: outcome.result,
            requiredRoles: outcome.requiredRoles,
          };
        } catch (error) {
          throw toToolError(error);
        }
      },
    }),
  );

  ctx.tools.register(
    defineTool({
      name: 'tenon_pending',
      description: PENDING_DESCRIPTION,
      parameters: {},
      output: {
        schema: pendingOutputSchema,
        render: (_args, value) => {
          const rows = Array.isArray(value.pending) ? value.pending : [];
          const lines = rows.map((row) => {
            const item = row as Record<string, unknown>;
            const flags = [
              item['expired'] === true ? '已过期' : '有效',
              item['canApprove'] === true ? '本会话可批' : '本会话不可批',
            ].join('／');
            return `- ${String(item['planId'])}｜${String(item['module'])}.${String(item['action'])}｜${flags}`;
          });
          return [
            {
              type: 'text',
              text: lines.length === 0 ? value.summary : `${value.summary}\n${lines.join('\n')}`,
            },
          ];
        },
      },
      async execute() {
        try {
          return readPendingBoard(config);
        } catch (error) {
          throw toToolError(error);
        }
      },
    }),
  );

  ctx.tools.register(
    defineTool({
      name: 'tenon_resume',
      description: RESUME_DESCRIPTION,
      parameters: RESUME_PARAMETERS,
      output: {
        schema: outputSchema,
        render: (_args, value) => [
          {
            type: 'text',
            text:
              `${value.status === 'needs_approval' ? '仍待人工批准（未执行）' : '已执行'}` +
              `｜${value.detail}`,
          },
        ],
      },
      async execute(rawArgs, exec) {
        try {
          const call = readResumeArgs(rawArgs);
          const outcome = await resumePlan({
            config,
            planId: call.planId,
            approval: ctx.approval,
            agent: exec?.agent,
            signal: exec?.signal,
          });
          return {
            status: outcome.status,
            planId: outcome.planId,
            module: outcome.module,
            action: outcome.action,
            detail: outcome.detail,
            result: outcome.result,
            requiredRoles: outcome.requiredRoles,
          };
        } catch (error) {
          throw toToolError(error);
        }
      },
    }),
  );
}

/**
 * 待批清单（`tenon_pending` 的实现）。
 *
 * `canApprove` 由部署配置里的 `operatorRoles` 现算——**它不是授权，只是提示**；真正的校验在
 * 续跑时做（`rolesIntersect` 只在那里决定「要不要问人」）。
 */
export function readPendingBoard(config: ModuleHostConfig): { summary: string; pending: Json } {
  const stateDir = config.stateDir ?? DEFAULT_STATE_DIR;
  const entries = listPending(stateDir);
  const operatorRoles = config.operatorRoles ?? [];

  const rows = entries.map(({ plan, expired }) => ({
    planId: plan.planId,
    module: plan.module,
    action: plan.action,
    args: plan.args,
    requiredRoles: plan.requiredRoles,
    createdAt: plan.createdAt,
    expiresAt: plan.expiresAt,
    expired,
    canApprove: rolesIntersect(operatorRoles, plan.requiredRoles),
  }));

  const expiredCount = rows.filter((row) => row.expired).length;
  const approvable = rows.filter((row) => row.canApprove && !row.expired).length;
  const summary =
    rows.length === 0
      ? '当前没有待批计划。'
      : `共 ${rows.length} 条待批（${expiredCount} 条已过期），其中 ${approvable} 条本会话操作员可批` +
        `（operatorRoles=${operatorRoles.join(',') || '∅'}）。`;

  return { summary, pending: rows as unknown as Json };
}

export { Config };
