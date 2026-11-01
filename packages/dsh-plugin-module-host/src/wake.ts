// 批准后的会话唤醒（M1 剩余项，设计 §10.3「仍未做：唤醒」）。
//
// 问题：公告板批准由确认轮询器执行，业务会完成，但 **DSH 会话里不会自动出现一句话**——
// 轮询器是后台定时器，没有「回合」可以回答。人要问 `tenon_pending` 或再续一次才知道结果。
//
// 做法（设计 §10.2 尾段给的方向）：挂起时把 `exec.agent` 留在内存 Map 里，按 planId 索引；
// 确认执行成功后用 `agent.followup(message)` 唤醒**提出这条计划的那个会话**。
// 限制必须写明：句柄只在同一进程存活期内有效。插件重启后 Map 为空 ⇒ 业务照常完成，
// 但聊天里不会自动冒出一句话（这不是故障，是「不猜持久句柄」的代价）。
//
// 消息形状来自已安装的 `@deepseek-ai/dsh-llm@0.2.0-rc.2` 类型：
//   UserMessage = { id: MessageId(string), role: 'user', content: ContentBlock[], source: {kind:'user'} }
// 本包不 import dsh-llm（避免为一条消息把整个 LLM 包变成运行时依赖），
// 但形状是**照类型抄的**，不是猜的；如果将来 dsh 加了运行时校验，followup 会抛错，
// 我们捕获后只记日志——业务已经完成，唤醒失败不该回滚任何东西。
import { randomUUID } from 'node:crypto';

/** DSH Agent 的最小结构：本模块只用到 `followup`（唤醒一个新回合）。 */
export interface WakeAgent {
  followup(message: WakeMessage): void;
}

/** 与 dsh-llm `UserMessage` 同形的最小子集。 */
export interface WakeMessage {
  readonly id: string;
  readonly role: 'user';
  readonly content: readonly { readonly type: 'text'; readonly text: string }[];
  readonly source: { readonly kind: 'user' };
}

/** 一次确认落地的结果（轮询器在真的执行/驳回后发出）。 */
export interface WakeEvent {
  planId: string;
  by: string;
  decision: 'approve' | 'reject';
  detail: string;
}

/** 计划号 → 会话句柄的登记表。 */
export interface WakeRegistry {
  /** 记录「提出这条计划的会话句柄」；形状不对（没有 followup）就忽略。 */
  remember(planId: string, agent: unknown): void;
  /** 唤醒对应会话；返回是否真的投递了。没有句柄或投递失败都返回 false（并写日志）。 */
  notify(event: WakeEvent): boolean;
  /** 当前登记的句柄数（用例断言「执行后不残留」用）。 */
  size(): number;
}

/**
 * 句柄上限：挂起后一直没人批的计划会一直占着句柄。
 *
 * 100 远大于一个人在 DSH 里同时挂起的计划数；超出时按插入序丢最旧——宁可唤醒不了
 * 很早以前的那条（业务仍会完成），也不让 Map 无界增长。
 */
const MAX_HANDLES = 100;

/** 建一个唤醒登记表。`log` 只用于说明「为什么没唤醒」，不参与业务结果。 */
export function createWakeRegistry(log: (line: string) => void = () => undefined): WakeRegistry {
  const handles = new Map<string, WakeAgent>();
  return {
    remember(planId: string, agent: unknown): void {
      if (!planId) return;
      if (typeof agent !== 'object' || agent === null) return;
      const candidate = agent as { followup?: unknown };
      if (typeof candidate.followup !== 'function') return;
      if (!handles.has(planId) && handles.size >= MAX_HANDLES) {
        const oldest = handles.keys().next().value;
        if (oldest !== undefined) {
          handles.delete(oldest);
          log(`tenon 唤醒：句柄超过 ${MAX_HANDLES} 个，丢掉最旧的 ${oldest}（业务不受影响，只是不会自动唤醒）`);
        }
      }
      handles.set(planId, agent as WakeAgent);
    },
    notify(event: WakeEvent): boolean {
      const agent = handles.get(event.planId);
      if (!agent) {
        log(`tenon 唤醒：没有 ${event.planId} 的会话句柄（插件重启或跨进程）；业务已完成，聊天里不会自动出现`);
        return false;
      }
      // 先摘掉再投递：投递失败也不重试——重复 wake 会在会话里刷出多条同样的结论。
      handles.delete(event.planId);
      try {
        agent.followup(wakeMessage(event));
        return true;
      } catch (error) {
        log(`tenon 唤醒失败（业务已完成，不影响审计与库存）：${error instanceof Error ? error.message : String(error)}`);
        return false;
      }
    },
    size: () => handles.size,
  };
}

/** 唤醒消息：一句话说清「哪一单、谁批的、执行了没有」。 */
export function wakeMessage(event: WakeEvent): WakeMessage {
  const head =
    event.decision === 'reject'
      ? `【Tenon】${event.planId} 已被 ${event.by} 驳回，未执行。`
      : `【Tenon】${event.planId} 已由 ${event.by} 在公告板批准并执行。`;
  const detail = event.detail.trim();
  return {
    id: randomUUID(),
    role: 'user',
    content: [{ type: 'text', text: detail ? `${head}${detail}` : head }],
    source: { kind: 'user' },
  };
}
