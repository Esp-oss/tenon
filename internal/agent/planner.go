// Package agent 是 Tenon 的 agent 运行时：把模块宣告的业务事实，变成一份可审阅的
// 执行计划，再按固定闸门顺序把它落地。它只做两件事，分别在本文件与 run.go：
//
//   - 规划（planner.go）：组装上下文（事件 + skill 正文 + **动态发现**的动作清单）→
//     调 LLM → 用 spec/0.1/schemas/plan.schema.json 校验；坏输出带着校验错误重试一次，
//     仍失败即报错。Plan 结构是 schema 的 Go 投影，不在这里重复定义字段语义。
//   - 执行（run.go）：规划 → policies:evaluate → grants:evaluate → 写动作的 :dry-run →
//     审计 dispatched → 执行 → 审计 result → 结局分类 → 发 Intent 帧。
//
// 本文件还承载 SkillStore：skill 的 frontmatter（tenon.subscribes）只决定「要不要为一个
// 事件开回合」，正文只提供业务语义参考，**不决定做什么**（设计增量 §4、§6.4）。
//
// # 一条禁令：本包的非测试代码里不得出现任何业务 action 字面量
//
// 形如 "inventory.DeductStock" 的字符串一律不许写进本包（含 cmd/agent）的 Go 代码。
// agent 能做什么只能来自两个动态来源：
//
//  1. 宿主注册表 GET /tenon/modules → 各模块 GET /.well-known/tenon 的动作摘要；
//  2. LLM 依据上面这份清单产出的 Plan。
//
// 理由很具体：D1 的判定是「摘掉 procurement 后，同一事件的计划必须改变」。一旦在 Go
// 代码里写下某个 action id，B 轮就可能「按固定映射」照样调到那个动作，差异测试就从
// 「证明 agent 真的用了动态清单」退化成「证明有人写对了映射」。所以清单之外的 action
// 既不在 prompt 里出现（TestPromptMentionsOnlyDiscoveredActions），也会被
// capabilityGapError 拦成 needs_human（TestCapabilityGapGoesNeedsHuman）。
//
// 测试文件不受这条禁令约束：用例需要固定的 action id 才能脚本化假 LLM 与假模块，
// 那是「验证禁令」的手段，不是生产映射。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tenon.dev/internal/spec"
)

const (
	// planSchemaName 是 Plan 的唯一判据。换文件名等于换判据，所以它是常量而不是参数。
	planSchemaName = "plan.schema.json"
	// maxPlanAttempts 是「首次 + 带上校验错误重试 1 次」的总次数（设计 §5）。
	maxPlanAttempts = 2
	// skillFileName 是 skill 文档的约定文件名（§6.4）：只有它带业务元数据。
	skillFileName = "SKILL.md"
)

// DomainEvent 是 domain-event.schema.json 的 Go 投影：模块宣告的一条业务事实。
//
// 它是 agent 的开回合依据，**不是指令**：payload 里没有任何「该调哪个动作」的暗示，
// 从事件类型到动作的映射只存在于模型对清单的推理里（§2.2 反模式判据）。
type DomainEvent struct {
	EventID       string          `json:"eventId"`
	Type          string          `json:"type"`
	Version       string          `json:"version"`
	Subject       json.RawMessage `json:"subject"`
	OccurredAt    string          `json:"occurredAt"`
	CausationID   *string         `json:"causationId"`
	CorrelationID string          `json:"correlationId"`
	Payload       json.RawMessage `json:"payload"`
}

// LLMClient 是规划器唯一依赖的模型能力。抽成接口只有一个目的：让测试注入脚本化的假
// LLM——internal/agent 的全部用例都不发真实请求、不花 token（设计 §6 测试策略）。
// internal/llm.Client 天然满足它。
type LLMClient interface {
	ChatJSON(ctx context.Context, system, user string) ([]byte, error)
}

// ModuleDigest 是一个**可达**模块的动作清单摘要。
//
// Actions 只含从 GET /.well-known/tenon 动态发现的 action id——它们不属于本包代码，
// 是每次运行从注册表与 manifest 拿到的数据。Enabled=false 的模块不会出现在这里：
// Run 在能力发现阶段就按注册表的 enabled 过滤（D1 的 B 轮靠这一点）。
type ModuleDigest struct {
	ID      string
	Actions []string
}

