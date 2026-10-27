// Command agent 是 Tenon 的 agent 运行时进程（三进程里的 A 框）。
//
// 它做四件事，每一件都必须能解释「为什么这条事件被处理 / 被跳过」：
//
//	1 拉 GET /tenon/modules，**自己按 enabled 过滤**（宿主返回的是管理面视图，
//	  含已摘除条目；D1 的 B 轮靠这一步让 procurement 从 agent 眼里消失）；
//	2 逐模块 GET /tenon/events?direction=out 读发件箱，按 nextCursor 前进；
//	3 对每个事件按 skill 的 tenon.subscribes 判断「要不要开回合」——没有 skill
//	  订阅就跳过并记一行日志，绝不替业务决定；
//	4 开回合就调 internal/agent.Run（规划 → 闸门 → 两段审计 → 执行 → Intent 帧），
//	  并把结局（completed / needs_human / failed）连同 planId 与动作序列记下来。
//
// 环境变量：
//
//	TENON_HOST_URL        宿主基地址，缺省 http://127.0.0.1:8080
//	TENON_SKILL_DIR       skill 目录（每个子目录一份 SKILL.md），缺省 skills
//	TENON_AGENT_ID        审计 actor.id，缺省 ag_tenon
//	TENON_ON_BEHALF_OF    代行委托人（例 user:alice）；**必填**，不提供默认值
//	TENON_POLL_INTERVAL   轮询间隔，缺省 1s
//	TENON_ONCE            真值时只跑一轮就退出（D1 差异测试用它取「一轮」的语义）
//	TENON_CURSOR_FILE     事件游标持久化文件；缺省只在内存里（重启即从头读，安全）
//	TENON_SPEC_DIR        schema 目录，缺省 spec/0.1/schemas
//	TENON_RESULT_FILE     每回合追加一行 JSON 的运行结果（供 D1 取 A/B 两轮的动作序列）
//
// 模型配置由 internal/llm 读取（TENON_LLM_BASE_URL / TENON_LLM_API_KEY[_FILE] /
// TENON_LLM_MODEL / TENON_LLM_TIMEOUT）。
//
// **缺 LLM key 必须启动即失败**（设计 §5）：没有模型就不可能规划，静默降级只会让
// agent 在没有模型的情况下假装「已规划」——那是最坏的失败方式。同理，schema 读不到
// 也不允许启动：没有 plan.schema.json 就没有判据，模型输出什么都会被当成可用。
//
// 关于 TENON_RESULT_FILE：它是**运维产物，不是协议端点**。一行一个回合，字段是
// {at, correlationId, outcome, planId, actions, detail}。其中 actions 是**规划出来的**
// 动作序列（按计划声明顺序），而不是「执行过的」——D1 判的正是规划层的差异：A 轮
// 常在预演闸门就 fail-closed 转人工，真正执行的动作是 0 个，只有计划里才看得见
// 「它原本打算调什么」。写失败只记日志、不改结局：一份观测文件的可用性不该决定
// 一笔业务的处置（与 Intent 帧同一条原则）。
//
// 关于 TENON_ON_BEHALF_OF 为什么必填：每一条写审计的 actor.onBehalfOf 都取自它，
// 而授权评估的权限是「agent 被授予 ∩ 委托人权限」。给它编一个默认身份，等于让审计
// 记录归属于一个不存在的人——宁可起不来，也不要写出一条追不到责的审计。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"tenon.dev/internal/agent"
	"tenon.dev/internal/llm"
	"tenon.dev/internal/protocol"
	"tenon.dev/internal/spec"
)

