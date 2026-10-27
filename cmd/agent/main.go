// Command agent 是 Tenon 的 agent 运行时进程（三进程里的 A 框）。
//
// 一个 tick 固定四段，顺序不可调换（spec §6.1，用例 TestPollerTickOrder 钉住）：
//
//	1 入站 drain：拉 GET /tenon/requests/poll，逐帧处理并逐帧 ack；
//	2 自检：把 pending/ 按 I7 分流（审计缺失 → stale、过期 → expired），并清理老归档；
//	3 公告板重推：把 pending/ 里现存的活条目幂等 upsert 回去（read-model 可重建）；
//	4 事件轮询：拉模块发件箱，命中 skill 订阅的事件开回合。
//
// 顺序的理由：先消费人说的话（那是有人正在等结果的路径），再做本地整理，最后才碰
// 事件驱动的回合。反过来做，公告板会在「刚批完还没执行」的中间态推出一张卡，自检也
// 会在下一段的写操作之前拿到过期快照。
//
// 第 4 段是原来的全部工作，每件事都必须能解释「为什么这条事件被处理 / 被跳过」：
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
// 入站与挂起相关的环境变量（P2）：
//
//	TENON_AGENT_STATE_DIR        持久状态根目录，缺省 out/agent-state
//	TENON_AGENT_USER             agent 服务账号，缺省 agent（入站消费要求 role:agent）
//	TENON_AGENT_PASSWORD         该账号口令，缺省 agent-dev（dev 账号表里仅此一个 agent）
//	TENON_PENDING_RETENTION_DAYS 归档件保留天数，缺省 30（超期由自检段清理）
//
// **登录失败即进程启动失败**：入站队列里装的是人说过的话，带着空 token 继续跑等于
// 「看起来在消费、实际什么也读不到」——那比起不来更难查。
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
	"path/filepath"
	"regexp"
	"strconv"
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

	// 入站与挂起（P2）。状态目录与宿主的 TENON_INBOUND_DIR 刻意分开：
	// 前者是「agent 手上还没办完的事」，后者是「人说过、还没被消费的话」。
	envStateDir      = "TENON_AGENT_STATE_DIR"
	envAgentUser     = "TENON_AGENT_USER"
	envAgentPassword = "TENON_AGENT_PASSWORD"
	envRetentionDays = "TENON_PENDING_RETENTION_DAYS"

	// defaultHost 是 interfaces.md §1 给宿主的端口。
	defaultHost = "http://127.0.0.1:8080"
	// defaultSkillDir 与计划约定一致（相对进程工作目录）。
	defaultSkillDir = "skills"
	// defaultAgentID 是审计 actor.id 的缺省值，满足 common.schema.json 的 Actor.id 词法。
	defaultAgentID = "ag_tenon"
	// defaultPollInterval 是缺省轮询间隔。
	defaultPollInterval = time.Second
	// defaultStateDir 是 pending/ 与 processed.jsonl 的缺省根目录（相对进程工作目录）。
	defaultStateDir = "out/agent-state"
	// defaultAgentUser / defaultAgentPass 对应 config/users.dev.json 里的服务账号：
	// 入站 poll/ack 与公告板写入都要求 role:agent，用户账号一律 403。
	defaultAgentUser = "agent"
	defaultAgentPass = "agent-dev"
	// defaultRetainDays 是归档件保留期：归档不删是因为 plan 全文是「当时规划了什么」的
	// 唯一副本（审计只有 planId/stepId/inputHash），但也不能无限长。
	defaultRetainDays = 30

	// ledgerFileName 是去重账本在状态目录下的文件名。
	ledgerFileName = "processed.jsonl"

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

	// 入站与挂起（P2）。
	stateDir   string // pending/、archived/ 与 processed.jsonl 的根目录
	agentUser  string // 入站消费用的服务账号
	agentPass  string
	retainDays int // 归档件保留天数
}

