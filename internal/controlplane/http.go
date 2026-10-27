// Package controlplane 把控制平面的四块引擎（策略、授权、审计链、注册表）与共驾帧
// 队列组装成 cmd/host 的一个 http.Handler。
//
// 依据：docs/superpowers/specs/2026-10-27-tenon-m05-host-agent-design.md §2.1（端点清单）
// 与 docs/interfaces.md §4（宿主侧接口）。子包各自负责语义，本包只负责「接起来」：
// 读请求 → 调引擎 → 写响应。它不含业务判断，也不主动调用任何模块。
//
// 端点：
//
//	POST /tenon/policies:evaluate          逐步骤求值整份 plan，取最严处置
//	POST /tenon/grants:evaluate            单动作授权评估
//	POST /tenon/audit                      追加一条审计记录（两段写的写入口）
//	GET  /tenon/audit?planId=&since=&limit= 审计查询（游标是写入顺序序号）
//	GET  /tenon/modules                    注册表（管理面视图，含已摘除条目）
//	GET  /healthz                          存活探针（不检查依赖）
//	GET  /readyz                           就绪探针（审计可写 + 策略可用）
//	POST /tenon/intents                    共驾帧入队
//	GET  /tenon/intents/poll?after=        按序回放未确认的帧
//	POST /tenon/intents/ack                确认到某个序号，此后不再回放
//	POST /tenon/admin/modules              登记/摘除模块（仅 TENON_TEST_MODE=1）
//	GET  /                                 同源静态页（P1 只做路由，页面内容在 P3）
//	GET  /ui/…                             静态页引用的资源（只读 TENON_UI_DIR 之内）
//
// 关于 WebSocket：0.1 把 /tenon/intents 写成 WS 端点，但 Go 标准库没有 WS 服务端，
// 为 M0.5 引一个第三方依赖不值得。本包**不做协议升级**：GET /tenon/intents 一律 405，
// 功能由上面三个 REST 端点等价提供（design §2.1 的「+ REST 降级」）。WS 升级留 M1，
// 届时与 REST 端点并存，帧格式不变。
//
// 规范未写死、由本包定下的几处判断（显式记在这里，便于审计）：
//   - **整份 plan 取最严**：policy 引擎的求值粒度是单个动作（见 policy 包注释），所以
//     这里逐 step 求值，再按 deny > approve > auto 合并；与最终处置同级的 step 其
//     decidedBy/approvers/reasons 取并集并去重。空 plan 或 steps 为空不进入循环，直接
//     deny + NO_POLICY_MATCH_DEFAULT_DENY——零步计划没有可执行语义，放它过闸门等于给
//     「什么都不做」发通行证；
//   - **求值出错一律 fail-closed**：policies 回 200 + deny + POLICY_EVALUATION_FAILED
//     （semantics.md §12：策略不可用默认 deny，不是 auto）；grants 的拒绝原因被 schema
//     锁死为四态，装不下引擎级错误（窗口/单位不受支持），所以回 503 retryable——两条
//     路径都不会让写操作通过；
//   - **授权评估的业务量**：grant.Evaluate 的 amount 由调用方给，而 host 在 M0.5 不做
//     能力发现（拿不到动作声明的 quotaUnit/quotaField），因此按 args.qty 取；没有 qty
//     时按 1 计（宁可多占配额，也不给「无法计量」留一个免费口子）；
//   - **协议版本头**：host 端点全部宽松处理（缺失视为 0.1，存在且不是 0.1 则 400），
//     响应一律带 Tenon-Version/Tenon-Min-Version。本轮只实现 0.1，不做版本协商；
//   - **GET /tenon/modules 返回管理面视图**（registry.List，含 enabled=false 的条目）：
//     D1 的 B 轮要能看见「被摘除」这件事。调用方（agent）按 enabled 过滤后再做能力发现；
//   - **读动作必须逐个登记**：G2 闸门对未命中的动作一律 deny（fail-closed），所以
//     config/policies.dev.json 显式给 inventory.GetStock、quote.GetQuote 各写了一条
//     outcome=auto 的低优先级规则。这里**刻意不做**「kind=read 即自动放行」的特例：
//     M0.5 的 host 拿不到 action 定义，无法独立核实调用方自称的 kind，那种特例等于给
//     「自称只读就放行」开一个洞。代价是接入方必须逐个登记——这正是 fail-closed 该有的
//     样子；漏登记一个动作的后果是该动作所在的整份 plan 被判 deny（闸门上的响失败），
//     不是静默放行。M1 引入能力发现（GET /.well-known/tenon）后，host 才能自己判定
//     kind 并据此放宽这条口子；
//   - **共驾帧队列是有界内存队列**（maxIntentFrames）。前端不接入时帧本就该丢弃
//     （design §3.2 决定 1），溢出时丢最旧的，并在回放响应里给出累计 dropped；
//   - **审计 GET 的 since 是写入顺序的 0 基序号**，不是过滤后的序号：只有它在
//     append-only 日志上稳定。nextCursor 给出下一页应当传入的 since。
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tenon.dev/internal/controlplane/audit"
	"tenon.dev/internal/controlplane/grant"
	"tenon.dev/internal/controlplane/policy"
	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/protocol"
)

