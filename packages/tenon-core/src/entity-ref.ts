// 实体寻址：对象形式 ↔ 字符串简写 `module:Type/id`（§4.0）。
//
// 判定条件逐条镜像 Go 参考实现 internal/protocol/protocol.go 的 ParseEntityRef，
// 包括错误消息要说明「缺哪一段」——两个实现必须给出同一组接受/拒绝结果，
// 否则同一份数据在两侧一边合法一边非法（那正是协议分叉的样子）。
//
// 注意一处刻意的宽松：schema 的 EntityRefString 还用正则限制了 module 的字符集
// （小写字母起始），这里**不做**字符集校验。理由：字符集校验属于 JSON Schema 校验器
// 的职责（本包另一个模块），解析器只负责「切三段」。Go 侧同样只切三段。
import type { EntityRef } from './types.js';

/** 解析失败时抛出。`ref` 是原始输入，便于排障时原样回报。 */
export class EntityRefError extends Error {
  readonly ref: string;

  constructor(ref: string, message: string) {
    super(message);
    this.name = 'EntityRefError';
    this.ref = ref;
  }
}

/** 对象形式 → 字符串简写 `module:Type/id`。 */
export function formatEntityRef(ref: EntityRef): string {
  return `${ref.module}:${ref.type}/${ref.id}`;
}

/**
 * 字符串简写 → 对象形式。
 *
 * 判据（与 Go 逐字一致）：
 * - `:` 必须存在，且不能在首位（否则没有 module 前缀）；
 * - `:` 之后必须出现 `/`，且不能在首位（否则没有 Type 段）；
 * - `/` 之后的 id 不能为空。
 *
 * module 里再出现 `:`、Type 里再出现 `/` 这类情况不单独检测：按「第一个 `:` 和其后第一个 `/`」
 * 切分，剩余部分归 id —— 与 Go 的 Index 语义相同。
 */
export function parseEntityRef(s: string): EntityRef {
  const colon = s.indexOf(':');
  if (colon <= 0) {
    throw new EntityRefError(s, `entity ref ${JSON.stringify(s)} 缺少 module 前缀`);
  }
  const slashInRest = s.slice(colon + 1).indexOf('/');
  if (slashInRest <= 0) {
    throw new EntityRefError(s, `entity ref ${JSON.stringify(s)} 缺少 Type/id 段`);
  }
  const slash = colon + 1 + slashInRest;
  const module = s.slice(0, colon);
  const type = s.slice(colon + 1, slash);
  const id = s.slice(slash + 1);
  if (id === '') {
    throw new EntityRefError(s, `entity ref ${JSON.stringify(s)} 的 id 为空`);
  }
  return { module, type, id };
}

/**
 * 解析 `EntityRefLike`：对象原样返回（仍做一次字段非空检查），字符串走 `parseEntityRef`。
 * 契约里四个位置（Event.subject、Plan.expectedDiff[].entity、Intent.target、
 * Audit.effects.entities）都允许两种写法，宿主不应各写一套。
 */
export function toEntityRef(value: EntityRef | string): EntityRef {
  if (typeof value === 'string') return parseEntityRef(value);
  if (value.module === '' || value.type === '' || value.id === '') {
    throw new EntityRefError(
      formatEntityRef(value),
      `entity ref 对象形式的 module/type/id 都不能为空：${JSON.stringify(value)}`,
    );
  }
  return { module: value.module, type: value.type, id: value.id };
}
