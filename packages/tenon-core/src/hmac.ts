// Webhook 的 HMAC-SHA256 签名与验签（§4.6）：
//   X-Tenon-Signature: t=<unix>,v1=<hex>
//   v1 = HMAC-SHA256(secret, "<t>." + rawBody)
// 时钟容差 ±300s（SignatureTolerance）。
//
// 镜像 Go 的 protocol.Sign / protocol.Verify：**同样的输入必须得到同样的十六进制**，
// 否则两个实现互相验不过。交叉校验向量见同目录 protocol.test.ts（由 Go 侧生成）。
import { createHmac, timingSafeEqual } from 'node:crypto';

/** 允许的时钟偏差（秒）。与 Go 的 SignatureTolerance = 300s 一致。 */
export const SIGNATURE_TOLERANCE_SECONDS = 300;

/** 验签失败的三种原因：格式错 / 超时 / 不匹配。分开是为了让排障知道该查谁。 */
export type SignatureErrorKind = 'format' | 'expired' | 'invalid';

export class SignatureError extends Error {
  readonly kind: SignatureErrorKind;

  constructor(kind: SignatureErrorKind, message: string) {
    super(message);
    this.name = 'SignatureError';
    this.kind = kind;
  }
}

function hmacHex(secret: string | Uint8Array, timestampSeconds: number, body: string | Uint8Array): string {
  return createHmac('sha256', secret)
    .update(`${timestampSeconds}.`)
    .update(body)
    .digest('hex');
}

/** 计算 `X-Tenon-Signature` 的值。 */
export function sign(
  secret: string | Uint8Array,
  timestampSeconds: number,
  body: string | Uint8Array,
): string {
  return `t=${timestampSeconds},v1=${hmacHex(secret, timestampSeconds, body)}`;
}

/**
 * 验签。通过则静默返回，失败抛 `SignatureError`（用 `kind` 区分三类原因）。
 *
 * `nowSeconds` 显式传入而不是内部取时钟：容差判断是**业务语义**，必须可测。
 */
export function verify(
  secret: string | Uint8Array,
  header: string,
  body: string | Uint8Array,
  nowSeconds: number,
  toleranceSeconds: number = SIGNATURE_TOLERANCE_SECONDS,
): void {
  if (header === '') {
    throw new SignatureError('format', '签名头为空');
  }
  let tRaw: string | undefined;
  let v1: string | undefined;
  for (const part of header.split(',')) {
    const eq = part.indexOf('=');
    if (eq < 0) {
      throw new SignatureError('format', `签名头分段缺少 '='：${JSON.stringify(part)}`);
    }
    const key = part.slice(0, eq).trim();
    const value = part.slice(eq + 1);
    // 与 Go 的 SplitN(…, "=", 2) 一致：值里再出现 '=' 归值（base64 风格的值不会被截断）。
    if (key === 't') tRaw = value;
    if (key === 'v1') v1 = value;
  }
  if (tRaw === undefined || v1 === undefined) {
    throw new SignatureError('format', '签名头缺少 t 或 v1');
  }
  if (!/^-?\d+$/.test(tRaw)) {
    throw new SignatureError('format', `签名头 t 不是整数：${JSON.stringify(tRaw)}`);
  }
  const timestamp = Number(tRaw);
  if (!Number.isSafeInteger(timestamp)) {
    throw new SignatureError('format', `签名头 t 超出安全整数范围：${tRaw}`);
  }
  const skew = nowSeconds - timestamp;
  if (skew < -toleranceSeconds || skew > toleranceSeconds) {
    throw new SignatureError('expired', `签名时间戳超出容差 ±${toleranceSeconds}s（偏差 ${skew}s）`);
  }
  if (!/^[0-9a-fA-F]+$/.test(v1) || v1.length % 2 !== 0) {
    throw new SignatureError('format', '签名 v1 不是合法的十六进制');
  }
  const want = Buffer.from(hmacHex(secret, timestamp, body), 'hex');
  const got = Buffer.from(v1, 'hex');
  if (want.length !== got.length || !timingSafeEqual(want, got)) {
    throw new SignatureError('invalid', '签名不匹配');
  }
}
