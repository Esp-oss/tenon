// Command procurement 是 Tenon 的采购模块（HTTP 侧车，实现模块侧六个端点）。
//
// 它是 demo/mini-erp 纵切面的**分支路径**：inventory.DeductStock 返回
// business_rejection/STOCK_INSUFFICIENT 之后，由 agent 自己重新规划出
// procurement.CreatePurchaseRequest。本模块不感知、也不映射那个错误码——
// 「错误码 → action」的硬编码映射会让差异测试（D1）失去意义（errors.md §4）。
//
// 它同时是一致性用例 C8 的载体：CreatePurchaseRequest 故意**不实现** dry-run
// （dryRun.supported=false 且 Module.DryRun 为 nil），用来验证「未实现预演时
// :dry-run 返回 404 空体，宿主按 unsupported 处理并强制审批」。
//
// 环境变量（监听与审计/密钥由 internal/module 读取）：
//
//	PORT                     监听端口或地址；未设置时用本模块约定的 8082
//	TENON_MODULE_ID          manifest.module.id 与审计 module 字段的覆盖值
//	TENON_HMAC_SECRET_FILE   事件投递验签密钥文件（读不到就拒绝投递）
//	TENON_MODULE_AUDIT_PATH  模块侧审计 JSONL，缺省 ./audit-procurement.jsonl
//
// purchase.requested 只出现在 manifest 的能力摘要里：0.1 没有定义「模块 → 宿主」
// 的推送通道（interfaces.md §5），这里不伪造投递。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"tenon.dev/internal/module"
	"tenon.dev/internal/protocol"
)

const (
	moduleID      = "procurement"
	moduleTitle   = "采购"
	moduleVersion = "0.1.0"

	// defaultPort 是 interfaces.md §1 给 procurement 约定的端口；PORT 可覆盖。
	defaultPort = "8082"

	actionCreatePurchaseRequest = "procurement.CreatePurchaseRequest"
)

func main() {
	store := &store{byReference: make(map[string]string)}
	mod := &module.Module{
		ID:       moduleID,
		Title:    moduleTitle,
		Version:  moduleVersion,
		TestMode: os.Getenv("TENON_TEST_MODE") == "1", // D1 的 A/B 两轮要靠 /tenon/admin/reset 造同输入
		Actions:  []module.Action{createPurchaseRequestAction()},
		Events:   []string{"purchase.requested"},
		Handle:   store.handle,
		// DryRun 刻意留空：C8 要的是「模块没实现预演」这个真实状态，
		// 而不是实现了却报告 unsupported。
		DryRun: nil,
	}
	if err := mod.Validate(); err != nil {
		log.Fatalf("procurement: 模块定义自检失败：%v", err)
	}
	addr := mod.Addr()
	if _, ok := os.LookupEnv(module.EnvPort); !ok {
		addr = ":" + defaultPort
	}
	if err := mod.ListenAndServeAt(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("procurement: %v", err)
	}
}

func createPurchaseRequestAction() module.Action {
	return module.Action{
		ID:              actionCreatePurchaseRequest,
		Version:         "0.1.0",
		Title:           "创建采购申请",
		SummaryTemplate: "缺 {qty} 件 {sku}，发起采购申请（单据 {referenceNo}）",
		RequiredArgs:    []string{"sku", "qty", "referenceNo"},
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
  "required": ["requestNo"],
  "properties": {
    "requestNo": {"type": "string", "minLength": 1}
  }
}`),
		Risk: module.Risk{Level: "medium", Reversible: true},
		// 幂等键取自 required 字段（semantics.md §5.3）：同一张单据的同一个 SKU
		// 只应产生一张采购申请。
		Idempotency: &module.Idempotency{
			KeyFields:         []string{"sku", "referenceNo"},
			Scope:             "module",
			DuplicateBehavior: "return_first_result",
		},
		TimeoutMs: 30000,
		// 不实现预演：这是 C8 的一致性载体，不是遗漏。
		DryRun: module.DryRunCapability{Supported: false},
	}
}

// store 是内存态的采购申请台账，故意不持久化（demo 关心协议行为）。
type store struct {
	mu sync.Mutex
	// seq 只用于生成单号；重复提交走 byReference 回放，不会消耗新号。
	seq         int
	byReference map[string]string // businessKey → 首次生成的采购申请号
}

func (s *store) handle(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
	switch actionID {
	case actionCreatePurchaseRequest:
		return s.create(args)
	default:
		return nil, contractViolation("UNKNOWN_ACTION", "采购模块不提供动作 "+actionID)
	}
}

func (s *store) create(args json.RawMessage) (json.RawMessage, *protocol.Error) {
	var in createInput
	if err := json.Unmarshal(args, &in); err != nil {
		// 未知字段按契约忽略（interfaces.md §3.2）：不设 DisallowUnknownFields。
		return nil, schemaMismatch("入参结构与 inputSchema 不符：" + err.Error())
	}
	if strings.TrimSpace(in.SKU) == "" {
		return nil, schemaMismatch("sku 不能为空")
	}
	if strings.TrimSpace(in.ReferenceNo) == "" {
		return nil, schemaMismatch("referenceNo 不能为空")
	}
	if in.Qty < 1 {
		return nil, contractViolation("INVALID_QUANTITY", fmt.Sprintf("qty 必须是正整数，收到 %d", in.Qty))
	}

	key := businessKey(in.SKU, in.ReferenceNo)
	s.mu.Lock()
	defer s.mu.Unlock()
	if requestNo, ok := s.byReference[key]; ok {
		// 业务级幂等：换一个 Idempotency-Key 重发同一张单据也不会多开一张。
		return mustJSON(createOutput{RequestNo: requestNo}), nil
	}
	s.seq++
	requestNo := fmt.Sprintf("PR-%04d", s.seq)
	s.byReference[key] = requestNo
	return mustJSON(createOutput{RequestNo: requestNo}), nil
}

type createInput struct {
	SKU         string `json:"sku"`
	Qty         int    `json:"qty"`
	ReferenceNo string `json:"referenceNo"`
}

type createOutput struct {
	RequestNo string `json:"requestNo"`
}

func businessKey(sku, referenceNo string) string {
	// 用不可出现在 SKU / 单号里的 NUL 作分隔，避免拼接歧义。
	return sku + "\x00" + referenceNo
}

func contractViolation(code, message string) *protocol.Error {
	e := protocol.NewError(protocol.ClassContractViolation, code, message)
	return &e
}

func schemaMismatch(message string) *protocol.Error {
	return contractViolation("SCHEMA_MISMATCH", message)
}

// mustJSON 编组出参。结构都是 JSON 安全类型，编组不会失败；真失败就 panic，
// 不静默返回半截报文（net/http 会兜住并记日志）。
func mustJSON(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("procurement: 出参编组失败：%v", err))
	}
	return body
}