// ledgerPath 是去重账本的**文件**路径（NewLedger 收文件路径，不是目录）。
func (c config) ledgerPath() string {
	return filepath.Join(c.stateDir, ledgerFileName)
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
		stateDir:     firstNonEmpty(os.Getenv(envStateDir), defaultStateDir),
		agentUser:    firstNonEmpty(os.Getenv(envAgentUser), defaultAgentUser),
		agentPass:    firstNonEmpty(os.Getenv(envAgentPassword), defaultAgentPass),
		retainDays:   defaultRetainDays,
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

	// 保留期必须为正数：0 或负数会让自检段把全部归档件删掉，而归档件是
	// 「当时规划了什么」的唯一副本（审计只有 planId/stepId/inputHash）。
	if raw := strings.TrimSpace(os.Getenv(envRetentionDays)); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil {
			return config{}, fmt.Errorf("%s=%q 不是整数天数（例：30）：%w", envRetentionDays, raw, err)
		}
		if days <= 0 {
			return config{}, fmt.Errorf("%s=%q 必须为正数（0 或负数会把归档件全删掉）",
				envRetentionDays, raw)
		}
		cfg.retainDays = days
	}
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

	// 状态目录与去重账本：NewStateStore("") 会报错，缺省值保证它非空；真报错就拒绝启动
	// ——没有状态目录就没有挂起通道，也没有去重，静默降级等于把「重复扣库存」打开。
	store, err := agent.NewStateStore(cfg.stateDir)
	if err != nil {
		return fmt.Errorf("准备状态目录 %q 失败（没有它就没有挂起通道与去重）：%w", cfg.stateDir, err)
	}
	ledger, err := agent.NewLedger(cfg.ledgerPath())
	if err != nil {
		return fmt.Errorf("打开去重账本 %q 失败（没有它就会重复执行同一句请求）：%w",
			cfg.ledgerPath(), err)
	}
	// 只有 NewStateStore 成功时才装配 Deps.State：nil 是「没有挂起通道」的约定，
	// 而不是「空目录」——approve 命中时一律转人工，不会在第一轮挂起时炸掉。
	deps.State = store

	// 入站通道：服务账号换 token。**登录失败即启动失败**（spec §8）：带着空 token
	// 继续跑，入站会静默读不到任何东西，那比起不来更难查。
	hostc := agent.NewHostClient(cfg.host, cfg.agentUser, cfg.agentPass, deps.HTTP)
	if err := hostc.Login(ctx); err != nil {
		return fmt.Errorf("宿主登录失败（%s / %s）：%w", envAgentUser, envAgentPassword, err)
	}

	p := &poller{cfg: cfg, deps: deps, client: deps.HTTP, cursors: cursors, plans: plans,
		hostc: hostc, store: store, ledger: ledger}
	log.Printf("agent: host=%s agent=%s onBehalfOf=%s skill=%d 模型=%s 轮询间隔=%s once=%v",
		cfg.host, cfg.agentID, cfg.onBehalfOf, skills.Len(), cfg.llm.Model, cfg.pollInterval, cfg.once)
	log.Printf("agent: 入站账号=%s 状态目录=%s 账本=%s 归档保留=%d 天",
		cfg.agentUser, cfg.stateDir, cfg.ledgerPath(), cfg.retainDays)
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
//
// requestId 只在「人工发起」的回合上有值：入站重放与去重账本都按它对齐，
// 事件回合没有这个概念（空串省略，与既有的 D1 读法向后兼容）。
type resultLine struct {
	At            string   `json:"at"`
	CorrelationID string   `json:"correlationId"`
	Outcome       string   `json:"outcome"`
	PlanID        string   `json:"planId"`
	Actions       []string `json:"actions"`
	Detail        string   `json:"detail"`
	RequestID     string   `json:"requestId,omitempty"`
}

// writeResult 把一个回合的结局追加成一行 JSON（未配结果文件时什么都不做）。
//
// 写失败只记日志、不改结局：审计与 Intent 帧已经承载了业务结论，一份观测文件的
// 可用性不该决定一笔业务的处置。代价是 D1 会因等不到结果行而失败——那条理由里
// 会带上路径与最后一次观测，排查方向仍然清楚。
func (p *poller) writeResult(event agent.DomainEvent, res agent.RunResult) {
	p.writeResultFor("", event, res)
}

// writeRequestResult 是人工发起回合的结果行：多一个 requestId，其余逐字同形。
//
// requestId 是重放与去重账本的对齐键，排障时「这条结果行对应哪句请求」必须能查。
func (p *poller) writeRequestResult(requestID string, event agent.DomainEvent, res agent.RunResult) {
	p.writeResultFor(requestID, event, res)
}

