// Command llmstub 是 OpenAI 兼容的最小桩模型，**只用于容器冒烟**。
//
// 它不做任何推理：从 agent 的 user prompt 里抓出事件锚点（eventId / correlationId），
// 原样写回一份固定的 Plan。存在的理由很具体——cmd/agent 的策略是「缺 LLM key 或模型
// 不可用即失败」（设计 §5：没有模型就不可能规划，静默降级只会让它在没规划的情况下
// 假装已规划），而本机没有真凭据。要么给它一个水龙头，要么整条链路只能空转。
//
// **它不是模型，也不是降级方案**：固定 plan 意味着「规划」这一环在冒烟里被短路了。
// 所以：
//   - compose 用 profile `smoke` 隔离它，正常 `up` 不会带上；
//   - 它不进任何生产制品，真实部署把 TENON_LLM_BASE_URL 指向真模型即可；
//   - 它返回的 plan 只在冒烟用 SKU/数量上有意义，不代表业务判断。
//
// 端点（internal/llm 只会 POST {TENON_LLM_BASE_URL}/chat/completions，其它都是附加）：
//
//	POST /v1/chat/completions   OpenAI 兼容的补全响应，content 是固定 Plan JSON
//	GET  /healthz               存活探针
//
// 环境变量：PORT（缺省 8099，与 cmd/* 的习惯一致）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort = "8099"

	completionsPath = "/v1/chat/completions"
	healthPath      = "/healthz"

	// maxBodyBytes 与 internal/llm 的响应上限同量级：prompt 是 KB 量级，1MiB 足够，
	// 超了就直接拒——桩模型不该成为放大器。
	maxBodyBytes = 1 << 20

	// 固定计划的业务参数。STD-1001 的种子库存是 120（cmd/inventory 的缺省 seed），
	// 扣 12 件后 108；qty>10 命中 config/policies.dev.json 的
	// pol-inventory-deduct-approve（approvers: role:warehouse_manager），所以这一轮
	// **必然挂起等人批准**——那正是冒烟要走的路径，不是巧合。
	planSKU    = "STD-1001"
	planQty    = 12
	planOnHand = 120
	planAction = "inventory.DeductStock"
	planRisk   = "medium"
	planDryRun = "pending" // 桩没有预演过：真预演由 agent 问模块要，这里不冒充 verified
	planStepID = "s1"
)

// 锚点只从 prompt 文本里取：internal/agent 的 userPrompt 把事件渲染成 JSON，
// trigger 要求「逐字照抄输入里的锚点」，所以桩必须把这两个值原样搬回去。
var (
	eventIDRe       = regexp.MustCompile(`"eventId"\s*:\s*"([^"]+)"`)
	correlationIDRe = regexp.MustCompile(`"correlationId"\s*:\s*"([^"]+)"`)
	// planId 的字符集是 ^pln_[A-Za-z0-9_-]{4,}$（plan.schema.json），事件 id 里
	// 可能带别的字符，一律折叠成下划线。
	unsafePlanIDChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// ---------- OpenAI 兼容报文（只声明本桩用到的字段） ----------

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ---------- Plan（plan.schema.json 的字段子集，形状必须完整） ----------

type planTrigger struct {
	EventID       string `json:"eventId"`
	CorrelationID string `json:"correlationId"`
}

type planStep struct {
	StepID    string          `json:"stepId"`
	Action    string          `json:"action"`
	Args      json.RawMessage `json:"args"`
	DependsOn []string        `json:"dependsOn"`
	Kind      string          `json:"kind"`
}

type expectedDiff struct {
	Entity string `json:"entity"`
	Field  string `json:"field"`
	From   int    `json:"from"`
	To     int    `json:"to"`
}

type dryRunReport struct {
	Status string `json:"status"`
}

type plan struct {
	PlanID        string         `json:"planId"`
	Supersedes    *string        `json:"supersedes"`
	Trigger       planTrigger    `json:"trigger"`
	Steps         []planStep     `json:"steps"`
	ExpectedDiff  []expectedDiff `json:"expectedDiff"`
	DryRun        dryRunReport   `json:"dryRun"`
	EstimatedRisk string         `json:"estimatedRisk"`
	Rationale     string         `json:"rationale"`
}

func main() {
	addr := ":" + listenPort()
	mux := http.NewServeMux()
	mux.HandleFunc(completionsPath, handleCompletions)
	mux.HandleFunc(healthPath, handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "llmstub 只提供 "+completionsPath+" 与 "+healthPath)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("llmstub: 桩模型监听 %s（POST %s；这不是真模型，只用于冒烟）", addr, completionsPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("llmstub: %v", err)
	}
}

// listenPort 解析监听端口：PORT 可给纯端口（缺省 8099）。
func listenPort() string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		return defaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		// 端口不合法就起不来：静默回落到缺省会让编排里写的端口与实际监听不一致。
		log.Fatalf("llmstub: PORT=%q 不是合法端口号", port)
	}
	return port
}

