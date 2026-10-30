// 待批计划的盘上真相（设计 §4）。
//
// 为什么由 adapter 自己落盘、而不是问 host 要：公告板的写入端点要求 `role:agent` bearer
// token（`internal/controlplane/pending.go:116-120`），adapter 没有 token；而 ADR 不变量 3
// 的原话是「pending 的权威副本在 **agent 状态目录 / 公告板**」——adapter 就是 DSH 侧的 agent，
// 它自己的状态目录正是被允许的那个位置。
//
// 与 Go 侧刻意同构（`internal/agent/state.go:82-84`）：一个 planId 一个 JSON 文件，归档是
// 「先写 archived 再删 pending」，状态是文件里的字段而不是改名——改名在崩溃时会留下两处
// 都不像的半截状态，字段不会。
import { randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { TenonCallError, type Json } from './bridge.js';

/**
 * planId 的词法。
 *
 * 这不只是格式检查：planId 来自模型（`tenon_resume` 的参数）并**直接拼进文件路径**，
 * 不关死就能用 `../../` 写到别的目录去。长度上限顺带挡住超长文件名。
 */
export const PLAN_ID_PATTERN = /^pln_[A-Za-z0-9_-]{4,64}$/;

/** 挂起中的状态。`resuming` 是「已开始执行」的粘滞标记，防二次批准。 */
export type PendingState = 'awaiting_approval' | 'resuming';

/** 终态。与 Go 侧同名同义（`state.go` 的 `done`/`rejected`/`expired`/`stale`）。 */
export type TerminalState = 'done' | 'rejected' | 'expired' | 'stale';

/** 一条待批计划：**完整到足以重放出那次调用**，不是展示摘要。 */
export interface PendingPlan {
  planId: string;
  module: string;
  action: string;
  /** 规范化后的入参（与 `inputHash` 摘要的那份字节完全一致）。 */
  args: Json;
  inputHash: string;
  idempotencyKey: string;
  /** 策略给出的审批角色；空数组表示这张卡谁都批不了（调用方必须拒绝挂起）。 */
  requiredRoles: string[];
  policyId: string | null;
  createdAt: string;
  expiresAt: string;
  state: PendingState;
  /** I7 第三步的回写标记：true 表示计划级 dispatched 审计已经落盘。 */
  auditSettled: boolean;
  /** 计划级 dispatched 的 auditId（挂起时生成，续跑时计划级 result 共用它）。 */
  dispatchedAuditId: string;
}

/** 归档后的计划：终态 + 结局，足以回答「这一单最后怎么了」。 */
export interface ArchivedPlan extends Omit<PendingPlan, 'state'> {
  state: TerminalState;
  finishedAt: string;
  /** 审批人（`user:<账号>`）。auto 路径与「没人批」时为 null。 */
  approvedBy: string | null;
  /** 计划级结局摘要（写进归档，便于事后查询不必翻审计）。 */
  result: Json;
  /** 归档原因（失败/过期的简述），没有则为 null。 */
  note: string | null;
}

/** 一条待批目录里的条目，带上「现在算不算过期」。 */
export interface PendingEntry {
  plan: PendingPlan;
  expired: boolean;
}

/** RFC 3339 UTC，无小数秒（与 Go 的 `time.Now().UTC().Format(time.RFC3339)` 同形）。 */
export function rfc3339(date: Date): string {
  return date.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** 待批目录。 */
export function pendingDir(stateDir: string): string {
  return path.join(stateDir, 'pending');
}

/** 归档目录。 */
export function archiveDir(stateDir: string): string {
  return path.join(stateDir, 'pending', 'archived');
}

/**
 * 校验 planId。
 *
 * @throws TenonCallError code=`INVALID_PLAN_ID`（词法不对就等于路径穿越尝试）
 */
export function assertPlanId(planId: unknown): asserts planId is string {
  if (typeof planId !== 'string' || !PLAN_ID_PATTERN.test(planId)) {
    throw new TenonCallError({
      code: 'INVALID_PLAN_ID',
      errorClass: 'contract_violation',
      retryable: false,
      message: `planId=${JSON.stringify(planId)} 不是合法的计划标识（必须匹配 ${PLAN_ID_PATTERN.source}）`,
    });
  }
}

/**
 * 写入一条待批计划（同 planId 覆盖 = 幂等 upsert）。
 *
 * 原子性靠「先写临时文件、再 rename」：rename 在同一目录内是原子的，因此读侧要么看到旧
 * 内容、要么看到新内容，**看不到半个 JSON**。I7 的第二步（写审计）依赖这个前提——
 * 盘上出现一条 `auditSettled=false` 的记录，必须能确定它是完整可读的。
 *
 * @throws TenonCallError planId 非法；或落盘失败（磁盘/权限）
 */
export function savePending(stateDir: string, plan: PendingPlan): void {
  assertPlanId(plan.planId);
  try {
    ensureDir(pendingDir(stateDir));
  } catch (error) {
    // 目录建不出来也是落盘失败：把它归一成同一个码，调用方只需要判一件事。
    throw writeFailed(path.basename(pendingDir(stateDir)), error);
  }
  atomicWrite(fileOf(pendingDir(stateDir), plan.planId), plan);
}

/**
 * 读回一条待批计划。
 *
 * @returns 不存在时 null（「没有这一单」是正常答案，不是错误）；内容损坏时抛。
 * @throws TenonCallError code=`INVALID_PLAN_ID` / `PENDING_CORRUPT`
 */
export function loadPending(stateDir: string, planId: string): PendingPlan | null {
  assertPlanId(planId);
  return readJson(fileOf(pendingDir(stateDir), planId), (value) => validatePending(value, planId));
}

/**
 * 列出全部待批计划（按 `expiresAt` 升序，最急的在前面）。
 *
 * 过期条目**照样返回**并标 `expired: true`：怎么处置（提示已过期 / 归档）是调用方的语义，
 * 存储层替它过滤会让「有一单过期了」这件事从界面上消失。
 *
 * @throws TenonCallError 目录里有读不动的文件（宁可红，也不静默丢一单）
 */
export function listPending(stateDir: string, now: number = Date.now()): PendingEntry[] {
  const dir = pendingDir(stateDir);
  if (!existsSync(dir)) return [];
  const entries: PendingEntry[] = [];
  for (const name of readdirSync(dir)) {
    if (!name.endsWith('.json')) continue;
    const planId = name.slice(0, -'.json'.length);
    const plan = readJson(path.join(dir, name), (value) => validatePending(value, planId, name));
    if (plan !== null) entries.push({ plan, expired: isExpired(plan, now) });
  }
  entries.sort((left, right) => left.plan.expiresAt.localeCompare(right.plan.expiresAt));
  return entries;
}

/** 是否过期。解析不出 `expiresAt` 时按**过期**处理（fail-closed：不执行没把握的东西）。 */
export function isExpired(plan: PendingPlan, now: number = Date.now()): boolean {
  const at = Date.parse(plan.expiresAt);
  return Number.isNaN(at) ? true : at <= now;
}

/**
 * 归档：先写 `pending/archived/<planId>.json`，再删待批原件。
 *
 * 顺序不能反：先删后写的话，中间崩溃会让这一单**同时**从待批和归档里消失——那才是真正的
 * 丢单。反过来崩溃只会留下一份「已归档但待批还在」的重复，续跑时以归档为准即可自愈。
 *
 * @returns 归档后的完整记录（调用方通常把它写进返回值）
 */
export function archivePlan(
  stateDir: string,
  plan: PendingPlan,
  state: TerminalState,
  extras: { approvedBy?: string | null; result?: Json; note?: string | null; now?: Date } = {},
): ArchivedPlan {
  assertPlanId(plan.planId);
  ensureDir(archiveDir(stateDir));
  const archived: ArchivedPlan = {
    ...plan,
    state,
    finishedAt: rfc3339(extras.now ?? new Date()),
    approvedBy: extras.approvedBy ?? null,
    result: extras.result ?? null,
    note: extras.note ?? null,
  };
  atomicWrite(fileOf(archiveDir(stateDir), plan.planId), archived);
  rmSync(fileOf(pendingDir(stateDir), plan.planId), { force: true });
  return archived;
}

/** 读归档记录（续跑的第一步：已归档 `done` 的再次续跑必须幂等返回，而不是重放副作用）。 */
export function loadArchived(stateDir: string, planId: string): ArchivedPlan | null {
  assertPlanId(planId);
  return readJson(fileOf(archiveDir(stateDir), planId), (value) => validateArchived(value, planId));
}

// ---------- 内部 ----------

function fileOf(dir: string, planId: string): string {
  return path.join(dir, `${planId}.json`);
}

function ensureDir(dir: string): void {
  mkdirSync(dir, { recursive: true });
}

/** 临时文件 + rename。失败时清掉临时文件，绝不留半截目标。 */
function atomicWrite(file: string, value: unknown): void {
  const tmp = `${file}.${randomBytes(6).toString('hex')}.tmp`;
  try {
    writeFileSync(tmp, `${JSON.stringify(value, null, 2)}\n`, 'utf8');
    renameSync(tmp, file);
  } catch (error) {
    rmSync(tmp, { force: true });
    throw writeFailed(path.basename(file), error);
  }
}

/** 落盘失败的统一形状：不吞原始原因，也不让调用方去猜错误类型。 */
function writeFailed(name: string, error: unknown): TenonCallError {
  return new TenonCallError({
    code: 'PENDING_WRITE_FAILED',
    errorClass: 'contract_violation',
    retryable: false,
    message: `待批计划落盘失败（${name}）：${error instanceof Error ? error.message : String(error)}`,
  });
}

/** 读一个 JSON 文件：不存在返回 null；解析或校验失败抛 `PENDING_CORRUPT`。 */
function readJson<T>(file: string, validate: (value: unknown) => T): T | null {
  if (!existsSync(file)) return null;
  let text: string;
  try {
    text = readFileSync(file, 'utf8');
  } catch (error) {
    throw corrupt(path.basename(file), `读不动：${error instanceof Error ? error.message : String(error)}`);
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw corrupt(path.basename(file), '不是合法 JSON');
  }
  return validate(parsed);
}

function corrupt(name: string, reason: string): TenonCallError {
  return new TenonCallError({
    code: 'PENDING_CORRUPT',
    errorClass: 'contract_violation',
    retryable: false,
    message: `待批记录 ${name} 不可用（${reason}）：拒绝把读不懂的状态当成「没有这一单」`,
  });
}

const PENDING_STATES: readonly string[] = ['awaiting_approval', 'resuming'];
const TERMINAL_STATES: readonly string[] = ['done', 'rejected', 'expired', 'stale'];

/** 校验一条待批记录的形状。字段少一个都算损坏——续跑要靠它重放那次调用。 */
function validatePending(value: unknown, planId: string, name = `${planId}.json`): PendingPlan {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw corrupt(name, '顶层不是对象');
  }
  const record = value as Record<string, unknown>;
  if (record['planId'] !== planId) {
    throw corrupt(name, `planId 字段（${String(record['planId'])}）与文件名不一致`);
  }
  const strings = ['module', 'action', 'inputHash', 'idempotencyKey', 'createdAt', 'expiresAt', 'dispatchedAuditId'];
  for (const key of strings) {
    if (typeof record[key] !== 'string') throw corrupt(name, `缺少字符串字段 ${key}`);
  }
  if (!PENDING_STATES.includes(String(record['state']))) {
    throw corrupt(name, `state=${String(record['state'])} 不是挂起态（终态记录不该出现在待批目录）`);
  }
  if (typeof record['auditSettled'] !== 'boolean') throw corrupt(name, '缺少布尔字段 auditSettled');
  if (record['policyId'] !== null && typeof record['policyId'] !== 'string') {
    throw corrupt(name, 'policyId 只能是字符串或 null');
  }
  if (!isJsonObject(record['args'])) throw corrupt(name, 'args 不是 JSON 对象');
  const roles = record['requiredRoles'];
  if (!Array.isArray(roles) || roles.some((role) => typeof role !== 'string')) {
    throw corrupt(name, 'requiredRoles 不是字符串数组');
  }
  return record as unknown as PendingPlan;
}

/** 校验一条归档记录。 */
function validateArchived(value: unknown, planId: string, name = `${planId}.json`): ArchivedPlan {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw corrupt(`archived/${name}`, '顶层不是对象');
  }
  const record = value as Record<string, unknown>;
  if (record['planId'] !== planId) {
    throw corrupt(`archived/${name}`, `planId 字段（${String(record['planId'])}）与文件名不一致`);
  }
  if (!TERMINAL_STATES.includes(String(record['state']))) {
    throw corrupt(`archived/${name}`, `state=${String(record['state'])} 不是终态`);
  }
  if (typeof record['finishedAt'] !== 'string') throw corrupt(`archived/${name}`, '缺少 finishedAt');
  return record as unknown as ArchivedPlan;
}

/** JSON 对象（不含数组与 null）：入参是 JsonObject，这是它的运行时判定。 */
function isJsonObject(value: unknown): value is Json {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
