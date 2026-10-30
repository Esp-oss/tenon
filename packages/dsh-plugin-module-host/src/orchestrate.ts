// 六步编排（设计 §4）：发现 → 策略 → 预演 → I7 dispatched 审计 → 执行 → result 审计。
//
// 顺序不是风格问题，而是本项目的铁律：**审计先于执行**。第 4 步写不进去就绝不发出第 5 步的
// 调用——没有审计的副作用比没有副作用更糟（事后无法证明发生了什么）。
// 同一顺序在 Go 侧是 `internal/agent/run.go` 的闸门 1..6，两侧刻意保持同序、同字段。
//
// 这里的每一步都只做「发一次请求 + 判一次语义」，没有重试、没有挂起、没有补偿——那是 agent
// 循环的职责，本轮最小版不做（见设计 §1「本轮不做」）。
import { idempotencyKey, sha256Digest } from '@tenon/core';
import { randomBytes } from 'node:crypto';
import {
  canonicalJSON,
  fetchJSON,
  HEADER_IDEMPOTENCY_KEY,
  invalidArguments,
  normalizeFailure,
  snippet,
  TenonCallError,
  type Json,
  type JsonResponse,
} from './bridge.js';
import {
  DEFAULT_PENDING_TTL_MINUTES,
  DEFAULT_STATE_DIR,
  DEFAULT_TIMEOUT_MS,
  type ModuleHostConfig,
} from './config.js';
import { archivePlan, rfc3339, savePending, type PendingPlan } from './pending.js';

/** 单步计划里的步骤标识（最小版只有一步）。 */
export const STEP_ID = 's1';

/** 动作 id 的两段词法（spec/0.1/schemas/common.schema.json#/$defs/ActionId）。 */
const MODULE_ID_PATTERN = /^[a-z][a-z0-9_-]*$/;
const ACTION_NAME_PATTERN = /^[A-Za-z][A-Za-z0-9_]*$/;
/** 代行委托人（common.schema.json#/$defs/Actor 的 onBehalfOf）。 */
const ON_BEHALF_OF_PATTERN = /^(user|agent):\S+$/;

/** {@link runAction} 的入参。`args` 是模型给的原始值，由本函数负责自检与规范化。 */
export interface RunActionInput {
  config: ModuleHostConfig;
  /** 模块 id（`inventory`）。 */
  module: string;
  /** 动作名（`DeductStock`），与模块 id 拼成契约里的动作 id。 */
  action: string;
  args: unknown;
  /** 工具的 `exec.signal`，转发给每一次 HTTP 调用。 */
  signal?: AbortSignal;
  /**
   * DSH 的审批端口（`ctx.approval`）。缺省时 approve 只挂起、不提问——**不猜、不假装问过**。
   */
  approval?: ApprovalPort;
  /**
   * 本轮调用的 agent（`exec.agent`）。DSH 的 `approval.request` 要求它，缺了就无法在回合内
   * 提问（`dsh-user-approval` 的 `ApprovalRequest.agent` 是必填）。
   */
  agent?: unknown;
}

/** DSH 审批的四个结局，全封闭（`dsh-user-approval/lib/types/types.d.ts:26`）。 */
export type ApprovalOutcome = 'allowed-once' | 'rejected' | 'cancelled' | 'unavailable';

/**
 * 审批端口的窄接口：本包只用到 `request` 一个方法，因此按结构类型声明，不引 cordis / dsh 依赖。
 *
 * 语义（照抄 `dsh-user-approval` 的契约）：`allowed-once` 是**唯一**放行值；其余三个一律
 * 视为「没批」；`request` 必须在开着的回合内调用，否则它会直接拒绝。
 */
export interface ApprovalPort {
  request(request: {
    agent?: unknown;
    toolName: string;
    callId?: string;
    reason?: string;
    signal?: AbortSignal;
  }): Promise<ApprovalOutcome>;
}

/** 执行成功。 */
export interface ExecutedAction {
  status: 'executed';
  planId: string;
  module: string;
  action: string;
  /** 一句话交代发生了什么（人可读，含 planId）。 */
  detail: string;
  /** 模块的出参原文（无出参时为 null）。 */
  result: Json;
  /** 最小版没有审批角色；保持字段存在，免得调用方按状态分支时两套形状。 */
  requiredRoles: string[];
}