const (
	// maxBodyBytes 是请求体上限：宿主只收评估请求、审计记录与共驾帧，
	// 1 MiB 远超实际需要。
	maxBodyBytes = 1 << 20
	// defaultAuditPage 是审计查询缺省分页大小。
	defaultAuditPage = 100
	// maxAuditPage 是审计查询分页上限。
	maxAuditPage = 500
	// maxIntentFrames 是共驾帧队列容量。
	maxIntentFrames = 1024
	// selfCheckAction 是启动/就绪自检用的动作 id。它不是任何模块的动作，必定不命中
	// 任何规则，所以求值没有副作用；而引擎会把「规则配置坏」的错误留到首次求值才
	// 暴露，宿主必须在启动时就用一次必然发生的求值把它问出来。
	selfCheckAction = "tenon.SelfCheck"
	// reasonEvaluationFailed 是求值出错时的理由码：大写蛇形，符合
	// policy.schema.json 对 reasons 的词法约束（^[A-Z][A-Z0-9_]*$）。
	reasonEvaluationFailed = "POLICY_EVALUATION_FAILED"
	// reasonMissingTrigger 是开启了 TENON_REQUIRE_TRIGGER 后请求缺 triggerEventId 的
	// 理由码。与 POLICY_EVALUATION_FAILED 分开：这是调用方没给来源凭证，不是规则坏了。
	reasonMissingTrigger = "MISSING_TRIGGER"
	// originRequest / originEvent 是两条来源轴取值，进 policy.PolicyRequest.Origin。
	// 判定权在宿主（origin 登记表），调用方自报的 origin 一律忽略。
	originRequest = "request"
	originEvent   = "event"
	// quotaField 是授权评估从 args 里取业务量的字段名（见包注释的取舍）。
	quotaField = "qty"
	// 三种共驾帧的判别值，见 frames.schema.json。
	frameTypeIntent  = "tenon.intent"
	frameTypeContext = "tenon.context"
	frameTypeConfirm = "tenon.confirm"
	// sessionTTL 是 dev token 有效期。
	sessionTTL = 8 * time.Hour
	// defaultInboundDir 是入站队列与 origin 登记表的缺省目录。
	defaultInboundDir = "out/inbound"
	// maxInboundFrames 是入站队列容量。**独立于 maxIntentFrames**：后者是出站内存队列
	// 的容量语义，给「人说过的话」复用它，会让两件事的理由互相矛盾。
	maxInboundFrames = 1024
)

// Config 是宿主控制平面的一次装配输入。
//
// Policies/Grants 为空表示「全部拒绝」，这是 fail-closed 的空配置，不是错误；
// 但 SessionSecret 必填：它没有安全的缺省值，猜一个等于给自己发通行证。
type Config struct {
	// Policies 是策略规则；同 outcome 时按 priority 与顺序裁决（见 policy 包）。
	Policies []policy.PolicyRule
	// Grants 是授权表，Subjects 是 onBehalfOf 的权限表——交集规则的下半部分。
	Grants []grant.Grant
	// Subjects 形如 {"user:alice": ["inventory.DeductStock"]}；为空表示没有委托人有权。
	Subjects map[string][]string
	// AuditPath 是审计 JSONL 路径，必填：审计链是宿主唯一不可省的持久状态。
	AuditPath string
	// Modules 是启动时登记的模块（TENON_MODULES 的解析结果）。
	Modules []registry.ModuleEntry
	// TestMode 为 true 时挂载 POST /tenon/admin/modules。
	TestMode bool
	// Users 是 dev 账号表；为空表示登录端点永远失败（fail-closed）。
	Users []User
	// SessionSecret 用于签发/校验 token；为空会在 New 里报错。
	SessionSecret []byte
	// InboundDir 是入站队列与 origin 登记表的目录。空则用 out/inbound。
	InboundDir string
	// UIDir 是同源静态页（`GET /` 与 `/ui/…`）的根目录。空表示不提供页面：
	// 此时 `GET /` 回 503 并说明原因，而不是 404（「页面没配」是部署问题）。
	UIDir string
	// RequireTrigger 为 true 时，policies:evaluate 缺少 triggerEventId 即 deny。
	// P1 的过渡开关（默认 false：缺 triggerEventId 视为事件驱动，保证现有 agent 可用），
	// P4 收口时把缺省行为改成强制。见 spec §3.11 的「分阶段启用」。
	RequireTrigger bool
}

// Server 是组装好的控制平面。构造后并发安全；Handler() 与服务共享同一份状态。
type Server struct {
	policyEngine  *policy.PolicyEngine
	grantEngine   *grant.GrantEngine
	auditStore    *audit.Store
	auditPath     string
	modules       *registry.Registry
	testMode      bool
	intents       Queue
	users         []User
	sessionSecret []byte
	// inbound 是落盘的入站队列（人 → agent）。它与 intents 是两条独立队列：出站的 Ack
	// 是累积游标，两个消费者共用一条会互相吞帧。
	inbound     Queue
	origin      *originStore
	policyCount int
	grantCount  int
	// requireTrigger 为 true 时缺 triggerEventId 直接 deny（TENON_REQUIRE_TRIGGER=1）。
	// 从 Config.RequireTrigger 装配，装配后不可变。
	requireTrigger bool
	// uiDir 是同源静态页的根目录（Config.UIDir）；空表示不提供页面。
	uiDir string
	// pending 是待批公告板：只读 read-model，agent 推、前端看。刻意不落盘（可重建）。
	pending *pendingBoard
}

