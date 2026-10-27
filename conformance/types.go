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
