// Package grant 实现授权（Grant）引擎：生效权限取 agent 被授予的动作与 onBehalfOf
// 委托人权限的交集，并按 (grantId, 窗口) 累计配额。
//
// 依据：spec/0.1/schemas/grant.schema.json（字段真源）、设计文档 §4.6 与决策 D5
// （权限取交集：agent 不放大委托人的权限）。语义要点：
//   - 交集：委托人没有的动作，即使 grant 白名单里有也不放行 → SUBJECT_NOT_PERMITTED。
//   - fail-closed：未列出的动作、未登记的委托人、缺失或已到期的 expiresAt 一律拒绝。
//   - 配额是滚动窗口：每条放行的调用记一条 (时刻, 数量)，只统计 (now-窗口, now] 内的记录。
//   - 窗口只认 PnD / PnW / PTnH；其他写法返回 ErrUnsupportedWindow —— 报错不放行，也不猜。
package grant

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 拒绝原因，取值集合与 grant.schema.json 的 GrantDenyReason 四态一致。
// 拒绝是终态：调用方不得换一条路径绕过（§4.9 的 policy_denied 语义）。
const (
	ReasonNotInGrant          = "NOT_IN_GRANT"          // 动作不在该 agent 的白名单里
	ReasonExpired             = "EXPIRED"               // 授权已到期
	ReasonQuotaExceeded       = "QUOTA_EXCEEDED"        // 单次或窗口内配额不足
	ReasonSubjectNotPermitted = "SUBJECT_NOT_PERMITTED" // 委托人权限不足（交集为空）
)

// 计量单位，取值集合与 grant.schema.json 的 QuotaUnit 两态一致。
const (
	UnitCalls    = "calls"    // 按调用次数计
	UnitQuantity = "quantity" // 按业务量计
)

// 引擎级错误：它们不是 GrantDenyReason 的取值，属于「配置读不懂」而非「授权判定」，
// 所以走 error 返回而不是 Reason。Evaluate 返回错误时调用方必须按拒绝处理。
var (
	// ErrUnsupportedWindow 表示配额窗口用了不受支持的 ISO-8601 duration 写法。
	ErrUnsupportedWindow = errors.New("UNSUPPORTED_WINDOW")
	// ErrUnsupportedQuotaUnit 表示动作的计量单位不在 calls|quantity 内。
	ErrUnsupportedQuotaUnit = errors.New("UNSUPPORTED_QUOTA_UNIT")
)

// Grant 是一条授权：某个 agent 的「模块 × 动作」白名单加上到期时刻。
// 字段名与 grant.schema.json 对齐；同一个 agent 可以有多个 Grant 并存。
type Grant struct {
	GrantID   string       `json:"grantId"`
	Agent     string       `json:"agent"`
	Allow     []GrantEntry `json:"allow"`
	ExpiresAt time.Time    `json:"expiresAt"`
}

// GrantEntry 是一条白名单：某模块下被允许的动作，附带可选的配额约束。
type GrantEntry struct {
	Module      string            `json:"module"`
	Actions     []string          `json:"actions"`
	Constraints *GrantConstraints `json:"constraints,omitempty"`
}

// GrantConstraints 是配额约束；字段为零值表示该项未设置（schema 的 exclusiveMinimum 0
// 意味着「写了就得大于 0」，所以 omitempty 的零值等价于「没写」）。
type GrantConstraints struct {
	MaxQtyPerCall     float64 `json:"maxQtyPerCall,omitempty"`
	Window            string  `json:"window,omitempty"`
	MaxUnitsPerWindow float64 `json:"maxUnitsPerWindow,omitempty"`
}

// QuotaStatus 是配额余量。Unit 与动作声明的 quotaUnit 一致。
type QuotaStatus struct {
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"`
}

// GrantResult 是评估结果。放行时给出命中的 grant 与余量；拒绝时只带原因。
//
// 拒绝结果不得夹带 MatchedGrantID / Quota：schema 的 GrantDeniedEvaluation 是
// additionalProperties=false 的封闭结构，多带字段就不是合法报文。
type GrantResult struct {
	Allowed        bool         `json:"allowed"`
	MatchedGrantID string       `json:"matchedGrantId,omitempty"`
	Quota          *QuotaStatus `json:"quota,omitempty"`
	Reason         string       `json:"reason,omitempty"`
}

