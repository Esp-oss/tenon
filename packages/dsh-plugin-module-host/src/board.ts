// 待批公告板的上推（`POST /tenon/pending`，设计见 interfaces.md §4.7）。
//
// 定位要先说清：**公告板是只读 read-model，不是真相**。真相是 adapter 状态目录里的
// pending 文件（`internal/controlplane/pending.go:34-38` 也是这么说的：公告板刻意不落盘、
// 可丢可重建）。因此这里的失败**一律不升级为业务失败**——推不上去只是「看不见」，
// 而看不见不等于这一单不存在。
//
// 权限：写入只允许 `role:agent`，所以需要 agent 身份（见 host-auth.ts）。没配就跳过，
// 并把原因作为告警字符串交回调用方，由它拼进 detail/note —— 让「为什么公告板上看不见」
// 有据可查，而不是静默消失。
import { fetchJSON, snippet } from './bridge.js';
import type { ModuleHostConfig } from './config.js';
import { DEFAULT_TIMEOUT_MS } from './config.js';
import { agentTokenFor } from './host-auth.js';
import type { PendingPlan, TerminalState } from './pending.js';

/** 公告板上一行的形状（宿主 `pendingItem`，`internal/controlplane/pending.go:20-32`）。 */
export interface BoardCard {
  planId: string;
  /** 人话摘要：卡片上一眼能看懂是什么事。 */
  summary: string;
  requiredRoles: string[];
  expiresAt: string;
  /** `awaiting_approval` 或终态（`done`/`rejected`/`expired`/`stale`）。 */
  state: string;
}

/** 摘要上限：卡片是给人扫一眼的，不是审计（审计那边有完整记录）。 */
const SUMMARY_LIMIT = 160;

/** 待批卡片：摘要 + 需要的角色 + 有效期。 */
export function pendingCard(plan: PendingPlan): BoardCard {
  return {
    planId: plan.planId,
    summary: summaryOf(plan),
    requiredRoles: plan.requiredRoles,
    expiresAt: plan.expiresAt,
    state: 'awaiting_approval',
  };
}

/** 终态卡片：同 planId 幂等 upsert，把待批那张覆盖成结果（宿主不按 state 过滤）。 */
export function terminalCard(plan: PendingPlan, state: TerminalState, note: string | null): BoardCard {
  return {
    planId: plan.planId,
    summary:
      `${plan.module}.${plan.action} ${state === 'done' ? '已执行' : `已终结（${state}）`}` +
      (note === null ? '' : `：${note}`),
    requiredRoles: plan.requiredRoles,
    expiresAt: plan.expiresAt,
    state,
  };
}

/**
 * 推一张卡片。
 *
 * @returns 成功 null；失败或未启用时返回一句人话（调用方拼进 detail/note）。
 * **不抛错**：公告板是展示层，它的失败不该让一次已经发生的业务动作变成失败。
 */
export async function pushBoardCard(
  config: ModuleHostConfig,
  card: BoardCard,
  signal?: AbortSignal,
): Promise<string | null> {
  let token: string | null;
  try {
    token = await agentTokenFor(config, signal);
  } catch (error) {
    return `公告板未更新（${error instanceof Error ? error.message : String(error)}）`;
  }
  if (token === null) return '公告板未更新（未配置 agent 凭据，属预期）';

  try {
    const response = await fetchJSON(`${config.hostURL.replace(/\/+$/, '')}/tenon/pending`, {
      method: 'POST',
      body: JSON.stringify(card),
      headers: { authorization: `Bearer ${token}` },
      timeoutMs: config.timeoutMs ?? DEFAULT_TIMEOUT_MS,
      signal,
    });
    if (response.status !== 200) {
      return `公告板未更新（HTTP ${response.status}：${snippet(response.text)}）`;
    }
    return null;
  } catch (error) {
    return `公告板未更新（${error instanceof Error ? error.message : String(error)}）`;
  }
}

/** 卡片摘要：动作 + 入参摘要，超长截断（不让一张卡把页面撑爆）。 */
function summaryOf(plan: PendingPlan): string {
  let args: string;
  try {
    args = JSON.stringify(plan.args);
  } catch {
    args = '（入参无法序列化）';
  }
  const text = `${plan.module}.${plan.action} ${args}`;
  return text.length <= SUMMARY_LIMIT ? text : `${text.slice(0, SUMMARY_LIMIT - 1)}…`;
}
