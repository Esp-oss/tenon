// 传输层与错误归一化：把「一次 HTTP 调用」压成两件东西——原始响应，或一个带协议错误分类
// 的 {@link TenonCallError}。
//
// 这里刻意不做「非 2xx 就抛」的策略决定：不同闸门对状态码的语义不同（策略评估要求 200、
// 预演只要 2xx、审计非 2xx 必须终止），由 orchestrate 按闸门语义判定，本层只负责
// 发出去、读回来、把 host 的 `tenonError` 信封翻译成可判定的分类。
/** 协议版本（与 Go 的 `protocol.Version` 一致）。所有请求都带它，见 `internal/agent/run.go` 的 postJSON。 */
export const TENON_VERSION = '0.1';

/** 请求头名（与 Go 的 `protocol` 常量逐字一致）。 */
export const HEADER_VERSION = 'Tenon-Version';
export const HEADER_IDEMPOTENCY_KEY = 'Idempotency-Key';
/** 宿主签发的执行票据（0.2.0）。写动作没它模块一律 403。 */
export const HEADER_TICKET = 'Tenon-Ticket';

/** 协议里「可容纳任意 JSON」的取值类型。与 dsh-tools 的 JsonValue 同构，但不必为此多引一个包。 */
export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

/** 错误分类四态（spec/0.1/errors.md）。 */
export type TenonErrorClass =
  | 'retryable'
  | 'business_rejection'
  | 'policy_denied'
  | 'contract_violation';

const ERROR_CLASSES: readonly TenonErrorClass[] = [
  'retryable',
  'business_rejection',
  'policy_denied',
  'contract_violation',
];

function isErrorClass(value: unknown): value is TenonErrorClass {
  return typeof value === 'string' && (ERROR_CLASSES as readonly string[]).includes(value);
}

/** 入参自检失败时用的错误码（与 host 的错误码同一字符集：大写蛇形）。 */
export const CODE_INVALID_ARGUMENTS = 'INVALID_ARGUMENTS';
/** 传输层没拿到响应（连不上、超时、被取消）时的错误码。 */
export const CODE_TRANSPORT_ERROR = 'TRANSPORT_ERROR';

/**
 * 一次协议调用失败。
 *
 * `code` 是给模型看的稳定机器码（`TENON_POLICY_DENIED` / `MODULE_NOT_FOUND` / host 自己给的码）；
 * `errorClass` 决定上层该重试、重规划还是转人工；`retryable` 与 class 必须一致（协议规定
 * 只有 retryable 可为 true）。字段名刻意与 Go 侧 `protocol.Error` 对齐，排障时两侧说同一种话。
 */
export class TenonCallError extends Error {
  readonly code: string;
  readonly errorClass: TenonErrorClass;
  readonly retryable: boolean;
  readonly status: number | null;
  readonly details: Json | null;

  constructor(init: {
    code: string;
    errorClass: TenonErrorClass;
    retryable: boolean;
    message: string;
    status?: number | null;
    details?: Json | null;
  }) {
    super(init.message);
    this.name = 'TenonCallError';
    this.code = init.code;
    this.errorClass = init.errorClass;
    this.retryable = init.retryable;
    this.status = init.status ?? null;
    this.details = init.details ?? null;
  }
}

/** 入参不是可以安全发出的 JSON（含函数、undefined、NaN、循环引用等）。 */
export function invalidArguments(message: string): TenonCallError {
  return new TenonCallError({
    code: CODE_INVALID_ARGUMENTS,
    errorClass: 'contract_violation',
    retryable: false,
    message,
  });
}

/**
 * 规范 JSON 文本：键按 UTF-16 码元序排序、无空白、数字用 ECMAScript 写法。
 *
 * **为什么自己写**：`@tenon/core` 目前没有导出 `canonicalJSON`——Go 侧那一份在
 * `internal/protocol/protocol.go`（RFC 8785 JCS），TS 侧没有对应实现。按计划的要求不复制
 * Go 的实现，这里只做「键排序 + 紧凑输出」这一段足够的能力，并且**同一个字符串被用在两处**：
 * 既作为请求体发出，又是 `inputHash` 的摘要输入。这样 `inputHash` 证明的就是「实际发出的
 * 那份字节」（与 Go 的 `hashPayload` 同一意图），而不是另编一份对不上的文本。
 *
 * **已知缺口**：Go 的 `hashPayload` 摘要的是计划里 `args` 的**原始字节**，其键序由计划 JSON
 * 决定、不经规范化；因此两侧对「同一份语义相同的 args」算出的 `inputHash` 可能不同。跨实现
 * 一致需要一个双方共用的 RFC 8785 实现，本轮不做（已记入交付说明）。
 *
 * @param value 任意 JSON 值。
 * @returns 规范文本。
 * @throws TenonCallError 取值不是合法 JSON（函数、undefined、非有限数、BigInt 等）。
 */
export function canonicalJSON(value: unknown): string {
  return writeCanonical(value, '$');
}

