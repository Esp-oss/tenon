// 消费入站队列里的 `tenon.confirm`，让**公告板上的批准真正落地**（设计 §10.2 选项 A）。
//
// 为什么必须有人做这一步：公告板点「批准」只是往入站队列塞一帧，而 `cmd/agent` 已冻结，
// DSH 路径上没有任何消费者——批准会一直躺在队列里，业务永远不动。
//
// **只读不 ack**（选项 A，刻意的）：
//   - 绝不调用 `/tenon/requests/ack`。ack 等于把队列的所有权从 agent 转到 adapter，
//     而队列里还有 `tenon.request`（用户原话）——那是一次治理变更，不该由本插件顺手完成；
//   - 代价：游标只在内存里，重启后从头读。幂等靠「已归档就不再执行」（`resumePlan` 第一步）
//     与模块的业务幂等键顶上；
//   - 收益：Go agent 哪天回来仍能接管这条队列，两边不会互相抢帧。
//
// 帧里的 `by` / `roles` 是**宿主用提交者 token 覆写过的**（`internal/controlplane/inbound.go:193-204`），
// 因此这是全仓唯一一条「审批人身份不由部署配置说了算」的路径。
import { fetchJSON, snippet, type Json } from './bridge.js';
import { DEFAULT_STATE_DIR, DEFAULT_TIMEOUT_MS, type ModuleHostConfig } from './config.js';
import { agentTokenFor } from './host-auth.js';
import { rejectPendingPlan, resumePlan } from './orchestrate.js';
import { loadArchived } from './pending.js';
import type { WakeEvent } from './wake.js';

/** 一次 tick 的结果（给人看的三行，也方便用例断言）。 */
export interface TickResult {
  /** 读到的 confirm 帧条数。 */
  confirms: number;
  /** 其中真的产生了动作（执行或驳回）的条数。 */
  applied: number;
  /** 一行一句的说明（失败、被拒、未执行）。 */
  notes: string[];
}

/** {@link createConfirmPoller} 的入参。 */
export interface ConfirmPollerOptions {
  config: ModuleHostConfig;
  /** 轮询间隔（毫秒），缺省 2000。 */
  intervalMs?: number;
  /** 一行日志；缺省丢弃（测试里通常传入收集器）。 */
  log?: (line: string) => void;
  /**
   * 确认**真的落地**之后的回调（执行成功或驳回成功）。
   *
   * 它是「批准后唤醒」的接入点：插件把 `exec.agent` 按 planId 存起来，
   * 这里用 `agent.followup` 唤醒提出计划的那个会话。回调抛错不会影响业务结果。
   */
  onExecuted?: (event: WakeEvent) => void;
}

/** 轮询器：手动 `tick()` 供测试与一次性调用，`start()`/`stop()` 管定时器。 */
export interface ConfirmPoller {
  /** 跑一轮：读帧、处理 confirm、推进游标。**不抛错**（失败写进 notes）。 */
  tick(): Promise<TickResult>;
  start(): void;
  stop(): void;
  /** 当前游标（只读，供用例断言「没有 ack，但游标确实前进了」）。 */
  cursor(): number;
  /** 累计处理过的入站帧条数（含被忽略的 request 帧）。 */
  seen(): number;
}

const DEFAULT_INTERVAL_MS = 2_000;

/**
 * 建一个轮询器。
 *
 * 注意它**不**决定自己何时跑：定时器由宿主（cordis 的 `ctx.effect`）负责启停，
 * 没有宿主时测试可以只调 `tick()`。
 */
export function createConfirmPoller(options: ConfirmPollerOptions): ConfirmPoller {
  const { config } = options;
  const intervalMs = options.intervalMs ?? DEFAULT_INTERVAL_MS;
  const log = options.log ?? (() => undefined);

  let cursor = 0;
  let seenFrames = 0;
  let timer: ReturnType<typeof setInterval> | null = null;
  let busy = false;

  async function tick(): Promise<TickResult> {
    const notes: string[] = [];
    const result: TickResult = { confirms: 0, applied: 0, notes };

    const token = await agentTokenFor(config);
    if (token === null) {
      notes.push('未配置 agent 凭据，未轮询（公告板批准不会被消费）');
      return result;
    }

    let body: Json;
    try {
      const response = await fetchJSON(
        `${config.hostURL.replace(/\/+$/, '')}/tenon/requests/poll?after=${cursor}`,
        {
          method: 'GET',
          headers: { authorization: `Bearer ${token}` },
          timeoutMs: config.timeoutMs ?? DEFAULT_TIMEOUT_MS,
        },
      );
      if (response.status !== 200) {
        notes.push(`轮询失败（HTTP ${response.status}：${snippet(response.text)}）`);
        return result;
      }
      body = JSON.parse(response.text) as Json;
    } catch (error) {
      notes.push(`轮询失败（${error instanceof Error ? error.message : String(error)}）`);
      return result;
    }

    const frames = readFrames(body);
    for (const entry of frames) {
      // 游标对**所有**帧前进（包括被忽略的 request 帧）：否则每一轮都会重读同一批，
      // 而「只读不 ack」已经把重读的代价限在内存游标上，不该再被卡住。
      if (entry.seq > cursor) cursor = entry.seq;
      seenFrames += 1;

      const frame = entry.frame;
      const type = typeof frame['type'] === 'string' ? frame['type'] : '';
      if (type !== 'tenon.confirm') continue;

      result.confirms += 1;
      const event = await handleConfirm(config, frame, notes, log);
      if (event !== null) {
        result.applied += 1;
        // 唤醒回调查完就断：它只负责「告诉提出计划的会话」，失败不能改写业务结论。
        // 回调内部的 try/catch 在插件侧（wake.notify）；这里再包一层防测试注入的裸回调。
        try {
          options.onExecuted?.(event);
        } catch (error) {
          log(`tenon 确认：唤醒回调异常（业务已完成）：${error instanceof Error ? error.message : String(error)}`);
        }
      }
    }
    return result;
  }

  return {
    tick,
    start(): void {
      if (timer !== null) return;
      timer = setInterval(() => {
        if (busy) return;
        busy = true;
        void tick()
          .then((result) => {
            for (const note of result.notes) log(`tenon 确认轮询：${note}`);
          })
          .catch((error: unknown) => log(`tenon 确认轮询异常：${String(error)}`))
          .finally(() => {
            busy = false;
          });
      }, intervalMs);
      // 不阻止进程退出：这是个后台辅助循环，不是进程存活理由。
      if (typeof timer === 'object' && timer !== null && 'unref' in timer) timer.unref();
    },
    stop(): void {
      if (timer === null) return;
      clearInterval(timer);
      timer = null;
    },
    cursor: () => cursor,
    seen: () => seenFrames,
  };
}

