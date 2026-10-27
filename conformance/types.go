package conformance

// Manifest 是模块 manifest 在 Go 侧的投影，只取跑用例需要的字段。
// 完整结构以 spec/0.1/schemas/module-manifest.schema.json 为准。
type Manifest struct {
	Tenon        string          `json:"tenon"`
	Module       ModuleInfo      `json:"module"`
	Actions      []ActionSummary `json:"actions"`
	Events       []EventSummary  `json:"events"`
	Transports   []string        `json:"transports"`
	Capabilities []string        `json:"capabilities"`
}

// ModuleInfo 是 manifest 里的模块标识。
type ModuleInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// ActionSummary 是 manifest 的摘要项：**不含 inputSchema**（按需拉取）。
type ActionSummary struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
}

// EventSummary 是 manifest 里的事件摘要。
type EventSummary struct {
	Type    string `json:"type"`
	Version string `json:"version,omitempty"`
}

// AuditRecordView 是 GET /tenon/audit 返回的审计记录在本包里需要的字段投影。
//
// 只取 C4 要断言的字段（auditId/stage/planId/hash/prevHash），其余字段（actor、decision、
// result、effects、tokenUsage）原样忽略：完整性由 audit-event.schema.json 与宿主的写入
// 校验负责，一致性用例不重复实现它。
type AuditRecordView struct {
	AuditID  string  `json:"auditId"`
	Stage    string  `json:"stage"`
	PlanID   string  `json:"planId,omitempty"`
	Hash     string  `json:"hash"`
	PrevHash *string `json:"prevHash"`
}

// AuditQueryResponse 是 GET /tenon/audit 的响应体投影（interfaces.md §4.3）：
// 记录按写入顺序排列，nextCursor 是下一页应当传入的 since。
type AuditQueryResponse struct {
	Records    []AuditRecordView `json:"records"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// PolicyEvaluationView 是 POST /tenon/policies:evaluate 的响应体投影
// （policy.schema.json#/$defs/PolicyEvaluationResponse）。approvers 与 C5 的判定无关，
// 故不投影：它只在 outcome=approve 时出现，而 C5 断言的是 deny。
type PolicyEvaluationView struct {
	Outcome   string   `json:"outcome"`
	DecidedBy []string `json:"decidedBy"`
	Reasons   []string `json:"reasons"`
}