const (
	envHost         = "TENON_HOST_URL"
	envSkillDir     = "TENON_SKILL_DIR"
	envAgentID      = "TENON_AGENT_ID"
	envOnBehalfOf   = "TENON_ON_BEHALF_OF"
	envPollInterval = "TENON_POLL_INTERVAL"
	envOnce         = "TENON_ONCE"
	envCursorFile   = "TENON_CURSOR_FILE"
	envSpecDir      = "TENON_SPEC_DIR"
	envResultFile   = "TENON_RESULT_FILE"

	// defaultHost 是 interfaces.md §1 给宿主的端口。
	defaultHost = "http://127.0.0.1:8080"
	// defaultSkillDir 与计划约定一致（相对进程工作目录）。
	defaultSkillDir = "skills"
	// defaultAgentID 是审计 actor.id 的缺省值，满足 common.schema.json 的 Actor.id 词法。
	defaultAgentID = "ag_tenon"
	// defaultPollInterval 是缺省轮询间隔。
	defaultPollInterval = time.Second

	// pollTimeout 是轮询用 HTTP 请求的超时：读注册表与发件箱都是轻请求。
	// 动作执行走 agent.Run 自己的超时（内部 http.Client 缺省 60s）。
	pollTimeout = 30 * time.Second
	// maxBodyBytes 是响应体上限（事件页与注册表都是 KB 量级）。
	maxBodyBytes = 1 << 20
	// maxObservedPlans 是每回合最多留存的规划原始输出份数（一次规划最多 2 次尝试，
	// 一个回合最多 3 轮：8 份足够找到「被接受的那份」，且不会无界增长）。
	maxObservedPlans = 8
)

