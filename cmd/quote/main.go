// Command quote 是 Tenon 的报价触发器模块（HTTP 侧车，实现模块侧六个端点）。
//
// 它是 M0.5 三进程数据流的第一跳（设计增量 §3 第 1 步）：business 侧调
// quote.CreateStandardPartQuote 建一张报价单，模块把「报价已完成」这个业务事实
// 写进发件箱（internal/module 的 EmitEvent），agent 用
//
//	GET /tenon/events?direction=out
//
// 轮询到 quote.completed 后开一个回合。本模块只宣告事实：事件载荷里没有任何
// 「该调哪个动作」的暗示（domain-event.schema.json 的 payload 说明、设计增量 §2.2），
// 否则 D1 差异测试会退化成「按固定映射执行」而失去意义。
//
// 环境变量（监听、审计、密钥由 internal/module 读取）：
//
//	PORT                     监听端口或地址；未设置时用本模块约定的 8083
//	TENON_MODULE_ID          manifest.module.id 与审计 module 字段的覆盖值
//	TENON_HMAC_SECRET_FILE   事件投递验签密钥文件（读不到就拒绝投递）
//	TENON_MODULE_AUDIT_PATH  模块侧审计 JSONL，缺省 ./audit-quote.jsonl
//
// 业务状态是内存里的 报价单号 → 报价单，故意不持久化：这个 demo 要证明的是
// 协议行为（幂等、发件箱、事件信封词法），不是报价记账。真实实现的崩溃恢复
// 窗口必须由持久化 + 至少一次投递 + 消费侧去重共同覆盖（semantics.md §5.5）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"tenon.dev/internal/module"
	"tenon.dev/internal/protocol"
)

const (
	moduleID      = "quote"
	moduleTitle   = "报价"
	moduleVersion = "0.1.0"

	// defaultPort 是设计增量 §1 给 quote 约定的端口；PORT 可覆盖。
	defaultPort = "8083"

	actionCreateStandardPartQuote = "quote.CreateStandardPartQuote"
	actionGetQuote                = "quote.GetQuote"

	// eventQuoteCompleted 是本模块唯一的产出事件类型：manifest 的事件摘要、
	// skill 的订阅点（tenon.subscribes）与 agent 的触发源都取这个字面量。
	eventQuoteCompleted = "quote.completed"

	// statusCompleted 是本模块出参里的报价单状态；
	// statusVerified 是预演结论（plan.schema.json#/$defs/DryRunStatus，只有
	// verified 允许自动执行，其余一律强制审批）。
	statusCompleted = "completed"
	statusVerified  = "verified"

	// quoteType 是事件 subject 里 Quote 实体的类型名（EntityRef.Type 要求 PascalCase）。
	quoteType = "Quote"
)

// entityIDPattern 是 EntityRefString 里 id 段的词法（common.schema.json 的
// ^[^\s:/]+$）。event subject 用 module:Type/id 简写寻址，所以 quoteNo 里出现
// 空白、冒号或斜杠就会产出一个不合法的信封——EmitEvent 只校验 subject 是字符串，
// 不校验它的词法，故这层判定必须由本模块自己做。
var entityIDPattern = regexp.MustCompile(`^[^\s:/]+$`)

func main() {
	store := newQuoteStore()
	mod := newQuoteModule(store)
	if err := mod.Validate(); err != nil {
		log.Fatalf("quote: 模块定义自检失败：%v", err)
	}
	addr := mod.Addr()
	if _, ok := os.LookupEnv(module.EnvPort); !ok {
		addr = ":" + defaultPort
	}
	if err := mod.ListenAndServeAt(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("quote: %v", err)
	}
}