// handleHealth 是存活探针：不检查任何依赖。
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// handleCompletions 是唯一被 agent 调用的端点：解析 prompt → 造固定 plan → 包成
// OpenAI 的补全响应。
func handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读请求体失败："+err.Error())
		return
	}
	if len(raw) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体超过 1MiB")
		return
	}

	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	user := lastUserContent(req.Messages)

	eventID, ok := matchAnchor(eventIDRe, user)
	if !ok {
		// 不猜锚点：编一个 eventId 会让审计里的 trigger 与真实事件对不上，
		// 而那种错误在报告里看起来像「策略拒绝」。宁可把 prompt 形状变化暴露出来。
		log.Printf("llmstub: prompt 里没有 eventId 锚点（prompt 形状变了吗？）")
		writeError(w, http.StatusUnprocessableEntity,
			"prompt 里找不到 eventId 锚点：桩模型只照抄锚点，不编造")
		return
	}
	correlationID, ok := matchAnchor(correlationIDRe, user)
	if !ok {
		log.Printf("llmstub: prompt 里没有 correlationId 锚点")
		writeError(w, http.StatusUnprocessableEntity,
			"prompt 里找不到 correlationId 锚点：桩模型只照抄锚点，不编造")
		return
	}

	p := buildPlan(eventID, correlationID)
	content, err := json.Marshal(p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "编组 plan 失败："+err.Error())
		return
	}

	log.Printf("llmstub: eventId=%s correlationId=%s → planId=%s action=%s qty=%d",
		eventID, correlationID, p.PlanID, planAction, planQty)

	resp := chatResponse{
		ID:      "chatcmpl-llmstub",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: string(content)},
			FinishReason: "stop",
		}},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("llmstub: 写响应失败：%v", err)
	}
}

// lastUserContent 取最后一条 user 消息：internal/llm 只发 system + user 两条，
// 重试时也是同一形状，取最后一条即可。
func lastUserContent(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(msgs[i].Role), "user") {
			return msgs[i].Content
		}
	}
	return ""
}

// matchAnchor 取第一个捕获组。
func matchAnchor(re *regexp.Regexp, text string) (string, bool) {
	m := re.FindStringSubmatch(text)
	if len(m) < 2 || strings.TrimSpace(m[1]) == "" {
		return "", false
	}
	return m[1], true
}

// buildPlan 造一份固定 Plan：一步 DeductStock，锚点逐字照抄输入。
//
// planId / referenceNo 由 eventId 确定性派生，于是同一个请求重放会落到同一个
// planId 与同一个业务键（sku+referenceNo），模块侧的业务级幂等会把它吸收掉——
// 桩不能比真模型更容易造成重复扣减。
func buildPlan(eventID, correlationID string) plan {
	suffix := newPlanSuffix(eventID)

	return plan{
		PlanID:     "pln_" + suffix,
		Supersedes: nil,
		Trigger: planTrigger{
			EventID:       eventID,
			CorrelationID: correlationID,
		},
		Steps: []planStep{{
			StepID: planStepID,
			Action: planAction,
			Args: json.RawMessage(fmt.Sprintf(
				`{"sku":%q,"qty":%d,"referenceNo":%q}`,
				planSKU, planQty, "REF-"+strings.ToUpper(suffix))),
			// 空数组而不是 null：schema 要求 dependsOn 是数组，nil 会编组成 null。
			DependsOn: []string{},
			Kind:      "write",
		}},
		ExpectedDiff: []expectedDiff{{
			Entity: "inventory:SKU/" + planSKU,
			Field:  "onHand",
			From:   planOnHand,
			To:     planOnHand - planQty,
		}},
		DryRun:        dryRunReport{Status: planDryRun},
		EstimatedRisk: planRisk,
		Rationale: "桩模型（非真模型）：固定扣减 " + strconv.Itoa(planQty) + " 件 " + planSKU +
			"，用于容器冒烟；规划这一环在冒烟里被短路了",
	}
}

// newPlanSuffix 把 eventId 折成 planId 的合法后缀（^[A-Za-z0-9_-]{4,}$）。
func newPlanSuffix(eventID string) string {
	suffix := unsafePlanIDChars.ReplaceAllString(strings.TrimPrefix(eventID, "evt_"), "_")
	suffix = strings.Trim(suffix, "_")
	if len(suffix) < 4 {
		// 太短就补齐：planId 的 schema 要求至少 4 个字符。
		suffix = (suffix + "stub")[:4]
	}
	return suffix
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
