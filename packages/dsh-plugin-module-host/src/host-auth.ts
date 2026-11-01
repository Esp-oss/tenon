// host 的 agent 身份：换 token 并缓存（公告板写入、入站队列消费都要 `role:agent`）。
//
// 为什么需要它：公告板 `POST /tenon/pending` 只允许 `role:agent`
// （`internal/controlplane/pending.go:116-120`），而 adapter 默认没有任何身份。
//
// **这是一项权限**，所以设计成显式配置、缺省关闭：
//   - 没有配 `agentToken` 也没有配 `agentUser`/`agentPassword` ⇒ `agentTokenFor` 返回 null，
//     调用方（公告板推送）直接跳过并说明原因，业务流程完全不受影响；
//   - 配了就用 `POST /tenon/session` 换 token（与 Go 的 `cmd/agent` 同一条路），或直接用现成的 token。
//
// token 只在这里出现，不进日志、不进审计、不进工具参数。
import { fetchJSON, TenonCallError, snippet, type Json } from './bridge.js';
import { DEFAULT_TIMEOUT_MS, type ModuleHostConfig } from './config.js';

/** 一个缓存条目：token + 它自己的过期时刻（毫秒）。 */
interface CachedToken {
  token: string;
  expiresAt: number;
}

/** 按 `host|账号` 缓存：同一部署里反复推送不该反复登录。 */
const cache = new Map<string, CachedToken>();

/** 提前 60 秒换新，避免「刚好在请求途中过期」。 */
const REFRESH_MARGIN_MS = 60_000;

/** 清空缓存（测试用；生产不需要）。 */
export function resetTokenCache(): void {
  cache.clear();
}

/** 是否配置了 agent 身份（没配就等于「公告板功能关闭」）。 */
export function hasAgentCredentials(config: ModuleHostConfig): boolean {
  if (typeof config.agentToken === 'string' && config.agentToken.trim().length > 0) return true;
  return (
    typeof config.agentUser === 'string' &&
    config.agentUser.trim().length > 0 &&
    typeof config.agentPassword === 'string' &&
    config.agentPassword.length > 0
  );
}

/**
 * 取一个可用于 `role:agent` 的 token。
 *
 * @returns 未配置 agent 身份时 null（这不是错误，是「功能没开」）。
 * @throws TenonCallError code=`AGENT_LOGIN_FAILED`（配了凭据但换不到 token）
 */
export async function agentTokenFor(
  config: ModuleHostConfig,
  signal?: AbortSignal,
): Promise<string | null> {
  if (!hasAgentCredentials(config)) return null;

  // 直接给了 token：不猜它的有效期，原样用（过期时由调用方看到 401 并写进告警）。
  const staticToken = config.agentToken?.trim();
  if (typeof staticToken === 'string' && staticToken.length > 0) return staticToken;

  const user = config.agentUser!.trim();
  const key = `${config.hostURL.replace(/\/+$/, '')}|${user}`;
  const cached = cache.get(key);
  if (cached !== undefined && cached.expiresAt - REFRESH_MARGIN_MS > Date.now()) return cached.token;

  const host = config.hostURL.replace(/\/+$/, '');
  const response = await fetchJSON(`${host}/tenon/session`, {
    method: 'POST',
    body: JSON.stringify({ user, password: config.agentPassword }),
    timeoutMs: config.timeoutMs ?? DEFAULT_TIMEOUT_MS,
    signal,
  });
  if (response.status !== 200) {
    throw new TenonCallError({
      code: 'AGENT_LOGIN_FAILED',
      errorClass: 'policy_denied',
      retryable: false,
      message:
        `agent 账号登录被拒绝（HTTP ${response.status}）：${snippet(response.text)}。` +
        '检查 agentUser/agentPassword 与宿主的 dev 账号表（TENON_USERS_FILE）',
    });
  }

  const body = JSON.parse(response.text) as Json;
  const token = typeof body === 'object' && body !== null && !Array.isArray(body) ? body['token'] : undefined;
  const exp = typeof body === 'object' && body !== null && !Array.isArray(body) ? body['exp'] : undefined;
  if (typeof token !== 'string' || token.length === 0) {
    throw new TenonCallError({
      code: 'AGENT_LOGIN_FAILED',
      errorClass: 'contract_violation',
      retryable: false,
      message: `POST /tenon/session 的响应里没有 token：${snippet(response.text)}`,
    });
  }

  const expiresAt = typeof exp === 'number' ? exp * 1000 : Date.now() + 5 * 60_000;
  cache.set(key, { token, expiresAt });
  return token;
}