// newQuoteModule 装配模块实例。main 与用例走同一条装配路径，避免测试覆盖的是
// 另一份定义。
func newQuoteModule(store *quoteStore) *module.Module {
	mod := &module.Module{
		ID:       moduleID,
		Title:    moduleTitle,
		Version:  moduleVersion,
		TestMode: os.Getenv("TENON_TEST_MODE") == "1", // D1 的 A/B 两轮要靠 /tenon/admin/reset 造同输入
		Actions: []module.Action{
			createStandardPartQuoteAction(),
			getQuoteAction(),
		},
		Events: []string{eventQuoteCompleted},
		Handle: store.handle,
		DryRun: store.dryRun,
	}
	// 发件箱出口在装配期注入一次、运行期只读：业务代码没有第二只手能往事件表里
	// 塞东西，事件必须过 EmitEvent 的信封校验。
	store.emit = mod.EmitEvent
	return mod
}

// ---------- 动作契约 ----------

func createStandardPartQuoteAction() module.Action {
	return module.Action{
		ID:              actionCreateStandardPartQuote,
		Version:         "0.1.0",
		Title:           "创建标准件报价单",
		SummaryTemplate: "按报价单 {quoteNo} 生成需求",
		Kind:            "write",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["quoteNo", "lines"],
  "properties": {
    "quoteNo": {"type": "string", "minLength": 1},
    "lines": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["sku", "qty"],
        "properties": {
          "sku": {"type": "string", "minLength": 1},
          "qty": {"type": "integer", "minimum": 1}
        }
      }
    }
  }
}`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["quoteId", "status"],
  "properties": {
    "quoteId": {"type": "string", "minLength": 1},
    "status": {"type": "string", "enum": ["completed"]}
  }
}`),
		// 建报价单不扣库存、不占金额，可逆（删单即可）：low + reversible 让它
		// 可以出现在多步 Plan 的任意位置，也让策略层无需强制审批。
		Risk: module.Risk{Level: "low", Reversible: true},
		// keyFields 必须是 inputSchema.required 的子集（semantics.md §5.3）：
		// 同一个 quoteNo 只允许产生一次副作用与一条事件。刻意只取 quoteNo 而不带
		// lines——重试时调用方可能把同一张单的明细顺序改动，带上就会退化成
		// 「每次都不相同」的幂等键。
		Idempotency: &module.Idempotency{
			KeyFields:         []string{"quoteNo"},
			Scope:             "module",
			DuplicateBehavior: "return_first_result",
		},
		TimeoutMs: 30000,
		DryRun:    module.DryRunCapability{Supported: true},
	}
}

func getQuoteAction() module.Action {
	return module.Action{
		ID:      actionGetQuote,
		Version: "0.1.0",
		Title:   "查询报价单",
		Kind:    "read",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["quoteNo"],
  "properties": {
    "quoteNo": {"type": "string", "minLength": 1}
  }
}`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["quoteNo", "quoteId", "status", "lines"],
  "properties": {
    "quoteNo": {"type": "string", "minLength": 1},
    "quoteId": {"type": "string", "minLength": 1},
    "status": {"type": "string", "enum": ["completed"]},
    "lines": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["sku", "qty"],
        "properties": {
          "sku": {"type": "string", "minLength": 1},
          "qty": {"type": "integer", "minimum": 1}
        }
      }
    }
  }
}`),
		Risk:      module.Risk{Level: "low", Reversible: true},
		TimeoutMs: 30000,
		// 读动作无副作用，无需预演（semantics.md §6.3）；且 read 不得声明
		// idempotency（action.schema.json 的 allOf）。
		DryRun: module.DryRunCapability{Supported: false},
	}
}

// ---------- 业务状态 ----------

// quoteStore 是内存态的报价单台账。
//
// 两层幂等：框架层按 Idempotency-Key 去重（请求级），本层按 quoteNo 去重
// （业务级）。第二层必要且不能省——宿主崩溃恢复后用新的幂等键重发同一笔业务时，
// 只有业务级去重能拦住第二次副作用与第二条事件。
type quoteStore struct {
	mu   sync.Mutex
	seq  int
	byNo map[string]quoteRecord
	// emit 是发件箱出口（module.EmitEvent），装配期注入。
	emit func(module.Event) int64
	// lastEventSeq 是最近一次成功入箱的序号，0 表示还没有事件入箱。
	// 保留它是因为「发出去了没有」必须可观测：EmitEvent 失败返回 0 且只记日志，
	// 仅凭 HTTP 200 推断「已触发 agent」是本模块最危险的误判。
	lastEventSeq int64
}

