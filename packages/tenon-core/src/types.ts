/**
 * Tenon Protocol（榫卯）协议类型定义。
 *
 * 唯一权威来源：docs/superpowers/specs/2026-10-27-tenon-ai-driven-system-design.md
 * 章节对照：§4（9 类契约）、§5.1（状态机）、§4.0（实体寻址）。
 *
 * 约定：
 * - 线格式的唯一真源是 `spec/0.1/schemas/*.json`（§2.1），本文件只是它的 TypeScript 投影；
 *   通用类型名与 `common.schema.json#/$defs` 一一对应，契约文件名以 `spec/0.1/schemas/` 为准；
 * - 本文件只含类型定义，不含任何运行时代码逻辑；枚举值来源是同名 `as const` 字符串数组（纯数据）；
 * - 字段可选性严格照 §4 的示例与硬约束，不做"看起来更方便"的放宽。
 */

/* ============================================================
 * 版本
 * ============================================================ */

/** 协议版本号。用于 manifest.tenon（§4.1）与 `Tenon-Version` 请求头（§4.10）；v0.1 期只有 0.1。 */
export const TENON_PROTOCOL_VERSION = '0.1' as const;

/** 协议版本的类型形式，对应 common.schema.json#/$defs/TenonVersion（`major.minor`）。 */
export type TenonVersion = typeof TENON_PROTOCOL_VERSION;

/* ============================================================
 * §4.0 通用：实体寻址与关联标识
 * ============================================================ */

/** 实体寻址的对象形式。§4.0：所有跨契约的实体引用统一用它；对账两侧必须使用同一寻址，否则无法比对。 */
export interface EntityRef {
  /** 模块 id，如 `inventory`（小写字母起始，全局唯一且稳定，--common.schema.json#/$defs/ModuleId）。 */
  module: string;
  /** 实体类型，如 `SKU`、`Quote`（PascalCase）。 */
  type: string;
  /** 实体业务主键，如 `STD-1001`；不得含空白、冒号或斜杠，以免与字符串简写产生歧义。 */
  id: string;
}

/** 实体寻址的字符串简写 `module:Type/id`（§4.0），如 `inventory:SKU/STD-1001`。 */
export type EntityRefString = `${string}:${string}/${string}`;

/** 实体寻址：对象形式或字符串简写，二者语义严格等价（§4.0）。§4.0 逐点列举的四个位置——Event.subject、Plan.expectedDiff[].entity、Intent.target、Audit.effects.entities——都用本类型。 */
export type EntityRefLike = EntityRef | EntityRefString;

/** 关联标识 `cor:<业务键>`。§4.0，如 `cor:quote/Q-2026-0417`；同一业务链条上的事件、Plan、审计共用同一个值。 */
export type CorrelationId = `cor:${string}`;

/** 带时区偏移的 ISO-8601 时间戳。协议内一律以 UTC 记录，毫秒精度可选（common.schema.json#/$defs/Iso8601Timestamp）。 */
export type Iso8601Timestamp = string;

/** ISO-8601 时长，如 `P1D`、`PT24H`；用于 Grant 配额窗口，按滚动窗口语义解释（§4.6、semantics.md）。 */
export type Iso8601Duration = string;

/** `sha256:<hex>` 哈希。§4.8：inputHash / outputHash / prevHash；大对象只存摘要，原文入对象存储（schema 里为 `sha256:` + 64 位小写十六进制）。 */
export type Sha256Hash = `sha256:${string}`;

/** 契约对象自身的语义化版本，如 `1.4.0`（action / event / module / skill 的 version 字段；common.schema.json#/$defs/SemVer）。 */
export type SemVer = string;

/** 幂等键。§4.4：宿主生成时形如 `tenon:<planId>/<stepId>`，同一笔业务重试必须复用同一个值且不含空白。 */
export type IdempotencyKey = string;

/** action 标识，格式为 `<module>.<action>`，如 `inventory.DeductStock`（§4.2）；映射到 MCP 时 tool 名为 `tenon_<module>_<action>`（§4.10）。 */
export type ActionId = string;

/** 模块标识，如 `inventory`（§4.1）。 */
export type ModuleId = string;

