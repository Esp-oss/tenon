// Command inventory 是 Tenon 的库存模块（HTTP 侧车，实现模块侧六个端点）。
//
// 它在 demo/mini-erp 的纵切面里承担协议最重的写路径：幂等、补偿声明、
// 模块前置条件（G4）、模块侧审计、dry-run。业务状态是内存里的
// SKU → 在库量，故意不持久化——这个 demo 要证明的是协议行为，不是库存记账。
//
// 环境变量（监听与审计/密钥由 internal/module 读取）：
//
//	PORT                     监听端口或地址；未设置时用本模块约定的 8081
//	TENON_MODULE_ID          manifest.module.id 与审计 module 字段的覆盖值
//	TENON_HMAC_SECRET_FILE   事件投递验签密钥文件（读不到就拒绝投递）
//	TENON_MODULE_AUDIT_PATH  模块侧审计 JSONL，缺省 ./audit-inventory.jsonl
//	TENON_INVENTORY_SEED     初始库存，格式 SKU:qty,SKU:qty，缺省 STD-1001:120,STD-1002:3
//
// 本模块声明了 stock.deducted 与 stock.insufficient 两个事件类型：它们出现在
// manifest 的能力摘要里。0.1 没有定义「模块 → 宿主」的推送通道（interfaces.md §5：
// 模块不主动调宿主），所以这里只做声明，不伪造投递；宿主投递进来的事件走
// POST /tenon/events，由框架验签去重。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"tenon.dev/internal/module"
	"tenon.dev/internal/protocol"
)

const (
	moduleID      = "inventory"
	moduleTitle   = "库存"
	moduleVersion = "0.1.0"

	// defaultPort 是 interfaces.md §1 给 inventory 约定的端口；PORT 可覆盖。
	defaultPort = "8081"

	actionGetStock     = "inventory.GetStock"
	actionDeductStock  = "inventory.DeductStock"
	actionReleaseStock = "inventory.ReleaseStock"

	// envSeed 覆盖初始库存，便于演示「库存不足 → 转采购」的分支。
	envSeed     = "TENON_INVENTORY_SEED"
	defaultSeed = "STD-1001:120,STD-1002:3"

	// 预演结论（plan.schema.json#/$defs/DryRunStatus）。只有 verified 允许自动执行。
	statusVerified = "verified"
	statusUnsafe   = "unsafe"
)

func main() {
	store, err := newStoreFromEnv()
	if err != nil {
		log.Fatalf("inventory: 初始库存不可用：%v", err)
	}
	mod := &module.Module{
		ID:       moduleID,
		Title:    moduleTitle,
		Version:  moduleVersion,
		TestMode: os.Getenv("TENON_TEST_MODE") == "1", // D1 的 A/B 两轮要靠 /tenon/admin/reset 造同输入
		Actions: []module.Action{
			getStockAction(),
			deductStockAction(),
			releaseStockAction(),
		},
		Events: []string{"stock.deducted", "stock.insufficient"},
		Handle: store.handle,
		DryRun: store.dryRun,
	}
	if err := mod.Validate(); err != nil {
		log.Fatalf("inventory: 模块定义自检失败：%v", err)
	}
	addr := mod.Addr()
	if _, ok := os.LookupEnv(module.EnvPort); !ok {
		addr = ":" + defaultPort
	}
	if err := mod.ListenAndServeAt(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("inventory: %v", err)
	}
}

// ---------- 动作契约 ----------

func getStockAction() module.Action {
	return module.Action{
		ID:              actionGetStock,
		Version:         "0.1.0",
		Title:           "查询在库量",
		SummaryTemplate: "查询 {sku} 在库量",
		Kind:            "read",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["sku"],
  "properties": {
    "sku": {"type": "string", "minLength": 1}
  }
}`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["onHand"],
  "properties": {
    "onHand": {"type": "integer", "minimum": 0}
  }
}`),
		Risk:      module.Risk{Level: "low", Reversible: true},
		TimeoutMs: 30000,
		// 读动作无副作用，无需预演（semantics.md §6.3：只含 read 的计划视为 verified）。
		DryRun: module.DryRunCapability{Supported: false},
	}
}