// writeResultFor 是结果行的唯一写入点。
func (p *poller) writeResultFor(requestID string, event agent.DomainEvent, res agent.RunResult) {
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
		RequestID:     strings.TrimSpace(requestID),
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
//
// **单线程是硬前提**：StateStore.Save 是整文件替换、无锁（§0.1.5），Ledger 也没打算
// 承受多写者。整个进程只有一个 tick 在跑，这条约束就成立。
type poller struct {
	cfg     config
	deps    agent.Deps
	client  *http.Client
	cursors *cursorStore
	plans   *planObserver

	// hostc 是入站通道（登录凭证藏在它里面）；store 是 pending/ 的读写口；
	// ledger 是 requestId 去重账本。三者都由 run 装配，nil 表示「这条通道没装上」。
	hostc  *agent.HostClient
	store  *agent.StateStore
	ledger *agent.Ledger

	// inbound 是入站游标：**只跟着已 ack 的序号前进**（不是跟 poll 的 nextCursor）。
	// 宿主按 seq 回放（Since(after) 只给 > after 的帧），游标一越过没 ack 的帧，
	// 那一帧就再也不会被送到——「下一 tick 重放」就成了假话。
	inbound int64

	// steps 记录本 tick 走过的阶段名；生产为 nil（不收集，零成本）。
	steps *[]string
}

// record 追加阶段名（生产为 no-op）。
func (p *poller) record(name string) {
	if p.steps != nil {
		*p.steps = append(*p.steps, name)
	}
}

// Steps 返回本 tick 记录的阶段名（测试用）。
func (p *poller) Steps() []string {
	if p.steps == nil {
		return nil
	}
	return append([]string(nil), *p.steps...)
}

// pollOnce 跑一个 tick。四段顺序固定（spec §6.1）：入站 drain → 自检/过期清理 →
// 公告板重推 → 事件轮询。返回值是本轮开出的回合数。
//
// 前两段出错**不阻止后两段**：入站/自检/公告板的故障都是「这一段没办成」，不该让
// 事件轮询跟着停——那是把一处的故障放大成整条链停摆。只有第 4 段（原逻辑）的错误
// 会作为 tick 的错误返回。
func (p *poller) pollOnce(ctx context.Context) (int, error) {
	rounds := 0

	// 1. 入站 drain（逐帧 ack）
	n, err := p.drainInbound(ctx)
	rounds += n
	if err != nil {
		log.Printf("agent: 入站处理出错（本 tick 继续跑其余段）：%v", err)
	}

	// 2. 自检与过期清理。Deps.State 为 nil 时**不许**调：SelfCheck 里第一件事就是
	// store.CountCorrupt()，nil store 会解引用崩掉——没挂起通道是合法装配，不是崩溃。
	if p.store == nil {
		log.Printf("agent: 没有挂起通道（StateStore 为 nil）：跳过自检与归档清理")
	} else {
		p.record("selfcheck")
		p.selfCheck(ctx)
		p.pruneArchived()
	}

	// 3. 公告板重推（read-model 可重建：推不上去只记日志，下一 tick 再来）。
	// RepushBoard 自己会报「没有挂起通道」，这里照记一段，让「这一段跑过」与
	// 「这一段有活干」分开。
	p.record("board")
	if pushed, rerr := agent.RepushBoard(ctx, p.hostc, p.store); rerr != nil {
		log.Printf("agent: 公告板重推失败（read-model 可重建，下一 tick 再来）：%v", rerr)
	} else if pushed > 0 {
		log.Printf("agent: 公告板已重推 %d 条", pushed)
	}

	// 4. 事件轮询（原逻辑）
	p.record("events")
	ev, err := p.pollEvents(ctx)
	return rounds + ev, err
}

// selfCheck 是第 2 段：把 pending/ 按 I7 分流并报数。
//
// 日志行必须带 Corrupt：坏 JSON 会被 List() 静默跳过，CountCorrupt 是它唯一的出口。
func (p *poller) selfCheck(ctx context.Context) {
	probe := agent.HostAuditProbe(p.hostc)
	rep, err := agent.SelfCheck(ctx, p.store, probe)
	if err != nil {
		log.Printf("agent: 自检失败（下一 tick 再来）：%v", err)
		return
	}
	if rep.Kept+rep.Expired+rep.Stale+rep.Settled+rep.Corrupt > 0 {
		log.Printf("agent: 自检 kept=%d expired=%d stale=%d settled=%d corrupt=%d",
			rep.Kept, rep.Expired, rep.Stale, rep.Settled, rep.Corrupt)
	}
}

// pruneArchived 清掉超过保留期的归档件。保留期的意义是「归档不删，但不能无限长」：
// plan 全文是「当时规划了什么」的唯一副本。
func (p *poller) pruneArchived() {
	retain := time.Duration(p.cfg.retainDays) * 24 * time.Hour
	removed, err := p.store.PruneArchived(time.Now().UTC(), retain)
	if err != nil {
		log.Printf("agent: 清理过期归档件失败（不影响本 tick）：%v", err)
		return
	}
	if removed > 0 {
		log.Printf("agent: 已清理 %d 份超过 %d 天保留期的归档件", removed, p.cfg.retainDays)
	}
}

// pollEvents 是第 4 段：注册表 → 每个 enabled 模块的发件箱 → 命中 skill 的事件开回合。
// 返回值是本段开出的回合数。
func (p *poller) pollEvents(ctx context.Context) (int, error) {
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

// ---------- 入站 drain ----------

const (
	// kindRequest / kindConfirm 是入站只接受的两种帧（frames.schema.json 的判别值）。
	kindRequest = "tenon.request"
	kindConfirm = "tenon.confirm"
)

// drainInbound 是 tick 的第 1 段：拉入站帧、逐帧处理、**逐帧** ack。
//
// ack 的三条分支写死在这里，别合并：
//
//	处理成功（含幂等跳过）      → ack
//	确定性拒绝（重放不会变）    → ack + 日志（留着只会每 tick 刷一次同样的错）
//	可重试（这一次没办成）      → **不 ack**，下一 tick 重放
//
// 「不 ack」要真的能重放，游标就必须只跟着已 ack 的序号走：宿主按 Since(after)
// 回放，游标一旦越过没 ack 的帧，那一帧再也不会被送到。
func (p *poller) drainInbound(ctx context.Context) (int, error) {
	p.record("poll")
	if p.hostc == nil || p.ledger == nil {
		// 装配缺件是配置问题，不是「今天没有入站」：必须报出来，不能静默当成空队列。
		return 0, fmt.Errorf("入站通道未装配（HostClient=%v Ledger=%v），本 tick 不消费入站",
			p.hostc != nil, p.ledger != nil)
	}
	frames, _, err := p.hostc.PollInbound(ctx, p.inbound)
	if err != nil {
		return 0, err
	}
	// 第二个返回值（nextCursor）刻意不用于推进游标：p.inbound 只跟着已 ack 的序号走，
	// 否则没 ack 的帧会被「下一次 poll 从 nextCursor 之后开始」永久跳过。

	started := 0
	for _, f := range frames {
		var probe struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			PlanID    string `json:"planId"`
		}
		if err := json.Unmarshal(f.Frame, &probe); err != nil {
			// 帧体都读不出来：重放一万次还是读不出来，ack 掉并留日志。
			log.Printf("agent: 入站帧 %d 不可解析，跳过（重放也不会变）：%v", f.Seq, err)
		} else {
			began, herr := p.handleInbound(ctx, probe.Type, f.Frame)
			switch {
			case herr == nil:
				started += b2i(began)
			case !agent.IsRetryable(herr):
				// 确定性拒绝：角色不匹配、已过期、未知 planId、未知处置……重放是同一个结论。
				log.Printf("agent: 入站帧 %d（%s）确定性拒绝，已 ack 掉：%v", f.Seq, probe.Type, herr)
			default:
				// 可重试：审计/策略/授权/能力发现/归档写失败。pending 与审计都还在，
				// 下一 tick 重放是正确动作——**不 ack、也不推进游标**。
				log.Printf("agent: 入站帧 %d（%s）可重试错误，不 ack（下一 tick 重放）：%v",
					f.Seq, probe.Type, herr)
				continue
			}
		}
		if aerr := p.hostc.AckInbound(ctx, f.Seq); aerr != nil {
			// ack 失败就不能推进游标：否则这一帧既不会被重放、也不算处理完。
			return started, fmt.Errorf("ack 帧 %d 失败（游标停在 %d）：%w", f.Seq, p.inbound, aerr)
		}
		p.inbound = f.Seq
		p.record("ack")
	}
	return started, nil
}

// handleInbound 分派一帧，返回第二值表示「本帧开了一个回合」。
func (p *poller) handleInbound(ctx context.Context, typ string, raw []byte) (bool, error) {
	switch typ {
	case kindRequest:
		return p.handleRequest(ctx, raw)
	case kindConfirm:
		return false, p.handleConfirm(ctx, raw)
	default:
		// 不认识的帧类型是宿主/客户端的契约问题：重放无解，日志留痕后 ack 掉。
		// 返回 nil 让调用方 ack；「不是可重试错误」不等于「处理成功了」，所以上面
		// 那行日志必须写明跳过原因（帧类型只可能是 request/confirm）。
		log.Printf("agent: 入站帧类型 %q 不认识（只认 %s / %s），跳过并 ack", typ, kindRequest, kindConfirm)
		return false, nil
	}
}

// handleRequest 处理一条人工请求：去重 → 开回合 → 记账 → 结果行。
func (p *poller) handleRequest(ctx context.Context, raw []byte) (bool, error) {
	var req struct {
		RequestID        string `json:"requestId"`
		SyntheticEventID string `json:"syntheticEventId"`
		Text             string `json:"text"`
		By               string `json:"by"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return false, fmt.Errorf("tenon.request 不是合法 JSON：%w", err)
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		// **必须在这里挡掉**：Claim("") 恒返回 (true, nil) 且不写盘，等于静默关掉去重
		// ——之后同一句话每重放一次就重跑一次回合（重复扣库存）。不 ack：让宿主侧
		// 的报文问题暴露出来，而不是把一条不可去重的请求当成已处理。
		return false, fmt.Errorf("tenon.request 缺少 requestId：空 requestId 会让去重静默失效，不处理")
	}
	if strings.TrimSpace(req.SyntheticEventID) == "" {
		// 宿主对每条 request 都会签发 syntheticEventId（interfaces.md §4.7）。缺失时
		// origin 登记表里就没有这枚键，策略的 origin=request 判定会退化成 event
		// ——「对话发起必须审批」会静默减弱。仍然处理（不丢弃人的话），但必须喊出来。
		log.Printf("agent: 警告 —— request %s 的帧里没有 syntheticEventId：origin 会被宿主判成 event，"+
			"依赖 origin=request 的策略（如采购单审批）会失效", requestID)
	}

	ok, err := p.ledger.Claim(requestID)
	if err != nil {
		return false, fmt.Errorf("账本 claim %s 失败（不处理，等下一 tick）：%w", requestID, err)
	}
	if !ok {
		return false, p.skipClaimedRequest(ctx, requestID)
	}

	event := agent.DomainEvent{
		EventID: syntheticEventID(req.SyntheticEventID, requestID),
		Type:    kindRequest,
		// 业务链锚点必须是合法 cor: 值（common.schema.json#/$defs/CorrelationId 的
		// ^cor:\S+$）：原先直接把 by（user:alice）当锚点，落进审计 trigger.correlationId
		// 就是一条不合规记录。合成事件没有自带锚点，所以由 requestId 确定性派生——同一句
		// 请求重放后必须仍落在同一条业务链上（不能用时间基的 newID）。
		CorrelationID: agent.RequestCorrelationID(requestID),
	}
	// by 是宿主用 token 的 sub 覆写过的**可信身份**（客户端自称一律不认）。每回合改
	// **副本**：改 p.deps 会让下一帧继承上一个人的委托关系。
	by := strings.TrimSpace(req.By)
	deps := p.deps
	if by != "" {
		deps.OnBehalfOf = by
	}
	p.plans.beginRound()
	res := agent.Run(ctx, deps, agent.RunInput{
		Event: event,
		// 与 Event 同一个锚点：run.go 会用 RunInput.CorrelationID 覆盖进 plan.Trigger，
		// 两处不一致会让审计与计划挂在不同业务链上。
		CorrelationID: event.CorrelationID,
		Request:       &agent.UserRequest{RequestID: requestID, Text: req.Text, By: by},
	})
	p.finishRequest(requestID, event, res)
	return true, nil
}

// finishRequest 收尾一个 request 回合：Done 覆盖所有返回路径（含 needs_human 挂起——
// 挂起也是一次完整的处理），并且**排在可能失败的调用之前**。
//
// 顺序的理由：Done 失败意味着「账本还停在 claimed」，下一 tick 会按「上次中断」处理
// 并丢弃这句请求（宁可丢一句，也不重复扣库存）。结果行只是观测，丢了不影响业务，
// 所以它排在 Done 之后。
func (p *poller) finishRequest(requestID string, event agent.DomainEvent, res agent.RunResult) {
	doneErr := p.ledger.Done(requestID, res.PlanID, string(res.Outcome))
	log.Printf("agent: request %s → 结局 %s planId=%s 动作=%v 说明=%s",
		requestID, res.Outcome, firstNonEmpty(res.PlanID, "-"), res.ActionsTaken, res.Detail)
	p.writeRequestResult(requestID, event, res)
	if doneErr != nil {
		log.Printf("agent: 账本 done %s 写失败（账本仍在 claimed，重放会按「上次中断」丢弃这句请求）：%v",
			requestID, doneErr)
	}
}

// skipClaimedRequest 处理「账本里已经有这个 requestId」的跳过路径，按 phase 分流。
//
// 这是整个入站路径里**唯一会主动丢弃用户输入的地方**，所以 claimed 分支必须让人看见：
// 显式告警 + 推一条「请重新发起」的公告板条目。done 分支才是普通跳过（同一条请求
// 已经处理完，重放不重跑）。
func (p *poller) skipClaimedRequest(ctx context.Context, requestID string) error {
	phase, err := p.phaseOf(requestID)
	if err != nil {
		// 判不出阶段就说不清该不该丢：不 ack（可重试），等下一 tick 磁盘恢复。
		return fmt.Errorf("账本里 %s 的 phase 读不出来（不猜，等下一 tick）：%w", requestID, err)
	}
	if phase == "done" {
		log.Printf("agent: request %s 已处理过（done），跳过（去重）", requestID)
		return nil
	}
	// phase == claimed，或账本里查不到这一行（文件被截断/半行）：**按 claimed 处理**。
	// 这是会丢用户输入的分支，宁可多喊一声也不要静默跳过。
	//
	// 上次处理中断（进程崩了 / 回合没写 done）：宁可丢这一句，也不重复扣库存——
	// 但因为丢的是人的话，必须显式告警 + 推一条「请重新发起」的公告板条目。
	log.Printf("agent: request %s 上次处理中断、未完成（phase=%s），按去重策略跳过（这是一句被舍弃的用户请求）",
		requestID, firstNonEmpty(phase, "?"))
	err = p.hostc.PushPending(ctx, map[string]any{
		"planId":  "req-" + requestID,
		"summary": "该请求处理中断，请重新发起",
		"items":   []map[string]any{{"label": "上次处理中断，请重新发起"}},
		// requiredRoles 留空：这条不是待批件，没有谁能「批」它——它是通知。
		"requiredRoles": []string{},
		"expiresAt":     time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
		"state":         "aborted",
	})
	if err != nil {
		// 推不上去 = 用户看不见自己的请求被丢了：不 ack，让下一 tick 再推一次
		// （PushPending 是幂等 upsert，重推安全）。
		return fmt.Errorf("推「请重新发起」公告板失败：%w", err)
	}
	return nil
}

// handleConfirm 处理一条确认帧：恢复执行，成功后推终态覆盖。
//
// 审批人角色只从**帧里的 roles** 取：那一份是宿主用提交者 token 的 claims.Roles 覆写过的
// （见 internal/controlplane 的 normalizeInboundFrame——它先删掉客户端自报的 roles 再写入）。
// 这里不读 agent 自己的登录 claims：那是服务账号的 role:agent，拿它当审批人角色就是
// 权限混淆（一份要求 role:agent 的计划会对任何人放行）。帧里没有 roles 就是空集，
// Resume 的 rolesIntersect 对空集 fail-closed → approve 403，reject 不受影响。
//
// **别再给这句话加兜底**（原来的 approverRoles 就是这么没的）：读登录 claims、给默认角色、
// 信客户端自报的 roles，三条路都是把人批过的事实降级成「谁都能批」。
func (p *poller) handleConfirm(ctx context.Context, raw []byte) error {
	var cf struct {
		ConfirmID string   `json:"confirmId"`
		PlanID    string   `json:"planId"`
		Decision  string   `json:"decision"`
		By        string   `json:"by"`
		Roles     []string `json:"roles"`
		At        string   `json:"at"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		return fmt.Errorf("tenon.confirm 不是合法 JSON：%w", err)
	}
	err := agent.Resume(ctx, p.deps, p.store, agent.Confirm{
		ConfirmID: cf.ConfirmID,
		PlanID:    cf.PlanID,
		Decision:  cf.Decision,
		By:        cf.By,
		Roles:     cf.Roles,
		At:        cf.At,
	})
	if err != nil {
		// 分类交给调用方（drain 用 agent.IsRetryable 决定 ack 还是重放）：这里只补上下文。
		return err
	}

	// 终态覆盖（spec §3.4）：Resume 返回 nil 表示**归档已落盘**，此时按 planId 取回归档件
	// 才能拿到真实终态——reject 也返回 nil，写死 done 会把一张被驳回的卡标成已执行。
	archived, aerr := p.store.LoadArchived(cf.PlanID)
	if aerr != nil {
		// 取不到归档件说明盘上状态与 Resume 的结论不一致：记日志、不回滚、也不猜终态
		// （重放时归档件仍在，再推一次是幂等 upsert）。
		log.Printf("agent: planId %s 已处理但取不到归档件，跳过终态覆盖：%v", cf.PlanID, aerr)
		return nil
	}
	if perr := agent.PushTerminal(ctx, p.hostc, archived, archived.State); perr != nil {
		// **绝不回滚归档**：回滚会让同一份 confirm 被重放去执行第二次。
		log.Printf("agent: planId %s 的终态覆盖（%s）推送失败（归档不回滚，重放时再推是幂等 upsert）：%v",
			archived.PlanID, archived.State, perr)
	}
	return nil
}

// phaseOf 回答「这个 requestId 在账本里到了哪个阶段」（""=没出现过，claimed=处理中断，
// done=已处理）。
//
// internal/agent 的 Ledger 没有导出只读查询，而这一层的纪律是**不碰 internal/agent**
// （计划 §Task 10 允许给 Ledger 加 Phase，但本切片的约束是不改包内代码），所以这里直接
// 读账本文件：它是 append-only 的 JSONL，同一 requestId 以**最后一行**为准——与
// NewLedger 载入时的覆盖顺序一致。只在去重命中时读一次，不在热路径上。
func (p *poller) phaseOf(requestID string) (string, error) {
	raw, err := os.ReadFile(p.cfg.ledgerPath())
	if err != nil {
		if os.IsNotExist(err) {
			// 文件没了就说不清阶段（账本本该在 claim 时就创建）：不猜，让调用方重试。
			return "", fmt.Errorf("账本文件 %q 不见了", p.cfg.ledgerPath())
		}
		return "", err
	}
	phase := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			RequestID string `json:"requestId"`
			Phase     string `json:"phase"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // 半行跳过：与 NewLedger 的容忍度一致
		}
		if entry.RequestID == requestID {
			phase = entry.Phase
		}
	}
	return phase, nil
}

// syntheticEventID 给人工请求合成一个**确定性**的事件 id。
//
// 优先用宿主签发的 syntheticEventId：它是宿主 origin 登记表的键（策略的 origin=request
// 判定靠它），也是重放时唯一稳定的锚点。帧里没有时退化成 evt_req_<requestId>——
// 同样确定，但**绝不**用时间基的 newID()：那样重放后的 triggerEventId 会漂移，
// 审计对账与「同一输入同一行为」都对不上。
func syntheticEventID(fromFrame, requestID string) string {
	return firstNonEmpty(strings.TrimSpace(fromFrame), "evt_req_"+requestID)
}

// b2i 把布尔转成计数（回合数只在「确实开了回合」时加一）。
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
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
