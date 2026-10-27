// Package module 是 Tenon 模块侧的参考实现框架。
//
// 它把「模块侧六个端点」里与业务无关的部分收进一个 http.Handler：线格式、
// 协议版本校验、幂等、dry-run 三态、事件验签与去重、补课游标、模块侧审计，
// 外加两个运维端点（/healthz、/readyz）。业务模块只提供 action 清单、一个
// 执行回调与（可选的）一个预演回调，见 cmd/inventory 与 cmd/procurement。
//
// 规范依据（以 spec/0.1 为唯一真源）：
//   - 端点与请求头：docs/interfaces.md §3；运维端点见 §7（非协议）；
//   - 执行语义：spec/0.1/semantics.md §2–§11（幂等、预演、事件、审计）；
//   - 错误分类与状态码：spec/0.1/errors.md §5，经 protocol.StatusFor 落地；
//   - 线格式词汇（版本常量、请求头、签名、错误信封）复用 internal/protocol，
//     本包不重复定义。
//
// 本包只依赖标准库，不含业务逻辑。
//
// 已知缺口（不实现、也不假装实现）：semantics.md §11 要求模块侧再校验一次
// X-Tenon-Grant（纵深防御），但 M0 没有 grant 的签发与存储，故本框架只承认
// 该头的存在而不做判定——伪造「已校验」比不校验更危险。
package module

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenon.dev/internal/protocol"
)

// DefaultEventVersion 是 manifest 里事件摘要项统一使用的契约版本。
// Module.Events 只有事件类型名，而 module-manifest.schema.json 的 EventSummary
// 必填 version；0.1 没有逐事件的版本表，故由框架给一个显式值而不是留空。
const DefaultEventVersion = "0.1.0"

// DefaultTimeoutMs 是 action.timeoutMs 未声明时的兜底值（semantics.md §2）。
// 框架把 TimeoutMs=0 显式化为该值，避免产出缺 timeoutMs 的非法 Action。
const DefaultTimeoutMs = 30000

// 环境变量与默认值（interfaces.md §6、§1）。
const (
	// EnvPort 监听端口；可以是纯端口（"8081"）或完整地址（"127.0.0.1:8081"）。
	EnvPort = "PORT"
	// DefaultPort 是模块的通用默认监听端口。
	DefaultPort = "8080"
	// EnvModuleID 覆盖 Module.ID（manifest.module.id 与审计记录的 module 字段）。
	EnvModuleID = "TENON_MODULE_ID"
	// EnvHMACSecretFile 事件投递验签用的 HMAC 密钥文件；读不到即拒绝投递。
	EnvHMACSecretFile = "TENON_HMAC_SECRET_FILE"
	// EnvAuditPath 模块侧审计 JSONL 路径；缺省 ./audit-<moduleID>.jsonl。
	EnvAuditPath = "TENON_MODULE_AUDIT_PATH"
)

const (
	// maxBodyBytes 是入参与事件信封的单次上限，防内存放大。
	maxBodyBytes = 1 << 20
	// 事件补课分页的缺省与上限（interfaces.md §3.5）。
	defaultEventPage = 100
	maxEventPage     = 500
	// cursorPrefix 是补课游标的前缀；游标本身是不透明字符串，宿主不得解析。
	cursorPrefix = "cur_"
	// directionIn / directionOut 是 GET /tenon/events 的 direction 取值（设计增量 §2.2）。
	// 缺省为 in：0.1 的「宿主投递进来」语义不能因为加了发件箱而改变。
	directionIn  = "in"
	directionOut = "out"
	// 游标带方向标签：in 的游标用在 out 上会判 INVALID_CURSOR，而不是静默跳过事件。
	cursorInPrefix  = cursorPrefix + directionIn + "_"
	cursorOutPrefix = cursorPrefix + directionOut + "_"
	// adminResetPath 是仅测试态挂载的管理端点（设计增量 §2.3）。TestMode 为 false 时
	// 它不进入路由表，请求得到的 404 与「这里本来就没有这个路径」完全同形。
	adminResetPath = "/tenon/admin/reset"
)

// 规范里的词法约束（spec/0.1/schemas/*.json），在启动自检时一次性校验。
var (
	moduleIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)                                                              // ModuleId
	actionIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.[A-Za-z][A-Za-z0-9_]*$`)                                       // ActionId
	semVerPattern    = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`) // SemVer
	eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)                                           // EventSummary.type
	fieldNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)                                                        // keyFields / argMap 键
	argPathPattern   = regexp.MustCompile(`^\$(in|result)(\.[A-Za-z_][A-Za-z0-9_]*)+$`)                                      // Compensation.argMap 值
)

// Risk 对应 action.schema.json#/$defs/Risk（同时用于 manifest 的 ActionSummary）。
type Risk struct {
	// Level 取值 low | medium | high。
	Level string `json:"level"`
	// Reversible 为 false 的动作不得出现在多步 Plan 的非末位（semantics.md §7.3）。
	Reversible bool `json:"reversible"`
	// MoneyAffecting 表示影响金额；摘要层可省略。
	MoneyAffecting bool `json:"moneyAffecting,omitempty"`
}

// Idempotency 对应 action.schema.json#/$defs/Idempotency。
type Idempotency struct {
	// KeyFields 是参与幂等键的入参字段；必须是 inputSchema.required 的子集。
	KeyFields []string `json:"keyFields"`
	// Scope 取值 module | system。
	Scope string `json:"scope"`
	// DuplicateBehavior 在 0.1 只有 return_first_result。
	DuplicateBehavior string `json:"duplicateBehavior"`
}

// Compensation 对应 action.schema.json#/$defs/Compensation。
type Compensation struct {
	// Action 是撤销本动作的补偿动作 id。
	Action string `json:"action"`
	// ArgMap 的值为 $in.<field> 或 $result.<field> 形式的取值路径。
	ArgMap map[string]string `json:"argMap"`
}

// DryRunCapability 对应 action.schema.json#/$defs/DryRunCapability。
type DryRunCapability struct {
	// Supported 为 true 表示实现了该动作的 :dry-run 端点。
	Supported bool `json:"supported"`
}

// Action 是一个动作的完整定义（spec/0.1/schemas/action.schema.json 的 Go 投影）。
//
// 字段刻意与 schema 一一对应：action.schema.json 是 additionalProperties=false，
// 多一个字段就会让 GET /tenon/actions/{id} 的响应不合法。
type Action struct {
	ID           string           `json:"id"`
	Version      string           `json:"version"`
	Title        string           `json:"title"`
	Kind         string           `json:"kind"` // read | write
	InputSchema  json.RawMessage  `json:"inputSchema"`
	OutputSchema json.RawMessage  `json:"outputSchema,omitempty"`
	Risk         Risk             `json:"risk"`
	Idempotency  *Idempotency     `json:"idempotency,omitempty"`
	Compensation *Compensation    `json:"compensation,omitempty"`
	TimeoutMs    int              `json:"timeoutMs,omitempty"`
	DryRun       DryRunCapability `json:"dryRun"`
}