func deductStockAction() module.Action {
	return module.Action{
		ID:              actionDeductStock,
		Version:         "0.1.0",
		Title:           "扣减库存",
		SummaryTemplate: "扣减 {sku} 库存 {qty} 件（单据 {referenceNo}）",
		Kind:            "write",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["sku", "qty", "referenceNo"],
  "properties": {
    "sku": {"type": "string", "minLength": 1},
    "qty": {"type": "integer", "minimum": 1},
    "referenceNo": {"type": "string", "minLength": 1}
  }
}`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["onHand"],
  "properties": {
    "onHand": {"type": "integer", "minimum": 0}
  }
}`),
		Risk: module.Risk{Level: "medium", Reversible: true},
		// keyFields 是 required 的子集（semantics.md §5.3）：同一个 referenceNo
		// 扣同一个 SKU 只允许生效一次。
		Idempotency: &module.Idempotency{
			KeyFields:         []string{"sku", "referenceNo"},
			Scope:             "module",
			DuplicateBehavior: "return_first_result",
		},
		// 补偿按逆序执行，失败步骤本身不补偿（semantics.md §7.1）。
		Compensation: &module.Compensation{
			Action: actionReleaseStock,
			ArgMap: map[string]string{
				"sku":         "$in.sku",
				"qty":         "$in.qty",
				"referenceNo": "$in.referenceNo",
			},
		},
		TimeoutMs: 30000,
		DryRun:    module.DryRunCapability{Supported: true},
	}
}

func releaseStockAction() module.Action {
	return module.Action{
		ID:              actionReleaseStock,
		Version:         "0.1.0",
		Title:           "回补库存（扣减的补偿动作）",
		SummaryTemplate: "回补 {sku} 库存 {qty} 件（单据 {referenceNo}）",
		Kind:            "write",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["sku", "qty", "referenceNo"],
  "properties": {
    "sku": {"type": "string", "minLength": 1},
    "qty": {"type": "integer", "minimum": 1},
    "referenceNo": {"type": "string", "minLength": 1}
  }
}`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["onHand"],
  "properties": {
    "onHand": {"type": "integer", "minimum": 0}
  }
}`),
		Risk: module.Risk{Level: "medium", Reversible: true},
		Idempotency: &module.Idempotency{
			KeyFields:         []string{"sku", "referenceNo"},
			Scope:             "module",
			DuplicateBehavior: "return_first_result",
		},
		TimeoutMs: 30000,
		DryRun:    module.DryRunCapability{Supported: true},
	}
}

// ---------- 业务状态 ----------

// store 是内存库存。
//
// 除框架层的 Idempotency-Key 去重外，业务层再按声明的 keyFields（sku+referenceNo）
// 去重：宿主若因为崩溃恢复换了一个幂等键重发同一笔业务，重复副作用仍不会发生。
// 两层去重的留存期在 demo 里都是「进程生命周期」——真实实现必须覆盖崩溃恢复窗口
// 加至少一次日切对账（semantics.md §5.5）。
type store struct {
	mu       sync.Mutex
	onHand   map[string]int
	deducted map[string]int // businessKey → 首次扣减后的在库量
	released map[string]int // businessKey → 首次回补后的在库量
}

func newStoreFromEnv() (*store, error) {
	seed := strings.TrimSpace(os.Getenv(envSeed))
	if seed == "" {
		seed = defaultSeed
	}
	onHand := make(map[string]int)
	for _, pair := range strings.Split(seed, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		sku, qtyText, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, fmt.Errorf("初始库存项 %q 不是 SKU:qty 形式", pair)
		}
		sku = strings.TrimSpace(sku)
		qtyText = strings.TrimSpace(qtyText)
		if sku == "" {
			return nil, fmt.Errorf("初始库存项 %q 的 SKU 为空", pair)
		}
		qty, err := strconv.Atoi(qtyText)
		if err != nil || qty < 0 {
			return nil, fmt.Errorf("SKU %s 的数量 %q 不是非负整数", sku, qtyText)
		}
		if _, dup := onHand[sku]; dup {
			return nil, fmt.Errorf("初始库存里 SKU %s 重复", sku)
		}
		onHand[sku] = qty
	}
	if len(onHand) == 0 {
		return nil, errors.New("初始库存为空")
	}
	return &store{
		onHand:   onHand,
		deducted: make(map[string]int),
		released: make(map[string]int),
	}, nil
}