// New 装配控制平面。审计日志打不开或策略规则自检不过都会返回错误：宿主不该带着
// 一份读不懂的规则表上线，然后让第一次闸门以 deny 的形式把配置错误伪装成业务拒绝。
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.AuditPath) == "" {
		return nil, errors.New("controlplane: 审计日志路径不能为空")
	}
	store, err := audit.NewAuditStore(cfg.AuditPath)
	if err != nil {
		return nil, fmt.Errorf("controlplane: %w", err)
	}
	s := &Server{
		policyEngine: policy.NewPolicyEngine(cfg.Policies),
		// WithSubjects 显式装配（传空表也一样）：交集规则要求「委托人有权」，
		// 漏装配等于所有调用 SUBJECT_NOT_PERMITTED，与装配了空表等价，这里不留歧义。
		grantEngine: grant.NewGrantEngine(cfg.Grants, grant.WithSubjects(cfg.Subjects)),
		auditStore:  store,
		auditPath:   cfg.AuditPath,
		modules:     registry.NewRegistry(),
		testMode:    cfg.TestMode,
		intents:     newMemoryQueue(maxIntentFrames),
		policyCount: len(cfg.Policies),
		grantCount:  len(cfg.Grants),
		// 过渡开关：默认 false，缺 triggerEventId 视为事件驱动（现有 agent 可用）。
		requireTrigger: cfg.RequireTrigger,
		uiDir:          cfg.UIDir,
		// 公告板是纯内存 read-model：可丢、可重建，重建靠 agent 每 tick 的幂等 upsert。
		pending: newPendingBoard(),
	}
	for _, entry := range cfg.Modules {
		s.modules.Upsert(entry)
	}
	// 会话密钥没有安全缺省值：空密钥签出的 token 任何人都能伪造，宁可拒绝启动。
	// 关掉审计句柄再返回，否则调用方拿不到 Server，文件却已经被这个失败的构造打开。
	if len(cfg.SessionSecret) == 0 {
		_ = store.Close()
		return nil, errors.New("controlplane: 会话密钥不能为空（TENON_SESSION_SECRET 或 dev 缺省值）")
	}
	s.users = cfg.Users
	s.sessionSecret = cfg.SessionSecret
	// 入站队列与 origin 登记表都落盘：入站承载「人说过的话」，丢了不会有人报错。
	// 装配放在会话密钥校验之后、策略自检之前：前者的失败是配置错误，不该先建目录。
	dir := cfg.InboundDir
	if strings.TrimSpace(dir) == "" {
		dir = defaultInboundDir
	}
	inboundQueue, err := newJSONLQueue(filepath.Join(dir, "requests.jsonl"), maxInboundFrames)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	origin, err := newOriginStore(filepath.Join(dir, "requests-index.jsonl"))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	s.inbound, s.origin = inboundQueue, origin
	if _, err := s.policyEngine.Evaluate(context.Background(), policy.PolicyRequest{Action: selfCheckAction}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("controlplane: 策略规则自检未通过：%w", err)
	}
	return s, nil
}

// Handler 返回控制平面的处理器。
//
// 处理器与 Server 共享同一份状态（审计存储、注册表、帧队列），因此一个 Server 应当
// 只创建一个监听器；测试里用 httptest.NewServer(s.Handler()) 即可。
func (s *Server) Handler() http.Handler { return s }

// Close 关闭审计文件与入站队列的句柄。
//
// 进程退出前必须调用：Windows 上不关句柄会锁住审计文件，下一次启动的链校验与重开
// 都会失败。Close 之后 Query/VerifyChain 仍可读（见 audit 包）。
func (s *Server) Close() error {
	err := s.auditStore.Close()
	if s.inbound != nil {
		if cerr := s.inbound.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// VerifyChain 重算整条审计哈希链，供测试与运维自检使用。
func (s *Server) VerifyChain() error { return s.auditStore.VerifyChain() }

// Registry 返回模块注册表，供调用方读取 Enabled()（agent 的能力发现就靠它）。
func (s *Server) Registry() *registry.Registry { return s.modules }

// ServeHTTP 按路径分派。每个处理函数自己判定方法，好让「路径存在但方法不对」回 405
// 而不是 404。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 静态页与静态资源放在 switch 之前：`/ui/` 之下路径不固定，枚举不出来。
	if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, uiPrefix) {
		s.serveStatic(w, r)
		return
	}
	switch r.URL.Path {
	case "/healthz":
		s.serveHealth(w, r)
	case "/readyz":
		s.serveReady(w, r)
	case "/tenon/policies:evaluate":
		s.servePoliciesEvaluate(w, r)
	case "/tenon/grants:evaluate":
		s.serveGrantsEvaluate(w, r)
	case "/tenon/session":
		s.serveSession(w, r)
	case "/tenon/audit":
		s.serveAudit(w, r)
	case "/tenon/modules":
		s.serveModules(w, r)
	case "/tenon/intents":
		// WS 升级留 M1（见包注释）：这里只接受 REST 入队，GET 一律 405。
		s.serveIntentEnqueue(w, r)
	case "/tenon/intents/poll":
		s.serveIntentPoll(w, r)
	case "/tenon/intents/ack":
		s.serveIntentAck(w, r)
	case "/tenon/requests":
		// 入站（人 → agent）。与 /tenon/intents 是两条队列：见 Server.inbound 的注释。
		s.serveInboundEnqueue(w, r)
	case "/tenon/requests/poll":
		s.serveInboundPoll(w, r)
	case "/tenon/requests/ack":
		s.serveInboundAck(w, r)
	case "/tenon/pending":
		// 待批公告板：agent 推摘要、前端按角色取列表（只读 read-model）。
		s.servePending(w, r)
	case "/tenon/admin/modules":
		if !s.testMode {
			// 非测试模式回 404 而不是 403：403 等于承认「这里本来有个测试面」。
			writeTenonErrorStatus(w, http.StatusNotFound, unknownEndpoint(r.URL.Path))
			return
		}
		s.serveAdminModules(w, r)
	default:
		writeTenonErrorStatus(w, http.StatusNotFound, unknownEndpoint(r.URL.Path))
	}
}

// serveHealth 是存活探针：不检查任何依赖，依赖挂了也返回 200，
// 否则编排会把「依赖故障」误判成「进程死了」并重启它（interfaces.md §7）。
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// serveReady 是就绪探针：审计可写 + 策略可用才算能接评估请求。
//
// 「授权已加载」没有独立的失败路径（grant.NewGrantEngine 不会失败），所以加载条数
// 直接写在响应里：条数为 0 说明授权表是空的（合法但会让所有调用被拒），运维一眼能看出来。
func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	missing := make([]string, 0, 2)
	details := make([]string, 0, 2)
	if err := s.auditWritable(); err != nil {
		missing = append(missing, "audit_file")
		details = append(details, "audit_file: "+err.Error())
	}
	if _, err := s.policyEngine.Evaluate(context.Background(), policy.PolicyRequest{Action: selfCheckAction}); err != nil {
		missing = append(missing, "policy_engine")
		details = append(details, "policy_engine: "+err.Error())
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready":   false,
			"missing": missing,
			"details": details,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":    true,
		"checks":   []string{"audit_file", "policy_engine"},
		"policies": s.policyCount,
		"grants":   s.grantCount,
		"modules":  len(s.modules.List()),
	})
}