/** 解析 poll 响应里的 `frames: [{seq, frame}]`；形状不对就当没有帧（不是致命错误）。 */
function readFrames(body: Json): Array<{ seq: number; frame: Record<string, unknown> }> {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return [];
  const frames = body['frames'];
  if (!Array.isArray(frames)) return [];
  const out: Array<{ seq: number; frame: Record<string, unknown> }> = [];
  for (const entry of frames) {
    if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) continue;
    const record = entry as Record<string, unknown>;
    const seq = typeof record['seq'] === 'number' ? record['seq'] : Number(record['seq']);
    const frame = record['frame'];
    if (!Number.isFinite(seq)) continue;
    if (typeof frame !== 'object' || frame === null || Array.isArray(frame)) continue;
    out.push({ seq, frame: frame as Record<string, unknown> });
  }
  return out;
}

/**
 * 处理一条 confirm 帧。
 *
 * @returns 真的产生了动作时返回唤醒事件（执行成功 / 驳回成功），否则 null。
 */
async function handleConfirm(
  config: ModuleHostConfig,
  frame: Record<string, unknown>,
  notes: string[],
  log: (line: string) => void,
): Promise<WakeEvent | null> {
  const planId = typeof frame['planId'] === 'string' ? frame['planId'] : '';
  const by = typeof frame['by'] === 'string' ? frame['by'] : '';
  const decision = typeof frame['decision'] === 'string' ? frame['decision'] : '';
  const roles = Array.isArray(frame['roles'])
    ? frame['roles'].filter((role): role is string => typeof role === 'string')
    : [];

  if (planId.length === 0 || by.length === 0) {
    notes.push('收到一条缺 planId 或 by 的 confirm，已跳过');
    return null;
  }

  // 已归档就跳过：没有 ack 意味着重启后会重读旧帧，这一句就是幂等的入口。
  // （`resumePlan` 自己也会幂等返回，但那条路径的 status 是 executed，
  //  这里要区分「真的执行了」与「只是回放结论」，否则计数与日志都会说谎。）
  const stateDir = config.stateDir ?? DEFAULT_STATE_DIR;
  try {
    if (loadArchived(stateDir, planId) !== null) {
      notes.push(`${planId} 已归档，跳过重放`);
      return null;
    }
  } catch (error) {
    notes.push(`${planId} 的归档记录读不动（${error instanceof Error ? error.message : String(error)}）`);
    return null;
  }

  try {
    if (decision === 'reject') {
      const outcome = await rejectPendingPlan(config, planId, by);
      if (outcome === 'rejected') {
        log(`tenon 确认：${planId} 被 ${by} 驳回`);
        return { planId, by, decision: 'reject', detail: '' };
      }
      notes.push(`${planId} 已不在待批（驳回被忽略）`);
      return null;
    }

    if (decision !== 'approve') {
      notes.push(`${planId} 的 decision=${JSON.stringify(decision)} 不认识，已跳过`);
      return null;
    }

    const outcome = await resumePlan({
      config,
      planId,
      grant: { approvedBy: by, roles },
    });
    if (outcome.status === 'executed') {
      log(`tenon 确认：${planId} 由 ${by} 批准并执行`);
      return { planId, by, decision: 'approve', detail: outcome.detail };
    }
    notes.push(`${planId} 未执行：${outcome.detail}`);
    return null;
  } catch (error) {
    // 单帧失败不该打断整轮：队列里可能还有别的 confirm。
    notes.push(`${planId} 处理失败（${error instanceof Error ? error.message : String(error)}）`);
    return null;
  }
}