/** 策略判定需要人批：本轮**只报告**，不挂起、不执行。 */
export interface NeedsApprovalAction {
  status: 'needs_approval';
  planId: string;
  module: string;
  action: string;
  /** 明写「未执行」：模型必须知道事情停在哪，而不是以为做完了（设计 §5、§8）。 */
  detail: string;
  result: Json;
  /** 需要的角色/审批人（来自策略的 approvers，可能为空）。 */
  requiredRoles: string[];
}

/** {@link runAction} 的结局：要么真的执行了，要么停在人批。失败一律抛 {@link TenonCallError}。 */
export type ActionResult = ExecutedAction | NeedsApprovalAction;

/** `policies:evaluate` 的响应（spec/0.1/schemas/policy.schema.json 的求值结果）。 */
interface PolicyEvaluation {
  outcome: string;
  decidedBy: string[];
  approvers: string[];
  reasons: string[];
}

/**
 * 审计两段的 result 段。
 *
 * 用类型别名而不是 interface：interface 没有隐式索引签名，赋给 `Json`（`{[k:string]: Json}`）
 * 会失败；这里要的正是「能直接放进 JSON 对象」的类型。
 */
type AuditResult = {
  status: 'ok' | 'error' | 'unknown';
  outputHash: string | null;
  errorClass: string | null;
};

/** 构造一条审计记录所需的、两段共用的材料。 */
interface AuditInput {
  config: ModuleHostConfig;
  actionId: string;
  planId: string;
  auditId: string;
  inputHash: string;
  idemKey: string;
  policyId: string | null;
  /** 这一步是怎么被允许执行的：策略判 auto，还是人批的。 */
  decisionOutcome: 'auto' | 'approve';
  /** 审批人（`user:<账号>`）；auto 路径为 null——没人批过，不能编一个。 */
  approvedBy: string | null;
}

/**
 * 走完六步闸门并执行一次动作。
 *
 * 结局只有三种：返回 `executed`、返回 `needs_approval`、抛 {@link TenonCallError}
 * （带 `code` / `errorClass` / `retryable`）。**不抛非 TenonCallError 的异常**——除了程序
 * 性错误（如 `@tenon/core` 内部断言），那些属于 bug，不该被当成协议结局。
 *
 * @param input 配置、动作与入参。
 * @returns 执行结果或待批结果。
 * @throws TenonCallError 入参非法、模块未注册、策略拒绝、预演失败、审计写失败、执行失败。
 */
export async function runAction(input: RunActionInput): Promise<ActionResult> {
  const { config, module, action, signal, approval, agent } = input;
  const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const host = config.hostURL.replace(/\/+$/, '');

  assertIdentity(config);
  assertActionId(module, action);

  const actionId = `${module}.${action}`;
  // 入参只序列化一次：**这份文本既是请求体也是 inputHash 的摘要输入**。摘要因此证明的就是
  // 「实际发出去的那份字节」，不会出现「审计里的 hash 与真正发的参数对不上」。
  const argsBody = canonicalJSON(input.args ?? {});
  if (!argsBody.startsWith('{')) {
    throw invalidArguments('args 必须是 JSON 对象（模块的入参是 JsonObject，数组/标量不合法）');
  }
  const args = JSON.parse(argsBody) as Json;

  const planId = randomToken('pln_', 16);
  const idemKey = idempotencyKey(planId, STEP_ID);
  const inputHash = sha256Digest(argsBody);

  // ---- 1. 发现：动作属于哪个模块、它在哪个地址 ----
  const baseURL = await discoverModule(host, module, timeoutMs, signal);

  // ---- 2. 策略：整份 plan 交宿主求值（宿主的粒度是「整份 plan 取最严」）----
  const evaluation = await evaluatePolicy(host, planId, actionId, args, timeoutMs, signal);
  switch (evaluation.outcome) {
    case 'deny':
      // 拒绝不写审计、不执行：这一步没有发生任何副作用，写一条「被拒」的 dispatched 会让
      // 审计链里出现一次并不存在的调用（与 Go 侧一致：策略拒绝直接转人工）。
      throw new TenonCallError({
        code: 'TENON_POLICY_DENIED',
        errorClass: 'policy_denied',
        retryable: false,
        message: `策略拒绝 ${actionId}（decidedBy=${evaluation.decidedBy.join(',') || '∅'}）：${evaluation.reasons.join('；') || '无理由码'}`,
        details: { decidedBy: evaluation.decidedBy, reasons: evaluation.reasons },
      });
    case 'approve':
      // 真挂起（设计 §6.1）：先落盘 + 计划级审计，再按资格决定要不要当场问人。
      return suspendAndMaybeApprove({
        config,
        baseURL,
        module,
        action,
        actionId,
        planId,
        args,
        argsBody,
        inputHash,
        idemKey,
        policyId: firstNonEmpty(evaluation.decidedBy),
        evaluation,
        timeoutMs,
        signal,
        approval,
        agent,
      });
    case 'auto':
      break;
    default:
      throw new TenonCallError({
        code: 'MALFORMED_POLICY_OUTCOME',
        errorClass: 'contract_violation',
        retryable: false,
        message: `policies:evaluate 的 outcome=${JSON.stringify(evaluation.outcome)} 非法（只能是 auto/approve/deny）`,
      });
  }

  return executePlan({
    config,
    baseURL,
    planId,
    module,
    action,
    actionId,
    args,
    argsBody,
    inputHash,
    idemKey,
    policyId: firstNonEmpty(evaluation.decidedBy),
    decisionOutcome: 'auto',
    approvedBy: null,
    timeoutMs,
    signal,
  });
}