// auditWritable 是就绪探针的一部分：能创建并追加才算审计可用。
func (s *Server) auditWritable() error {
	f, err := os.OpenFile(s.auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// ---------- 策略评估 ----------

// policyEvaluateRequest 是 policies:evaluate 的请求体（interfaces.md §4.2）。
type policyEvaluateRequest struct {
	Plan json.RawMessage `json:"plan"`
	// TriggerEventID 是本次计划的触发事件 id；宿主拿它查 origin 登记表判定来源。
	// 请求体里若出现 origin 字段，json 解码时被忽略——来源必须由宿主判定，agent 不得自报。
	TriggerEventID string `json:"triggerEventId,omitempty"`
}

// planView 是 plan.schema.json 里宿主做策略评估需要的子集：只取 steps 的动作与入参。
//
// 兄弟字段（planId/trigger/dryRun/…）不参与 G2 闸门，按「未知字段忽略」原则跳过。
// §4.7 的「自动升审批」也不在这里做：它要的是步骤级信息（compensation、预演结论、
// 幂等声明），属 agent 执行循环的判定，宿主不猜。
type planView struct {
	PlanID string     `json:"planId"`
	Steps  []planStep `json:"steps"`
}

// planStep 是 plan.schema.json#/$defs/PlanStep 的求值子集。
type planStep struct {
	StepID string          `json:"stepId"`
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args"`
}

// servePoliciesEvaluate 对整份 plan 求值：逐 step 评估后取最严。
func (s *Server) servePoliciesEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req policyEvaluateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {\"plan\": {...}}："+err.Error()))
		return
	}
	plan, perr := decodePlan(req.Plan)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	// origin 由宿主判定：登记表里有这枚事件 id 就是人发起的 request，否则是事件驱动。
	// 缺 triggerEventId 时的缺省值是 event（过渡期保持现有 agent 可用）；
	// RequireTrigger 为真才 deny——开关方向不能反过来，否则 P1 上线即停摆整个 agent。
	origin := originEvent
	if strings.TrimSpace(req.TriggerEventID) == "" {
		if s.requireTrigger {
			writeJSON(w, http.StatusOK, policy.PolicyEvaluation{
				Outcome:   policy.OutcomeDeny,
				DecidedBy: []string{},
				Reasons:   []string{reasonMissingTrigger},
			})
			return
		}
	} else if s.origin.IsRequest(req.TriggerEventID) {
		origin = originRequest
	}
	writeJSON(w, http.StatusOK, s.evaluatePlan(r.Context(), plan, origin))
}

// decodePlan 把 plan 字段解成求值子集；缺省、null 与 {} 都当作「零步计划」，
// 由 evaluatePlan 统一判成 deny。
func decodePlan(raw json.RawMessage) (planView, *protocol.Error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return planView{}, nil
	}
	if trimmed[0] != '{' {
		return planView{}, newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"plan 必须是 JSON 对象（plan.schema.json）")
	}
	var plan planView
	if err := json.Unmarshal(trimmed, &plan); err != nil {
		return planView{}, newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"plan 结构不符：steps 必须是数组，元素含 action/args："+err.Error())
	}
	return plan, nil
}

// evaluatePlan 逐步骤求值并按 deny > approve > auto 合并出整份计划的处置。
//
// origin 由调用方（servePoliciesEvaluate）从宿主侧判定后传入，逐步注入
// policy.PolicyRequest.Origin：同一份计划的所有步骤共享同一次调用的来源。
//
// 求值出错（规则配置坏、step 的 action 不是 <module>.<Action>）直接 fail-closed：
// 返回 deny + POLICY_EVALUATION_FAILED。理由码与真正的「策略拒绝」（POLICY_DENY）和
// 「无命中」（NO_POLICY_MATCH_DEFAULT_DENY）区分开，排障时不会把配置错误看成业务判定。
func (s *Server) evaluatePlan(ctx context.Context, plan planView, origin string) policy.PolicyEvaluation {
	if len(plan.Steps) == 0 {
		return defaultDeny()
	}
	evals := make([]policy.PolicyEvaluation, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		got, err := s.policyEngine.Evaluate(ctx, policy.PolicyRequest{
			Action: step.Action,
			Args:   step.Args,
			Origin: origin,
		})
		if err != nil {
			log.Printf("controlplane: 策略求值失败（planId=%q stepId=%q action=%q）：%v",
				plan.PlanID, step.StepID, step.Action, err)
			return policy.PolicyEvaluation{
				Outcome:   policy.OutcomeDeny,
				DecidedBy: []string{},
				Reasons:   []string{reasonEvaluationFailed},
			}
		}
		evals = append(evals, got)
	}
	return mergeEvaluations(evals)
}

// mergeEvaluations 把逐步骤结果合并成整份计划的处置：先取最严处置，再收集与它同级
// 的步骤的裁决者、审批人与理由码（并集去重）。
func mergeEvaluations(evals []policy.PolicyEvaluation) policy.PolicyEvaluation {
	if len(evals) == 0 {
		return defaultDeny()
	}
	worst := 0
	for i := range evals {
		if rank := outcomeRank(evals[i].Outcome); rank > worst {
			worst = rank
		}
	}
	merged := policy.PolicyEvaluation{
		Outcome:   outcomeForRank(worst),
		DecidedBy: []string{},
		Reasons:   []string{},
	}
	for _, ev := range evals {
		if outcomeRank(ev.Outcome) != worst {
			continue
		}
		merged.DecidedBy = appendUnique(merged.DecidedBy, ev.DecidedBy...)
		merged.Approvers = appendUnique(merged.Approvers, ev.Approvers...)
		merged.Reasons = appendUnique(merged.Reasons, ev.Reasons...)
	}
	return merged
}

// outcomeRank 给出合并时的强弱序：deny(2) > approve(1) > auto(0)。
// 未知取值按 deny 处理：引擎只返回三态，真出现别的值也不能在合并里变成最弱的一档。
func outcomeRank(outcome string) int {
	switch outcome {
	case policy.OutcomeAuto:
		return 0
	case policy.OutcomeApprove:
		return 1
	default:
		return 2
	}
}