// 规范词法（common.schema.json#/$defs/Actor）：启动时就把不合法的身份拦下来，
// 否则它们会以「审计字段不合法」的形式出现在下游，排查方向完全错。
var (
	agentIDPattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:@-]*$`)
	onBehalfOfPattern = regexp.MustCompile(`^(user|agent):[^\s]+$`)
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

// config 是进程的静态配置。它只由环境变量决定，运行期不再变化。
type config struct {
	host         string
	skillDir     string
	agentID      string
	onBehalfOf   string
	pollInterval time.Duration
	once         bool
	cursorFile   string
	specDir      string
	resultFile   string
	llm          llm.Config
}

// loadConfig 读环境变量并校验。LLM 配置最先读：缺 key 要在碰任何网络与文件之前失败。
func loadConfig() (config, error) {
	llmCfg, err := llm.ConfigFromEnv()
	if err != nil {
		return config{}, err
	}

	cfg := config{
		host:         firstNonEmpty(os.Getenv(envHost), defaultHost),
		skillDir:     firstNonEmpty(os.Getenv(envSkillDir), defaultSkillDir),
		agentID:      firstNonEmpty(os.Getenv(envAgentID), defaultAgentID),
		onBehalfOf:   strings.TrimSpace(os.Getenv(envOnBehalfOf)),
		pollInterval: defaultPollInterval,
		cursorFile:   strings.TrimSpace(os.Getenv(envCursorFile)),
		specDir:      firstNonEmpty(os.Getenv(envSpecDir), spec.DefaultDir),
		resultFile:   strings.TrimSpace(os.Getenv(envResultFile)),
		llm:          llmCfg,
	}

	host, err := normalizeHost(cfg.host)
	if err != nil {
		return config{}, err
	}
	cfg.host = host

	if !agentIDPattern.MatchString(cfg.agentID) {
		return config{}, fmt.Errorf("%s=%q 不符合审计 actor.id 的词法 %s",
			envAgentID, cfg.agentID, agentIDPattern)
	}
	if cfg.onBehalfOf == "" {
		return config{}, fmt.Errorf("缺少 %s：agent 必须知道自己代谁执行（例 user:alice），"+
			"否则授权评估与审计都无从追责", envOnBehalfOf)
	}
	if !onBehalfOfPattern.MatchString(cfg.onBehalfOf) {
		return config{}, fmt.Errorf("%s=%q 不符合委托人词法 %s（例 user:alice）",
			envOnBehalfOf, cfg.onBehalfOf, onBehalfOfPattern)
	}

	if raw := strings.TrimSpace(os.Getenv(envPollInterval)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("%s=%q 不是合法时长（例：1s、500ms）：%w", envPollInterval, raw, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("%s=%q 必须为正数（不接受 0 间隔的忙轮询）", envPollInterval, raw)
		}
		cfg.pollInterval = d
	}

	once, err := parseBool(os.Getenv(envOnce))
	if err != nil {
		return config{}, fmt.Errorf("%s：%w", envOnce, err)
	}
	cfg.once = once
	return cfg, nil
}

// normalizeHost 校验并规范化宿主基地址。
func normalizeHost(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q 不是合法 URL：%w", envHost, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s=%q 的 scheme 必须是 http 或 https", envHost, raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s=%q 缺少主机名", envHost, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s=%q 不应带查询串或锚点", envHost, raw)
	}
	return raw, nil
}

// parseBool 解析「开关型」环境变量：空串是假，常见的真值写法都认。
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("%q 不是可识别的布尔值（用 1/true 打开）", raw)
	}
}

// firstNonEmpty 返回第一个非空值（去首尾空白）。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// run 装配并运行 agent。返回错误即进程应当以非 0 退出。
func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	specSet, err := spec.Load(cfg.specDir)
	if err != nil {
		return fmt.Errorf("加载 schema 失败（没有 %s 就无法判断模型输出是否可用，拒绝启动）：%w",
			spec.DefaultDir, err)
	}
	skills, err := agent.LoadSkills(cfg.skillDir)
	if err != nil {
		return err
	}
	if skills.Len() == 0 {
		// 不 fail-closed 但不静默：0 个 skill 意味着「事件读得到、回合永远不开」。
		log.Printf("agent: 警告 —— %s 下没有加载到 skill：会轮询事件但不会为任何事件开回合"+
			"（skill 的 tenon.subscribes 决定是否思考）", cfg.skillDir)
	}

	// 规划器观测层：只是把模型返回的原始 Plan 记下来，供结果行报告「规划出来的
	// 动作序列」（RunResult.ActionsTaken 只含执行过的动作，而闸门拦下时是 0 个）。
	plans := newPlanObserver(llm.New(cfg.llm))

	deps := agent.Deps{
		Host:       cfg.host,
		AgentID:    cfg.agentID,
		OnBehalfOf: cfg.onBehalfOf,
		Planner:    &agent.Planner{LLM: plans, Spec: specSet},
		HTTP:       &http.Client{Timeout: pollTimeout},
		Skills:     skills,
	}
	cursors, err := loadCursors(cfg.cursorFile)
	if err != nil {
		return err
	}

	p := &poller{cfg: cfg, deps: deps, client: deps.HTTP, cursors: cursors, plans: plans}
	log.Printf("agent: host=%s agent=%s onBehalfOf=%s skill=%d 模型=%s 轮询间隔=%s once=%v",
		cfg.host, cfg.agentID, cfg.onBehalfOf, skills.Len(), cfg.llm.Model, cfg.pollInterval, cfg.once)
	if cfg.resultFile != "" {
		log.Printf("agent: 每回合向 %s 追加一行结果（供 D1 取 A/B 两轮的动作序列）", cfg.resultFile)
	}

	for {
		rounds, pollErr := p.pollOnce(ctx)
		if pollErr != nil {
			if cfg.once {
				return pollErr
			}
			// 常驻模式下不退出：宿主/模块短暂不可用是常态，下一轮再试；
			// 每一轮都失败都会被记下来，不会静默。
			log.Printf("agent: 轮询失败（下一轮继续）：%v", pollErr)
		} else {
			log.Printf("agent: 本轮开回合 %d 个", rounds)
		}
		if cfg.once {
			return nil
		}
		select {
		case <-ctx.Done():
			log.Printf("agent: 收到停止信号，退出")
			return nil
		case <-time.After(cfg.pollInterval):
		}
	}
}

// ---------- 结果行 ----------

// resultLine 是结果文件的一行：一回合一条。
//
// 它是**运维产物**，不是协议端点——协议面只有 interfaces.md 里那几个端点，
// 这里不做版本协商、不发事件、不参与任何闸门判定。字段是 D1 需要的最小集合：
// 结局、计划标识、规划出来的动作序列，以及能串回审计的业务链标识。
type resultLine struct {
	At            string   `json:"at"`
	CorrelationID string   `json:"correlationId"`
	Outcome       string   `json:"outcome"`
	PlanID        string   `json:"planId"`
	Actions       []string `json:"actions"`
	Detail        string   `json:"detail"`
}

// writeResult 把一个回合的结局追加成一行 JSON（未配结果文件时什么都不做）。
//
// 写失败只记日志、不改结局：审计与 Intent 帧已经承载了业务结论，一份观测文件的
// 可用性不该决定一笔业务的处置。代价是 D1 会因等不到结果行而失败——那条理由里
// 会带上路径与最后一次观测，排查方向仍然清楚。
func (p *poller) writeResult(event agent.DomainEvent, res agent.RunResult) {
	if p.cfg.resultFile == "" {
		return
	}
	actions := p.plans.actionsFor(res.PlanID)
	line := resultLine{
		At:            time.Now().UTC().Format(time.RFC3339Nano),
		CorrelationID: strings.TrimSpace(event.CorrelationID),
		Outcome:       string(res.Outcome),
		PlanID:        res.PlanID,
		Actions:       actions,
		Detail:        res.Detail,
	}
	if err := appendResultLine(p.cfg.resultFile, line); err != nil {
		log.Printf("agent: 写结果行失败（不改结局；D1 会因等不到结果行而失败）：%v", err)
	}
}

// appendResultLine 追加一行。用 O_APPEND 打开：多回合之间只加不改，
// 也不会把别的进程写的行覆盖掉。
func appendResultLine(path string, line resultLine) error {
	if line.Actions == nil {
		// 空序列要序列化成 [] 而不是 null：D1 按数组读，null 会让判据变成「没有观测」。
		line.Actions = []string{}
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("结果行编组失败：%w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开结果文件 %q 失败：%w", path, err)
	}
	// 一次 Write 写完整行：读侧不会读到半行（换行是行分隔符，不是格式细节）。
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("写结果文件 %q 失败：%w", path, err)
	}
	return f.Close()
}

// planObserver 包在模型客户端外面，只做一件事：把模型返回的原始 Plan 记下来。
//
// 为什么需要这一层：RunResult.ActionsTaken 只含**真正执行过**的动作，而 D1 判的是
// 规划层——A 轮在预演闸门（procurement 没有 :dry-run，404 即 fail-closed）就转人工，
// 一个动作都没执行，ActionsTaken 必然是空；B 轮也可能根本不派发。只有模型返回的
// Plan 里才有「A/B 两轮的规划差异」。
//
// 这不改 internal/agent 一行代码：Planner.LLM 是 agent.LLMClient 接口，本就是给
// 外部注入客户端的缝（设计 §6 用它注入假 LLM），这里注入的是一个纯透传包装。
type planObserver struct {
	inner agent.LLMClient

	mu   sync.Mutex
	raws [][]byte
}

func newPlanObserver(inner agent.LLMClient) *planObserver {
	return &planObserver{inner: inner}
}

// ChatJSON 透传模型调用，顺带留存返回的原始字节。
func (o *planObserver) ChatJSON(ctx context.Context, system, user string) ([]byte, error) {
	raw, err := o.inner.ChatJSON(ctx, system, user)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	if len(o.raws) >= maxObservedPlans {
		o.raws = o.raws[len(o.raws)-maxObservedPlans+1:]
	}
	o.raws = append(o.raws, append([]byte(nil), raw...))
	o.mu.Unlock()
	return raw, nil
}

// beginRound 清掉上一回合的观测。每回合只用本回合的计划，避免跨回合串味。
func (o *planObserver) beginRound() {
	o.mu.Lock()
	o.raws = o.raws[:0]
	o.mu.Unlock()
}

// actionsFor 取「被接受的那份计划」的动作序列。
//
// 先按 planId 精确匹配（一次回合里的重规划会留下多份计划，结局说的是最终那份）；
// 匹配不上（例如模型给的 Plan 没通过 schema，Run 的 planId 来自别处）就退化成
// 最近一份能解出 steps 的原始输出——这时它只是尽力而为的观测，不是判据。
func (o *planObserver) actionsFor(planID string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	var fallback []string
	for i := len(o.raws) - 1; i >= 0; i-- {
		var plan struct {
			PlanID string `json:"planId"`
			Steps  []struct {
				Action string `json:"action"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(o.raws[i], &plan); err != nil {
			continue
		}
		actions := make([]string, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			if action := strings.TrimSpace(step.Action); action != "" {
				actions = append(actions, action)
			}
		}
		if len(actions) == 0 {
			continue
		}
		if planID != "" && plan.PlanID == planID {
			return actions
		}
		if fallback == nil {
			fallback = actions
		}
	}
	if fallback == nil {
		return []string{}
	}
	return fallback
}