// ---------- 挂起（approve 分支）与执行（auto / 已批准共用） ----------

/** 走到 approve 分支时手上已有的全部上下文（都是前面几步算出来的，不重新求值）。 */
interface ApprovalContext {
  config: ModuleHostConfig;
  baseURL: string;
  module: string;
  action: string;
  actionId: string;
  planId: string;
  args: Json;
  argsBody: string;
  inputHash: string;
  idemKey: string;
  policyId: string | null;
  evaluation: PolicyEvaluation;
  timeoutMs: number;
  signal?: AbortSignal;
  approval?: ApprovalPort;
  agent?: unknown;
}

/** 一次执行所需的全部材料（auto 与「已批准」两条路径的唯一差别是 decision 与 pending）。 */
interface ExecutionInput {
  config: ModuleHostConfig;
  baseURL: string;
  planId: string;
  module: string;
  action: string;
  actionId: string;
  args: Json;
  argsBody: string;
  inputHash: string;
  idemKey: string;
  policyId: string | null;
  decisionOutcome: 'auto' | 'approve';
  approvedBy: string | null;
  /** 挂起时落盘的那份计划；auto 路径没有它，因此不写计划级审计、也不归档。 */
  pending?: PendingPlan;
  timeoutMs: number;
  signal?: AbortSignal;
}

/**
 * approve 分支：**先挂起，再按资格决定要不要当场问人**（设计 §6.1）。
 *
 * 顺序是刻意的：落盘与计划级审计在提问**之前**。反过来的话，人在审批框上犹豫的几十秒里
 * 盘上什么都没有——审批人一刷新、进程一重启，这一单就凭空消失了。
 */