/** 传输方式与能力标签的通用词法。§4.1：取值域开放，消费者遇到未知取值必须忽略该项而不是拒绝整个报文。 */
export type CapabilityToken = string;

/** JSON Schema 原文。§2.1：schema 是唯一真源；校验库（zod 或 ajv）的选型留给 M0 实现。 */
export type JsonSchema = Record<string, unknown>;

/** 任意 JSON 值。用于无法预先约束的内容：差异的 from/to、JSON Patch 的 value、错误 details 的值（common.schema.json#/$defs/JsonValue）。 */
export type JsonValue =
  | null
  | boolean
  | number
  | string
  | JsonValue[]
  | { [key: string]: JsonValue };

/** 任意 JSON 对象体：event.payload、action 入参 args、error.details（common.schema.json#/$defs/JsonObject）。 */
export type JsonObject = { [key: string]: JsonValue };

/* ============================================================
 * §4.2 Action
 * ============================================================ */

/** action 的读写分类，`kind` 必填（§4.2）。C1 的断言按 kind 分支。 */
export const ACTION_KINDS = ['read', 'write'] as const;

/** `kind` 的类型形式。 */
export type ActionKind = (typeof ACTION_KINDS)[number];

/** 风险等级。§4.2 `risk.level`；§6.3「命中高风险策略 → approve 转人工」。 */
export const RISK_LEVELS = ['low', 'medium', 'high'] as const;

/** 风险等级的类型形式。 */
export type RiskLevel = (typeof RISK_LEVELS)[number];

/** action 风险。§4.2：`reversible=false` 不得出现在多步 Plan 的非末位（§4.2、§5.2），且无 compensation 时强制审批（§4.2、§4.7）。 */
export interface Risk {
  level: RiskLevel;
  reversible: boolean;
  /** 是否涉及金额；摘要层（manifest）可省略，需要时从完整 Action 读取（§4.2）。 */
  moneyAffecting?: boolean;
}

/** 幂等作用域。§4.2：`scope ∈ {module, system}`。 */
export const IDEMPOTENCY_SCOPES = ['module', 'system'] as const;

/** 幂等作用域的类型形式。 */
export type IdempotencyScope = (typeof IDEMPOTENCY_SCOPES)[number];

/** 重复提交行为。§4.2：v0.1 取值域只有 `return_first_result`，未来可扩展。 */
export const DUPLICATE_BEHAVIORS = ['return_first_result'] as const;

/** 重复提交行为的类型形式。 */
export type DuplicateBehavior = (typeof DUPLICATE_BEHAVIORS)[number];

/** 幂等声明，仅 write 必填（§4.2）。`keyFields` 必须是 `inputSchema.required` 的子集，否则幂等键可能缺失 → 重复副作用。 */
export interface Idempotency {
  keyFields: string[];
  scope: IdempotencyScope;
  duplicateBehavior: DuplicateBehavior;
}

/** saga 补偿声明。§4.2、§5.2：失败时逆序补偿已成功步骤，失败步骤本身不补偿。`argMap` 的值为 `$in.<字段>` 形式的取参路径。 */
export interface Compensation {
  action: ActionId;
  argMap: Record<string, string>;
}

/** dry-run 能力声明。§4.1、§4.2；未实现则运行时强制审批（§8 降级矩阵）。 */
export interface DryRunCapability {
  supported: boolean;
}

/** 配额计量单位。§4.2：`quantity` 表示按 args 里的业务量计，字段名由 `quotaField` 指定。 */
export const QUOTA_UNITS = ['calls', 'quantity'] as const;

/** 配额计量单位的类型形式。 */
export type QuotaUnit = (typeof QUOTA_UNITS)[number];

