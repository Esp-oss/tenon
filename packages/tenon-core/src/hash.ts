// 摘要工具：`sha256:<hex>`（§4.8）。审计的 inputHash / outputHash / prevHash 与模块侧
// argsHash 共用这一种写法，少一个前缀就会让「同一个内容」在两侧算出不同字符串。
import { createHash } from 'node:crypto';

/** SHA-256 的十六进制小写摘要（64 位）。 */
export function sha256Hex(input: string | Uint8Array): string {
  return createHash('sha256').update(input).digest('hex');
}

/** 规范写法：`sha256:<64 位小写十六进制>`。 */
export function sha256Digest(input: string | Uint8Array): string {
  return `sha256:${sha256Hex(input)}`;
}