// usageKey 是配额记账的键：按 (grantId, window) 累计。同一 grant 内两条 entry 用同一个
// window 时共用一个计数器（保守做法，偏严不偏松）。
type usageKey struct {
	grantID string
	window  string
}

// usageRecord 是一条已放行调用的记账：发生时刻与折算后的配额消耗。
type usageRecord struct {
	at     time.Time
	amount float64
}

// candidate 是一条命中「agent 匹配 + 动作在白名单里 + 未过期」的候选授权。
type candidate struct {
	grantID    string
	narrowness int           // 该 grant 白名单里的动作总数，用于「最小满足者」排序
	perCall    float64       // maxQtyPerCall，0 表示未设
	window     string        // 窗口原文（schema 的 ISO-8601 duration）
	windowLen  time.Duration // 解析后的窗口长度
	windowMax  float64       // maxUnitsPerWindow，0 表示未设
}

// GrantEngine 是并发安全的授权评估器。用 NewGrantEngine 构造，零值不可用。
type GrantEngine struct {
	mu       sync.Mutex
	grants   []Grant
	subjects map[string]map[string]bool // onBehalfOf → 该委托人被允许的动作集合
	units    map[string]string          // action → 计量单位
	now      func() time.Time
	usage    map[usageKey][]usageRecord
}

// Option 调整引擎的装配方式（委托人权限表、动作计量单位、时钟）。
type Option func(*GrantEngine)

// WithSubjects 装配 onBehalfOf 的权限表：键是委托人身份（例 "user:alice"），值是该委托人
// 被允许的动作 id。交集规则的下半部分靠它，因此**不装配就等于没有委托人有权**：
// Evaluate 一律返回 SUBJECT_NOT_PERMITTED（fail-closed，不默认放行）；装配了但未列出的
// 委托人同样没有权限。
func WithSubjects(subjects map[string][]string) Option {
	return func(e *GrantEngine) {
		e.subjects = make(map[string]map[string]bool, len(subjects))
		for subject, actions := range subjects {
			set := make(map[string]bool, len(actions))
			for _, action := range actions {
				set[action] = true
			}
			e.subjects[subject] = set
		}
	}
}

// WithQuotaUnits 声明动作的计量单位（取值 calls 或 quantity）。未声明的动作按 quantity
// 处理：Evaluate 的 amount 就是业务量。声明了集合外的值是配置错误，评估时返回
// ErrUnsupportedQuotaUnit 而不是挑一个默认值。
func WithQuotaUnits(units map[string]string) Option {
	return func(e *GrantEngine) {
		e.units = make(map[string]string, len(units))
		for action, unit := range units {
			e.units[action] = unit
		}
	}
}

// WithClock 替换时间源，用于到期与滚动窗口的确定性测试；传 nil 表示恢复 time.Now。
func WithClock(now func() time.Time) Option {
	return func(e *GrantEngine) { e.now = now }
}