async function suspendAndMaybeApprove(ctx: ApprovalContext): Promise<ActionResult> {
  const { config, module, action, actionId, planId, evaluation, timeoutMs, signal } = ctx;
  const requiredRoles = evaluation.approvers;

  // 与 Go 侧同一条铁律（internal/agent/run.go:432-435）：**不挂一张谁都批不掉的卡**。
  // 策略给了 approve 却没给 approvers，等于让人去点一个必然 403 的按钮。
  if (requiredRoles.length === 0) {
    throw new TenonCallError({
      code: 'APPROVAL_UNREACHABLE',
      errorClass: 'contract_violation',
      retryable: false,
      message:
        `策略判定 ${actionId} 需要人工批准，但 approvers 为空：没人能批的卡不挂起。` +
        '请在策略里补齐 approvers（例 role:warehouse_manager）',
      details: { planId, module, action },
    });
  }

  const ttlMinutes = config.pendingTTLMinutes ?? DEFAULT_PENDING_TTL_MINUTES;
  const now = Date.now();
  const draft: PendingPlan = {
    planId,
    module,
    action,
    args: ctx.args,
    inputHash: ctx.inputHash,
    idempotencyKey: ctx.idemKey,
    requiredRoles,
    policyId: ctx.policyId,
    createdAt: rfc3339(new Date(now)),
    expiresAt: rfc3339(new Date(now + ttlMinutes * 60_000)),
    state: 'awaiting_approval',
    auditSettled: false,
    dispatchedAuditId: randomToken('aud_', 20),
  };
  const stateDir = stateDirOf(config);

  // ---- I7 三次落盘（与 internal/agent/suspend.go:108-163 同序）----
  // ① 先写待批：下一步就算崩，盘上也有一条完整可读的待批记录。
  savePending(stateDir, draft);

  // ② 计划级 dispatched：stepId=null / action="" / inputHash="" / approvedBy=null。
  //    形状与 Go 侧逐字段同形（suspend.go:131-154）；它不满足 schema 完整 required 集是
  //    既有的刻意取舍（README:265），不要顺手补成 step 形状。
  try {
    await postAudit(
      hostOf(config),
      buildPlanAuditRecord(config, draft, 'dispatched', {
        outcome: 'approve',
        approvedBy: null,
      }, { status: 'unknown', outputHash: null, errorClass: null }),
      timeoutMs,
      signal,
    );
  } catch (error) {
    // 审计写不进去 ⇒ 不留卡：一张来源不明的待批卡比没有卡更糟。
    archivePlan(stateDir, draft, 'stale', { note: '计划级 dispatched 审计写失败，拒绝挂起' });
    throw auditFailed(actionId, planId, error, '拒绝挂起');
  }

  // ③ 回写 auditSettled。
  const settled: PendingPlan = { ...draft, auditSettled: true };
  try {
    savePending(stateDir, settled);
  } catch (error) {
    archivePlan(stateDir, draft, 'stale', { note: 'I7 第三步回写失败' });
    throw auditFailed(actionId, planId, error, '拒绝挂起');
  }

  const head =
    `策略判定需要人工批准，**未执行**（planId=${planId}）：` +
    `decidedBy=${evaluation.decidedBy.join(',') || '∅'}，需要 ${requiredRoles.join('/')}。`;

  // 资格：本会话操作员是否真的持有需要的角色（部署配置，模型不可见 —— 设计 §3.2 裁决 A）。
  const operatorRoles = config.operatorRoles ?? [];
  if (!rolesIntersect(operatorRoles, requiredRoles)) {
    return needsApproval(ctx, `${head} 本会话操作员（${config.onBehalfOf}）不持有上述任一角色，` +
      '因此不会向任何人提问；需由持有该角色的人在自己的会话里续跑（tenon_resume），' +
      `或在该 profile 配置 operatorRoles。待批已落盘，${draft.expiresAt} 前有效。`);
  }
  if (ctx.approval === undefined || typeof ctx.approval.request !== 'function') {
    return needsApproval(ctx, `${head} 宿主没有提供审批服务（ctx.approval 未装载），已挂起，` +
      '可用 tenon_resume 续跑。');
  }
  if (ctx.agent === undefined) {
    return needsApproval(ctx, `${head} 拿不到本轮 agent，无法在回合内发起审批` +
      '（DSH 的 approval.request 要求 agent），已挂起，可用 tenon_resume 续跑。');
  }

  // 回合内问人。`allowed-once` 是唯一放行值；其余三个一律当「没批」。
  const outcome = await ctx.approval.request({
    agent: ctx.agent,
    toolName: 'tenon_action',
    reason: `Tenon 需要人工批准：${actionId}，角色 ${requiredRoles.join('/')}（planId=${planId}）`,
    signal,
  });
  if (outcome !== 'allowed-once') {
    return needsApproval(ctx, `${head} DSH 审批结局=${outcome}，仍未执行；待批仍然有效，` +
      `可换人/换回合用 tenon_resume 续跑（${draft.expiresAt} 前）。`);
  }

  return executePlan({
    config,
    baseURL: ctx.baseURL,
    planId,
    module,
    action,
    actionId,
    args: ctx.args,
    argsBody: ctx.argsBody,
    inputHash: ctx.inputHash,
    idemKey: ctx.idemKey,
    policyId: ctx.policyId,
    decisionOutcome: 'approve',
    approvedBy: config.onBehalfOf,
    pending: settled,
    timeoutMs,
    signal,
  });
}

/** 构造 `needs_approval` 的返回体（三条拒绝分支共用，避免形状漂移）。 */
function needsApproval(ctx: ApprovalContext, detail: string): NeedsApprovalAction {
  return {
    status: 'needs_approval',
    planId: ctx.planId,
    module: ctx.module,
    action: ctx.action,
    detail,
    result: null,
    requiredRoles: ctx.evaluation.approvers,
  };
}

/**
 * 执行一段已定下 decision 的计划：预演 → dispatched 审计 → 执行 → result 审计
 * （设计 §6.2，与 Go 的闸门 3..6 同序）。auto 与「已批准」共用这一条路径——两条路各写一份
 * 执行逻辑，正是闸门顺序漂移的开始。
 */