// ---------- 轮询 ----------

// moduleEntry 是 GET /tenon/modules 的一行（管理面视图，含 enabled=false）。
type moduleEntry struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Enabled bool   `json:"enabled"`
}

// poller 持有轮询所需的配置与游标。它只在 run 的循环里使用，不并发。
type poller struct {
	cfg     config
	deps    agent.Deps
	client  *http.Client
	cursors *cursorStore
	plans   *planObserver
}

// pollOnce 跑一轮：注册表 → 每个 enabled 模块的发件箱 → 命中 skill 的事件开回合。
// 返回值是本轮开出的回合数。
func (p *poller) pollOnce(ctx context.Context) (int, error) {
	modules, err := p.enabledModules(ctx)
	if err != nil {
		return 0, err
	}
	rounds := 0
	for _, m := range modules {
		events, next, err := p.fetchEvents(ctx, m)
		if err != nil {
			return rounds, err
		}
		for _, raw := range events {
			var event agent.DomainEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return rounds, fmt.Errorf("模块 %q 的事件不是合法的 domain-event：%w", m.ID, err)
			}
			if strings.TrimSpace(p.deps.Skills.TextFor(event.Type)) == "" {
				log.Printf("agent: 事件 %s（%s）没有 skill 订阅，跳过", event.EventID, event.Type)
				continue
			}
			// 每回合清掉上一回合的规划观测：结果行只能报告本回合的计划。
			p.plans.beginRound()
			res := agent.Run(ctx, p.deps, agent.RunInput{Event: event})
			rounds++
			log.Printf("agent: 事件 %s（%s）→ 结局 %s planId=%s 动作=%v 说明=%s",
				event.EventID, event.Type, res.Outcome, res.PlanID, res.ActionsTaken, res.Detail)
			p.writeResult(event, res)
		}
		// 游标在整页处理完之后才前进，并且**逐模块落盘**：后面某个模块读失败时，
		// 前面已经处理过的模块不会因此重放（重放会产出新 planId → 新幂等键 →
		// 同一个业务可能被再执行一次，模块侧的幂等去重挡不住新键）。
		//
		// 事件级不重试：结局（completed / needs_human / failed）已经写进日志与审计，
		// 自动重放同一事件只会得到同一份计划；需要重跑时由人 reset 游标或重发事件。
		p.cursors.set(m.ID, next)
		if err := p.cursors.save(p.cfg.cursorFile); err != nil {
			return rounds, err
		}
	}
	return rounds, nil
}

