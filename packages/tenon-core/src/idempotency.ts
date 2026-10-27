// 幂等键（§4.4）：`tenon:<planId>/<stepId>`。
//
// 单独成文件是因为它比看起来重要：拼错一个字节，重试就会被模块当成**新业务**，
// 于是「重放」变成「重复扣库存」。字符串形状本身是契约。
/** 按规范构造幂等键（与 Go 的 protocol.IdempotencyKey 逐字一致）。 */
export function idempotencyKey(planId: string, stepId: string): string {
  return `tenon:${planId}/${stepId}`;
}