// PlanInput 是一次规划的全部输入。
//
// Feedback 是上一轮执行被 business_rejection 拒绝的原因（首次规划为空串）。没有它，
// temperature=0 的模型会对同一份 prompt 返回同一份计划，「重新规划」就退化成三次
// 一模一样的撞墙（设计 §5 的「重新规划上限 2 轮」要求每一轮都拿到新信息）。
type PlanInput struct {
	Event     DomainEvent
	SkillText string
	Modules   []ModuleDigest
	Feedback  string
	// Request 是人工发起时的原话；事件回合为 nil。
	// 它与 Event 语义不同：Event 是领域事实，Request 是**指令**。
	Request *UserRequest
}

// UserRequest 是 tenon.request 帧在 agent 侧的投影：人要什么，以及是谁要的。
//
// 它刻意只有原话，没有领域事实：事实仍然只来自 Event 与动态发现的动作清单，
// 指令只来自人。两者混进同一个 prompt 块，模型就会把「人希望做的事」读成
// 「系统已经发生的事」——那是把指令当事实，方向正好反了。
type UserRequest struct {
	RequestID string
	Text      string
	By        string
}

// Plan 是 plan.schema.json 的 Go 投影（schema 是 additionalProperties=false，多一个
// 字段就不是合法 Plan）。只有 steps[].args 用 json.RawMessage 原样透传：入参结构由
// 目标动作的 inputSchema 定义，agent 不解释、不重排、不改写它。
type Plan struct {
	PlanID        string         `json:"planId"`
	Supersedes    *string        `json:"supersedes"`
	Trigger       PlanTrigger    `json:"trigger"`
	Steps         []PlanStep     `json:"steps"`
	ExpectedDiff  []ExpectedDiff `json:"expectedDiff"`
	DryRun        DryRunReport   `json:"dryRun"`
	EstimatedRisk string         `json:"estimatedRisk"`
	Rationale     *string        `json:"rationale"`
}

// PlanTrigger 是计划与业务链的锚点：审计回放与对账都靠它对齐。
type PlanTrigger struct {
	EventID       string `json:"eventId"`
	CorrelationID string `json:"correlationId"`
}