async function executePlan(input: ExecutionInput): Promise<ExecutedAction> {
  const { config, baseURL, planId, module, action, actionId, argsBody, timeoutMs, signal } = input;
  const host = hostOf(config);

  // ---- 3. 预演：模块自己说这一步会不会成 ----
  await dryRun(baseURL, actionId, argsBody, timeoutMs, signal);

  // ---- 4. I7：dispatched 审计必须先落盘，写不进去就绝不执行 ----
  const auditInput: AuditInput = {
    config,
    actionId,
    planId,
    auditId: randomToken('aud_', 20),
    inputHash: input.inputHash,
    idemKey: input.idemKey,
    policyId: input.policyId,
    decisionOutcome: input.decisionOutcome,
    approvedBy: input.approvedBy,
  };
  try {
    await postAudit(
      host,
      buildAuditRecord(auditInput, 'dispatched', {
        status: 'unknown',
        outputHash: null,
        errorClass: null,
      }),
      timeoutMs,
      signal,
    );
  } catch (error) {
    // 这条分支是本项目最重要的一行语义：**宁可这次不做，也不能做了一次没人记得的事**。
    throw auditFailed(actionId, planId, error, '未执行任何动作');
  }

  // ---- 5. 执行 ----
  let response: JsonResponse;
  try {
    response = await fetchJSON(`${baseURL}/tenon/actions/${actionId}`, {
      method: 'POST',
      body: argsBody,
      headers: { [HEADER_IDEMPOTENCY_KEY]: input.idemKey },
      timeoutMs,
      signal,
    });
  } catch (error) {
    // 传输层失败：写调用可能已经落地，结果未定。按 unknown 记账，再把这个可重试错误抛上去
    // （重试用同一个幂等键，模块会回放首次结果而不是重复副作用）。
    const reason = error instanceof Error ? error.message : String(error);
    await settleAudit(
      host,
      auditInput,
      { status: 'unknown', outputHash: null, errorClass: 'retryable' },
      timeoutMs,
      signal,
    );
    const planNote = await finishPlan(input, { status: 'unknown', outputHash: null, errorClass: 'retryable' }, 'stale');
    throw new TenonCallError({
      code: 'ACTION_UNKNOWN_OUTCOME',
      errorClass: 'retryable',
      retryable: true,
      message: `${actionId} 的调用结果未定（已写 result 审计 status=unknown）：${reason}${planNote}`,
      details: { planId, module, action, idempotencyKey: input.idemKey },
    });
  }

  if (response.status < 200 || response.status > 299) {
    const failure = normalizeFailure(response.status, response.text);
    await settleAudit(
      host,
      auditInput,
      { status: 'error', outputHash: null, errorClass: failure.errorClass },
      timeoutMs,
      signal,
    );
    const planNote = await finishPlan(input, { status: 'error', outputHash: null, errorClass: failure.errorClass }, 'stale');
    throw withNote(failure, planNote);
  }

  // ---- 6. 收尾：result 审计无论成败都写；它写不进去**不改业务结局**（调用已经发生了）----
  const auditNote = await settleAudit(
    host,
    auditInput,
    { status: 'ok', outputHash: sha256Digest(response.text), errorClass: null },
    timeoutMs,
    signal,
  );
  const planNote = await finishPlan(input, { status: 'ok', outputHash: sha256Digest(response.text), errorClass: null }, 'done');

  return {
    status: 'executed',
    planId,
    module,
    action,
    detail:
      `已执行 ${actionId}（planId=${planId}，幂等键 ${input.idemKey}` +
      `${input.approvedBy === null ? '' : `，经 ${input.approvedBy} 批准`}）` +
      (auditNote === null ? '' : `；**警告**：${auditNote}`) +
      planNote,
    result: parseBodyOrNull(response.text),
    requiredRoles: [],
  };
}

/**
 * 收尾「计划级」：写计划级 result 审计 + 归档。
 *
 * 只在有 pending 时生效（auto 路径没有挂起这回事，因此不产生计划级记录）。
 * **审计写失败不改业务结局**——调用已经发生了；但要把这件事说出来，所以返回一句可拼接的告警。
 */