// enabledModules 读注册表并**按 enabled 过滤**。
//
// 这一步不能交给宿主：GET /tenon/modules 是管理面视图（含已摘除条目），
// 而 agent 的视野必须是「还在场的模块」——否则 D1 的 B 轮会看见被摘除的 procurement。
func (p *poller) enabledModules(ctx context.Context) ([]moduleEntry, error) {
	status, raw, err := p.get(ctx, p.cfg.host+"/tenon/modules")
	if err != nil {
		return nil, fmt.Errorf("GET /tenon/modules 失败：%w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /tenon/modules 返回 %d：%s", status, snippet(raw))
	}
	var resp struct {
		Modules []moduleEntry `json:"modules"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("GET /tenon/modules 的响应不是合法 JSON：%w", err)
	}
	out := make([]moduleEntry, 0, len(resp.Modules))
	for _, m := range resp.Modules {
		if !m.Enabled {
			continue
		}
		if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.BaseURL) == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// fetchEvents 读一个模块发件箱的一页，返回事件原文与该模块的新游标。
func (p *poller) fetchEvents(ctx context.Context, m moduleEntry) ([]json.RawMessage, string, error) {
	base := strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
	target := base + "/tenon/events?direction=out"
	if since := p.cursors.get(m.ID); since != "" {
		target += "&since=" + url.QueryEscape(since)
	}
	status, raw, err := p.get(ctx, target)
	if err != nil {
		return nil, "", fmt.Errorf("模块 %q：GET /tenon/events 失败：%w", m.ID, err)
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("模块 %q：GET /tenon/events 返回 %d：%s", m.ID, status, snippet(raw))
	}
	var resp struct {
		Events     []json.RawMessage `json:"events"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("模块 %q：事件页不是合法 JSON：%w", m.ID, err)
	}
	return resp.Events, resp.NextCursor, nil
}