/** Action 的公共字段。§4.2。 */
export interface ActionDefinitionCommon {
  id: ActionId;
  version: SemVer;
  title: string;
  inputSchema: JsonSchema;
  outputSchema: JsonSchema;
  risk: Risk;
  compensation?: Compensation;
  /** 单步超时；协议默认 30000ms，接入方可覆盖但必须声明（§5.2）。 */
  timeoutMs: number;
  dryRun: DryRunCapability;
  /** 模块自校验的前置条件说明。§4.2：给 agent 读的说明性字段，agent 不得据此短路——前置条件永远由模块判定（§3.2）。 */
  preconditions?: string;
  /** 配额单位；不声明则按 `calls` 计（§4.2）。 */
  quotaUnit?: QuotaUnit;
  /** 仅 `quotaUnit=quantity` 时有意义：按 args 里哪个字段计量（§4.2）。 */
  quotaField?: string;
}

/** 写 action：`idempotency` 必填（§4.2 硬约束）。 */
export interface WriteActionDefinition extends ActionDefinitionCommon {
  kind: 'write';
  idempotency: Idempotency;
}

/** 读 action：不得声明 `idempotency`（§4.2 硬约束）。 */
export interface ReadActionDefinition extends ActionDefinitionCommon {
  kind: 'read';
  idempotency?: never;
}

/** Action 契约（§4.2）。用读写联合在类型层强制「write 必填 idempotency / read 不得声明」这两条硬约束。 */
export type ActionDefinition = ReadActionDefinition | WriteActionDefinition;

/** manifest 里的 action 摘要项：不含 inputSchema/outputSchema，按需拉取完整 Action 以控上下文成本（§4.1、D15）。 */
export interface ActionSummary {
  id: ActionId;
  version: SemVer;
  title: string;
  kind: ActionKind;
  risk: Risk;
  dryRun: DryRunCapability;
}

/* ============================================================
 * §4.1 Module Manifest（发现）
 * ============================================================ */

/** manifest 里的 event 摘要。§4.1。 */
export interface EventSummary {
  /** 点分层小写事件名，如 `quote.completed`。 */
  type: string;
  version: SemVer;
}

/** 模块自述。§4.1 `module`；id 全局唯一且发布后不改。 */
export interface ModuleInfo {
  id: ModuleId;
  version: SemVer;
  title: string;
}

/** 发现报文 `GET /.well-known/tenon`（§4.1、module-manifest.schema.json）。摘要级，不含 inputSchema。 */
export interface ModuleManifest {
  /** 模块实现的协议版本，当前 `0.1`（见 TENON_PROTOCOL_VERSION）。 */
  tenon: string;
  module: ModuleInfo;
  actions: ActionSummary[];
  events: EventSummary[];
  /** 传输方式。0.1 定义的取值：http / mcp / webhook；取值域开放，未知取值必须忽略。 */
  transports: CapabilityToken[];
  /** 可选能力标签。0.1 定义的取值：dry-run / event-replay；取值域开放，未识别即忽略。 */
  capabilities: CapabilityToken[];
  /** 当前不可调用的 action（熔断打开或健康检查失败时填入，§5.2、§8）；省略等价于全部可用。 */
  unavailableActions?: ActionId[];
}

/* ============================================================
 * §4.3 Domain Event
 * ============================================================ */

/** 领域事件信封。§4.3：至少一次投递、可重放（`GET /tenon/events?since=cursor`），消费侧按 `eventId` 去重。 */
export interface DomainEvent {
  eventId: string;
  /** 事件类型，如 `quote.completed`、`stock.insufficient`。 */
  type: string;
  version: SemVer;
  subject: EntityRefLike;
  occurredAt: Iso8601Timestamp;
  /** 指向上一跳的 eventId 或 auditId；无上一跳时为 null（§4.0：协议不定义 Command 契约）。 */
  causationId: string | null;
  correlationId: CorrelationId;
  payload: JsonObject;
}

/* ============================================================
 * §4.4 Plan
 * ============================================================ */

/** dry-run 结果状态，三态+1（§4.4）：承载降级矩阵里「unverified → 强制审批」。 */
export const DRY_RUN_STATUSES = ['verified', 'unsafe', 'unsupported', 'pending'] as const;

/** dry-run 状态的类型形式。 */
export type DryRunStatus = (typeof DRY_RUN_STATUSES)[number];

/** 预期数据差异。§4.4 `expectedDiff`。 */
export interface ExpectedDiff {
  entity: EntityRefLike;
  field: string;
  from: JsonValue;
  to: JsonValue;
}