async function finishPlan(
  input: ExecutionInput,
  result: AuditResult,
  terminal: 'done' | 'stale',
): Promise<string> {
  const pending = input.pending;
  if (pending === undefined) return '';

  const notes: string[] = [];
  try {
    await postAudit(
      hostOf(input.config),
      buildPlanAuditRecord(
        input.config,
        pending,
        'result',
        { outcome: input.decisionOutcome, approvedBy: input.approvedBy },
        result,
      ),
      input.timeoutMs,
      input.signal,
    );
  } catch (error) {
    notes.push(`计划级 result 审计写失败（${error instanceof Error ? error.message : String(error)}）`);
  }
  try {
    archivePlan(stateDirOf(input.config), pending, terminal, {
      approvedBy: input.approvedBy,
      result: { status: result.status },
      note: notes.length === 0 ? null : notes.join('；'),
    });
  } catch (error) {
    notes.push(`归档失败（${error instanceof Error ? error.message : String(error)}）`);
  }
  return notes.length === 0 ? '' : `；**警告**：${notes.join('；')}`;
}

/**
 * 计划级审计记录。
 *
 * 与 step 级刻意不同（`internal/agent/suspend.go:131-154` 同形）：`stepId` 为 null、
 * `action`/`inputHash` 为空串、`idempotencyKey` 为 null、`decision.policyId` 为 null——
 * 人工批准的对象是**整份 plan**，不是某一步，因此这些 step 字段在这里没有意义。
 */
function buildPlanAuditRecord(
  config: ModuleHostConfig,
  pending: PendingPlan,
  stage: 'dispatched' | 'result',
  decision: { outcome: 'auto' | 'approve'; approvedBy: string | null },
  result: AuditResult,
): Json {
  return {
    auditId: pending.dispatchedAuditId,
    stage,
    at: timestamp(),
    actor: { type: 'agent', id: config.agentID, onBehalfOf: config.onBehalfOf },
    trigger: { eventId: null, correlationId: null },
    planId: pending.planId,
    stepId: null,
    action: '',
    inputHash: '',
    idempotencyKey: null,
    decision: {
      outcome: decision.outcome,
      policyId: null,
      grantId: null,
      approvedBy: decision.approvedBy,
    },
    result,
    effects: { entities: [] },
    tokenUsage: { in: 0, out: 0 },
  };
}

/** 两个角色集合是否有交集。`need` 为空即无人匹配（与宿主 `rolesIntersect` 同义）。 */
function rolesIntersect(have: readonly string[], need: readonly string[]): boolean {
  if (need.length === 0) return false;
  return need.some((role) => have.includes(role));
}

/** 状态目录（配置缺省时与 Schemastery 的 default 一致）。 */
function stateDirOf(config: ModuleHostConfig): string {
  return config.stateDir ?? DEFAULT_STATE_DIR;
}

/** 宿主根地址（去掉尾部斜杠）。 */
function hostOf(config: ModuleHostConfig): string {
  return config.hostURL.replace(/\/+$/, '');
}

/** 审计写失败的统一错误（挂起与执行两条路径共用同一个码）。 */
function auditFailed(actionId: string, planId: string, error: unknown, what: string): TenonCallError {
  const reason = error instanceof Error ? error.message : String(error);
  return new TenonCallError({
    code: 'AUDIT_WRITE_FAILED',
    errorClass: 'contract_violation',
    retryable: false,
    message: `dispatched 审计写失败，${what}（${actionId}, planId=${planId}）：${reason}`,
    details: { planId },
  });
}

/** 给已有的协议错误追加一句计划级告警（不改 code / class / retryable）。 */
function withNote(error: TenonCallError, note: string): TenonCallError {
  if (note.length === 0) return error;
  return new TenonCallError({
    code: error.code,
    errorClass: error.errorClass,
    retryable: error.retryable,
    message: `${error.message}${note}`,
    details: error.details,
    status: error.status,
  });
}

// ---------- 各步的实现 ----------

/** 身份只进审计；格式不对就是配置错，早抛比写出不合规审计好。 */
function assertIdentity(config: ModuleHostConfig): void {
  if (!ON_BEHALF_OF_PATTERN.test(config.onBehalfOf)) {
    throw new TenonCallError({
      code: 'INVALID_CONFIG',
      errorClass: 'contract_violation',
      retryable: false,
      message:
        `onBehalfOf=${JSON.stringify(config.onBehalfOf)} 不合规：审计的 actor.onBehalfOf 必须形如 ` +
        'user:<账号> 或 agent:<名>（common.schema.json#/$defs/Actor）',
    });
  }
  if (typeof config.agentID !== 'string' || config.agentID.trim().length === 0) {
    throw new TenonCallError({
      code: 'INVALID_CONFIG',
      errorClass: 'contract_violation',
      retryable: false,
      message: 'agentID 不能为空：审计的 actor.id 是这次调用的全部身份事实',
    });
  }
}