function writeCanonical(value: unknown, path: string): string {
  if (value === null) return 'null';
  switch (typeof value) {
    case 'string':
      return JSON.stringify(value);
    case 'boolean':
      return value ? 'true' : 'false';
    case 'number':
      if (!Number.isFinite(value)) {
        throw invalidArguments(`args 的 ${path} 不是有限数值（NaN/Infinity 不是 JSON 值）`);
      }
      // JSON.stringify(number) 用的正是 ECMAScript Number::toString，与 RFC 8785 §3.2.2.3 同。
      return JSON.stringify(value);
    case 'object':
      break;
    default:
      throw invalidArguments(`args 的 ${path} 类型是 ${typeof value}，不是合法的 JSON 值`);
  }
  if (Array.isArray(value)) {
    const items = value.map((item, index) => writeCanonical(item, `${path}[${index}]`));
    return `[${items.join(',')}]`;
  }
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record).sort();
  const pairs = keys.map(
    (key) => `${JSON.stringify(key)}:${writeCanonical(record[key], `${path}.${key}`)}`,
  );
  return `{${pairs.join(',')}}`;
}

/** 一次 HTTP 调用的结果：状态码 + 原始响应体文本（不做 JSON 解析，摘要要的正是原始字节）。 */
export interface JsonResponse {
  status: number;
  text: string;
}

/** {@link fetchJSON} 的入参。 */
export interface RequestOptions {
  method: 'GET' | 'POST';
  /** 已序列化好的 JSON 文本；省略表示没有请求体。 */
  body?: string;
  headers?: Record<string, string>;
  timeoutMs: number;
  /** 调用方的取消信号（工具的 `exec.signal`），与本层超时信号合并。 */
  signal?: AbortSignal;
}

/**
 * 发一次协议调用。
 *
 * - 所有请求带 `Tenon-Version`（Go 侧 postJSON 亦如此）；
 * - 超时与调用方取消合并成一个信号；
 * - 拿到响应就返回（**不**因非 2xx 抛错），拿不到响应才抛 retryable 的传输错误。
 *
 * @param url 绝对地址。
 * @param options 方法、请求体、附加头、超时与取消信号。
 * @returns 状态码与原始响应体。
 * @throws TenonCallError 传输层失败（连不上 / 超时 / 被取消）。
 */
export async function fetchJSON(url: string, options: RequestOptions): Promise<JsonResponse> {
  const headers: Record<string, string> = {
    [HEADER_VERSION]: TENON_VERSION,
    accept: 'application/json',
    ...options.headers,
  };
  if (options.body !== undefined) headers['content-type'] = 'application/json';

  const signals = [AbortSignal.timeout(options.timeoutMs)];
  if (options.signal) signals.push(options.signal);

  let response: Response;
  try {
    response = await fetch(url, {
      method: options.method,
      headers,
      body: options.body,
      signal: AbortSignal.any(signals),
    });
  } catch (error) {
    throw transportError(url, error);
  }

  let text: string;
  try {
    text = await response.text();
  } catch (error) {
    throw transportError(url, error);
  }
  return { status: response.status, text };
}

function transportError(url: string, cause: unknown): TenonCallError {
  const reason = cause instanceof Error ? (cause.cause instanceof Error ? cause.cause.message : cause.message) : String(cause);
  return new TenonCallError({
    code: CODE_TRANSPORT_ERROR,
    errorClass: 'retryable',
    retryable: true,
    message: `${url} 没有拿到响应（结果未定，可用同一幂等键重试）：${reason}`,
  });
}

/**
 * 把一次失败响应归一化成 {@link TenonCallError}。
 *
 * 优先信 host 的 `{tenonError:{class,code,message,retryable,details}}`；信封缺失或不合规时按
 * 状态码兜底（5xx/429 → retryable，其余 → contract_violation），并把原始片段留在 message 里，
 * 免得「host 回了个 HTML 500」时排障只能靠猜。
 *
 * @param status HTTP 状态码。
 * @param text 原始响应体。
 * @returns 归一化后的错误。
 */
export function normalizeFailure(status: number, text: string): TenonCallError {
  const envelope = parseTenonError(text);
  if (envelope) return envelope;
  const fallbackClass: TenonErrorClass =
    status >= 500 || status === 429 ? 'retryable' : 'contract_violation';
  return new TenonCallError({
    code: `HTTP_${status}`,
    errorClass: fallbackClass,
    retryable: fallbackClass === 'retryable',
    message: `HTTP ${status}：${snippet(text)}`,
    status,
  });
}

function parseTenonError(text: string): TenonCallError | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null) return null;
  const body = (parsed as Record<string, unknown>)['tenonError'];
  if (typeof body !== 'object' || body === null) return null;
  const record = body as Record<string, unknown>;
  const code = record['code'];
  const message = record['message'];
  const errorClass = record['class'];
  if (typeof code !== 'string' || code.length === 0) return null;
  if (typeof message !== 'string' || message.length === 0) return null;
  if (!isErrorClass(errorClass)) return null;
  const retryable = record['retryable'];
  const details = record['details'];
  return new TenonCallError({
    code,
    errorClass,
    retryable: typeof retryable === 'boolean' ? retryable : errorClass === 'retryable',
    message,
    status: null,
    details: isJson(details) ? details : null,
  });
}

function isJson(value: unknown): value is Json {
  if (value === null) return true;
  const kind = typeof value;
  if (kind === 'string' || kind === 'boolean') return true;
  if (kind === 'number') return Number.isFinite(value as number);
  if (Array.isArray(value)) return value.every(isJson);
  if (kind === 'object') return Object.values(value as Record<string, unknown>).every(isJson);
  return false;
}

/** 截取响应体前一段放进错误信息，按字符截断以免切坏汉字。 */
export function snippet(text: string, limit = 200): string {
  const trimmed = text.trim();
  const chars = [...trimmed];
  return chars.length <= limit ? trimmed : `${chars.slice(0, limit).join('')}…`;
}
