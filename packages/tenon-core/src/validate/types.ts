// 校验层的对外类型：契约名、单条问题、校验失败、校验选项。
//
// 这一层刻意不依赖 ajv：调用方只应看到「哪一类报文、哪一条路径、什么关键字」，
// 换校验库不该改这里的形状。

/**
 * 有根实例契约的 schema 名（`common.schema.json` 只有 $defs、没有实例契约，故不在此列）。
 * 名字与 `spec/0.1/schemas/<name>.schema.json` 逐字对应。
 */
export type ContractName =
  | 'module-manifest'
  | 'action'
  | 'plan'
  | 'policy'
  | 'grant'
  | 'audit-event'
  | 'domain-event'
  | 'frames'
  | 'error'

/** 一条校验问题。`path` 是 JSON Pointer（根为 `/`），与 Go 侧 `at '/actions/0'` 同形。 */
export interface ValidationIssue {
  path: string
  /**
   * ajv 的 keyword（type / required / additionalProperties / oneOf …）。
   * 调用方应该用它做程序判断，而不是解析 message（措辞会随库版本变）。
   */
  keyword: string
  /** 人能读的一句话。不保证与 Go 侧逐字相同——对齐的是 path 与 keyword。 */
  message: string
}

/** 校验失败。带 `contract` 是为了让调用方知道「是哪一类报文不合法」。 */
export class ValidationError extends Error {
  readonly contract: ContractName
  readonly issues: ValidationIssue[]

  constructor(contract: ContractName, issues: ValidationIssue[]) {
    super(`${contract} 校验失败（${issues.length} 处）：${issues[0]?.message ?? '无详情'}`)
    this.name = 'ValidationError'
    this.contract = contract
    this.issues = issues
  }
}

export interface ValidateOptions {
  /**
   * true = 一致性视图（未知字段视为错误，用于校验**自产**报文）；
   * 默认 false = 接收视图（剥离并忽略未知字段，符合 versioning.md §3.2）。
   */
  strict?: boolean
}