// Module 是一个模块的声明与回调集合。
//
// Module 含 sync.Once 与两把 sync.Mutex，**不可复制**：Handler() 返回的处理器与
// 模块实例共享同一份幂等表、收件箱、发件箱与游标状态。
type Module struct {
	ID      string
	Title   string
	Version string

	Actions []Action
	// Events 是模块会产出的领域事件类型名（manifest 摘要用）。
	Events []string

	// Handle 执行业务逻辑。返回 (output, nil) 表示成功；
	// 返回 (nil, *protocol.Error) 表示业务拒绝等；其它 error 视为内部错误。
	Handle func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error)
	// DryRun 可选：返回预期 diff（DryRunReport 对象）。为 nil 表示所有 action 都不支持预演。
	DryRun func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error)

	// TestMode 打开仅测试态的管理端点 POST /tenon/admin/reset（设计增量 §2.3）。
	// 用途单一：D1 差异测试要跑 A/B 两轮，两轮输入必须完全相同，因此需要一个
	// 把内存状态清回起点的入口（库存、幂等表、收件箱、发件箱、两套游标）。
	// 生产部署必须保持 false——该端点会抹掉幂等记账与两张事件表。
	// 与 Actions/Events 一样属于启动配置：Handler() 时读一次，之后不再读取。
	TestMode bool
	// Reset 可选：业务状态的恢复回调（例如库存回种子），由 reset 端点在框架状态
	// 清空之后调用。此时 m.mu 仍被持有、outMu 已释放：回调可以调 EmitEvent 发一条
	// 「已重置」事件，但**不得回调本模块自己的 HTTP 端点**（会在 m.mu 上自死锁）。
	// 为 nil 表示本模块没有要恢复的业务状态，框架状态照清。
	Reset func()

	// 以下为框架自用状态，不属于模块契约。
	once       sync.Once
	prepareErr error
	index      map[string]*actionMeta

	mu       sync.Mutex
	idem     map[string]*idemRecord
	eventLog []storedEvent
	eventIDs map[string]struct{}
	seq      int64

	// 发件箱：模块自己产生的事件，供 agent 用 direction=out 轮询（设计增量 §2.2）。
	// 用独立互斥锁而不是复用 mu：EmitEvent 的常见调用点就在 Handle 回调内部，
	// 而写动作执行期间 mu 已由调用栈持有，复用同一把锁会自死锁。
	outMu     sync.Mutex
	outbox    []storedEvent
	outboxIdx map[string]int // eventId → outbox 下标，用于重复入箱去重
	outSeq    int64
}