// handle 是 module.Handle：入参已由框架校验过 required 字段，这里仍做类型与取值判定。
func (s *store) handle(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
	switch actionID {
	case actionGetStock:
		return s.getStock(args)
	case actionDeductStock:
		return s.applyStock(args, false)
	case actionReleaseStock:
		return s.applyStock(args, true)
	default:
		return nil, contractViolation("UNKNOWN_ACTION", "库存模块不提供动作 "+actionID)
	}
}

func (s *store) getStock(args json.RawMessage) (json.RawMessage, *protocol.Error) {
	var in getStockInput
	if perr := decodeArgs(args, &in); perr != nil {
		return nil, perr
	}
	if strings.TrimSpace(in.SKU) == "" {
		return nil, schemaMismatch("sku 不能为空")
	}
	s.mu.Lock()
	onHand, ok := s.onHand[in.SKU]
	s.mu.Unlock()
	if !ok {
		return nil, unknownSKU(in.SKU)
	}
	return mustJSON(getStockOutput{OnHand: onHand}), nil
}

// applyStock 同时实现扣减与回补：两者只差方向，业务级幂等口径完全一致。
func (s *store) applyStock(args json.RawMessage, release bool) (json.RawMessage, *protocol.Error) {
	var in stockWriteInput
	if perr := decodeArgs(args, &in); perr != nil {
		return nil, perr
	}
	if perr := validateStockWrite(in); perr != nil {
		return nil, perr
	}
	key := businessKey(in.SKU, in.ReferenceNo)

	s.mu.Lock()
	defer s.mu.Unlock()

	applied := s.deducted
	if release {
		applied = s.released
	}
	if onHand, ok := applied[key]; ok {
		// 同一笔业务重复提交：返回首次结果，不重复改动库存。
		return mustJSON(stockWriteOutput{OnHand: onHand}), nil
	}
	onHand, ok := s.onHand[in.SKU]
	if !ok {
		return nil, unknownSKU(in.SKU)
	}
	next := onHand - in.Qty
	if release {
		next = onHand + in.Qty
	}
	// G4：前置条件永远由模块判定，agent 不可绕过（demo/mini-erp/README.md）。
	// 库存不足是业务拒绝而不是错误处理：agent 应据此重新规划（errors.md §4）。
	if next < 0 {
		return nil, stockInsufficient(onHand, in.Qty)
	}
	s.onHand[in.SKU] = next
	applied[key] = next
	return mustJSON(stockWriteOutput{OnHand: next}), nil
}

// dryRun 是 module.DryRun：只读当前状态算预期差异，绝不改动任何状态。
func (s *store) dryRun(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
	switch actionID {
	case actionDeductStock:
		return s.dryRunStock(args, false)
	case actionReleaseStock:
		return s.dryRunStock(args, true)
	default:
		// 框架只会对声明 supported=true 的动作转发到这里；走到这里是接线错了。
		return nil, contractViolation("DRY_RUN_UNSUPPORTED", "库存模块不为动作 "+actionID+" 提供预演")
	}
}