// outcomeForRank 把强弱序换回规范的处置字符串，保证响应里的 outcome 永远合法。
func outcomeForRank(rank int) string {
	switch rank {
	case 0:
		return policy.OutcomeAuto
	case 1:
		return policy.OutcomeApprove
	default:
		return policy.OutcomeDeny
	}
}

// defaultDeny 是「无规则可命中」的 fail-closed 结果：空 plan 与零步计划的回答。
func defaultDeny() policy.PolicyEvaluation {
	return policy.PolicyEvaluation{
		Outcome:   policy.OutcomeDeny,
		DecidedBy: []string{},
		Reasons:   []string{policy.ReasonNoMatch},
	}
}

// appendUnique 把 items 中尚未出现过的非空值接到 dst 后面，保持首次出现的顺序。
func appendUnique(dst []string, items ...string) []string {
	for _, item := range items {
		if item == "" || contains(dst, item) {
			continue
		}
		dst = append(dst, item)
	}
	return dst
}

// contains 判断切片里有没有某个值（并集去重用，元素量级只有个位数）。
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// ---------- 授权评估 ----------

// grantEvaluateRequest 是 grants:evaluate 的请求体（grant.schema.json#/$defs/GrantEvaluationRequest）。
type grantEvaluateRequest struct {
	Agent      string          `json:"agent"`
	OnBehalfOf string          `json:"onBehalfOf"`
	Action     string          `json:"action"`
	Args       json.RawMessage `json:"args"`
}

// serveGrantsEvaluate 对单次调用做授权评估。拒绝是 200 + allowed:false（interfaces.md §4.1：
// 这是「评估」不是「执行」），只有引擎读不懂授权数据时才回 503。
func (s *Server) serveGrantsEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req grantEvaluateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {agent, onBehalfOf, action, args}："+err.Error()))
		return
	}
	if perr := requireGrantFields(req); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	amount, perr := requestAmount(req.Args)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	result, err := s.grantEngine.Evaluate(req.Agent, req.OnBehalfOf, req.Action, amount)
	if err != nil {
		// 引擎错误是「授权数据读不懂」（窗口/计量单位不在支持范围），不是一次判定：
		// GrantResult 的拒绝原因被 schema 锁死为四态，无法承载它，所以走 503。
		// 调用方按语义 fail-closed（不做任何写），retryable 只表示「重试也许有救」。
		writeTenonError(w, *newError(protocol.ClassRetryable, "GRANT_EVALUATION_FAILED",
			"授权评估失败（不构成放行）："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// requireGrantFields 检查必填项。缺字段是接入方 bug（schema 里它们都是 required），
// 回 400 而不是伪造一个 deny：把「报文不合法」伪装成「业务拒绝」会让 agent 停机排查方向错。
func requireGrantFields(req grantEvaluateRequest) *protocol.Error {
	missing := make([]string, 0, 3)
	if strings.TrimSpace(req.Agent) == "" {
		missing = append(missing, "agent")
	}
	if strings.TrimSpace(req.OnBehalfOf) == "" {
		missing = append(missing, "onBehalfOf")
	}
	if strings.TrimSpace(req.Action) == "" {
		missing = append(missing, "action")
	}
	if len(missing) == 0 {
		return nil
	}
	e := newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
		fmt.Sprintf("授权评估请求缺少必填字段 %v", missing))
	e.Details = map[string]any{"missing": missing}
	return e
}

// requestAmount 从 args 里取本次调用的业务量。
//
// 取舍见包注释：host 在 M0.5 拿不到动作声明的 quotaField，所以固定认 args.qty；
// 拿不到数值时按 1 计（对 unit=calls 的动作本来就记 1，对 quantity 动作是保守估计）。
func requestAmount(args json.RawMessage) (float64, *protocol.Error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 1, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return 0, newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"args 必须是 JSON 对象（grant.schema.json 的 JsonObject）："+err.Error())
	}
	raw, ok := obj[quotaField]
	if !ok {
		return 1, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		// qty 不是数字：入参结构由动作的 inputSchema 负责，配额这里按一次调用计。
		return 1, nil
	}
	amount, err := number.Float64()
	if err != nil {
		return 1, nil
	}
	return amount, nil
}

// ---------- 审计 ----------

// auditQueryResponse 是 GET /tenon/audit 的响应体（interfaces.md §4.3）。
type auditQueryResponse struct {
	Records    []audit.AuditRecord `json:"records"`
	NextCursor string              `json:"nextCursor"`
}

// okResponse 是「写成功了」这种没有更多信息的响应。
type okResponse struct {
	OK bool `json:"ok"`
}