/** dry-run 报告。§4.4 `Plan.dryRun`；`unsupported` / `pending` 时通常无差异可报。 */
export interface DryRunReport {
  status: DryRunStatus;
  diff?: ExpectedDiff[];
}

/** Plan 的一步。§4.4 steps[]。 */
export interface PlanStep {
  stepId: string;
  action: ActionId;
  args: JsonObject;
  /** 依赖的前序 stepId；空数组表示无依赖（§4.4）。 */
  dependsOn: string[];
  kind: ActionKind;
  /** 幂等键，形如 `tenon:<planId>/<stepId>`（§4.4）；write 步骤必须携带，重复提交由模块返回首次结果。 */
  idempotencyKey?: IdempotencyKey;
  /** 本步失败时的补偿 action（§4.2、§5.2）。 */
  compensation?: ActionId;
}

/** Plan 的触发源。§4.4 `trigger`。 */
export interface PlanTrigger {
  eventId: string;
  correlationId: CorrelationId;
}

/** 计划。§4.4：必须人能读懂、可被策略评估、可回放重建。 */
export interface Plan {
  planId: string;
  /** amend 产生新 planId 并置本字段为旧 planId（§4.4、§5.1）；首版为 null。 */
  supersedes: string | null;
  trigger: PlanTrigger;
  steps: PlanStep[];
  expectedDiff: ExpectedDiff[];
  /** dry-run 结果必填（§4.4）；状态直接参与策略升级与强制审批（§4.7、§8）。 */
  dryRun: DryRunReport;
  estimatedRisk: RiskLevel;
  /** 可选内容但字段常在：v0.1 不强制，为 null 时审计无「为何这么选」可展示（§4.4、§14 D1）。 */
  rationale: string | null;
}

/* ============================================================
 * §4.5 Intent / Context / Confirm（人机共驾三帧）
 * ============================================================ */

/** 三帧的类型判别值（§4.5）。 */
export const TENON_FRAME_TYPES = ['tenon.intent', 'tenon.context', 'tenon.confirm'] as const;

/** 帧类型的类型形式。 */
export type TenonFrameType = (typeof TENON_FRAME_TYPES)[number];

/** Intent 帧里的界面动作种类（§4.5 示例 open / highlight）。 */
export const INTENT_ACTION_KINDS = ['open', 'highlight'] as const;

/** Intent 动作种类的类型形式。 */
export type IntentActionKind = (typeof INTENT_ACTION_KINDS)[number];

/** Intent 帧的一条界面动作。§4.5：只表达"界面应进入什么状态"，帧内不含 DOM 选择器、不含坐标。 */
export interface IntentAction {
  kind: IntentActionKind;
  target: EntityRefLike;
  /** 高亮徽标文案，如 `-12 已扣减`（§4.5）。 */
  badge?: string;
  /** 是否可撤销（§4.5）。 */
  undoable?: boolean;
  /** 撤销时指向的 Plan，通常是补偿计划（§4.5）。 */
  undoPlanId?: string;
}

/** 帧一：Intent（agent → 前端）。§4.5；无共驾 SDK 时该帧直接丢弃，业务结果不变（§3.2）。 */
export interface IntentFrame {
  type: 'tenon.intent';
  intentId: string;
  planId: string;
  actions: IntentAction[];
}

/** Context 帧里的一处用户编辑。§4.5 `edits[]`。 */
export interface ContextEdit {
  field: string;
  from: JsonValue;
  to: JsonValue;
}

/** 帧二：Context（前端 → agent）。§4.5。 */
export interface ContextFrame {
  type: 'tenon.context';
  view: EntityRefLike;
  edits: ContextEdit[];
  at: Iso8601Timestamp;
}

/** 用户对 Plan 的决定（§4.5）：`amend` 时 patch 为 JSON Patch（RFC 6902），只作用于未执行步骤。 */
export const CONFIRM_DECISIONS = ['approve', 'reject', 'amend'] as const;

/** 确认决定的类型形式。 */
export type ConfirmDecision = (typeof CONFIRM_DECISIONS)[number];

