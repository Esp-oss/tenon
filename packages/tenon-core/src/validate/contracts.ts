// 对外校验门面：每类契约一个类型安全函数 + 一个按契约名分派的动态入口。
//
// 设计取舍：通过时返回**校验后的值**（接收视图已剥离未知字段），失败抛 ValidationError。
// 不返回 true/false —— 否则每个调用方都要再 `as` 一次，且容易忘记判断。
import type {
  ActionDefinition,
  AuditEvent,
  DomainEvent,
  Grant,
  ModuleManifest,
  Plan,
  PolicyRule,
  TenonErrorEnvelope,
  TenonFrame,
} from '../types.js'
import { check } from './core.js'
import { ValidationError, type ContractName, type ValidateOptions } from './types.js'

// ValidationError 是**值**（class），必须值导出；其余是类型。
export { ValidationError }
export type { ContractName, ValidateOptions, ValidationIssue } from './types.js'

export function validateModuleManifest(value: unknown, opts?: ValidateOptions): ModuleManifest {
  return check<ModuleManifest>('module-manifest', value, opts)
}

export function validateAction(value: unknown, opts?: ValidateOptions): ActionDefinition {
  return check<ActionDefinition>('action', value, opts)
}

export function validatePlan(value: unknown, opts?: ValidateOptions): Plan {
  return check<Plan>('plan', value, opts)
}

export function validatePolicyRule(value: unknown, opts?: ValidateOptions): PolicyRule {
  return check<PolicyRule>('policy', value, opts)
}

export function validateGrant(value: unknown, opts?: ValidateOptions): Grant {
  return check<Grant>('grant', value, opts)
}

export function validateAuditEvent(value: unknown, opts?: ValidateOptions): AuditEvent {
  return check<AuditEvent>('audit-event', value, opts)
}

export function validateDomainEvent(value: unknown, opts?: ValidateOptions): DomainEvent {
  return check<DomainEvent>('domain-event', value, opts)
}

export function validateFrame(value: unknown, opts?: ValidateOptions): TenonFrame {
  return check<TenonFrame>('frames', value, opts)
}

export function validateError(value: unknown, opts?: ValidateOptions): TenonErrorEnvelope {
  return check<TenonErrorEnvelope>('error', value, opts)
}

/** wire 上按契约名分派（帧的 `type` 字段就是契约名）。 */
export function validateByContract(contract: ContractName, value: unknown, opts?: ValidateOptions): unknown {
  return check(contract, value, opts)
}