/**
 * 动作 id 的词法检查。
 *
 * 不只是「好看」：`action` 会被拼进 URL 路径（`/tenon/actions/<id>:dry-run`），因此这里必须
 * 按契约的字符集把它关死，`..`、`/`、`?`、`%` 一个都进不来。
 */
function assertActionId(module: string, action: string): void {
  if (!MODULE_ID_PATTERN.test(module)) {
    throw invalidArguments(
      `module=${JSON.stringify(module)} 不是合法模块 id（必须匹配 ${MODULE_ID_PATTERN.source}）`,
    );
  }
  if (!ACTION_NAME_PATTERN.test(action)) {
    throw invalidArguments(
      `action=${JSON.stringify(action)} 不是合法动作名（必须匹配 ${ACTION_NAME_PATTERN.source}，` +
        '不含点号与路径分隔符）',
    );
  }
}

/** 第 1 步：从注册表可见视图里找模块的 baseURL。 */
async function discoverModule(
  host: string,
  module: string,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<string> {
  const response = await fetchJSON(`${host}/tenon/modules`, { method: 'GET', timeoutMs, signal });
  if (response.status !== 200) throw normalizeFailure(response.status, response.text);

  const body = parseJson(response.text, 'GET /tenon/modules');
  const modules = (body as { modules?: unknown }).modules;
  if (!Array.isArray(modules)) {
    throw new TenonCallError({
      code: 'MALFORMED_MODULES_RESPONSE',
      errorClass: 'contract_violation',
      retryable: false,
      message: `GET /tenon/modules 的响应缺少 modules 数组：${snippet(response.text)}`,
    });
  }
  for (const candidate of modules) {
    if (typeof candidate !== 'object' || candidate === null) continue;
    const entry = candidate as Record<string, unknown>;
    if (entry['id'] !== module) continue;
    const baseURL = entry['baseURL'];
    if (typeof baseURL !== 'string' || baseURL.length === 0) {
      throw new TenonCallError({
        code: 'MALFORMED_MODULE_ENTRY',
        errorClass: 'contract_violation',
        retryable: false,
        message: `模块 ${module} 的注册表条目没有 baseURL`,
      });
    }
    return baseURL.replace(/\/+$/, '');
  }
  throw new TenonCallError({
    code: 'MODULE_NOT_FOUND',
    errorClass: 'contract_violation',
    retryable: false,
    message:
      `模块 ${module} 不在注册表可见视图里（未注册、未心跳或已被摘除）。` +
      '可见视图只含 enabled && alive 的模块，别拿管理面的全量视图当它。',
    details: { module },
  });
}

/**
 * 第 2 步：把整份单步 plan 交宿主求值。
 *
 * 注意报文形状：宿主收的是 `{plan:{planId,steps:[{stepId,action,args}]}}`，**不是**
 * `{action,args,origin}`。`origin` 也绝不能自报——host 直接忽略请求体里的 origin 字段，
 * 来源只能由它按 `triggerEventId` 自己判定（见 `servePoliciesEvaluate`）。最小版没有触发事件，
 * 因此不传 `triggerEventId`，由宿主取缺省来源。
 */
async function evaluatePolicy(
  host: string,
  planId: string,
  actionId: string,
  args: Json,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<PolicyEvaluation> {
  const plan = { planId, steps: [{ stepId: STEP_ID, action: actionId, args }] };
  const response = await fetchJSON(`${host}/tenon/policies:evaluate`, {
    method: 'POST',
    body: JSON.stringify({ plan }),
    timeoutMs,
    signal,
  });
  if (response.status !== 200) throw normalizeFailure(response.status, response.text);

  const body = parseJson(response.text, 'POST /tenon/policies:evaluate');
  const record = body as Record<string, unknown>;
  return {
    outcome: typeof record['outcome'] === 'string' ? record['outcome'] : '',
    decidedBy: stringArray(record['decidedBy']),
    approvers: stringArray(record['approvers']),
    reasons: stringArray(record['reasons']),
  };
}

/** 第 3 步：预演。非 2xx 一律抛（class 取自响应），不写审计、不执行。 */
async function dryRun(
  baseURL: string,
  actionId: string,
  argsBody: string,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<void> {
  const response = await fetchJSON(`${baseURL}/tenon/actions/${actionId}:dry-run`, {
    method: 'POST',
    body: argsBody,
    timeoutMs,
    signal,
  });
  if (response.status < 200 || response.status > 299) {
    throw normalizeFailure(response.status, response.text);
  }
}

/**
 * 第 6 步（失败的版本）：写 result 审计，**失败不影响调用方要判定的结局**。
 *
 * @returns 审计写失败时的简述，成功则 null（由调用方决定怎么告知）。
 */
async function settleAudit(
  host: string,
  input: AuditInput,
  result: AuditResult,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<string | null> {
  try {
    await postAudit(host, buildAuditRecord(input, 'result', result), timeoutMs, signal);
    return null;
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    return `result 审计写失败（调用已发生，审计链不完整）：${reason}`;
  }
}

/** 写一条审计。宿主承诺「200 即已落盘」，因此非 200 一律当失败。 */
async function postAudit(
  host: string,
  record: Json,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<void> {
  const response = await fetchJSON(`${host}/tenon/audit`, {
    method: 'POST',
    body: canonicalJSON(record),
    timeoutMs,
    signal,
  });
  if (response.status !== 200) throw normalizeFailure(response.status, response.text);
}

/**
 * 构造一条审计记录。
 *
 * 字段与 `audit-event.schema.json` 的 required 一一对应，与 Go 的 `auditRecord` 同形同序。
 * `hash` / `prevHash` **刻意不发**：链的权威只有宿主审计存储一个，客户端算一份只会制造第二种真相。
 */
function buildAuditRecord(input: AuditInput, stage: 'dispatched' | 'result', result: AuditResult): Json {
  const record: { [key: string]: Json } = {
    auditId: input.auditId,
    stage,
    at: timestamp(),
    actor: {
      type: 'agent',
      id: input.config.agentID,
      onBehalfOf: input.config.onBehalfOf,
    },
    trigger: { eventId: null, correlationId: null },
    planId: input.planId,
    stepId: STEP_ID,
    action: input.actionId,
    inputHash: input.inputHash,
    idempotencyKey: input.idemKey,
    decision: {
      outcome: input.decisionOutcome,
      policyId: input.policyId,
      // 本轮不调 grants:evaluate（M0.5 没有 grant 签发，见设计 §2）：没有授权依据就写 null，
      // 不伪造一个 grantId。
      grantId: null,
      // auto 路径没有人按批准；approve 路径写真实的审批人（部署配置里的操作员，见设计 §3.2）。
      approvedBy: input.approvedBy,
    },
    result,
    effects: { entities: [] },
    // internal/llm 不回传用量（Go 侧同样写 0/0）。审计要求字段必须出现，缺失与 null 是不同字节。
    tokenUsage: { in: 0, out: 0 },
  };
  return record;
}

/** RFC 3339 UTC，与 Go 的 `time.Now().UTC().Format(time.RFC3339)` 同形（无小数秒）。 */
function timestamp(): string {
  return new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** `pln_` / `aud_` 标识：base32（RFC 4648 字母表，满足词法 `[A-Za-z0-9_-]{4,}`）。 */
function randomToken(prefix: string, length: number): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const bytes = randomBytes(length);
  let out = '';
  for (let index = 0; index < length; index += 1) {
    out += alphabet[(bytes[index] ?? 0) % alphabet.length];
  }
  return prefix + out;
}

function parseJson(text: string, what: string): Json {
  try {
    return JSON.parse(text) as Json;
  } catch {
    throw new TenonCallError({
      code: 'MALFORMED_RESPONSE',
      errorClass: 'contract_violation',
      retryable: false,
      message: `${what} 的响应不是合法 JSON：${snippet(text)}`,
    });
  }
}

function parseBodyOrNull(text: string): Json {
  if (text.trim().length === 0) return null;
  try {
    return JSON.parse(text) as Json;
  } catch {
    // 出参不是 JSON 不改变业务结局（动作确实成功了），但要把原文交出去而不是吞掉。
    return text;
  }
}

function stringArray(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is string => typeof item === 'string' && item.trim().length > 0);
}

function firstNonEmpty(values: readonly string[]): string | null {
  for (const value of values) {
    if (value.trim().length > 0) return value.trim();
  }
  return null;
}