/** RFC 6902 JSON Patch 的操作类型（common.schema.json#/$defs/JsonPatchOperation）。 */
export const JSON_PATCH_OPERATIONS = ['add', 'remove', 'replace', 'move', 'copy', 'test'] as const;

/** JSON Patch 操作类型的类型形式。 */
export type JsonPatchOp = (typeof JSON_PATCH_OPERATIONS)[number];

/** RFC 6902 JSON Patch 的单条操作。§4.5 `ConfirmFrame.patch`；move/copy 必须带 from，add/replace/test 必须带 value。 */
export interface JsonPatchOperation {
  op: JsonPatchOp;
  /** 目标位置的 RFC 6901 JSON Pointer。 */
  path: string;
  value?: JsonValue;
  from?: string;
}

/** 帧三：Confirm（前端 → agent）。§4.5。 */
export interface ConfirmFrame {
  type: 'tenon.confirm';
  confirmId: string;
  planId: string;
  decision: ConfirmDecision;
  /** approve/reject 时为 null（§4.5 示例）。 */
  patch: JsonPatchOperation[] | null;
  /** 决定者，如 `user:alice`（§4.5）。 */
  by: string;
  at: Iso8601Timestamp;
}

/** 三帧的联合。§4.5；任何框架都只认这三种帧。 */
export type TenonFrame = IntentFrame | ContextFrame | ConfirmFrame;

/* ============================================================
 * §4.6 Grant（授权）
 * ============================================================ */

/** 授权拒绝原因。§4.6 评估报文 `reason` 的取值域。 */
export const GRANT_DENY_REASONS = [
  'NOT_IN_GRANT',
  'EXPIRED',
  'QUOTA_EXCEEDED',
  'SUBJECT_NOT_PERMITTED',
] as const;

/** 授权拒绝原因的类型形式。 */
export type GrantDenyReason = (typeof GRANT_DENY_REASONS)[number];

/** 授权约束。§4.6：窗口用 ISO-8601 duration；其余约束键由 grant.schema.json 定稿，未知字段必须忽略（§4.10）。 */
export interface GrantConstraints {
  /** 单次调用业务量上限（§4.6 示例 `maxQtyPerCall`）。 */
  maxQtyPerCall?: number;
  /** 配额窗口（§4.6 示例 `P1D`）。 */
  window?: Iso8601Duration;
  /** 窗口内累计业务量上限（§4.6 示例 `maxUnitsPerWindow`）。 */
  maxUnitsPerWindow?: number;
}

/** 授权条目：某模块下允许的 action 列表及其约束。§4.6 `allow[]`。 */
export interface GrantEntry {
  module: ModuleId;
  actions: ActionId[];
  constraints?: GrantConstraints;
}

/** 授权。§4.6：`effective = agent 被授予 ∩ onBehalfOf 用户权限`（D5 取交集，不放大权限）。 */
export interface Grant {
  grantId: string;
  /** agent 身份，如 `ag_fulfillment`。 */
  agent: string;
  allow: GrantEntry[];
  expiresAt: Iso8601Timestamp;
}

/** 授权通过的评估结果。§4.6。 */
export interface GrantAllowedEvaluation {
  allowed: true;
  matchedGrantId: string;
  quota: {
    remaining: number;
    unit: QuotaUnit;
  };
}

/** 授权拒绝的评估结果。§4.6。 */
export interface GrantDeniedEvaluation {
  allowed: false;
  reason: GrantDenyReason;
}

/** 授权评估报文结果 `POST /tenon/grants:evaluate`（§4.6）。模块侧仍必须再拒一次（§4.6、§8 #6）。 */
export type GrantEvaluation = GrantAllowedEvaluation | GrantDeniedEvaluation;

/* ============================================================
 * §4.7 Policy（风险分级）
 * ============================================================ */

/** 策略算子集合，v0.1 封闭（§4.7）。 */
export const POLICY_OPERATORS = ['eq', 'ne', '>', '>=', '<', '<=', 'in', 'notIn', 'matches'] as const;

/** 策略算子的类型形式（9 个）。 */
export type PolicyOperator = (typeof POLICY_OPERATORS)[number];

