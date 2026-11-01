// 包入口（barrel）：把协议层的公开能力按包名暴露给其他 `@tenon/*` 包与 dsh 插件。
//
// 没有这个文件时，`import ... from '@tenon/core'` 根本解析不到（package.json 既无 `exports`
// 也无 `main`，包根也没有 index）。之前没有暴露问题，只是因为还没有第二个 TS 包引用它。
// 这里只做**再导出**，不新增任何逻辑：真源仍是各 `src/*.ts`。
export * from './types.js';
export { sha256Digest, sha256Hex } from './hash.js';
export { idempotencyKey } from './idempotency.js';
export { EntityRefError, formatEntityRef, parseEntityRef, toEntityRef } from './entity-ref.js';
export { SIGNATURE_TOLERANCE_SECONDS, SignatureError, sign, verify } from './hmac.js';
export type { SignatureErrorKind } from './hmac.js';
export {
  ValidationError,
  validateAction,
  validateAuditEvent,
  validateByContract,
  validateDomainEvent,
  validateError,
  validateFrame,
  validateGrant,
  validateModuleManifest,
  validatePlan,
  validatePolicyRule,
} from './validate/contracts.js';
export type { ContractName, ValidateOptions, ValidationIssue } from './validate/contracts.js';
export { check, resetValidators, warmUp } from './validate/core.js';