func newQuoteStore() *quoteStore {
	return &quoteStore{byNo: make(map[string]quoteRecord)}
}

// handle 是 module.Handle：入参已由框架校验过 required 字段，这里仍做类型与取值判定。
func (s *quoteStore) handle(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
	switch actionID {
	case actionCreateStandardPartQuote:
		return s.create(args)
	case actionGetQuote:
		return s.get(args)
	default:
		return nil, contractViolation("UNKNOWN_ACTION", "报价模块不提供动作 "+actionID)
	}
}

// create 建单并发事件。顺序是「先入发件箱，后落状态」：事件没进箱就整笔不成立，
// 不会出现「报价单已存在但永远不会触发 agent」的半成品。
func (s *quoteStore) create(args json.RawMessage) (json.RawMessage, *protocol.Error) {
	var in createQuoteInput
	if perr := decodeArgs(args, &in); perr != nil {
		return nil, perr
	}
	if perr := validateCreate(in); perr != nil {
		return nil, perr
	}
	if s.emit == nil {
		// 装配错误（newQuoteModule 未调用）：没有出口的报价模块不可能触发 agent。
		return nil, contractViolation("EVENT_EMIT_UNWIRED", "报价模块未注入发件箱出口，拒绝建单")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.byNo[in.QuoteNo]; ok {
		// 同一张报价单重复提交：返回首次结果，不再发第二条事件。发两条会让 agent
		// 对同一笔业务开两个回合，而 duplicateBehavior 只承诺「不产生第二次副作用」
		// （action.schema.json#/$defs/Idempotency）。
		return mustJSON(createQuoteOutput{QuoteID: rec.QuoteID, Status: rec.Status}), nil
	}

	s.seq++
	rec := quoteRecord{
		QuoteNo: in.QuoteNo,
		QuoteID: fmt.Sprintf("q_%04d", s.seq),
		Status:  statusCompleted,
		// 明细拷一份：调用方释放后入参字节可能被复用，台账不能跟着变。
		Lines: append([]quoteLine(nil), in.Lines...),
	}
	subject, payload, perr := buildQuoteCompleted(rec)
	if perr != nil {
		return nil, perr
	}
	seq := s.emit(module.Event{
		EventID: eventIDForQuote(rec.QuoteNo),
		Type:    eventQuoteCompleted,
		// 显式写契约版本而不是靠框架兜底：manifest 的事件摘要与信封里的
		// version 必须是同一个值，否则消费方无法按 (type, version) 选契约。
		Version:       module.DefaultEventVersion,
		Subject:       subject,
		CorrelationID: correlationIDFor(rec.QuoteNo),
		Payload:       payload,
	})
	if seq == 0 {
		// EmitEvent 返回 0 时框架只记日志。此处绝不报 completed：报价单存在的意义
		// 就是触发 agent，报告成功却发不出事件是最坏的静默失败。状态尚未落库，
		// 所以这次调用可以整体重试（幂等键不变即安全）。
		return nil, eventEmitFailed(rec.QuoteNo)
	}
	s.lastEventSeq = seq
	s.byNo[rec.QuoteNo] = rec
	log.Printf("quote: 报价单 %s 已创建（%s），%s 已入发件箱（序号 %d）", rec.QuoteNo, rec.QuoteID, eventQuoteCompleted, seq)
	return mustJSON(createQuoteOutput{QuoteID: rec.QuoteID, Status: rec.Status}), nil
}

func (s *quoteStore) get(args json.RawMessage) (json.RawMessage, *protocol.Error) {
	var in getQuoteInput
	if perr := decodeArgs(args, &in); perr != nil {
		return nil, perr
	}
	if in.QuoteNo == "" {
		return nil, schemaMismatch("quoteNo 不能为空")
	}
	s.mu.Lock()
	rec, ok := s.byNo[in.QuoteNo]
	s.mu.Unlock()
	if !ok {
		// 未知单号是业务拒绝而不是 404：路径上的资源是这个动作，不是某张单。
		return nil, unknownQuote(in.QuoteNo)
	}
	return mustJSON(rec), nil
}

// dryRun 是 module.DryRun：只做校验，绝不建单、绝不发事件。
func (s *quoteStore) dryRun(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
	if actionID != actionCreateStandardPartQuote {
		// 框架只会把声明 supported=true 的动作转发到这里；走到这里是接线错了。
		return nil, contractViolation("DRY_RUN_UNSUPPORTED", "报价模块不为动作 "+actionID+" 提供预演")
	}
	var in createQuoteInput
	if perr := decodeArgs(args, &in); perr != nil {
		return nil, perr
	}
	if perr := validateCreate(in); perr != nil {
		return nil, perr
	}
	// 建单没有可比的数值前后值，所以只给结论不给 diff（DryRunReport.diff 可选，
	// 见 plan.schema.json#/$defs/DryRunReport）。
	return mustJSON(dryRunReport{Status: statusVerified}), nil
}

// lastSeq 返回最近一次成功入箱的序号；0 表示发件箱里还没有本模块的事件。
func (s *quoteStore) lastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastEventSeq
}