/** 算子对象：键为算子、值为比较值，如 `{ ">": 10 }`（§4.7）。 */
export type PolicyOperatorObject = Partial<Record<PolicyOperator, JsonValue>>;

/** 条件叶：字段路径 → 算子对象，如 `{ "arg.qty": { ">": 10 } }`（§4.7）。路径前缀 `arg.` 指向 action 入参。 */
export type PolicyLeafCondition = Record<string, PolicyOperatorObject>;

/** 条件表达式：`all` / `any` / `not` 可嵌套，外加算子叶（§4.7）。 */
export type PolicyCondition =
  | { all: PolicyCondition[] }
  | { any: PolicyCondition[] }
  | { not: PolicyCondition }
  | PolicyLeafCondition;

/** 策略结果（§4.7）：`auto` 直接执行 / `approve` 转人工 / `deny` 拒绝。合并规则 deny > approve > auto。 */
export const POLICY_OUTCOMES = ['auto', 'approve', 'deny'] as const;

/** 策略结果的类型形式。 */
export type PolicyOutcome = (typeof POLICY_OUTCOMES)[number];

/** 策略匹配范围。§4.7 `match`。 */
export interface PolicyMatch {
  module: ModuleId;
  /** 省略表示该模块下所有 action。 */
  actions?: ActionId[];
}

/** 策略规则。§4.7：多策略命中时 deny > approve > auto；同 outcome 取 priority 最高者。策略不可被 agent 读取或改写。 */
export interface PolicyRule {
  policyId: string;
  priority: number;
  match: PolicyMatch;
  when: PolicyCondition;
  outcome: PolicyOutcome;
  /** 审批人，如 `role:warehouse_manager`（§4.7）。 */
  approvers?: string[];
}

/** 策略评估结果 `POST /tenon/policies:evaluate`（§4.7）。`decidedBy` 记录命中的 policyId。 */
export interface PolicyEvaluation {
  outcome: PolicyOutcome;
  decidedBy: string[];
  approvers?: string[];
  /** 判定理由码，如 `NO_POLICY_MATCH_DEFAULT_DENY`（§4.7）；取值域由 policy.schema.json 定稿。 */
  reasons?: string[];
}

/* ============================================================
 * §4.8 Audit Event（不可变，两段写）
 * ============================================================ */

/** 审计阶段。§4.8：`dispatched` 必须先写成功才允许执行；写失败即拒绝执行（§8 #5）。 */
export const AUDIT_STAGES = ['dispatched', 'result'] as const;

/** 审计阶段的类型形式。 */
export type AuditStage = (typeof AUDIT_STAGES)[number];

/** 执行结果状态，四态（§4.8）。`unknown` 表示崩溃恢复后结果未定，需按 idempotencyKey 向模块回查（§5.1）。 */
export const AUDIT_RESULT_STATUSES = ['ok', 'error', 'partially_failed', 'unknown'] as const;

/** 执行结果状态的类型形式。 */
export type AuditResultStatus = (typeof AUDIT_RESULT_STATUSES)[number];

/** 行为主体类型。§4.8；取值域与 common.schema.json#/$defs/Actor 一致：agent 自动化运行时、user 真人、system 无人类在场的系统进程。 */
export const ACTOR_TYPES = ['agent', 'user', 'system'] as const;

/** 行为主体类型的类型形式。 */
export type ActorType = (typeof ACTOR_TYPES)[number];

/** 审计主体。§4.8 `actor`；`onBehalfOf` 字段必须出现——缺失与 null 在哈希链里是不同的字节。 */
export interface AuditActor {
  type: ActorType;
  id: string;
  /** 委托人身份，如 `user:alice`；直连调用或 system 主体为 null。 */
  onBehalfOf: string | null;
}

/** 审计的触发源。§4.8 `trigger`。 */
export interface AuditTrigger {
  eventId: string | null;
  correlationId: CorrelationId | null;
}

/** 审计的决策留痕。§4.8 `decision`：`policyId` 与 `outcome` 分离，用于记录是哪条策略批的。 */
export interface AuditDecision {
  outcome: PolicyOutcome;
  policyId: string | null;
  grantId: string | null;
  approvedBy: string | null;
}