// NewGrantEngine 构造引擎。grants 会被深拷贝：构造后调用方改动入参不影响引擎，
// 引擎的记账也不必担心外部别名。
func NewGrantEngine(grants []Grant, opts ...Option) *GrantEngine {
	e := &GrantEngine{
		grants: cloneGrants(grants),
		now:    time.Now,
		usage:  make(map[usageKey][]usageRecord),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e
}

// Evaluate 判断某次调用是否放行，放行时把业务量记进配额。
//
// amount 是本次调用的业务量：计量单位为 quantity 的动作按 |amount| 累加（负调整同样占
// 配额），为 calls 的动作每次记 1。NaN 与 ±Inf 按 0 计，避免用特殊值刷高窗口余额。
//
// 判定顺序：白名单（含到期）→ 委托人交集 → 配额。任一环不过即拒绝，拒绝结果只有
// allowed 与 reason。返回 error 表示授权数据本身读不懂（窗口/单位不受支持），此时结果
// 不可用，调用方必须按拒绝处理。
func (e *GrantEngine) Evaluate(agent, onBehalfOf, action string, amount float64) (GrantResult, error) {
	if e == nil {
		return deny(ReasonNotInGrant), nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	live, sawExpired := e.collect(agent, action, now)
	if len(live) == 0 {
		if sawExpired {
			return deny(ReasonExpired), nil
		}
		return deny(ReasonNotInGrant), nil
	}
	if !e.subjectPermits(onBehalfOf, action) {
		return deny(ReasonSubjectNotPermitted), nil
	}

	unit, err := e.unitFor(action)
	if err != nil {
		return GrantResult{}, err
	}
	charge := chargeFor(amount, unit)

	// 先把窗口全部读懂再算配额：一条配置错误不该被后面的候选或配额结果掩盖。
	for i := range live {
		if live[i].window == "" {
			continue
		}
		length, err := parseWindow(live[i].window)
		if err != nil {
			return GrantResult{}, fmt.Errorf("grant %s 的配额窗口不可用: %w", live[i].grantID, err)
		}
		live[i].windowLen = length
	}

	// 「最小满足者」：白名单动作最少的那条优先（最窄的授权最贴合请求），
	// 同宽时按 grantId 字典序，保证结果与输入顺序无关。
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].narrowness != live[j].narrowness {
			return live[i].narrowness < live[j].narrowness
		}
		return live[i].grantID < live[j].grantID
	})

	for _, c := range live {
		if c.perCall > 0 && charge > c.perCall {
			continue // 单次超限：不部分放行，试下一条候选
		}
		if c.windowLen <= 0 || c.windowMax <= 0 {
			// 没有窗口上限 → 不限量，也没有可报的余量（schema 的 remaining 是非负有限数，
			// 无上限无法表达，省略 quota 字段）。
			return GrantResult{Allowed: true, MatchedGrantID: c.grantID}, nil
		}
		key := usageKey{grantID: c.grantID, window: c.window}
		used := e.prune(key, c.windowLen, now)
		if used+charge > c.windowMax {
			continue // 窗口已满：限额记在别的 grant 上可能还有余额
		}
		e.usage[key] = append(e.usage[key], usageRecord{at: now, amount: charge})
		return GrantResult{
			Allowed:        true,
			MatchedGrantID: c.grantID,
			Quota:          &QuotaStatus{Remaining: clampRemaining(c.windowMax - used - charge), Unit: unit},
		}, nil
	}
	return deny(ReasonQuotaExceeded), nil
}

// collect 收集命中的候选授权，并记录是否存在「本来放行但已过期」的授权，
// 好把 NOT_IN_GRANT 与 EXPIRED 区分开。
//
// 每条 grant 只取第一条命中动作的 entry：schema 的 entry 按模块划分，同一动作不会
// 需要两条 entry 叠加（叠加会同时放宽单次与窗口两个上限，语义含糊）。
func (e *GrantEngine) collect(agent, action string, now time.Time) (live []candidate, sawExpired bool) {
	for _, g := range e.grants {
		if g.Agent != agent {
			continue
		}
		entry, ok := firstEntryWith(g.Allow, action)
		if !ok {
			continue
		}
		// now >= expiresAt 即视为过期（到期时刻是闭区间的终点）。
		if !now.Before(g.ExpiresAt) {
			sawExpired = true
			continue
		}
		c := candidate{grantID: g.GrantID, narrowness: countActions(g.Allow)}
		if entry.Constraints != nil {
			c.perCall = entry.Constraints.MaxQtyPerCall
			c.window = entry.Constraints.Window
			c.windowMax = entry.Constraints.MaxUnitsPerWindow
		}
		live = append(live, c)
	}
	return live, sawExpired
}

// subjectPermits 判断委托人是否也被授予了该动作——交集规则的下半部分（决策 D5）。
// 没有装配权限表或委托人未登记时一律不通过：宁可全拒，也不默认放行。
func (e *GrantEngine) subjectPermits(subject, action string) bool {
	actions, ok := e.subjects[subject]
	if !ok {
		return false
	}
	return actions[action]
}

// unitFor 取动作的计量单位：未声明时按 quantity。
func (e *GrantEngine) unitFor(action string) (string, error) {
	unit, ok := e.units[action]
	if !ok || unit == "" {
		return UnitQuantity, nil
	}
	switch unit {
	case UnitCalls, UnitQuantity:
		return unit, nil
	default:
		return "", fmt.Errorf("%w: 动作 %q 的计量单位 %q 不在 calls|quantity 内", ErrUnsupportedQuotaUnit, action, unit)
	}
}