// ---------- 请求/响应结构 ----------

type quoteLine struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// quoteRecord 是台账里的一张报价单，也是 quote.GetQuote 的出参。
type quoteRecord struct {
	QuoteNo string      `json:"quoteNo"`
	QuoteID string      `json:"quoteId"`
	Status  string      `json:"status"`
	Lines   []quoteLine `json:"lines"`
}

type createQuoteInput struct {
	QuoteNo string      `json:"quoteNo"`
	Lines   []quoteLine `json:"lines"`
}

type createQuoteOutput struct {
	QuoteID string `json:"quoteId"`
	Status  string `json:"status"`
}

type getQuoteInput struct {
	QuoteNo string `json:"quoteNo"`
}

// quoteCompletedPayload 是 quote.completed 的载荷（事件契约 type+version 定义）：
// 只陈述事实（哪张单、哪几行怎么报），不含任何动作暗示（设计增量 §2.2 的反模式判据）。
type quoteCompletedPayload struct {
	QuoteNo string      `json:"quoteNo"`
	QuoteID string      `json:"quoteId"`
	Lines   []quoteLine `json:"lines"`
}

// dryRunReport 对应 plan.schema.json#/$defs/DryRunReport（additionalProperties=false，
// 所以只给 status）。
type dryRunReport struct {
	Status string `json:"status"`
}

// ---------- 事件 ----------

// buildQuoteCompleted 把一张报价单编成 quote.completed 的信封素材。
// 载荷与 subject 只含字符串与整数切片，编组失败意味着进程内状态已坏，故按契约
// 违反上报而不是硬发一个半成品信封。
func buildQuoteCompleted(rec quoteRecord) (subject, payload json.RawMessage, perr *protocol.Error) {
	payload, err := json.Marshal(quoteCompletedPayload{
		QuoteNo: rec.QuoteNo,
		QuoteID: rec.QuoteID,
		Lines:   rec.Lines,
	})
	if err != nil {
		return nil, nil, contractViolation("EVENT_UNENCODABLE", "报价单载荷无法编组："+err.Error())
	}
	subject, err = json.Marshal(moduleID + ":" + quoteType + "/" + rec.QuoteNo)
	if err != nil {
		return nil, nil, contractViolation("EVENT_UNENCODABLE", "报价单 subject 无法编组："+err.Error())
	}
	return subject, payload, nil
}

// eventIDForQuote 由业务键派生事件 id：同一张报价单永远对应同一个 evt_ 标识。
// 这样即使调用方因重试又走到发件箱，框架的「同 eventId 且输入相同」去重也只会
// 留下一条；小写十六进制满足 domain-event.schema.json 的 ^evt_[A-Za-z0-9_-]{4,}$。
func eventIDForQuote(quoteNo string) string {
	sum := sha256.Sum256([]byte(moduleID + "\x00" + eventQuoteCompleted + "\x00" + quoteNo))
	return "evt_" + hex.EncodeToString(sum[:12])
}