// Event 是模块自己产出的领域事件（domain-event.schema.json 的 Go 投影）。
//
// 它是发件箱的入参，不是线格式：缺省字段由框架补成完整信封后再入箱，见 EmitEvent。
// Subject 与 Payload 按原始 JSON 传入：subject 既可是字符串简写
// （"quote:Quote/Q-1"）也可是对象（{"module":"quote","type":"Quote","id":"Q-1"}）。
type Event struct {
	EventID       string          `json:"eventId"`
	Type          string          `json:"type"`
	Version       string          `json:"version,omitempty"`
	Subject       json.RawMessage `json:"subject,omitempty"`
	OccurredAt    string          `json:"occurredAt,omitempty"`
	CausationID   string          `json:"causationId,omitempty"`
	CorrelationID string          `json:"correlationId,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// outboxEnvelope 是入箱后的完整信封：字段与 domain-event.schema.json 的 required
// 一一对应，全部无条件输出（causationId 无上一跳时显式写 null）。
type outboxEnvelope struct {
	EventID       string          `json:"eventId"`
	Type          string          `json:"type"`
	Version       string          `json:"version"`
	Subject       json.RawMessage `json:"subject"`
	OccurredAt    string          `json:"occurredAt"`
	CausationID   *string         `json:"causationId"`
	CorrelationID string          `json:"correlationId"`
	Payload       json.RawMessage `json:"payload"`
}

// EmitEvent 把模块自己产生的一条业务事件放进发件箱，供 agent 轮询
// GET /tenon/events?direction=out（0.1 缺口 #6 的出口）。
//
// 并发安全，且刻意不持有 mu：常见调用点是业务动作的 Handle 回调内部，而写动作
// 执行期间 mu 由调用栈持有，复用同一把锁会自死锁。
//
// 返回该事件在发件箱里的序号（从 1 起单调递增）。以下两种情况返回 0 且只记日志：
//   - 信封不可用：eventId 为空、type 不符合事件类型词法、subject/payload 不是
//     合法 JSON、payload 不是 JSON 对象；
//   - eventId 与已入箱事件冲突但内容不同：同一个 eventId 不可能对应两个事实。
//
// 重复入箱是幂等的：同 eventId 且输入相同不追加第二条，返回首次序号。至少一次
// 投递带来重复是常态，这与收件箱按 eventId 去重（POST /tenon/events）对称。
// 「输入相同」比的是调用方给的内容，不含框架补的 occurredAt 等缺省值。
func (m *Module) EmitEvent(ev Event) int64 {
	envelope, ok := m.buildEnvelope(ev)
	if !ok {
		return 0
	}
	input, err := json.Marshal(ev)
	if err != nil {
		log.Printf("tenon 模块 %s 的 EmitEvent 入参无法指纹化，事件未入箱：%v", m.ID, err)
		return 0
	}
	fingerprint := hashArgs(input)
	eventID := strings.TrimSpace(ev.EventID)

	m.outMu.Lock()
	defer m.outMu.Unlock()
	// 惰性初始化（而不是在 build() 里）：build 由 Handler()/Validate() 触发，
	// 可能与业务线程的 EmitEvent 并发，而 EmitEvent 只持有 outMu。
	if m.outboxIdx == nil {
		m.outboxIdx = make(map[string]int)
	}
	if idx, dup := m.outboxIdx[eventID]; dup {
		if m.outbox[idx].inputHash == fingerprint {
			return m.outbox[idx].seq
		}
		log.Printf("tenon 模块 %s 已入箱的 eventId=%s 与本条内容不同，拒绝重复入箱", m.ID, eventID)
		return 0
	}
	m.outSeq++
	m.outbox = append(m.outbox, storedEvent{
		seq:       m.outSeq,
		eventID:   eventID,
		envelope:  envelope,
		inputHash: fingerprint,
	})
	m.outboxIdx[eventID] = len(m.outbox) - 1
	return m.outSeq
}

// resetState 抹掉框架的全部内存状态，再把业务状态交还给 Reset 回调。
// 只有测试态端点会走到这里（Handler 在 TestMode=false 时根本不挂这条路由）。
//
// 加锁顺序全局唯一：先 m.mu，后 outMu。写动作执行期间 m.mu 由调用栈持有
// （serveActionExecute → run → record），收件箱追加也只持 m.mu；EmitEvent 只持
// outMu；没有任何路径先取 outMu 再去取 m.mu，所以这一对锁不会成环。将来若有
// 代码需要同时持有两把锁，必须沿用 m.mu → outMu 这个顺序，否则这里就有死锁。
//
// m.mu 一直持有到 Reset() 返回：reset 与进行中的写动作因此是串行化的，不会出现
// 某个写动作在状态清空之后才把业务变更落地（那才是真正意义上的「半清」）。
// outMu 则在调用 Reset() 之前就释放：回调常常要发一条「已重置」事件，而
// sync.Mutex 不可重入，持着 outMu 去调 EmitEvent 会自死锁。
func (m *Module) resetState() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 幂等表与收件箱（含它自己的序号）。两张 map 都换成新的而不是置 nil：
	// record 与 deliverEvent 会直接往这两张 map 里写，nil map 赋值会 panic。
	m.idem = make(map[string]*idemRecord)
	m.eventLog = nil
	m.eventIDs = make(map[string]struct{})
	m.seq = 0

	// 发件箱与它的去重索引必须在同一个临界区里成对清空：只清其中一个，
	// EmitEvent 就会拿旧下标去索引新表（越界 panic），或让去重表指向不存在的事件。
	m.outMu.Lock()
	m.outbox = nil
	m.outboxIdx = make(map[string]int)
	m.outSeq = 0
	m.outMu.Unlock()

	if m.Reset != nil {
		m.Reset()
	}
}

// buildEnvelope 把 Event 补成完整信封。返回 false 表示事件不可入箱（已记日志）。
func (m *Module) buildEnvelope(ev Event) (json.RawMessage, bool) {
	eventID := strings.TrimSpace(ev.EventID)
	if eventID == "" {
		log.Printf("tenon 模块 %s 的 EmitEvent 缺少 eventId，事件未入箱", m.ID)
		return nil, false
	}
	eventType := strings.TrimSpace(ev.Type)
	if !eventTypePattern.MatchString(eventType) {
		log.Printf("tenon 模块 %s 的 EmitEvent type=%q 不符合事件类型词法 %s，事件未入箱", m.ID, ev.Type, eventTypePattern)
		return nil, false
	}
	subject := json.RawMessage(bytes.TrimSpace(ev.Subject))
	if len(subject) == 0 {
		// schema 必填而业务没给主角：框架不知道谁是主角，退化成「事件自身」比留空诚实。
		subject = m.defaultSubject(eventID)
	} else if !json.Valid(subject) || (subject[0] != '"' && subject[0] != '{') {
		log.Printf("tenon 模块 %s 的 EmitEvent subject 既不是字符串简写也不是对象，事件未入箱：%s", m.ID, subject)
		return nil, false
	}
	payload := json.RawMessage(bytes.TrimSpace(ev.Payload))
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	} else if !json.Valid(payload) || payload[0] != '{' {
		log.Printf("tenon 模块 %s 的 EmitEvent payload 不是 JSON 对象，事件未入箱：%s", m.ID, payload)
		return nil, false
	}
	version := strings.TrimSpace(ev.Version)
	if version == "" {
		version = DefaultEventVersion
	}
	occurredAt := strings.TrimSpace(ev.OccurredAt)
	if occurredAt == "" {
		occurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	correlationID := strings.TrimSpace(ev.CorrelationID)
	if correlationID == "" {
		correlationID = "cor:" + m.contractID() + "/" + eventID
	}
	var causation *string
	if c := strings.TrimSpace(ev.CausationID); c != "" {
		causation = &c
	}
	envelope, err := json.Marshal(outboxEnvelope{
		EventID:       eventID,
		Type:          eventType,
		Version:       version,
		Subject:       subject,
		OccurredAt:    occurredAt,
		CausationID:   causation,
		CorrelationID: correlationID,
		Payload:       payload,
	})
	if err != nil {
		log.Printf("tenon 模块 %s 的事件信封编组失败，事件未入箱：%v", m.ID, err)
		return nil, false
	}
	return envelope, true
}

// defaultSubject 用「事件自身」作缺省主角寻址（<moduleId>:Event/<eventId>）：
// 它是一个合法的 EntityRefString，比对端去猜一个业务实体更诚实。
func (m *Module) defaultSubject(eventID string) json.RawMessage {
	raw, err := json.Marshal(m.contractID() + ":Event/" + eventID)
	if err != nil {
		return json.RawMessage(`"tenon:Event/unknown"`)
	}
	return raw
}

// contractID 返回对外报告用的模块 id：优先 EnvModuleID 覆盖值（与 manifest、
// 审计一致），但它必须是合法 ModuleId，否则退回声明值——缺省字段不能因为一个
// 非法覆盖就被弄成非法信封。
func (m *Module) contractID() string {
	if id := m.id(); moduleIDPattern.MatchString(id) {
		return id
	}
	return m.ID
}

// actionMeta 是启动自检时算好的派生信息，避免每个请求重复解析 schema。
type actionMeta struct {
	action   *Action
	required []string
}

// idemRecord 是「幂等键 → 首次结果」的留存记录（semantics.md §5.2）。
type idemRecord struct {
	actionID string
	argsHash string
	status   int
	body     []byte
}

// storedEvent 是事件表里的一条。
type storedEvent struct {
	seq      int64
	eventID  string
	envelope json.RawMessage
	// inputHash 只有发件箱用：输入事件的指纹，用来区分「同一事实重复入箱」与
	// 「同一 eventId 对应两个不同事实」。不能用信封字节来比——框架补的
	// occurredAt 精确到秒，跨秒重复会被误判成冲突。
	inputHash string
}

// auditRecord 是模块侧审计的一行（审计双写里模块自己那一份，semantics.md §10.4）。
type auditRecord struct {
	At             string `json:"at"`
	Module         string `json:"module"`
	Kind           string `json:"kind"` // action | event
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotencyKey"`
	ArgsHash       string `json:"argsHash"`
	Result         string `json:"result"` // ok | error
	ErrorCode      string `json:"errorCode"`
	EventID        string `json:"eventId,omitempty"`
	EventType      string `json:"eventType,omitempty"`
}

// manifest 是 GET /.well-known/tenon 的响应体（module-manifest.schema.json）。
type manifest struct {
	Tenon        string          `json:"tenon"`
	Module       moduleInfo      `json:"module"`
	Actions      []actionSummary `json:"actions"`
	Events       []eventSummary  `json:"events"`
	Transports   []string        `json:"transports"`
	Capabilities []string        `json:"capabilities"`
}

type moduleInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title"`
}

// actionSummary 只有六个字段：摘要级刻意不含 inputSchema（D15 上下文成本）。
type actionSummary struct {
	ID      string           `json:"id"`
	Version string           `json:"version"`
	Title   string           `json:"title"`
	Kind    string           `json:"kind"`
	Risk    Risk             `json:"risk"`
	DryRun  DryRunCapability `json:"dryRun"`
}

type eventSummary struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

// Validate 做一次性的启动自检：ID 与版本词法、read/write 与 idempotency 的
// 对应关系、keyFields ⊆ inputSchema.required（semantics.md §5.3）、补偿目标
// 是否存在、dryRun 声明与回调是否自洽。首次调用会把 TimeoutMs=0 兜底为
// DefaultTimeoutMs，之后重复调用返回同一结论。
func (m *Module) Validate() error { return m.prepare() }

func (m *Module) prepare() error {
	m.once.Do(func() { m.prepareErr = m.build() })
	return m.prepareErr
}