// serveAudit 分派审计的读写：POST 追加一条，GET 按条件查询。
func (s *Server) serveAudit(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.serveAuditAppend(w, r)
	case http.MethodGet:
		s.serveAuditQuery(w, r)
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

// serveAuditAppend 追加一条审计记录（两段写的第一段与第二段都走这里）。
//
// 返回 200 表示这条记录**已经落盘**（audit.Store 在 Append 里 fsync）；返回非 2xx 时
// 调用方必须拒绝执行下游调用（design §5：dispatched 审计写失败 = 不执行）。
func (s *Server) serveAuditAppend(w http.ResponseWriter, r *http.Request) {
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var rec audit.AuditRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"审计记录结构不符（at 必须是 RFC 3339 时刻）："+err.Error()))
		return
	}
	if strings.TrimSpace(rec.AuditID) == "" {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "MISSING_AUDIT_ID",
			"审计记录缺少 auditId"))
		return
	}
	switch rec.Stage {
	case audit.StageDispatched, audit.StageResult:
	default:
		writeTenonError(w, *newError(protocol.ClassContractViolation, "INVALID_AUDIT_STAGE",
			fmt.Sprintf("审计记录 %s 的 stage=%q 非法，只能是 %s 或 %s",
				rec.AuditID, rec.Stage, audit.StageDispatched, audit.StageResult)))
		return
	}
	if err := s.auditStore.Append(rec); err != nil {
		// 落盘失败不是「这次没记上」，而是「审计链已不可用」：回 503 retryable，
		// 让调用方按 fail-closed 中止，而不是把一次无审计的写放出去。
		writeTenonError(w, *newError(protocol.ClassRetryable, "AUDIT_WRITE_FAILED",
			"审计写入失败，必须拒绝执行下游调用："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// serveAuditQuery 按 planId 与写入顺序游标查询审计记录。
//
// since 是**写入顺序的 0 基序号**；nextCursor 是下一页应当传入的 since。这里不直接用
// audit.Store.Query 的分页参数，是因为它按 planId 过滤后无法回报「扫描到第几条」，
// 而游标必须锚在写入顺序上才稳定。
func (s *Server) serveAuditQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	planID := strings.TrimSpace(query.Get("planId"))
	since, perr := parseSince(query.Get("since"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	limit, perr := parseLimit(query.Get("limit"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	records, err := s.auditStore.Query(audit.Query{})
	if err != nil {
		writeTenonError(w, *newError(protocol.ClassRetryable, "AUDIT_READ_FAILED",
			"审计读取失败："+err.Error()))
		return
	}
	next := since
	if next > len(records) {
		next = len(records)
	}
	out := make([]audit.AuditRecord, 0, limit)
	for next < len(records) && len(out) < limit {
		rec := records[next]
		next++
		if planID != "" && rec.PlanID != planID {
			continue
		}
		out = append(out, rec)
	}
	writeJSON(w, http.StatusOK, auditQueryResponse{Records: out, NextCursor: strconv.Itoa(next)})
}

// parseSince 解析写入顺序游标：缺省 0，必须是非负整数。
func parseSince(raw string) (int, *protocol.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, newError(protocol.ClassContractViolation, "INVALID_CURSOR",
			"since 必须是非负整数（写入顺序的 0 基序号），收到 "+strconv.Quote(raw))
	}
	return n, nil
}

// parseLimit 解析分页大小：缺省 defaultAuditPage，超过 maxAuditPage 截断。
func parseLimit(raw string) (int, *protocol.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultAuditPage, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, newError(protocol.ClassContractViolation, "INVALID_LIMIT",
			"limit 必须是正整数，收到 "+strconv.Quote(raw))
	}
	if n > maxAuditPage {
		return maxAuditPage, nil
	}
	return n, nil
}

// ---------- 注册表 ----------

// modulesResponse 是 GET /tenon/modules 的响应体。用结构体包一层而不是直接输出数组，
// 是为了留出后续加字段的余地（0.1 的 module-manifest 与注册表都在长）。
type modulesResponse struct {
	Modules []registry.ModuleEntry `json:"modules"`
}

// serveModules 返回管理面视图：含 enabled=false 的条目（D1 的 B 轮要看得见「被摘除」）。
func (s *Server) serveModules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, modulesResponse{Modules: s.modules.List()})
}

// adminModulesRequest 是 POST /tenon/admin/modules 的请求体。
//
// enabled 必须是 *bool：省略与 false 在布尔里无法区分，而「省略」的语义是「不改开关」。
// 若用 bool，一次只想更新地址的请求会把模块意外关掉（registry.Upsert 也不接受入参的
// 开关值，见其注释）。
type adminModulesRequest struct {
	ID      string  `json:"id"`
	BaseURL *string `json:"baseURL"`
	Enabled *bool   `json:"enabled"`
}

// serveAdminModules 登记模块或摘除/恢复它（仅 TEST_MODE 挂载）。
//
// 顺序是有意的：先 Upsert（新条目一律 enabled）再 SetEnabled(false)，这样「登记同时
// 禁用」能一次请求表达完；而只给 enabled 的请求绝不动地址。
func (s *Server) serveAdminModules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req adminModulesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {id, baseURL?, enabled?}："+err.Error()))
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "MISSING_MODULE_ID",
			"请求体缺少 id"))
		return
	}
	if req.BaseURL == nil && req.Enabled == nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "EMPTY_MODULE_PATCH",
			"baseURL 与 enabled 至少要给一个：只给 id 的请求没有任何可执行语义"))
		return
	}
	if req.BaseURL != nil {
		base, err := normalizeBaseURL(*req.BaseURL)
		if err != nil {
			e := newError(protocol.ClassContractViolation, "INVALID_MODULE_BASE_URL", err.Error())
			e.Details = map[string]any{"module": id}
			writeTenonError(w, *e)
			return
		}
		s.modules.Upsert(registry.ModuleEntry{ID: id, BaseURL: base})
	}
	if req.Enabled != nil {
		if err := s.modules.SetEnabled(id, *req.Enabled); err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				e := newError(protocol.ClassContractViolation, "MODULE_NOT_FOUND",
					"模块 "+strconv.Quote(id)+" 未登记：开关只能作用于已登记的模块")
				e.Details = map[string]any{"module": id}
				writeTenonErrorStatus(w, http.StatusNotFound, *e)
				return
			}
			writeTenonError(w, *newError(protocol.ClassRetryable, "MODULE_TOGGLE_FAILED",
				"模块开关写入失败："+err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, modulesResponse{Modules: s.modules.List()})
}

// normalizeBaseURL 校验并规范化管理面给的模块地址。
//
// 规则与 registry 里那份一致（http/https、有主机名、无查询串或锚点、去掉结尾 '/'），
// 但 registry 没有导出它，而管理面必须自己给出 400 的理由与字段名，所以这里重写一份。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("模块地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("模块地址 %q 不是合法 URL：%w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("模块地址 %q 的 scheme 必须是 http 或 https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("模块地址 %q 缺少主机名", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("模块地址 %q 不应带查询串或锚点", raw)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// ---------- 共驾帧（REST 降级通道） ----------