// get 发一个带协议版本头的 GET 并读出响应体。
func (p *poller) get(ctx context.Context, target string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("构造请求失败（%s）：%w", target, err)
	}
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读取响应体失败：%w", err)
	}
	return resp.StatusCode, body, nil
}

// ---------- 游标 ----------

// cursorStore 是「模块 id → 该模块发件箱游标」的映射。
//
// 游标是不透明字符串（模块侧形如 cur_out_<b64>），agent 不解析它，只原样回传。
//
// 缺省不持久化（只在内存里）：进程重启后会从头发件箱重新读，而重放会重新规划、
// 产出**新的 planId 与新幂等键**——模块侧的幂等去重挡不住新键，同一个业务可能被
// 再执行一次。所以除 D1 的 `--once` 单轮用法外，应当配 TENON_CURSOR_FILE 落盘。
type cursorStore struct {
	path    string
	entries map[string]string
}

// loadCursors 读游标文件。文件不存在 → 空表；文件损坏 → 记一行警告后从空表开始
// （重放是安全的，崩溃比带病继续更贵）。
func loadCursors(path string) (*cursorStore, error) {
	store := &cursorStore{path: path, entries: make(map[string]string)}
	if strings.TrimSpace(path) == "" {
		return store, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, fmt.Errorf("读取游标文件 %q 失败：%w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(raw, &store.entries); err != nil {
		log.Printf("agent: 游标文件 %q 无法解析（从空游标开始，事件会被重放但不重复生效）：%v", path, err)
		store.entries = make(map[string]string)
	}
	return store, nil
}

// get 返回某模块的游标（无则空串，表示从头读）。
func (c *cursorStore) get(moduleID string) string {
	if c == nil {
		return ""
	}
	return c.entries[moduleID]
}

// set 更新某模块的游标；空游标不落地（回显空串等于「从头读」，没必要写盘）。
func (c *cursorStore) set(moduleID, cursor string) {
	if c == nil || moduleID == "" || cursor == "" {
		return
	}
	c.entries[moduleID] = cursor
}

// save 把游标写回磁盘（只在配了路径时写）。
func (c *cursorStore) save(path string) error {
	if c == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("游标编组失败：%w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("写游标文件 %q 失败：%w", path, err)
	}
	return nil
}

// snippet 截取响应体前面一段放进错误信息。
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	runes := []rune(text)
	if len(runes) <= 300 {
		return text
	}
	return string(runes[:300]) + "…"
}