// prune 丢掉窗口外的旧记录，返回窗口内已用的配额之和。
// 窗口是左开右闭区间 (now-window, now]：恰好在边界上的记录不再计入。
func (e *GrantEngine) prune(key usageKey, window time.Duration, now time.Time) float64 {
	old := e.usage[key]
	if len(old) == 0 {
		return 0
	}
	cutoff := now.Add(-window)
	kept := make([]usageRecord, 0, len(old))
	var used float64
	for _, r := range old {
		if !r.at.After(cutoff) {
			continue
		}
		kept = append(kept, r)
		used += r.amount
	}
	if len(kept) == 0 {
		delete(e.usage, key)
		return 0
	}
	e.usage[key] = kept
	return used
}

// deny 构造拒绝结果。拒绝报文只有 allowed 与 reason（schema 的
// GrantDeniedEvaluation 是封闭结构）。
func deny(reason string) GrantResult {
	return GrantResult{Reason: reason}
}

// chargeFor 把本次调用的业务量折算成配额消耗：calls 每次记 1；quantity 取绝对值
// （负调整同样占配额），NaN/±Inf 按 0 计，避免用特殊值刷高窗口余额。
func chargeFor(amount float64, unit string) float64 {
	if unit == UnitCalls {
		return 1
	}
	quantity := math.Abs(amount)
	if math.IsNaN(quantity) || math.IsInf(quantity, 0) {
		return 0
	}
	return quantity
}

// clampRemaining 把浮点误差造成的负余量夹到 0（schema 要求 remaining ≥ 0）。
func clampRemaining(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	return v
}

// parseWindow 把 ISO-8601 duration 解析成滚动窗口长度。
//
// 只支持三种写法：PnD（n 天）、PnW（n 周）、PTnH（n 小时）。其他写法——月份 PnM、
// 分钟 PTnM、年 PnY、混合 P1DT12H、小数、前导零、零长度、小写——一律返回
// ErrUnsupportedWindow：窗口猜错会静默放大或缩小配额，宁可报错。
func parseWindow(window string) (time.Duration, error) {
	unsupported := func() (time.Duration, error) {
		return 0, fmt.Errorf("%w: 只支持 PnD/PnW/PTnH，得到 %q", ErrUnsupportedWindow, window)
	}

	var (
		digits string
		unit   time.Duration
	)
	switch {
	case strings.HasPrefix(window, "PT"):
		digits, unit = strings.TrimPrefix(window, "PT"), time.Hour
		if !strings.HasSuffix(digits, "H") {
			return unsupported()
		}
		digits = strings.TrimSuffix(digits, "H")
	case strings.HasPrefix(window, "P"):
		rest := strings.TrimPrefix(window, "P")
		switch {
		case strings.HasSuffix(rest, "D"):
			digits, unit = strings.TrimSuffix(rest, "D"), 24*time.Hour
		case strings.HasSuffix(rest, "W"):
			digits, unit = strings.TrimSuffix(rest, "W"), 7*24*time.Hour
		default:
			return unsupported()
		}
	default:
		return unsupported()
	}

	// 必须是非零的正整数：前导零与 0 都算读不懂（零长度窗口永远攒不满，语义矛盾）。
	if digits == "" || digits[0] == '0' {
		return unsupported()
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return unsupported()
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return unsupported()
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if n > int64(maxDuration)/int64(unit) {
		return unsupported() // 乘出来会溢出 time.Duration
	}
	return time.Duration(n) * unit, nil
}

// firstEntryWith 返回第一条列出该动作的 entry。
func firstEntryWith(entries []GrantEntry, action string) (GrantEntry, bool) {
	for _, entry := range entries {
		for _, a := range entry.Actions {
			if a == action {
				return entry, true
			}
		}
	}
	return GrantEntry{}, false
}

// countActions 数一条 grant 白名单里的动作总数：越小表示这条授权越窄。
func countActions(entries []GrantEntry) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.Actions)
	}
	return total
}

// cloneGrants 深拷贝授权表，避免引擎状态跟着调用方的后续改动漂移。
func cloneGrants(grants []Grant) []Grant {
	out := make([]Grant, len(grants))
	for i, g := range grants {
		out[i] = g
		out[i].Allow = make([]GrantEntry, len(g.Allow))
		for j, entry := range g.Allow {
			out[i].Allow[j] = entry
			out[i].Allow[j].Actions = append([]string(nil), entry.Actions...)
			if entry.Constraints != nil {
				constraints := *entry.Constraints
				out[i].Allow[j].Constraints = &constraints
			}
		}
	}
	return out
}