// pollResponse 是 GET /tenon/intents/poll 的响应体。
//
// Dropped 是「因为队列满而丢掉的累计帧数」：前端不接入时帧本就该丢弃，但悄悄丢和
// 明确告知是两回事——回放侧据此能判断自己是不是漏了内容。
type pollResponse struct {
	Frames     []queuedFrame `json:"frames"`
	NextCursor int64         `json:"nextCursor"`
	Dropped    int64         `json:"dropped,omitempty"`
}

// ackRequest 是 POST /tenon/intents/ack 的请求体。省略 upto 表示确认全部已入队的帧。
type ackRequest struct {
	Upto *int64 `json:"upto"`
}

// ackResponse 回报本次确认到的序号。
type ackResponse struct {
	OK    bool  `json:"ok"`
	Acked int64 `json:"acked"`
}

// enqueueResponse 回报入队后分配的序号。
type enqueueResponse struct {
	OK  bool  `json:"ok"`
	Seq int64 `json:"seq"`
}

// serveIntentEnqueue 把一帧放进内存队列。返回 200 即已入队（可能因队列满挤掉了最旧的一帧）。
func (s *Server) serveIntentEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	if perr := validateFrame(body); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	writeJSON(w, http.StatusOK, enqueueResponse{OK: true, Seq: s.intents.Push(body)})
}

// serveIntentPoll 回放 after 之后未确认的帧；nextCursor 是下一次应当传入的 after。
func (s *Server) serveIntentPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	after, perr := parseAfter(r.URL.Query().Get("after"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	frames, cursor, dropped := s.intents.Since(after)
	writeJSON(w, http.StatusOK, pollResponse{Frames: frames, NextCursor: cursor, Dropped: dropped})
}

// serveIntentAck 确认到某个序号，此后 poll 不再回放它。
func (s *Server) serveIntentAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req ackRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {\"upto\": <序号>}："+err.Error()))
		return
	}
	// 省略 upto 表示「确认到目前为止已入队的全部帧」：这是无参 ack 唯一说得通的语义。
	upto := s.intents.Highest()
	if req.Upto != nil {
		if *req.Upto < 0 {
			writeTenonError(w, *newError(protocol.ClassContractViolation, "INVALID_CURSOR",
				"upto 不允许为负"))
			return
		}
		upto = *req.Upto
	}
	writeJSON(w, http.StatusOK, ackResponse{OK: true, Acked: s.intents.Ack(upto)})
}

// validateFrame 只做宿主必须做的最小判定：它是不是三种帧之一。
// 帧内字段的完整校验属 internal/spec（frames.schema.json 是封闭结构，宿主不重复实现）。
func validateFrame(raw json.RawMessage) *protocol.Error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"共驾帧结构不符："+err.Error())
	}
	switch probe.Type {
	case frameTypeIntent, frameTypeContext, frameTypeConfirm:
		return nil
	}
	return newError(protocol.ClassContractViolation, "UNKNOWN_FRAME_TYPE",
		fmt.Sprintf("共驾帧的 type 必须是 %s/%s/%s 之一，收到 %q",
			frameTypeIntent, frameTypeContext, frameTypeConfirm, probe.Type))
}

// parseAfter 解析回放游标：缺省 0，必须是非负整数。
func parseAfter(raw string) (int64, *protocol.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, newError(protocol.ClassContractViolation, "INVALID_CURSOR",
			"after 必须是非负整数序号，收到 "+strconv.Quote(raw))
	}
	return n, nil
}

// ---------- 配置加载 ----------

// LoadPolicies 读入策略规则文件（config/policies.dev.json 的形状：规则对象数组）。
//
// 未知字段一律报错：配置里写错一个键名会让规则静默失效，而策略静默失效的代价是
// 本该审批的动作被 deny 或反过来——两种都不该靠人肉 review 发现。
//
// 但注意这里的「未知字段」只指 JSON 的**字段名**（DisallowUnknownFields 的语义），
// 不包括 match.actions 里的**动作 id**：host 没有动作清单（M0.5 不做能力发现），
// 无从判断一个动作 id 是否真实存在（见包注释「读动作必须逐个登记」）。因此写错动作名
// 的规则会一直加载成功、但永不命中，代价是该动作所在的整份 plan 被 G2 判 deny——
// 失败是闸门上的响失败（响应里有 NO_POLICY_MATCH_DEFAULT_DENY），不是启动期报错，
// 也不是静默放行。when 里的算子/取值写法错误则相反：引擎在构造期就记下错误，
// New 的启动自检会直接让宿主起不来。
func LoadPolicies(path string) ([]policy.PolicyRule, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取策略文件 %s 失败：%w", path, err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("策略文件 %s 是空的：请明确写 [] 表示「无规则、全部 deny」", path)
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("策略文件 %s 应是 JSON 规则数组（policy.schema.json）", path)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var rules []policy.PolicyRule
	if err := dec.Decode(&rules); err != nil {
		return nil, fmt.Errorf("策略文件 %s 解析失败：%w", path, err)
	}
	if err := requireEOF(dec); err != nil {
		return nil, fmt.Errorf("策略文件 %s：%w", path, err)
	}
	return rules, nil
}

// grantsFile 是授权文件的形状：授权数组 + 委托人权限表（交集规则的下半部分）。
type grantsFile struct {
	Grants   []grant.Grant       `json:"grants"`
	Subjects map[string][]string `json:"subjects"`
}

// LoadGrants 读入授权文件，返回授权表与委托人权限表。
//
// 与 LoadPolicies 一样拒绝未知字段。取值域（grantId 词法、expiresAt 格式、
// actions 是否真实存在）的完整校验属 internal/spec，宿主只保证结构读得懂。
func LoadGrants(path string) ([]grant.Grant, map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取授权文件 %s 失败：%w", path, err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("授权文件 %s 是空的：请明确写 {\"grants\": [], \"subjects\": {}}", path)
	}
	if trimmed[0] != '{' {
		return nil, nil, fmt.Errorf(`授权文件 %s 应是 {"grants": [...], "subjects": {...}} 形式的 JSON 对象`, path)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var file grantsFile
	if err := dec.Decode(&file); err != nil {
		return nil, nil, fmt.Errorf("授权文件 %s 解析失败：%w", path, err)
	}
	if err := requireEOF(dec); err != nil {
		return nil, nil, fmt.Errorf("授权文件 %s：%w", path, err)
	}
	return file.Grants, file.Subjects, nil
}