func (m *Module) build() error {
	if m.Handle == nil {
		return errors.New("module.Handle 不能为 nil")
	}
	if err := requireMatch(moduleIDPattern, m.ID, "module id"); err != nil {
		return err
	}
	if strings.TrimSpace(m.Title) == "" {
		return errors.New("module.Title 不能为空")
	}
	if err := requireMatch(semVerPattern, m.Version, "module version"); err != nil {
		return err
	}
	if len(m.Actions) == 0 {
		return errors.New("module.Actions 不能为空")
	}

	m.index = make(map[string]*actionMeta, len(m.Actions))
	m.idem = make(map[string]*idemRecord)
	m.eventIDs = make(map[string]struct{})

	for i := range m.Actions {
		a := &m.Actions[i]
		if a.TimeoutMs == 0 {
			// 声明即生效：未声明就按协议兜底值显式化，避免产出缺 timeoutMs 的动作。
			a.TimeoutMs = DefaultTimeoutMs
		}
		if a.TimeoutMs < 0 {
			return fmt.Errorf("action %q 的 timeoutMs 不能为负", a.ID)
		}
		if err := requireMatch(actionIDPattern, a.ID, "action id"); err != nil {
			return err
		}
		if !strings.HasPrefix(a.ID, m.ID+".") {
			return fmt.Errorf("action id %q 必须以 %q 为前缀（common.schema.json#/$defs/ActionId）", a.ID, m.ID+".")
		}
		if _, dup := m.index[a.ID]; dup {
			return fmt.Errorf("action id %q 重复", a.ID)
		}
		if strings.TrimSpace(a.Title) == "" {
			return fmt.Errorf("action %q 的 title 不能为空", a.ID)
		}
		if err := requireMatch(semVerPattern, a.Version, "action "+a.ID+" 的 version"); err != nil {
			return err
		}
		switch a.Kind {
		case "read":
			if a.Idempotency != nil {
				return fmt.Errorf("read action %q 不得声明 idempotency（action.schema.json 的 allOf）", a.ID)
			}
		case "write":
			if a.Idempotency == nil {
				return fmt.Errorf("write action %q 必须声明 idempotency（action.schema.json 的 allOf）", a.ID)
			}
		default:
			return fmt.Errorf("action %q 的 kind %q 必须是 read 或 write", a.ID, a.Kind)
		}
		required, err := requiredOf(a.InputSchema)
		if err != nil {
			return fmt.Errorf("action %q 的 inputSchema 不可用：%w", a.ID, err)
		}
		if len(bytes.TrimSpace(a.OutputSchema)) == 0 {
			return fmt.Errorf("action %q 必须声明 outputSchema（action.schema.json 的 required）", a.ID)
		}
		if !json.Valid(a.OutputSchema) {
			return fmt.Errorf("action %q 的 outputSchema 不是合法 JSON", a.ID)
		}
		if a.Idempotency != nil {
			if err := checkIdempotency(a, required); err != nil {
				return err
			}
		}
		if a.Compensation != nil {
			if err := checkCompensation(a); err != nil {
				return err
			}
		}
		if a.DryRun.Supported && m.DryRun == nil {
			return fmt.Errorf("action %q 声明了 dryRun.supported=true，但 module.DryRun 为 nil：manifest 会与实现不一致", a.ID)
		}
		m.index[a.ID] = &actionMeta{action: a, required: required}
	}

	// 补偿目标必须存在。跨模块补偿不在本框架的校验范围内（schema 只要求它是合法 ActionId）。
	for i := range m.Actions {
		a := &m.Actions[i]
		if a.Compensation == nil {
			continue
		}
		if _, ok := m.index[a.Compensation.Action]; !ok && strings.HasPrefix(a.Compensation.Action, m.ID+".") {
			return fmt.Errorf("action %q 的补偿动作 %q 不在本模块的 action 列表里", a.ID, a.Compensation.Action)
		}
	}

	seen := make(map[string]struct{}, len(m.Events))
	for _, ev := range m.Events {
		if err := requireMatch(eventTypePattern, ev, "事件类型"); err != nil {
			return err
		}
		if _, dup := seen[ev]; dup {
			return fmt.Errorf("事件类型 %q 重复", ev)
		}
		seen[ev] = struct{}{}
	}
	return nil
}

// requiredOf 取出 inputSchema.required。布尔 schema（true/false）不约束字段。
func requiredOf(schema json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 {
		return nil, errors.New("inputSchema 缺失")
	}
	switch {
	case bytes.Equal(trimmed, []byte("true")), bytes.Equal(trimmed, []byte("false")):
		return nil, nil
	case trimmed[0] != '{':
		return nil, errors.New("inputSchema 必须是 JSON 对象或布尔值")
	}
	var parsed struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return nil, fmt.Errorf("不是合法 JSON：%w", err)
	}
	return parsed.Required, nil
}

// checkIdempotency 校验幂等声明，并落实 semantics.md §5.3 的跨文档集合约束：
// keyFields 的每一项都必须是 inputSchema.required 的成员，否则调用方可以省略
// 该字段，幂等键退化为「每次都不相同」，重试就变成重复副作用。
func checkIdempotency(a *Action, required []string) error {
	idem := a.Idempotency
	if len(idem.KeyFields) == 0 {
		return fmt.Errorf("action %q 的 idempotency.keyFields 不能为空", a.ID)
	}
	switch idem.Scope {
	case "module", "system":
	default:
		return fmt.Errorf("action %q 的 idempotency.scope %q 必须是 module 或 system", a.ID, idem.Scope)
	}
	if idem.DuplicateBehavior != "return_first_result" {
		return fmt.Errorf("action %q 的 idempotency.duplicateBehavior %q 在 0.1 只能是 return_first_result", a.ID, idem.DuplicateBehavior)
	}
	requiredSet := make(map[string]struct{}, len(required))
	for _, f := range required {
		requiredSet[f] = struct{}{}
	}
	seen := make(map[string]struct{}, len(idem.KeyFields))
	for _, f := range idem.KeyFields {
		if err := requireMatch(fieldNamePattern, f, "action "+a.ID+" 的 keyFields 项"); err != nil {
			return err
		}
		if _, dup := seen[f]; dup {
			return fmt.Errorf("action %q 的 keyFields 里 %q 重复", a.ID, f)
		}
		seen[f] = struct{}{}
		if _, ok := requiredSet[f]; !ok {
			return fmt.Errorf("action %q 的 idempotency.keyFields 含 %q，但它不在 inputSchema.required %v 里（semantics.md §5.3，C1 用例会断言这一点）", a.ID, f, required)
		}
	}
	return nil
}

// checkCompensation 校验补偿声明的词法。argMap 的键随动作语义变化，故不封闭。
func checkCompensation(a *Action) error {
	c := a.Compensation
	if err := requireMatch(actionIDPattern, c.Action, "action "+a.ID+" 的补偿动作 id"); err != nil {
		return err
	}
	for k, v := range c.ArgMap {
		if err := requireMatch(fieldNamePattern, k, "action "+a.ID+" 的补偿 argMap 键"); err != nil {
			return err
		}
		if err := requireMatch(argPathPattern, v, "action "+a.ID+" 的补偿 argMap["+k+"]"); err != nil {
			return err
		}
	}
	return nil
}

func requireMatch(re *regexp.Regexp, value, what string) error {
	if !re.MatchString(value) {
		return fmt.Errorf("%s %q 不符合规范词法 %s", what, value, re.String())
	}
	return nil
}