// correlationIDFor 是贯穿整条业务链的关联标识（common.schema.json#/$defs/CorrelationId）。
// 同一笔报价的 Plan 与审计会复用它，这是跨契约串联的唯一手段（§4.0）。
func correlationIDFor(quoteNo string) string {
	return "cor:" + moduleID + "/" + quoteNo
}

// eventEmitFailed 是「事件没入箱」的唯一报错。归 retryable：入箱失败只可能来自
// 信封 bug 或进程内状态异常，幂等键不变的重试是安全的。
func eventEmitFailed(quoteNo string) *protocol.Error {
	e := protocol.NewError(protocol.ClassRetryable, "EVENT_EMIT_FAILED",
		fmt.Sprintf("报价单 %q 的 %s 未能写入发件箱（EmitEvent 返回 0，原因见模块日志），本次调用不成立", quoteNo, eventQuoteCompleted))
	e.Details = map[string]any{"quoteNo": quoteNo, "event": eventQuoteCompleted}
	return &e
}

// ---------- 工具 ----------

// decodeArgs 反序列化入参。未知字段按契约忽略——这正是 interfaces.md §3.2 的要求，
// 所以这里显式不使用 DisallowUnknownFields。
func decodeArgs(args json.RawMessage, v any) *protocol.Error {
	if err := json.Unmarshal(args, v); err != nil {
		return schemaMismatch("入参结构与 inputSchema 不符：" + err.Error())
	}
	return nil
}

// validateCreate 落地 schema 里 minLength/minItems/minimum 与事件寻址词法对应的人工判定。
// 框架只保证 required 字段存在，类型与取值都在这里判（与 cmd/inventory 同一分工）。
func validateCreate(in createQuoteInput) *protocol.Error {
	if in.QuoteNo == "" {
		return schemaMismatch("quoteNo 不能为空")
	}
	if !entityIDPattern.MatchString(in.QuoteNo) {
		return schemaMismatch(fmt.Sprintf(
			"quoteNo %q 不能含空白、冒号或斜杠：事件 subject 用 module:Type/id 简写寻址（common.schema.json#/$defs/EntityRefString）",
			in.QuoteNo))
	}
	if len(in.Lines) == 0 {
		// 空明细的报价单没有业务含义，转给 agent 只会让它无从规划：这是业务拒绝
		// （可重新规划），不是报文错误（errors.md §4）。
		return businessRejection("EMPTY_LINES", "lines 不能为空：报价单至少要有一行明细")
	}
	for i, line := range in.Lines {
		if strings.TrimSpace(line.SKU) == "" {
			return schemaMismatch(fmt.Sprintf("lines[%d].sku 不能为空", i))
		}
		if line.Qty <= 0 {
			e := businessRejection("INVALID_QTY", fmt.Sprintf("lines[%d].qty 必须是正整数，收到 %d", i, line.Qty))
			e.Details = map[string]any{"index": i, "qty": line.Qty}
			return e
		}
	}
	return nil
}

func contractViolation(code, message string) *protocol.Error {
	e := protocol.NewError(protocol.ClassContractViolation, code, message)
	return &e
}

func schemaMismatch(message string) *protocol.Error {
	return contractViolation("SCHEMA_MISMATCH", message)
}

func businessRejection(code, message string) *protocol.Error {
	e := protocol.NewError(protocol.ClassBusinessRejection, code, message)
	return &e
}

func unknownQuote(quoteNo string) *protocol.Error {
	e := protocol.NewError(protocol.ClassBusinessRejection, "UNKNOWN_QUOTE",
		fmt.Sprintf("报价模块里没有报价单 %q", quoteNo))
	e.Details = map[string]any{"quoteNo": quoteNo}
	return &e
}

// mustJSON 编组本文件的出参结构。这些类型都是 JSON 安全类型，编组不会失败；
// 真失败就 panic——静默返回半截报文比崩溃更难查（net/http 会兜住并记日志）。
func mustJSON(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("quote: 出参编组失败：%v", err))
	}
	return body
}
