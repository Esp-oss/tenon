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
import { DEFAULT_TIMEOUT_MS, type ModuleHostConfig } from './config.js';

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
  const { config, module, action, signal } = input;
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
      return {
        status: 'needs_approval',
        planId,
        module,
        action,
        // 「未执行」这四个字是刻意的：模型把 needs_approval 读成「完成」是设计里点名的风险。
        detail:
          `策略判定需要人工批准，**未执行**（planId=${planId}）：` +
          `decidedBy=${evaluation.decidedBy.join(',') || '∅'}` +
          `${evaluation.approvers.length > 0 ? `，需要 ${evaluation.approvers.join('/')}` : ''}。` +
          '本轮只报告待批，不挂起、不重试、不得换路绕过。',
        result: null,
        requiredRoles: evaluation.approvers,
      };
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

  // ---- 3. 预演：模块自己说这一步会不会成 ----
  await dryRun(baseURL, actionId, argsBody, timeoutMs, signal);

  // ---- 4. I7：dispatched 审计必须先落盘，写不进去就绝不执行 ----
  const auditId = randomToken('aud_', 20);
  const auditInput: AuditInput = {
    config,
    actionId,
    planId,
    auditId,
    inputHash,
    idemKey,
    policyId: firstNonEmpty(evaluation.decidedBy),
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
    const reason = error instanceof Error ? error.message : String(error);
    throw new TenonCallError({
      code: 'AUDIT_WRITE_FAILED',
      errorClass: 'contract_violation',
      retryable: false,
      message: `dispatched 审计写失败，**未执行任何动作**（${actionId}）：${reason}`,
      details: { planId, module, action },
    });
  }

  // ---- 5. 执行 ----
  let response: JsonResponse;
  try {
    response = await fetchJSON(`${baseURL}/tenon/actions/${actionId}`, {
      method: 'POST',
      body: argsBody,
      headers: { [HEADER_IDEMPOTENCY_KEY]: idemKey },
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
    throw new TenonCallError({
      code: 'ACTION_UNKNOWN_OUTCOME',
      errorClass: 'retryable',
      retryable: true,
      message: `${actionId} 的调用结果未定（已写 result 审计 status=unknown）：${reason}`,
      details: { planId, module, action, idempotencyKey: idemKey },
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
    throw failure;
  }

  // ---- 6. 收尾：result 审计无论成败都写；它写不进去**不改业务结局**（调用已经发生了）----
  const auditNote = await settleAudit(
    host,
    auditInput,
    { status: 'ok', outputHash: sha256Digest(response.text), errorClass: null },
    timeoutMs,
    signal,
  );

  return {
    status: 'executed',
    planId,
    module,
    action,
    detail:
      `已执行 ${actionId}（planId=${planId}，幂等键 ${idemKey}）` +
      (auditNote === null ? '' : `；**警告**：${auditNote}`),
    result: parseBodyOrNull(response.text),
    requiredRoles: [],
  };
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
      outcome: 'auto',
      policyId: input.policyId,
      // 本轮不调 grants:evaluate（M0.5 没有 grant 签发，见设计 §2）：没有授权依据就写 null，
      // 不伪造一个 grantId。
      grantId: null,
      // auto 路径没有人按批准。审批人只能由闸门结论带进来，不能凭配置编。
      approvedBy: null,
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