// Handler 返回实现了模块侧六个端点与两个运维端点的处理器。
//
// 每次调用共享同一个 Module 的状态（幂等表、事件表），因此一个模块实例应当
// 只创建一个监听器；测试里用 httptest.NewServer(m.Handler()) 即可。
// TestMode 为 true 时额外挂一个仅测试态的管理端点 POST /tenon/admin/reset
// （设计增量 §2.3），让 D1 的 A/B 两轮拿到完全相同的输入。
func (m *Module) Handler() http.Handler {
	if err := m.prepare(); err != nil {
		// 定义自检失败是接入方的 bug，不是运行时故障：所有请求一律 500 + 明确报文。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTenonError(w, protocol.NewError(protocol.ClassContractViolation, "MODULE_DEFINITION_INVALID", "模块定义自检未通过："+err.Error()))
		})
	}
	return &handler{m: m, resetEnabled: m.TestMode}
}

// Addr 返回监听地址：PORT 环境变量优先（端口或完整地址），否则 DefaultPort。
func (m *Module) Addr() string {
	port := strings.TrimSpace(os.Getenv(EnvPort))
	if port == "" {
		port = DefaultPort
	}
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}

// ListenAndServe 在 Addr() 上提供 HTTP 服务。返回 http.ErrServerClosed 之外
// 的错误时调用方应当退出（demo 用 log.Fatalf）。
func (m *Module) ListenAndServe() error { return m.ListenAndServeAt(m.Addr()) }

// ListenAndServeAt 在给定地址上提供 HTTP 服务。demo 用它把默认端口钉到
// interfaces.md §1 约定的 8081/8082，而框架的通用默认值仍是 PORT 或 8080。
func (m *Module) ListenAndServeAt(addr string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           m.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("tenon 模块 %s（协议 %s）监听 %s", m.id(), protocol.Version, srv.Addr)
	return srv.ListenAndServe()
}

// id 返回当前生效的模块 id：TENON_MODULE_ID 可覆盖 Module.ID。
func (m *Module) id() string {
	if override := strings.TrimSpace(os.Getenv(EnvModuleID)); override != "" {
		return override
	}
	return m.ID
}

type handler struct {
	m *Module
	// resetEnabled 是 TestMode 在 Handler() 时的快照：路由表不随之后的字段改动
	// 变化，也就不存在对 TestMode 的并发读写（它和 Actions/Events 一样是启动配置）。
	resetEnabled bool
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/healthz":
		h.serveHealth(w, r)
	case path == "/readyz":
		h.serveReady(w, r)
	case path == "/.well-known/tenon":
		h.serveManifest(w, r)
	case path == "/tenon/events":
		h.serveEvents(w, r)
	case h.resetEnabled && path == adminResetPath:
		h.serveReset(w, r)
	case strings.HasPrefix(path, "/tenon/actions/"):
		h.serveAction(w, r, strings.TrimPrefix(path, "/tenon/actions/"))
	default:
		writeTenonErrorStatus(w, http.StatusNotFound,
			protocol.NewError(protocol.ClassContractViolation, "UNKNOWN_ENDPOINT", "模块不提供路径 "+path))
	}
}

// serveHealth 是存活探针：不检查任何依赖，依赖挂了也返回 200，
// 否则编排会把「依赖故障」误判成「进程死了」并重启它（interfaces.md §7）。
func (h *handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// serveReady 是就绪探针：HMAC 密钥可读 + 审计可写才算能接协议请求。
func (h *handler) serveReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	var (
		missing []string
		details []string
	)
	if _, err := h.m.secret(); err != nil {
		missing = append(missing, "hmac_secret")
		details = append(details, "hmac_secret: "+err.Error())
	}
	if err := h.m.auditWritable(); err != nil {
		missing = append(missing, "audit_file")
		details = append(details, "audit_file: "+err.Error())
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
		"ready":  true,
		"checks": []string{"hmac_secret", "audit_file"},
	})
}

// serveReset 是仅测试态的管理端点（设计增量 §2.3）：把模块的内存状态清回起点，
// 让 D1 的 A/B 两轮输入完全相同。TestMode 为 false 时这条路由根本没挂上，请求
// 落到 UNKNOWN_ENDPOINT 的 404——与任何不存在的路径同形，而不是 405/403 那种
// 「存在但不允许」的信号（那等于告诉外部「这里有个隐藏开关」）。
func (h *handler) serveReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	// 不校验 Tenon-Version：这是框架的运维端点（与 /healthz、/readyz 同类），
	// 不属于六个协议端点，清的是本进程的内存状态而不是协议状态机。
	h.m.resetState()
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

// serveManifest 返回摘要级能力清单：动作项只有六个字段，不含 inputSchema。
func (h *handler) serveManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if perr := checkVersion(r, false); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	m := h.m
	out := manifest{
		Tenon:        protocol.Version,
		Module:       moduleInfo{ID: m.id(), Version: m.Version, Title: m.Title},
		Actions:      make([]actionSummary, 0, len(m.Actions)),
		Events:       make([]eventSummary, 0, len(m.Events)),
		Transports:   []string{"http"},
		Capabilities: capabilitiesOf(m),
	}
	for i := range m.Actions {
		a := &m.Actions[i]
		out.Actions = append(out.Actions, actionSummary{
			ID: a.ID, Version: a.Version, Title: a.Title, Kind: a.Kind, Risk: a.Risk, DryRun: a.DryRun,
		})
	}
	for _, ev := range m.Events {
		out.Events = append(out.Events, eventSummary{Type: ev, Version: DefaultEventVersion})
	}
	writeJSON(w, http.StatusOK, out)
}

// capabilitiesOf 声明可选能力标签。dry-run 只有在「有回调且确有动作支持」时
// 才出现，否则声明的能力会与实现对不上（module-manifest.schema.json 的 DryRunCapability 说明）。
func capabilitiesOf(m *Module) []string {
	caps := make([]string, 0, 2)
	if m.DryRun != nil {
		for i := range m.Actions {
			if m.Actions[i].DryRun.Supported {
				caps = append(caps, "dry-run")
				break
			}
		}
	}
	return append(caps, "event-replay")
}