/** 审计的结果段。§4.8 `result`；大对象只存 hash。 */
export interface AuditResult {
  status: AuditResultStatus;
  outputHash: Sha256Hash | null;
  errorClass: ErrorClass | null;
}

/** 审计的业务影响。§4.8 `effects`：v2 用量看板的聚合来源，看板只需新增聚合查询、不需改 schema。 */
export interface AuditEffects {
  entities: EntityRefLike[];
  documentCount?: number;
  quantity?: number;
}

/** token 用量。§4.8 `tokenUsage`。 */
export interface TokenUsage {
  in: number;
  out: number;
}

/** 审计事件，append-only + prevHash 哈希链（§4.8）。同一次调用产生两条记录：`auditId` 相同、`stage` 不同。 */
export interface AuditEvent {
  auditId: string;
  stage: AuditStage;
  at: Iso8601Timestamp;
  actor: AuditActor;
  trigger: AuditTrigger;
  planId: string | null;
  stepId: string | null;
  action: ActionId;
  inputHash: Sha256Hash;
  idempotencyKey: IdempotencyKey | null;
  decision: AuditDecision;
  result: AuditResult;
  effects: AuditEffects;
  tokenUsage: TokenUsage;
  /** 前一条记录的哈希，串联成链；创世记录的取值形式由 audit-event.schema.json 定稿（§4.8 未规定）。 */
  prevHash: Sha256Hash | null;
}

/* ============================================================
 * §4.9 Error
 * ============================================================ */

/** 错误分类，四类（§4.9）。C9 断言四类错误码与 `retryable` 标志正确。 */
export const ERROR_CLASSES = [
  'retryable',
  'business_rejection',
  'policy_denied',
  'contract_violation',
] as const;

/** 错误分类的类型形式。 */
export type ErrorClass = (typeof ERROR_CLASSES)[number];

/** 协议错误体。§4.9。`business_rejection` 的 code（如 `STOCK_INSUFFICIENT`）不带任何 action 映射语义——改走哪条路由 skill 引导 agent 自行决定（§6.1、§6.3）。 */
export interface TenonError {
  class: ErrorClass;
  code: string;
  message: string;
  details?: JsonObject;
  /** 仅 `retryable` 为 true 时重试，且幂等键不变（§4.9）。 */
  retryable: boolean;
}

/** 错误响应包壳。§4.9 的线格式为 `{ "tenonError": { ... } }`。 */
export interface TenonErrorEnvelope {
  tenonError: TenonError;
}

/* ============================================================
 * §5.1 状态机（v0.1 写死）
 * ============================================================ */

/** Plan 状态（§5.1）。`amended` 不是状态：amend 产生新 Plan 并置 `supersedes`（§4.4）。 */
export const PLAN_STATES = [
  'draft',
  'evaluating',
  'auto_approved',
  'approved',
  'rejected',
  'executing',
  'completed',
  'partially_failed',
  'failed',
  'aborted',
] as const;

/** Plan 状态的类型形式。 */
export type PlanState = (typeof PLAN_STATES)[number];

/** 审批状态（§5.1）。`expiresAt` 到期自动 `expired`，等价 reject。 */
export const APPROVAL_STATES = ['pending', 'approved', 'rejected', 'amended', 'expired'] as const;

/** 审批状态的类型形式。 */
export type ApprovalState = (typeof APPROVAL_STATES)[number];

/** 执行步骤状态（§5.1）。进入 `dispatched` 的前置条件是该 step 的 dispatched 审计已写成功；`unknown` 只能靠 idempotencyKey 回查收敛。 */
export const STEP_STATES = [
  'pending',
  'dispatched',
  'succeeded',
  'failed',
  'compensated',
  'unknown',
] as const;

/** 执行步骤状态的类型形式。 */
export type StepState = (typeof STEP_STATES)[number];

/** 同 StepState：common.schema.json 里该枚举的 $defs 名为 ExecutionStepState，此处提供别名以免引用歧义。 */
export type ExecutionStepState = StepState;