func (s *store) dryRunStock(args json.RawMessage, release bool) (json.RawMessage, *protocol.Error) {
	var in stockWriteInput
	if perr := decodeArgs(args, &in); perr != nil {
		return nil, perr
	}
	if perr := validateStockWrite(in); perr != nil {
		return nil, perr
	}
	key := businessKey(in.SKU, in.ReferenceNo)

	s.mu.Lock()
	defer s.mu.Unlock()

	applied := s.deducted
	if release {
		applied = s.released
	}
	if _, ok := applied[key]; ok {
		// 这笔业务已经生效过：真正执行时会走幂等回放，不会再有变化。
		return mustJSON(dryRunReport{Status: statusVerified}), nil
	}
	onHand, ok := s.onHand[in.SKU]
	if !ok {
		// 预演不抛错：未知 SKU 意味着计划本身可疑，判 unsafe 让宿主强制审批
		// （fail-closed：风险不许被静默放大，semantics.md §6.2）。
		return mustJSON(dryRunReport{Status: statusUnsafe}), nil
	}
	next := onHand - in.Qty
	if release {
		next = onHand + in.Qty
	}
	if next < 0 {
		return mustJSON(dryRunReport{Status: statusUnsafe}), nil
	}
	return mustJSON(dryRunReport{
		Status: statusVerified,
		Diff: []diffItem{{
			Entity: entityRef(in.SKU),
			Field:  "onHand",
			From:   onHand,
			To:     next,
		}},
	}), nil
}

// ---------- 请求/响应结构 ----------

type getStockInput struct {
	SKU string `json:"sku"`
}

type getStockOutput struct {
	OnHand int `json:"onHand"`
}

type stockWriteInput struct {
	SKU         string `json:"sku"`
	Qty         int    `json:"qty"`
	ReferenceNo string `json:"referenceNo"`
}

type stockWriteOutput struct {
	OnHand int `json:"onHand"`
}

// dryRunReport 对应 plan.schema.json#/$defs/DryRunReport（additionalProperties=false，
// 所以只给 status 与 diff；schema 没有 reason 字段，多余的字段会让报文不合法）。
type dryRunReport struct {
	Status string     `json:"status"`
	Diff   []diffItem `json:"diff,omitempty"`
}

type diffItem struct {
	Entity string `json:"entity"`
	Field  string `json:"field"`
	From   int    `json:"from"`
	To     int    `json:"to"`
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

// validateStockWrite 做 schema 里 minimum/minLength 对应的人工判定。
func validateStockWrite(in stockWriteInput) *protocol.Error {
	if strings.TrimSpace(in.SKU) == "" {
		return schemaMismatch("sku 不能为空")
	}
	if strings.TrimSpace(in.ReferenceNo) == "" {
		return schemaMismatch("referenceNo 不能为空")
	}
	if in.Qty < 1 {
		return contractViolation("INVALID_QUANTITY", fmt.Sprintf("qty 必须是正整数，收到 %d", in.Qty))
	}
	return nil
}

func businessKey(sku, referenceNo string) string {
	// 用不可出现在 SKU / 单号里的 NUL 作分隔，避免 "a"+"b|c" 与 "a|b"+"c" 撞键。
	return sku + "\x00" + referenceNo
}

func entityRef(sku string) string { return moduleID + ":SKU/" + sku }

func contractViolation(code, message string) *protocol.Error {
	e := protocol.NewError(protocol.ClassContractViolation, code, message)
	return &e
}

func schemaMismatch(message string) *protocol.Error {
	return contractViolation("SCHEMA_MISMATCH", message)
}

func unknownSKU(sku string) *protocol.Error {
	e := protocol.NewError(protocol.ClassBusinessRejection, "UNKNOWN_SKU", fmt.Sprintf("库存里没有 SKU %q", sku))
	e.Details = map[string]any{"sku": sku}
	return &e
}

// stockInsufficient 的消息必须带关键数值（errors.md §6：message 给人看，含可用量与需求量）。
func stockInsufficient(available, requested int) *protocol.Error {
	e := protocol.NewError(protocol.ClassBusinessRejection, "STOCK_INSUFFICIENT",
		fmt.Sprintf("可用 %d，需求 %d", available, requested))
	e.Details = map[string]any{"available": available, "requested": requested}
	return &e
}

// mustJSON 编组本文件的出参结构。这些类型都是 JSON 安全类型，编组不会失败；
// 真失败就 panic——静默返回半截报文比崩溃更难查（net/http 会兜住并记日志）。
func mustJSON(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("inventory: 出参编组失败：%v", err))
	}
	return body
}