// serveAction 处理 /tenon/actions/{id} 与 /tenon/actions/{id}:dry-run。
func (h *handler) serveAction(w http.ResponseWriter, r *http.Request, rest string) {
	if id, ok := strings.CutSuffix(rest, ":dry-run"); ok {
		h.serveDryRun(w, r, id)
		return
	}
	meta, ok := h.m.index[rest]
	if !ok {
		writeTenonErrorStatus(w, http.StatusNotFound, unknownAction(rest))
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.serveActionDefinition(w, r, meta.action)
	case http.MethodPost:
		h.serveActionExecute(w, r, meta)
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

// serveActionDefinition 返回完整 Action（含 inputSchema），供宿主命中候选后按 id 拉取。
func (h *handler) serveActionDefinition(w http.ResponseWriter, r *http.Request, a *Action) {
	if perr := checkVersion(r, false); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// serveActionExecute 是唯一可能产生副作用的端点，因此也是唯一强制要求
// Tenon-Version 头的端点（写路径缺版本无法保证状态机一致，versioning.md §4.2）。
func (h *handler) serveActionExecute(w http.ResponseWriter, r *http.Request, meta *actionMeta) {
	a := meta.action
	if perr := checkVersion(r, true); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	if perr := checkContentType(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	args, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	if perr := requireFields(meta, args); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	argsHash := hashArgs(args)

	if a.Kind != "write" {
		// read 无幂等语义（semantics.md §5.1）：不查表、不写审计、不要求 Idempotency-Key。
		h.run(w, r.Context(), a, args, argsHash, "")
		return
	}

	key := strings.TrimSpace(r.Header.Get(protocol.HeaderIdempotencyKey))
	if key == "" {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "MISSING_IDEMPOTENCY_KEY",
			fmt.Sprintf("写操作 %s 必须携带 %s 请求头（semantics.md §5.1）", a.ID, protocol.HeaderIdempotencyKey)))
		return
	}

	// 写路径持锁执行：同键并发请求只会有一个真正落地，另一个必然命中首次结果。
	// 代价是写操作串行化；参考实现选正确性，真实实现应改成键级 in-flight 占位。
	h.m.mu.Lock()
	defer h.m.mu.Unlock()

	if rec, ok := h.m.idem[key]; ok {
		if rec.actionID != a.ID || rec.argsHash != argsHash {
			// 同一笔业务不可能有两种参数：静默按首次执行会掩盖调用方 bug（semantics.md §5.2）。
			e := newError(protocol.ClassContractViolation, "IDEMPOTENCY_KEY_REUSED",
				fmt.Sprintf("幂等键 %q 已被 %s 以不同参数使用过，本次请求不执行", key, rec.actionID))
			e.Details = map[string]any{
				"idempotencyKey": key,
				"firstAction":    rec.actionID,
				"firstArgsHash":  rec.argsHash,
				"argsHash":       argsHash,
			}
			writeTenonError(w, *e)
			return
		}
		writeJSONBody(w, rec.status, rec.body)
		return
	}

	h.run(w, r.Context(), a, args, argsHash, key)
}

// run 执行一次已经通过契约校验的调用，并写出响应。
//
// 写操作要求调用方持有 m.mu：幂等记录与审计追加都要串行化，否则同键并发会
// 产生两次副作用、审计行也会交错。
func (h *handler) run(w http.ResponseWriter, ctx context.Context, a *Action, args json.RawMessage, argsHash, idemKey string) {
	now := time.Now().UTC()
	out, perr := h.m.Handle(ctx, a.ID, args)
	var body []byte
	if perr == nil {
		body = normalizeOutput(out)
		if !json.Valid(body) {
			perr = newError(protocol.ClassContractViolation, "INVALID_MODULE_OUTPUT",
				fmt.Sprintf("动作 %s 的成功出参不是合法 JSON", a.ID))
		}
	}
	status := http.StatusOK
	if perr != nil {
		status = protocol.StatusFor(perr.Class)
		body = encodeError(*perr)
	}
	if a.Kind == "write" {
		// 幂等记录先落、审计后写。审计写失败时响应改成 retryable/AUDIT_WRITE_FAILED，
		// 但幂等记录**保留首次真实结果**——宿主带同一个键重试会拿到真实结论，
		// 不会重复副作用，也不会把「已落库」误判成「从未发生」。
		if err := h.record(a, argsHash, idemKey, body, status, perr, now); err != nil {
			writeTenonError(w, protocol.NewError(protocol.ClassRetryable, "AUDIT_WRITE_FAILED",
				"模块侧审计写入失败，拒绝确认本次结果："+err.Error()))
			return
		}
	}
	writeJSONBody(w, status, body)
}

// record 落幂等记录并追加一行模块侧审计；返回值只表示审计写失败。
func (h *handler) record(a *Action, argsHash, idemKey string, body []byte, status int, perr *protocol.Error, now time.Time) error {
	h.m.idem[idemKey] = &idemRecord{
		actionID: a.ID,
		argsHash: argsHash,
		status:   status,
		body:     append([]byte(nil), body...),
	}
	rec := auditRecord{
		At:             now.Format(time.RFC3339),
		Module:         h.m.id(),
		Kind:           "action",
		Action:         a.ID,
		IdempotencyKey: idemKey,
		ArgsHash:       argsHash,
		Result:         "ok",
	}
	if perr != nil {
		rec.Result = "error"
		rec.ErrorCode = perr.Code
	}
	return h.m.appendAudit(rec)
}

// serveDryRun 处理预演。能力不存在时返回 404 空体而不是错误报文：宿主据此把
// Plan 的 dryRun.status 降级为 unsupported 并强制审批（interfaces.md §3.3），
// 所以「没有预演」必须是可判定的信号，而不是一个看起来像失败的 4xx。
func (h *handler) serveDryRun(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	meta, ok := h.m.index[id]
	if !ok {
		writeTenonErrorStatus(w, http.StatusNotFound, unknownAction(id))
		return
	}
	if h.m.DryRun == nil || !meta.action.DryRun.Supported {
		// 端点等于不存在：只给状态码，不给响应体。
		setVersionHeaders(w)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if perr := checkVersion(r, false); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	if perr := checkContentType(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	args, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	if perr := requireFields(meta, args); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	report, perr := h.m.DryRun(r.Context(), meta.action.ID, args)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, err := wrapDryRun(report)
	if err != nil {
		writeTenonError(w, protocol.NewError(protocol.ClassContractViolation, "INVALID_MODULE_OUTPUT",
			"动作 "+meta.action.ID+" 的预演回调返回了不可用的报告："+err.Error()))
		return
	}
	writeJSONBody(w, http.StatusOK, body)
}

// serveEvents 分发事件投递（POST，宿主 → 模块）与补课/重放（GET，两个方向）。
func (h *handler) serveEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.deliverEvent(w, r)
	case http.MethodGet:
		h.listEvents(w, r)
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

// deliverEvent 校验 HMAC 签名、按 eventId 去重、写审计并入内存事件表。
func (h *handler) deliverEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "BODY_READ_FAILED", "读取请求体失败："+err.Error()))
		return
	}
	if len(body) > maxBodyBytes {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "BODY_TOO_LARGE", "事件信封超过 1 MiB 上限"))
		return
	}

	// 验签必须基于原始字节（semantics.md §9）：先反序列化再序列化会改变字节，签名必然对不上。
	secret, err := h.m.secret()
	if err != nil {
		// 读不到密钥就一律拒（fail-closed）：验签能力缺失时「接受」等于关掉这道闸门。
		writeTenonError(w, protocol.NewError(protocol.ClassRetryable, "SIGNATURE_SECRET_UNAVAILABLE",
			"读不到 HMAC 密钥，拒绝本次投递："+err.Error()))
		return
	}
	if err := protocol.Verify(secret, r.Header.Get(protocol.HeaderSignature), body, time.Now().UTC(), protocol.SignatureTolerance); err != nil {
		code, message := "SIGNATURE_INVALID", "事件签名校验失败"
		switch {
		case errors.Is(err, protocol.ErrSignatureExpired):
			code = "SIGNATURE_EXPIRED"
			message = "事件签名时间戳超出 ±300s 容差（防重放，interfaces.md §3.4）"
		case errors.Is(err, protocol.ErrSignatureFormat):
			code = "SIGNATURE_MALFORMED"
			message = protocol.HeaderSignature + " 头格式非法，应为 t=<unix>,v1=<hex>"
		}
		// 状态码按接口约定取 401（认证失败）；响应体仍按错误分类给出语义（errors.md §5）。
		writeTenonErrorStatus(w, http.StatusUnauthorized,
			protocol.NewError(protocol.ClassContractViolation, code, message))
		return
	}
	if perr := checkVersion(r, false); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	if perr := checkContentType(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}

	var envelope struct {
		EventID string `json:"eventId"`
		Type    string `json:"type"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"事件信封不是合法 JSON："+err.Error()))
		return
	}
	if envelope.EventID == "" || envelope.Type == "" {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"事件信封缺少 eventId 或 type（domain-event.schema.json 的 required）"))
		return
	}

	h.m.mu.Lock()
	defer h.m.mu.Unlock()

	if _, dup := h.m.eventIDs[envelope.EventID]; dup {
		// 至少一次投递意味着重复是常态：去重后仍回 200，让宿主的重试停下来。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true, "eventId": envelope.EventID})
		return
	}
	rec := auditRecord{
		At:        time.Now().UTC().Format(time.RFC3339),
		Module:    h.m.id(),
		Kind:      "event",
		ArgsHash:  hashArgs(body),
		Result:    "ok",
		EventID:   envelope.EventID,
		EventType: envelope.Type,
	}
	// 审计先写、事件表后写：审计失败时不入表，宿主重试时还能补上这一行，
	// 不会出现「内存里有事件、日志里没有」的静默缺口。
	if err := h.m.appendAudit(rec); err != nil {
		writeTenonError(w, protocol.NewError(protocol.ClassRetryable, "AUDIT_WRITE_FAILED",
			"模块侧审计写入失败，事件未入库："+err.Error()))
		return
	}
	h.m.seq++
	h.m.eventLog = append(h.m.eventLog, storedEvent{
		seq:      h.m.seq,
		eventID:  envelope.EventID,
		envelope: append(json.RawMessage(nil), body...),
	})
	h.m.eventIDs[envelope.EventID] = struct{}{}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": false, "eventId": envelope.EventID})
}

// listEvents 提供补课/重放。direction 决定读哪张事件表：
//   - in（缺省）：宿主投递进来的事件（0.1 语义，向后兼容）；
//   - out：模块自己产生的事件（发件箱），供 agent 轮询。
//
// 两张表各有自己的序号与游标，互不干扰；响应形状对两个方向一致。
func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	if perr := checkVersion(r, false); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	query := r.URL.Query()
	direction, perr := parseDirection(query.Get("direction"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	since, perr := parseCursor(direction, query.Get("since"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	limit, perr := parseLimit(query.Get("limit"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}

	page, next := h.m.readEventPage(direction, since, limit)

	writeJSON(w, http.StatusOK, map[string]any{
		"events":     page,
		"nextCursor": encodeCursor(direction, next),
	})
}

// readEventPage 读出某个方向 since 之后的一页，并返回下一页游标。
func (m *Module) readEventPage(direction string, since int64, limit int) ([]json.RawMessage, int64) {
	mu, table := m.eventTable(direction)
	mu.Lock()
	defer mu.Unlock()

	// 两张事件表都只在内存里：进程重启（或测试态 reset）后序号从 1 重新开始，
	// 而消费方可能持有更大的游标。这时按 0 处理等于整段重放，而不是让
	// ev.seq <= since 把新事件全过滤掉、静默饿死消费方。
	// 「可重放 + 消费侧按 eventId 去重」本就是 domain-event.schema.json 的前提。
	if since > m.lastSeq(direction) {
		since = 0
	}

	page := make([]json.RawMessage, 0, limit)
	next := since
	for _, ev := range *table {
		if ev.seq <= since {
			continue
		}
		if len(page) == limit {
			break
		}
		page = append(page, ev.envelope)
		next = ev.seq
	}
	return page, next
}

// lastSeq 返回某个方向已分配到的最新序号；调用方必须已持有该方向的锁。
func (m *Module) lastSeq(direction string) int64 {
	if direction == directionOut {
		return m.outSeq
	}
	return m.seq
}

// eventTable 返回某个方向的事件表及其互斥锁。两个方向的状态没有任何共享，
// 所以游标不会因为另一个方向被读取而前进。
func (m *Module) eventTable(direction string) (*sync.Mutex, *[]storedEvent) {
	if direction == directionOut {
		return &m.outMu, &m.outbox
	}
	return &m.mu, &m.eventLog
}

// parseDirection 解析 direction：缺省与显式 in 等价，其余一律契约违反。
func parseDirection(raw string) (string, *protocol.Error) {
	switch strings.TrimSpace(raw) {
	case "", directionIn:
		return directionIn, nil
	case directionOut:
		return directionOut, nil
	default:
		e := newError(protocol.ClassContractViolation, "INVALID_DIRECTION",
			"direction 只能是 in 或 out（缺省 in），收到 "+strconv.Quote(raw))
		e.Details = map[string]any{"direction": raw}
		return "", e
	}
}

// secret 读取 HMAC 密钥文件。密钥就是文件内容去掉首尾空白后的原始字节，
// 不做十六进制解码（docker/secrets/hmac.txt 是 openssl rand -hex 32 的文本输出，
// 宿主与模块必须用同一份字节材料）。
func (m *Module) secret() ([]byte, error) {
	path := strings.TrimSpace(os.Getenv(EnvHMACSecretFile))
	if path == "" {
		return nil, errors.New("环境变量 " + EnvHMACSecretFile + " 未设置")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("密钥文件 " + path + " 为空")
	}
	return trimmed, nil
}

// auditPath 返回模块侧审计路径，缺省 ./audit-<moduleID>.jsonl。
func (m *Module) auditPath() string {
	if path := strings.TrimSpace(os.Getenv(EnvAuditPath)); path != "" {
		return path
	}
	return "./audit-" + m.id() + ".jsonl"
}

// appendAudit 追加一行 JSON。调用方负责串行化（写路径与事件路径都持 m.mu）。
func (m *Module) appendAudit(rec auditRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(m.auditPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// auditWritable 是就绪探针的一部分：能创建并追加才算审计可用。
func (m *Module) auditWritable() error {
	f, err := os.OpenFile(m.auditPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// requireFields 按 inputSchema.required 做存在性检查。
// 只检查「字段在不在」：类型与约束由业务回调反序列化时判定，字段集合不封闭
// （未知字段必须忽略，interfaces.md §3.2）。
func requireFields(meta *actionMeta, args json.RawMessage) *protocol.Error {
	if len(meta.required) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"入参必须是 JSON 对象："+err.Error())
	}
	var missing []string
	for _, f := range meta.required {
		if _, ok := obj[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	e := newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
		fmt.Sprintf("入参缺少必填字段 %v（%s 的 inputSchema.required）", missing, meta.action.ID))
	e.Details = map[string]any{"missing": missing, "action": meta.action.ID}
	return e
}

// checkVersion 校验协议版本头。
//
// strict=true（只有真正可能产生副作用的执行端点）：缺失即拒——写路径在版本
// 未协商时执行会产生「状态机不同却已经落库」的后果（versioning.md §4.2）。
// strict=false：头缺失按 versioning.md §4.3 视为 0.1；头存在且不兼容仍拒。
func checkVersion(r *http.Request, strict bool) *protocol.Error {
	got := strings.TrimSpace(r.Header.Get(protocol.HeaderVersion))
	if got == "" {
		if !strict {
			return nil
		}
		return newError(protocol.ClassContractViolation, "VERSION_HEADER_MISSING",
			"缺少 "+protocol.HeaderVersion+" 请求头（本模块实现 "+protocol.Version+"）")
	}
	if got != protocol.Version {
		return newError(protocol.ClassContractViolation, "PROTOCOL_VERSION_UNSUPPORTED",
			fmt.Sprintf("对端协议版本 %q 与本模块 %q 的 major 不同，拒绝执行", got, protocol.Version))
	}
	if min := strings.TrimSpace(r.Header.Get(protocol.HeaderMinVersion)); min != "" && versionLess(protocol.Version, min) {
		return newError(protocol.ClassContractViolation, "PROTOCOL_MIN_VERSION_TOO_HIGH",
			fmt.Sprintf("对端要求最低版本 %q，高于本模块实现的 %q", min, protocol.Version))
	}
	return nil
}

// versionLess 比较两个 major.minor 形式的协议版本。
func versionLess(a, b string) bool {
	am, an, ok1 := splitVersion(a)
	bm, bn, ok2 := splitVersion(b)
	if !ok1 || !ok2 {
		return false
	}
	if am != bm {
		return am < bm
	}
	return an < bn
}

func splitVersion(v string) (major, minor int, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// checkContentType 只在头存在时校验：头缺失按 JSON 处理，避免把健康探测与
// 手工 curl 打成 400；头存在却写明非 JSON 才是真正的契约违反（interfaces.md §3.1）。
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
// 未知字段一律保留原文，由业务解码器忽略（不设 DisallowUnknownFields）。
func readBody(r *http.Request) (json.RawMessage, *protocol.Error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, newError(protocol.ClassContractViolation, "BODY_READ_FAILED", "读取请求体失败："+err.Error())
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
			"入参必须是单个 JSON 对象："+err.Error())
	}
	return json.RawMessage(trimmed), nil
}

// normalizeOutput 把空出参与 null 出参规范化成 {}：schema 里出参是对象，
// 写 null 会让调用方的反序列化路径分叉。
func normalizeOutput(out json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("{}")
	}
	return append([]byte(nil), trimmed...)
}

// wrapDryRun 把回调返回的 DryRunReport 包成 {"dryRun": ...}，并确认它是带
// status 的对象——报告不合格是模块 bug，不能让它伪装成成功或降级。
func wrapDryRun(report json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(report)
	if len(trimmed) == 0 {
		return nil, errors.New("报告为空")
	}
	if !json.Valid(trimmed) {
		return nil, errors.New("报告不是合法 JSON")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("报告必须是 DryRunReport JSON 对象")
	}
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, err
	}
	if probe.Status == "" {
		return nil, errors.New("报告缺少 status（plan.schema.json#/$defs/DryRunReport 的 required）")
	}
	body := make([]byte, 0, len(trimmed)+len(`{"dryRun":}`))
	body = append(body, `{"dryRun":`...)
	body = append(body, trimmed...)
	body = append(body, '}')
	return body, nil
}

// hashArgs 产出审计里的 argsHash（规范写法 sha256:<64 位小写十六进制>）。
func hashArgs(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// encodeCursor 把某个方向的递增序号编码成不透明游标。
func encodeCursor(direction string, seq int64) string {
	return cursorPrefix + direction + "_" + base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}

// parseCursor 解出 since；空串表示从头开始。
//
// 游标带方向标签，所以把 in 的游标用到 out 上是 INVALID_CURSOR，而不是被当成
// 同一张表的序号静默跳过一段事件。0.1 的无标签游标（cur_<b64>）仍可用于 in。
func parseCursor(direction, raw string) (int64, *protocol.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	var payload string
	switch {
	case strings.HasPrefix(raw, cursorOutPrefix):
		if direction != directionOut {
			return 0, invalidCursor(raw)
		}
		payload = strings.TrimPrefix(raw, cursorOutPrefix)
	case strings.HasPrefix(raw, cursorInPrefix):
		if direction != directionIn {
			return 0, invalidCursor(raw)
		}
		payload = strings.TrimPrefix(raw, cursorInPrefix)
	case strings.HasPrefix(raw, cursorPrefix):
		// 0.1 的游标没有方向标签；只有 in 方向可能收到它。
		if direction != directionIn {
			return 0, invalidCursor(raw)
		}
		payload = strings.TrimPrefix(raw, cursorPrefix)
	default:
		return 0, invalidCursor(raw)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return 0, invalidCursor(raw)
	}
	seq, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil || seq < 0 {
		return 0, invalidCursor(raw)
	}
	return seq, nil
}

func invalidCursor(raw string) *protocol.Error {
	return newError(protocol.ClassContractViolation, "INVALID_CURSOR",
		"since 游标不是本模块为该方向发出的值："+strconv.Quote(raw))
}

// parseLimit 解析补课分页大小：缺省 100，超过 500 截断。
func parseLimit(raw string) (int, *protocol.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultEventPage, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, newError(protocol.ClassContractViolation, "INVALID_LIMIT",
			"limit 必须是正整数，收到 "+strconv.Quote(raw))
	}
	if n > maxEventPage {
		return maxEventPage, nil
	}
	return n, nil
}

// unknownAction 是「动作不存在」的统一报文。状态码取 404（资源不存在）而
// 不是 400（报文不合法）：id 是路径的一部分，未知 id 意味着这里没有这个资源。
func unknownAction(id string) protocol.Error {
	e := protocol.NewError(protocol.ClassContractViolation, "UNKNOWN_ACTION",
		"模块不提供动作 "+strconv.Quote(id))
	e.Details = map[string]any{"action": id}
	return e
}

func newError(class protocol.ErrorClass, code, message string) *protocol.Error {
	e := protocol.NewError(class, code, message)
	return &e
}

func setVersionHeaders(w http.ResponseWriter) {
	w.Header().Set(protocol.HeaderVersion, protocol.Version)
	w.Header().Set(protocol.HeaderMinVersion, protocol.Version)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// 编组失败只可能是本包的 bug：退化为最小合法错误体，绝不写半截 JSON。
		writeJSONBody(w, http.StatusInternalServerError, fallbackErrorBody("RESPONSE_ENCODE_FAILED"))
		return
	}
	writeJSONBody(w, status, body)
}

func writeJSONBody(w http.ResponseWriter, status int, body []byte) {
	setVersionHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func writeTenonError(w http.ResponseWriter, e protocol.Error) {
	writeTenonErrorStatus(w, protocol.StatusFor(e.Class), e)
}

// writeTenonErrorStatus 用于「状态码由接口约定决定、与 class 映射不一致」的两处：
// 未知 action（404）与事件验签失败（401）。响应体仍按分类给出语义。
func writeTenonErrorStatus(w http.ResponseWriter, status int, e protocol.Error) {
	writeJSONBody(w, status, encodeError(e))
}

func encodeError(e protocol.Error) []byte {
	body, err := json.Marshal(protocol.ErrorEnvelope{TenonError: e})
	if err != nil {
		return fallbackErrorBody("RESPONSE_ENCODE_FAILED")
	}
	return body
}

func fallbackErrorBody(code string) []byte {
	return []byte(`{"tenonError":{"class":"contract_violation","code":"` + code + `","message":"响应编组失败","retryable":false}}`)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	setVersionHeaders(w)
	w.Header().Set("Allow", allow)
	w.WriteHeader(http.StatusMethodNotAllowed)
}