// requireEOF 确认 JSON 之后没有多余内容：半个文件被解析成功会让配置看起来「生效了」。
func requireEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON 之后还有多余内容")
	}
	return nil
}

// ---------- HTTP 工具 ----------
//
// 以下与 internal/module/module.go 的同名实现是刻意重复的：两者是不同包里的
// 处理器，共享的只有 protocol 包的错误信封与版本头；抽一个"通用 HTTP 工具包"
// 会把「模块侧」与「宿主侧」的响应契约搅在一起。

// preflight 做 POST 请求的公共前置检查：Content-Type 与协议版本头。
func preflight(r *http.Request) *protocol.Error {
	if perr := checkContentType(r); perr != nil {
		return perr
	}
	return checkVersion(r)
}

// checkVersion 校验协议版本头。缺失按 0.1 处理（versioning.md §4.3 的宽松侧），
// 存在则必须是 0.1：本轮不做版本协商（见包注释）。
func checkVersion(r *http.Request) *protocol.Error {
	if got := strings.TrimSpace(r.Header.Get(protocol.HeaderVersion)); got != "" && got != protocol.Version {
		return newError(protocol.ClassContractViolation, "PROTOCOL_VERSION_UNSUPPORTED",
			fmt.Sprintf("对端协议版本 %q 与本宿主的 %q 不兼容", got, protocol.Version))
	}
	if min := strings.TrimSpace(r.Header.Get(protocol.HeaderMinVersion)); min != "" && min != protocol.Version {
		return newError(protocol.ClassContractViolation, "PROTOCOL_MIN_VERSION_UNSUPPORTED",
			fmt.Sprintf("对端要求最低版本 %q，本宿主只实现 %q 且不做版本协商", min, protocol.Version))
	}
	return nil
}

// checkContentType 只在头存在时校验：头缺失按 JSON 处理（手工 curl 与健康探测不必带），
// 头存在却写明非 JSON 才是真正的契约违反（interfaces.md §3.1）。
func checkContentType(r *http.Request) *protocol.Error {
	raw := strings.TrimSpace(r.Header.Get("Content-Type"))
	if raw == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return newError(protocol.ClassContractViolation, "UNSUPPORTED_CONTENT_TYPE",
			"Content-Type 无法解析："+err.Error())
	}
	if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return newError(protocol.ClassContractViolation, "UNSUPPORTED_CONTENT_TYPE",
			fmt.Sprintf("Content-Type 必须是 application/json，收到 %q", mediaType))
	}
	return nil
}

// readBody 读出请求体并确认它是 JSON 对象；空体按 {} 处理。
// 未知字段一律保留原文，由各处理函数忽略（interfaces.md §3.2）。
func readBody(r *http.Request) (json.RawMessage, *protocol.Error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, newError(protocol.ClassContractViolation, "BODY_READ_FAILED",
			"读取请求体失败："+err.Error())
	}
	if len(body) > maxBodyBytes {
		return nil, newError(protocol.ClassContractViolation, "BODY_TOO_LARGE", "请求体超过 1 MiB 上限")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是单个 JSON 对象："+err.Error())
	}
	return json.RawMessage(trimmed), nil
}

// unknownEndpoint 是路径不存在的统一报文。
func unknownEndpoint(path string) protocol.Error {
	e := protocol.NewError(protocol.ClassContractViolation, "UNKNOWN_ENDPOINT",
		"宿主不提供路径 "+strconv.Quote(path))
	e.Details = map[string]any{"path": path}
	return e
}

// newError 构造一个错误值，省去每处取地址。
func newError(class protocol.ErrorClass, code, message string) *protocol.Error {
	e := protocol.NewError(class, code, message)
	return &e
}

// setVersionHeaders 在所有响应上声明本宿主的协议版本。
func setVersionHeaders(w http.ResponseWriter) {
	w.Header().Set(protocol.HeaderVersion, protocol.Version)
	w.Header().Set(protocol.HeaderMinVersion, protocol.Version)
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// 编组失败只可能是本包的 bug：退化为最小合法错误体，绝不写半截 JSON。
		writeJSONBody(w, http.StatusInternalServerError, fallbackErrorBody("RESPONSE_ENCODE_FAILED"))
		return
	}
	writeJSONBody(w, status, body)
}

// writeJSONBody 写出已编组的响应体并补上版本头与 Content-Type。
func writeJSONBody(w http.ResponseWriter, status int, body []byte) {
	setVersionHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// writeTenonError 按错误分类映射的状态码输出错误信封。
func writeTenonError(w http.ResponseWriter, e protocol.Error) {
	writeTenonErrorStatus(w, protocol.StatusFor(e.Class), e)
}

// writeTenonErrorStatus 用于「状态码由接口约定决定、与 class 映射不一致」的地方
// （例如管理面里「模块未登记」是 404 而不是 400）。响应体仍按分类给出语义。
func writeTenonErrorStatus(w http.ResponseWriter, status int, e protocol.Error) {
	writeJSONBody(w, status, encodeError(e))
}

// encodeError 编组错误信封。
func encodeError(e protocol.Error) []byte {
	body, err := json.Marshal(protocol.ErrorEnvelope{TenonError: e})
	if err != nil {
		return fallbackErrorBody("RESPONSE_ENCODE_FAILED")
	}
	return body
}

// fallbackErrorBody 是编组失败时仍能发出去的最小合法错误体。
func fallbackErrorBody(code string) []byte {
	return []byte(`{"tenonError":{"class":"contract_violation","code":"` + code + `","message":"响应编组失败","retryable":false}}`)
}

// methodNotAllowed 回 405 并声明允许的方法。
func methodNotAllowed(w http.ResponseWriter, allow string) {
	setVersionHeaders(w)
	w.Header().Set("Allow", allow)
	w.WriteHeader(http.StatusMethodNotAllowed)
}