// PlanStep 是计划的一步。Kind 由模型按清单里的动作声明填写，宿主与模块仍会各自复核。
type PlanStep struct {
	StepID         string          `json:"stepId"`
	Action         string          `json:"action"`
	Args           json.RawMessage `json:"args"`
	DependsOn      []string        `json:"dependsOn"`
	Kind           string          `json:"kind"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Compensation   string          `json:"compensation,omitempty"`
}

// ExpectedDiff 是一处预期数据变化。Entity 用 EntityRefLike（对象或字符串简写）。
type ExpectedDiff struct {
	Entity json.RawMessage `json:"entity"`
	Field  string          `json:"field"`
	From   json.RawMessage `json:"from"`
	To     json.RawMessage `json:"to"`
}

// DryRunReport 是预演结论（四态枚举，刻意不是布尔）。
type DryRunReport struct {
	Status string         `json:"status"`
	Diff   []ExpectedDiff `json:"diff,omitempty"`
}

// Planner 把上下文变成一份过 schema 的 Plan。
//
// 两个字段都是必填：没有 LLM 就没有计划，没有 schema 就无法判断模型输出是否可用。
// 任一缺失时 Plan 直接报错——「跳过校验先跑起来」是最坏的降级方式。
type Planner struct {
	LLM  LLMClient
	Spec *spec.Set
}

// Plan 组装 prompt、调模型、校验输出；失败时带上校验错误重试一次，仍失败即返回错误。
//
// 三处判据分开，避免互相污染：
//   - 模型/传输层错误 → 立即返回（不把网关故障当成「模型给了坏计划」去重试）；
//   - schema 校验失败 → 重试一次（错误文本回填进 prompt）；
//   - 动作不在动态清单里 → capabilityGapError（能力不足，由 Run 判 needs_human）。
func (p *Planner) Plan(ctx context.Context, in PlanInput) (Plan, error) {
	if p == nil || p.LLM == nil {
		return Plan{}, errors.New("agent: 规划器缺少 LLM 客户端")
	}
	if p.Spec == nil {
		return Plan{}, errors.New("agent: 规划器缺少 schema 集：没有 " + planSchemaName + " 就无法判断模型输出是否可用")
	}

	system := systemPrompt()
	user := userPrompt(in)
	var lastErr error
	for attempt := 1; attempt <= maxPlanAttempts; attempt++ {
		raw, err := p.LLM.ChatJSON(ctx, system, user)
		if err != nil {
			if lastErr != nil {
				return Plan{}, fmt.Errorf("agent: 模型调用失败（此前一次输出未通过 %s 校验：%v）：%w", planSchemaName, lastErr, err)
			}
			return Plan{}, fmt.Errorf("agent: 模型调用失败：%w", err)
		}
		plan, err := p.check(raw, in)
		if err == nil {
			return plan, nil
		}
		var gap *capabilityGapError
		if errors.As(err, &gap) {
			// 能力不足不是格式问题：temperature=0 下再问一次多半得到同一份计划，
			// 而 Run 需要把这个信号判成 needs_human（设计 §5）而不是 failed。
			return Plan{}, err
		}
		lastErr = err
		user = userPrompt(in) + retryHint(err)
	}
	return Plan{}, fmt.Errorf("agent: 模型输出连续 %d 次未通过 %s 校验：%w", maxPlanAttempts, planSchemaName, lastErr)
}

// check 先按 schema 校验原始字节（模型给什么就校验什么，不先做「宽容解析」），
// 再反序列化成 Plan，最后做 schema 表达不了的集合校验。
func (p *Planner) check(raw []byte, in PlanInput) (Plan, error) {
	if err := p.Spec.Validate(planSchemaName, raw); err != nil {
		return Plan{}, fmt.Errorf("%s 校验未通过：%w", planSchemaName, err)
	}
	var plan Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return Plan{}, fmt.Errorf("Plan 通过 schema 后仍无法反序列化：%w", err)
	}
	if err := checkActionsDiscovered(plan, in.Modules); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// capabilityGapError 表示一份 schema 合法、但要求了动态清单里不存在的动作的计划。
//
// 它与「输出不合 schema」必须分开：前者是能力不足（设计 §5：转人工，Intent 帧说明
// 缺口），后者是模型输出坏了（失败）。两者混成一个错误，D1 的 B 轮就会被记成
// 「agent 崩了」而不是「agent 正确地举手」。
type capabilityGapError struct {
	PlanID  string
	Actions []string
}

func (e *capabilityGapError) Error() string {
	return fmt.Sprintf("计划 %s 引用了动态清单里没有的动作：%s", e.PlanID, strings.Join(e.Actions, "、"))
}

// checkActionsDiscovered 落实「agent 只能调清单里的动作」这条硬约束。
//
// schema 只能约束 action 是一个合法 ActionId，无法判断它存不存在（跨文档比对）。这里补上：
//   - 没做能力发现（modules 为空）→ 不做判定：直接使用 Planner 的调用方自担其责；
//   - 做了能力发现 → 每个 step 的 action 都必须在清单里，否则返回 capabilityGapError。
//     注意「模块都声明了零个动作」也走这条严格分支：available 为空意味着**任何**动作
//     都不该出现，放行等于把「没有能力」当成「什么都能做」。
func checkActionsDiscovered(plan Plan, modules []ModuleDigest) error {
	if len(modules) == 0 {
		return nil
	}
	available := make(map[string]struct{})
	for _, m := range modules {
		for _, a := range m.Actions {
			if a = strings.TrimSpace(a); a != "" {
				available[a] = struct{}{}
			}
		}
	}
	var missing []string
	seen := make(map[string]struct{})
	for _, step := range plan.Steps {
		action := strings.TrimSpace(step.Action)
		if _, ok := available[action]; ok {
			continue
		}
		if _, dup := seen[action]; dup {
			continue
		}
		seen[action] = struct{}{}
		missing = append(missing, action)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &capabilityGapError{PlanID: plan.PlanID, Actions: missing}
}

// retryHint 是第二次尝试时追加到 prompt 末尾的校验错误回填：只说「错了」而不说
// 「错在哪」，模型没有可修正的信息。
func retryHint(err error) string {
	return "\n上一次输出未通过 " + planSchemaName + " 校验，错误如下（请修正后重新输出一个完整 JSON 对象，" +
		"不要解释、不要 Markdown 围栏）：\n" + err.Error() + "\n"
}

// systemPrompt 是固定的系统约束。它刻意不含任何 action id，也不含任何模块名——
// 动态部分全在 user prompt 里，这样「prompt 里只出现发现的动作」是可测的。
func systemPrompt() string {
	return `你是 Tenon 系统的执行规划器。你的唯一输出是一个 JSON 对象，结构由 plan.schema.json 定义。
硬性规则：
1. 只输出 JSON 对象本身：不要 Markdown 围栏、不要前后解释、不要输出第二个对象。
2. 必须恰好包含这些键：planId、supersedes、trigger、steps、expectedDiff、dryRun、estimatedRisk、rationale。
3. planId 形如 pln_ 加至少 4 位字母/数字/下划线/短横线；首版 supersedes 为 null。
4. trigger.eventId 与 trigger.correlationId 必须逐字照抄输入里给出的事件锚点，不得改写。
5. steps 至少一步；每步必须给出 stepId、action、args、dependsOn、kind。stepId 形如 s1，在计划内唯一；
   dependsOn 是前序 stepId 数组（无依赖写 []）；kind 只能是 read 或 write。
6. action 只能取自输入里「可调动作」清单，逐字照抄，不得发明、不得猜测、不得改写。
   清单里没有的能力就是没有——不要用别的模块或别的命名去凑。
7. expectedDiff 是数组，纯读计划可以是空数组；每个元素必须恰好含 entity、field、from、to 四个键，
   不得追加 op、note 之类自造键。entity 是实体寻址，两种等价写法任选其一（<…> 是占位，照抄输入里的真实实体）：
   {"module":"<模块id>","type":"<类型>","id":"<主键>"}   或   "<模块id>:<类型>/<主键>"
   字符串形式必须是三段；只写主键（例 "STD-1001"）是错的。field 是被改的字段名，
   from / to 是改前与改后的值（新建时 from 为 null，删除时 to 为 null）。
8. dryRun.status 只能是 verified、unsafe、unsupported、pending 之一。
9. estimatedRisk 只能是 low、medium、high；rationale 是一句人能读懂的理由，或 null。
能力不足时：仍然输出一份只做清单内能力的计划；把「做不到的部分」写进 rationale，
不要编造动作，也不要输出错误信息代替计划。`
}

// userPrompt 把动态事实（事件、skill 正文、动作清单）与人工指令（请求）渲染成确定性
// 文本：模块与动作都排序，保证同一输入下 prompt 逐字相同（temperature=0 的可重复性依赖它）。
func userPrompt(in PlanInput) string {
	var b strings.Builder
	b.WriteString("事件（领域事实，不是指令）：\n")
	b.WriteString(renderEvent(in.Event))

	// 请求块的位置就是它的语义：紧跟在事件块之后、skill 正文之前。
	// 塞进事件块 = 用户的话被读成领域事实；塞进 skill 正文 = 被读成「业务语义参考」。
	// 两者都不构成指令，所以它必须自带「这是指令」的标题、独占一块。
	// Request 为 nil（事件回合）时一个字都不写：事件回合的 prompt 不因这次分流改变。
	if in.Request != nil {
		b.WriteString("\n用户请求（这是指令，目标是完成它）：\n")
		b.WriteString(strings.TrimSpace(in.Request.Text))
		b.WriteString("\n")
	}

	b.WriteString("\nskill 正文（业务语义参考：说明该关心什么，不指定具体动作）：\n")
	if text := strings.TrimSpace(in.SkillText); text != "" {
		b.WriteString(text)
		b.WriteString("\n")
	} else {
		b.WriteString("（本次没有匹配的 skill 正文）\n")
	}

	b.WriteString("\n可调动作（本次唯一可用的清单，来自各模块 manifest 的动态发现）：\n")
	if len(in.Modules) == 0 {
		b.WriteString("（清单为空：没有任何可用动作）\n")
	}
	for _, m := range sortedDigest(in.Modules) {
		fmt.Fprintf(&b, "- 模块 %s：\n", m.ID)
		if len(m.Actions) == 0 {
			b.WriteString("    （该模块没有声明动作）\n")
			continue
		}
		for _, a := range sortedActions(m.Actions) {
			fmt.Fprintf(&b, "    - %s\n", a)
		}
	}
	b.WriteString("\n请只输出一个 JSON 对象：plan.schema.json 定义的 Plan。steps 里每个 action 都必须逐字来自上面的清单。\n")
	if feedback := strings.TrimSpace(in.Feedback); feedback != "" {
		b.WriteString("\n上一轮执行被业务拒绝，请在这一轮换一条路（仍然只能使用上面的清单）：\n")
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	return b.String()
}

// renderEvent 把事件渲染成 prompt 里的事实块。编组失败时退回一个只含关键字段的文本：
// 事件是数据，渲染失败不该让整轮规划崩掉。
func renderEvent(ev DomainEvent) string {
	raw, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return fmt.Sprintf("eventId=%s type=%s correlationId=%s", ev.EventID, ev.Type, ev.CorrelationID)
	}
	return string(raw)
}

// sortedDigest 返回按 ID 排序的清单副本（不排序会让 prompt 随 map 遍历顺序抖动）。
func sortedDigest(modules []ModuleDigest) []ModuleDigest {
	out := append([]ModuleDigest(nil), modules...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sortedActions 返回排序去重后的动作 id。
func sortedActions(actions []string) []string {
	seen := make(map[string]struct{}, len(actions))
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ---------- skill 存储 ----------

// 解析 frontmatter 时的行级约定（只支持这几个键，其余忽略）。
const (
	frontmatterFence = "---"
	skillNameKey     = "name"
	skillTenonKey    = "tenon"
	skillSubscribes  = "subscribes"
)

// Skill 是一份 skill 文档：订阅声明 + 正文。
//
// Subscribes 为空表示「不订阅任何事件」——它不会被任何事件唤醒，这正是「skill 只决定
// 要不要开回合」的直接后果：没有订阅就没有回合。
type Skill struct {
	Name       string
	Path       string
	Subscribes []string
	Body       string
}

// Matches 判断该 skill 是否订阅了某事件类型。
func (s Skill) Matches(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return false
	}
	for _, sub := range s.Subscribes {
		if sub == eventType {
			return true
		}
	}
	return false
}

// SkillStore 是按事件类型索引的 skill 集合。零值可用（等于「没有 skill」）。
type SkillStore struct {
	skills []Skill
}

// EmptySkills 返回一个空集合。它与「目录不存在」等价，供调用方显式表达
// 「本次没有 skill 可用」，而不是拿 nil 到处判空。
func EmptySkills() *SkillStore { return &SkillStore{} }

// LoadSkills 读取 dir 下所有 SKILL.md。
//
// 边界是刻意选的：
//   - dir 为空或不存在 → 空集合 + nil：没有 skill 是合法部署，但调用方必须把条数
//     打出来（0 个 skill 意味着「永远不开回合」，静默等于把故障藏起来）；
//   - dir 存在而某个文件读不懂（frontmatter 不闭合、订阅项为空）→ 报错：
//     静默跳过一份写坏的 skill，会让「该开回合却没开」变成看不见的故障。
func LoadSkills(dir string) (*SkillStore, error) {
	store := &SkillStore{}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return store, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("agent: 读不到 skill 目录 %q：%w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("agent: skill 路径 %q 不是目录", dir)
	}

	var paths []string
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), skillFileName) {
			paths = append(paths, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("agent: 遍历 skill 目录 %q 失败：%w", dir, walkErr)
	}
	sort.Strings(paths)

	for _, path := range paths {
		skill, err := loadSkill(path)
		if err != nil {
			return nil, err
		}
		store.skills = append(store.skills, skill)
	}
	return store, nil
}

// All 返回全部 skill（按路径排序，稳定可重复）。
func (s *SkillStore) All() []Skill {
	if s == nil {
		return nil
	}
	return append([]Skill(nil), s.skills...)
}

// Len 返回 skill 条数（nil 接收者为 0）。
func (s *SkillStore) Len() int {
	if s == nil {
		return 0
	}
	return len(s.skills)
}

// Matching 返回订阅了 eventType 的 skill。
func (s *SkillStore) Matching(eventType string) []Skill {
	if s == nil {
		return nil
	}
	out := make([]Skill, 0, len(s.skills))
	for _, skill := range s.skills {
		if skill.Matches(eventType) {
			out = append(out, skill)
		}
	}
	return out
}

// TextFor 返回订阅了 eventType 的 skill 正文（多份按路径顺序拼接）。
// 返回空串表示「没有 skill 愿意为这个事件开回合」——调用方据此跳过，而不是自己猜。
func (s *SkillStore) TextFor(eventType string) string {
	matched := s.Matching(eventType)
	if len(matched) == 0 {
		return ""
	}
	parts := make([]string, 0, len(matched))
	for _, skill := range matched {
		body := strings.TrimSpace(skill.Body)
		if body == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("## skill %s\n%s", skill.Name, body))
	}
	return strings.Join(parts, "\n\n")
}

// noBodyPlaceholder 是「选中的 skill 没有正文」的渲染文本。
//
// 它必须是一句显式的话，不能是空串：候选集是按模块名命中的，一份只靠 subscribes 命中、
// 正文为空的 skill 被选中很正常。渲染成空串，userPrompt 就会写下「本次没有匹配的
// skill 正文」——选了 N 份却告诉模型一份都没有。
const noBodyPlaceholder = "（无正文，仅按订阅命中）"

// TextForSelected 渲染**已选中**的 skill（候选集），格式与 TextFor 逐字一致：
// 每份一个「## skill <name>」小标题加正文，份与份之间空一行。
//
// 与 TextFor 的唯一区别是空正文的渲染（见 noBodyPlaceholder）——所以不能用 TextFor
// 来渲染候选集：它跳过空 Body，选中的 skill 越多，prompt 越可能声称「没有正文」。
// selected 为空时返回空串，让 userPrompt 走它自己的「本次没有匹配」分支。
//
// 接收者用不到：留在 SkillStore 上是为了与 TextFor 同一调用形状，并且 nil *SkillStore
// 也能直接调用（Run 里因此不必为它单独判空）。
func (s *SkillStore) TextForSelected(selected []Skill) string {
	if len(selected) == 0 {
		return ""
	}
	parts := make([]string, 0, len(selected))
	for _, skill := range selected {
		body := strings.TrimSpace(skill.Body)
		if body == "" {
			body = noBodyPlaceholder
		}
		parts = append(parts, fmt.Sprintf("## skill %s\n%s", skill.Name, body))
	}
	return strings.Join(parts, "\n\n")
}

// loadSkill 读一份 SKILL.md 并解析 frontmatter。
func loadSkill(path string) (Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, fmt.Errorf("agent: 读取 skill %q 失败：%w", path, err)
	}
	// 统一换行：frontmatter 的分行判定不该因为 CRLF 而失效。
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	front, body, hasFront, err := splitFrontmatter(text)
	if err != nil {
		return Skill{}, fmt.Errorf("agent: skill %q 的 frontmatter 不可用：%w", path, err)
	}
	skill := Skill{Path: path, Name: filepath.Base(filepath.Dir(path)), Body: body}
	if !hasFront {
		return skill, nil
	}
	if name := frontValue(front, skillNameKey); name != "" {
		skill.Name = name
	}
	subs, err := parseSubscribes(front)
	if err != nil {
		return Skill{}, fmt.Errorf("agent: skill %q 的 %s.%s 不可用：%w", path, skillTenonKey, skillSubscribes, err)
	}
	skill.Subscribes = subs
	return skill, nil
}

// splitFrontmatter 把文档切成 frontmatter 与正文。
//
// 没有以 --- 开头就是「没有 frontmatter」（整篇都是正文）；开始了却没有闭合的 ---
// 则报错——半截 frontmatter 会让订阅声明被当成正文，于是该开的回合永远不开。
func splitFrontmatter(text string) (front, body string, hasFront bool, err error) {
	if !strings.HasPrefix(text, frontmatterFence+"\n") {
		return "", text, false, nil
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") != frontmatterFence {
			continue
		}
		front = strings.Join(lines[1:i], "\n")
		body = strings.Join(lines[i+1:], "\n")
		return front, body, true, nil
	}
	return "", "", false, errors.New("以 " + frontmatterFence + " 开始但没有闭合的 " + frontmatterFence + " 行")
}

// frontValue 取 frontmatter 顶格里某个键的标量值（去引号）。
func frontValue(front, key string) string {
	for _, line := range strings.Split(front, "\n") {
		if line != strings.TrimLeft(line, " \t") {
			continue // 有缩进的行属于某个块，不是顶格键
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// parseSubscribes 解析 tenon.subscribes。
//
// 只支持两种写法（不引入 YAML 依赖，也不猜第三种）：
//
//	tenon:
//	  subscribes: ["quote.completed", "quote.revised"]
//
//	tenon:
//	  subscribes:
//	    - quote.completed
//
// 其它 key（expectedModules 等）忽略：本层只关心「要不要开回合」。
func parseSubscribes(front string) ([]string, error) {
	lines := strings.Split(front, "\n")
	start := -1
	for i, line := range lines {
		if line != strings.TrimLeft(line, " \t") {
			continue // 有缩进的行属于某个块，不是顶格键
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.TrimSpace(name) != skillTenonKey || strings.TrimSpace(value) != "" {
			continue
		}
		start = i
		break
	}
	if start < 0 {
		return nil, nil
	}

	// 扫描 tenon 块：块在第一个顶格、非空的行处结束。
	blockEnd := len(lines)
	for i := start + 1; i < len(lines); i++ {
		raw := lines[i]
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if raw == strings.TrimLeft(raw, " \t") {
			blockEnd = i
			break
		}
	}

	for i := start + 1; i < blockEnd; i++ {
		line := lines[i]
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.TrimSpace(name) != skillSubscribes {
			continue
		}
		inline := strings.TrimSpace(value)
		if inline != "" {
			return parseInlineList(inline)
		}
		// 块列表：收集后续缩进更深的 "- item" 行。
		var items []string
		for j := i + 1; j < blockEnd; j++ {
			item := lines[j]
			if strings.TrimSpace(item) == "" {
				continue
			}
			itemIndent := len(item) - len(strings.TrimLeft(item, " \t"))
			if itemIndent <= indent {
				break
			}
			trimmed := strings.TrimSpace(item)
			if !strings.HasPrefix(trimmed, "- ") && trimmed != "-" {
				break
			}
			entry := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), " "))
			if entry == "" {
				return nil, errors.New("块列表里有空项")
			}
			items = append(items, strings.Trim(entry, `"'`))
		}
		return items, nil
	}
	return nil, nil
}

// parseInlineList 解析行内列表：优先按 JSON 数组（双引号、逗号分隔），
// 退回按逗号切分（容忍 [a, b] 这种不带引号的写法）。
func parseInlineList(inline string) ([]string, error) {
	text := strings.TrimSpace(inline)
	var items []string
	if strings.HasPrefix(text, "[") {
		if err := json.Unmarshal([]byte(text), &items); err != nil {
			// 不是合法 JSON 数组：退回宽松切分，但要求它确实被方括号包着。
			if !strings.HasSuffix(text, "]") {
				return nil, fmt.Errorf("行内列表 %q 缺少右方括号", inline)
			}
			text = strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
			for _, part := range strings.Split(text, ",") {
				if item := strings.Trim(strings.TrimSpace(part), `"'`); item != "" {
					items = append(items, item)
				}
			}
			if len(items) == 0 {
				return nil, fmt.Errorf("行内列表 %q 为空", inline)
			}
			return items, nil
		}
	} else {
		item := strings.Trim(text, `"'`)
		if item == "" {
			return nil, errors.New("订阅项为空")
		}
		return []string{item}, nil
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, errors.New("列表里有空字符串")
		}
		out = append(out, item)
	}
	return out, nil
}
