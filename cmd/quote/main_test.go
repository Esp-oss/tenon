package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenon.dev/internal/module"
	"tenon.dev/internal/protocol"
)

const (
	createPath   = "/tenon/actions/" + actionCreateStandardPartQuote
	createDryRun = createPath + ":dry-run"
	getPath      = "/tenon/actions/" + actionGetQuote
	eventsOut    = "/tenon/events?direction=out"
	manifestPath = "/.well-known/tenon"
)

// eventTypeLexical 是 domain-event.schema.json 的事件类型词法。EmitEvent 遇到不合
// 词法的 type 会返回 0 且只记日志，所以用例必须独立断言一次。
var eventTypeLexical = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)

// httpClient 带超时：用例不发真实网络，但卡死的请求必须变成失败而不是挂起。
var httpClient = &http.Client{Timeout: 10 * time.Second}

// TestMain 把审计与密钥路径指到用例专属临时目录：写动作会追加模块侧审计，
// 忘了改路径就会往工作区里写 audit-quote.jsonl（本任务只允许出现受版本控制的文件）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tenon-quote-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时目录失败：%v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv(module.EnvAuditPath, filepath.Join(dir, "audit.jsonl"))
	_ = os.Setenv(module.EnvHMACSecretFile, filepath.Join(dir, "absent-secret.txt"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newTestServer 起一个进程内模块服务（httptest，不监听公网）并返回业务状态。
// 模块定义先过 Validate：装配错误会让 Handler 把每个请求都变成 500，那会把
// 契约缺陷伪装成运行时故障。
func newTestServer(t *testing.T) (*httptest.Server, *quoteStore) {
	t.Helper()
	t.Setenv(module.EnvAuditPath, filepath.Join(t.TempDir(), "audit.jsonl"))
	store := newQuoteStore()
	mod := newQuoteModule(store)
	if err := mod.Validate(); err != nil {
		t.Fatalf("模块定义自检未通过：%v", err)
	}
	srv := httptest.NewServer(mod.Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

// request 发一次请求并读回响应体。
//
// 返回 error 而不是直接 Failf：并发用例要在自己的 goroutine 里判定，而
// t.Fatalf 只能从测试主 goroutine 调用。
func request(method, url, body string, headers map[string]string) (int, string, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, "", fmt.Errorf("构造 %s %s 失败：%w", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// 执行端点强制要求版本头（versioning.md §4.2）：缺了它会先拿到
	// 400 VERSION_HEADER_MISSING，用例就测不到业务判定。读路径也一并带上，
	// 与真实调用方一致。
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("%s %s 失败：%w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("读取 %s %s 的响应失败：%w", method, url, err)
	}
	return resp.StatusCode, string(raw), nil
}

// postJSON 发一个 JSON POST，返回状态码与响应原文。
func postJSON(t *testing.T, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	st, raw, err := request(http.MethodPost, url, body, headers)
	if err != nil {
		t.Fatal(err)
	}
	return st, raw
}

// getJSON 发一个 GET，返回状态码与响应原文。
func getJSON(t *testing.T, url string) (int, string) {
	t.Helper()
	st, raw, err := request(http.MethodGet, url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return st, raw
}

// idempotency 给写路径用的请求头。
func idempotency(key string) map[string]string {
	return map[string]string{protocol.HeaderIdempotencyKey: key}
}

// decodeInto 把响应原文解到 v 里；解不动就当成断言失败（顺带打出原文）。
func decodeInto(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("响应不是预期 JSON：%v（原文 %s）", err, raw)
	}
}

// wantError 断言错误信封的分类与错误码。
func wantError(t *testing.T, raw string, class protocol.ErrorClass, code string) protocol.Error {
	t.Helper()
	var env protocol.ErrorEnvelope
	decodeInto(t, raw, &env)
	got := env.TenonError
	if got.Class != class || got.Code != code {
		t.Fatalf("期望 %s/%s，得到 %s/%s（原文 %s）", class, code, got.Class, got.Code, raw)
	}
	if got.Message == "" {
		t.Fatalf("错误报文缺少 message（errors.md §6）：%s", raw)
	}
	if got.Retryable != class.Retryable() {
		t.Fatalf("retryable 应由 class 推导：%s 得到 %v", class, got.Retryable)
	}
	return got
}

// outboxEvent 是发件箱里线程格式的信封（domain-event.schema.json 的 required 全集）。
// 刻意不复用 module.Event：那是 EmitEvent 的入参，缺省字段由框架补，形状不同。
type outboxEvent struct {
	EventID       string          `json:"eventId"`
	Type          string          `json:"type"`
	Version       string          `json:"version"`
	Subject       json.RawMessage `json:"subject"`
	OccurredAt    string          `json:"occurredAt"`
	CausationID   *string         `json:"causationId"`
	CorrelationID string          `json:"correlationId"`
	Payload       json.RawMessage `json:"payload"`
}

// outbox 读出发件箱里的全部事件：事件「发出去了几条」只能这样数，
// 因为 EmitEvent 失败既不报错也不影响 HTTP 状态码。
func outbox(t *testing.T, baseURL string) []outboxEvent {
	t.Helper()
	st, raw := getJSON(t, baseURL+eventsOut)
	if st != http.StatusOK {
		t.Fatalf("读发件箱失败：%d %s", st, raw)
	}
	var page struct {
		Events     []outboxEvent `json:"events"`
		NextCursor string        `json:"nextCursor"`
	}
	decodeInto(t, raw, &page)
	if page.NextCursor == "" {
		t.Fatalf("发件箱响应缺少 nextCursor（响应形状必须与 0.1 一致）：%s", raw)
	}
	return page.Events
}

// quotePayload 是 quote.completed 载荷的断言视图。
type quotePayload struct {
	QuoteNo string      `json:"quoteNo"`
	QuoteID string      `json:"quoteId"`
	Lines   []quoteLine `json:"lines"`
}

// TestQuoteCompletedEmitsEvent 是计划 Task 9 的用例：一次真实建单必须让
// quote.completed 进发件箱，并且载荷带明细。
func TestQuoteCompletedEmitsEvent(t *testing.T) {
	srv, store := newTestServer(t)

	st, body := postJSON(t, srv.URL+createPath,
		`{"quoteNo":"Q-1","lines":[{"sku":"STD-1002","qty":12}]}`,
		idempotency("tenon:t/Q-1"))
	if st != http.StatusOK {
		t.Fatalf("报价失败 %d %s", st, body)
	}

	// 关键断言：EmitEvent 失败时返回 0 且只记日志，HTTP 200 完全可能伴随
	// 「事件根本没入箱」。所以必须看模块拿到的序号。
	if seq := store.lastSeq(); seq <= 0 {
		t.Fatalf("EmitEvent 返回序号 %d（<=0 表示未入箱，框架只记日志不报错）", seq)
	}

	events := outbox(t, srv.URL)
	if len(events) != 1 {
		t.Fatalf("发件箱里应只有 1 条事件，得到 %d：%+v", len(events), events)
	}
	ev := events[0]
	if ev.Type != eventQuoteCompleted {
		t.Fatalf("事件类型 = %q，期望 %q", ev.Type, eventQuoteCompleted)
	}
	if !eventTypeLexical.MatchString(ev.Type) {
		t.Fatalf("事件类型 %q 不符合 domain-event.schema.json 的词法", ev.Type)
	}
	if !strings.HasPrefix(ev.EventID, "evt_") {
		t.Fatalf("eventId = %q，必须以 evt_ 开头", ev.EventID)
	}
	if ev.Version != module.DefaultEventVersion {
		t.Fatalf("事件版本 = %q，期望 %q", ev.Version, module.DefaultEventVersion)
	}
	if ev.CorrelationID != "cor:quote/Q-1" {
		t.Fatalf("correlationId = %q，期望 cor:quote/Q-1", ev.CorrelationID)
	}
	if ev.CausationID != nil {
		t.Fatalf("首跳事件的 causationId 必须为 null，得到 %q", *ev.CausationID)
	}
	if string(ev.Subject) != `"quote:Quote/Q-1"` {
		t.Fatalf("subject = %s，期望 %q", ev.Subject, "quote:Quote/Q-1")
	}
	if _, err := time.Parse(time.RFC3339, ev.OccurredAt); err != nil {
		t.Fatalf("occurredAt %q 不是 RFC3339：%v", ev.OccurredAt, err)
	}
	if len(ev.Payload) == 0 || ev.Payload[0] != '{' {
		t.Fatalf("payload 必须是 JSON 对象，得到 %s", ev.Payload)
	}
	var payload quotePayload
	decodeInto(t, string(ev.Payload), &payload)
	if payload.QuoteNo != "Q-1" {
		t.Fatalf("载荷 quoteNo = %q，期望 Q-1", payload.QuoteNo)
	}
	if len(payload.Lines) != 1 || payload.Lines[0].SKU != "STD-1002" || payload.Lines[0].Qty != 12 {
		t.Fatalf("载荷缺明细：%s", ev.Payload)
	}

	// 出参契约：只有 quoteId 与 status，且 quoteId 与事件载荷一致。
	var out struct {
		QuoteID string `json:"quoteId"`
		Status  string `json:"status"`
	}
	decodeInto(t, body, &out)
	if out.Status != statusCompleted {
		t.Fatalf("出参 status = %q，期望 %q", out.Status, statusCompleted)
	}
	if out.QuoteID == "" || out.QuoteID != payload.QuoteID {
		t.Fatalf("出参 quoteId = %q 与事件载荷 %q 不一致", out.QuoteID, payload.QuoteID)
	}
}

// TestCreateIsIdempotentForSameQuoteNo 覆盖两层幂等：同键重试（框架层）与
// 换键重发（业务层）都只能有一条 quote.completed，且响应逐字节一致。
func TestCreateIsIdempotentForSameQuoteNo(t *testing.T) {
	srv, store := newTestServer(t)
	const body = `{"quoteNo":"Q-7","lines":[{"sku":"STD-1001","qty":3}]}`

	st1, first := postJSON(t, srv.URL+createPath, body, idempotency("tenon:t/Q-7"))
	if st1 != http.StatusOK {
		t.Fatalf("首次报价失败 %d %s", st1, first)
	}
	// 同一个幂等键重试：框架直接回放首次结果。
	st2, second := postJSON(t, srv.URL+createPath, body, idempotency("tenon:t/Q-7"))
	if st2 != http.StatusOK {
		t.Fatalf("同键重试失败 %d %s", st2, second)
	}
	if second != first {
		t.Fatalf("同键重试的响应与首次不一致：\n首次 %s\n重试 %s", first, second)
	}
	// 换一个幂等键重发同一张单：业务层按 quoteNo 去重，仍回放首次结果。
	st3, third := postJSON(t, srv.URL+createPath, body, idempotency("tenon:t/Q-7-retry"))
	if st3 != http.StatusOK {
		t.Fatalf("换键重发失败 %d %s", st3, third)
	}
	if third != first {
		t.Fatalf("换键重发的响应与首次不一致：\n首次 %s\n重发 %s", first, third)
	}

	events := outbox(t, srv.URL)
	if len(events) != 1 {
		t.Fatalf("重复提交只允许一条 %s，得到 %d 条：%+v", eventQuoteCompleted, len(events), events)
	}
	if seq := store.lastSeq(); seq != 1 {
		t.Fatalf("入箱序号 = %d，期望 1（重复提交不得再入箱）", seq)
	}
}

// TestCreateRequiresIdempotencyKey：写路径缺幂等键是契约违反，必须在执行前拒绝。
func TestCreateRequiresIdempotencyKey(t *testing.T) {
	srv, store := newTestServer(t)

	st, body := postJSON(t, srv.URL+createPath,
		`{"quoteNo":"Q-2","lines":[{"sku":"STD-1001","qty":1}]}`, nil)
	if st != http.StatusBadRequest {
		t.Fatalf("缺 Idempotency-Key 应得 400，得到 %d %s", st, body)
	}
	wantError(t, body, protocol.ClassContractViolation, "MISSING_IDEMPOTENCY_KEY")

	if events := outbox(t, srv.URL); len(events) != 0 {
		t.Fatalf("被拒绝的调用不得产生事件：%+v", events)
	}
	if seq := store.lastSeq(); seq != 0 {
		t.Fatalf("被拒绝的调用不得入箱，lastSeq = %d", seq)
	}
}

// TestCreateRefusesWhenEventCannotBeQueued 锁住「事件没入箱就整笔不成立」这条：
// 报价单存在的意义就是触发 agent，报 completed 却发不出事件是最坏的静默失败。
// 这里直接调用 create（不经 HTTP），因为要注入的正是 EmitEvent 返回 0 这一分支。
func TestCreateRefusesWhenEventCannotBeQueued(t *testing.T) {
	const args = `{"quoteNo":"Q-1","lines":[{"sku":"STD-1002","qty":12}]}`

	t.Run("EmitEvent 返回 0", func(t *testing.T) {
		store := newQuoteStore()
		store.emit = func(module.Event) int64 { return 0 }
		_, perr := store.create(json.RawMessage(args))
		if perr == nil {
			t.Fatal("事件未入箱时必须报错，不得返回成功")
		}
		if perr.Class != protocol.ClassRetryable || perr.Code != "EVENT_EMIT_FAILED" {
			t.Fatalf("期望 retryable/EVENT_EMIT_FAILED，得到 %s/%s", perr.Class, perr.Code)
		}
		if _, ok := store.byNo["Q-1"]; ok {
			t.Fatal("事件未入箱时不得落状态：否则这张单永远不会触发 agent")
		}
		if seq := store.lastSeq(); seq != 0 {
			t.Fatalf("lastSeq = %d，期望 0", seq)
		}
	})

	t.Run("发件箱出口未装配", func(t *testing.T) {
		store := newQuoteStore()
		_, perr := store.create(json.RawMessage(args))
		if perr == nil || perr.Code != "EVENT_EMIT_UNWIRED" {
			t.Fatalf("期望 EVENT_EMIT_UNWIRED，得到 %+v", perr)
		}
		if _, ok := store.byNo["Q-1"]; ok {
			t.Fatal("没有发件箱出口时不得落状态")
		}
	})
}

// TestCreateValidation 锁住入参校验的判定与状态码。
//
// 「缺 lines」拿到的是框架的 400 SCHEMA_MISMATCH（required 检查先于业务判定），
// 不是 422 EMPTY_LINES——这是真实行为，用例如实锁定，而不是假装两者等价。
func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		code  string
		class protocol.ErrorClass
		// status 是 errors.md §5 的分类→状态码映射结果。
		status int
	}{
		{
			name:   "lines 为空数组",
			body:   `{"quoteNo":"Q-1","lines":[]}`,
			code:   "EMPTY_LINES",
			class:  protocol.ClassBusinessRejection,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "qty 为 0",
			body:   `{"quoteNo":"Q-1","lines":[{"sku":"STD-1001","qty":0}]}`,
			code:   "INVALID_QTY",
			class:  protocol.ClassBusinessRejection,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "qty 为负",
			body:   `{"quoteNo":"Q-1","lines":[{"sku":"STD-1001","qty":-3}]}`,
			code:   "INVALID_QTY",
			class:  protocol.ClassBusinessRejection,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "缺 lines 字段",
			body:   `{"quoteNo":"Q-1"}`,
			code:   "SCHEMA_MISMATCH",
			class:  protocol.ClassContractViolation,
			status: http.StatusBadRequest,
		},
		{
			name:   "缺 quoteNo 字段",
			body:   `{"lines":[{"sku":"STD-1001","qty":1}]}`,
			code:   "SCHEMA_MISMATCH",
			class:  protocol.ClassContractViolation,
			status: http.StatusBadRequest,
		},
		{
			name:   "quoteNo 含斜杠",
			body:   `{"quoteNo":"Q/1","lines":[{"sku":"STD-1001","qty":1}]}`,
			code:   "SCHEMA_MISMATCH",
			class:  protocol.ClassContractViolation,
			status: http.StatusBadRequest,
		},
		{
			name:   "明细缺 sku",
			body:   `{"quoteNo":"Q-1","lines":[{"qty":1}]}`,
			code:   "SCHEMA_MISMATCH",
			class:  protocol.ClassContractViolation,
			status: http.StatusBadRequest,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, store := newTestServer(t)
			// 幂等键只用 ASCII：它最终是请求头，不拿中文用例名当线格式素材。
			key := fmt.Sprintf("tenon:t/case-%d", i)
			st, body := postJSON(t, srv.URL+createPath, tc.body, idempotency(key))
			if st != tc.status {
				t.Fatalf("状态码 = %d，期望 %d（%s）", st, tc.status, body)
			}
			wantError(t, body, tc.class, tc.code)
			if events := outbox(t, srv.URL); len(events) != 0 {
				t.Fatalf("被拒绝的建单不得产生事件：%+v", events)
			}
			if seq := store.lastSeq(); seq != 0 {
				t.Fatalf("被拒绝的建单不得入箱，lastSeq = %d", seq)
			}
		})
	}
}

// TestGetQuoteReturnsCreated：read 动作回读已建报价单，且不需要幂等键。
func TestGetQuoteReturnsCreated(t *testing.T) {
	srv, _ := newTestServer(t)

	st, body := postJSON(t, srv.URL+createPath,
		`{"quoteNo":"Q-9","lines":[{"sku":"STD-1002","qty":12},{"sku":"STD-1001","qty":2}]}`,
		idempotency("tenon:t/Q-9"))
	if st != http.StatusOK {
		t.Fatalf("报价失败 %d %s", st, body)
	}
	var created struct {
		QuoteID string `json:"quoteId"`
	}
	decodeInto(t, body, &created)

	st, body = postJSON(t, srv.URL+getPath, `{"quoteNo":"Q-9"}`, nil)
	if st != http.StatusOK {
		t.Fatalf("查询失败 %d %s", st, body)
	}
	var got quoteRecord
	decodeInto(t, body, &got)
	if got.QuoteNo != "Q-9" || got.QuoteID != created.QuoteID || got.Status != statusCompleted {
		t.Fatalf("回读结果与建单不一致：%+v（建单 quoteId %s）", got, created.QuoteID)
	}
	if len(got.Lines) != 2 || got.Lines[0].SKU != "STD-1002" || got.Lines[1].Qty != 2 {
		t.Fatalf("回读的明细不对：%+v", got.Lines)
	}
}

// TestGetQuoteUnknown：未知单号是业务拒绝（422），让 agent 能据此重新规划。
func TestGetQuoteUnknown(t *testing.T) {
	srv, _ := newTestServer(t)

	st, body := postJSON(t, srv.URL+getPath, `{"quoteNo":"Q-404"}`, nil)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("未知报价单应得 422，得到 %d %s", st, body)
	}
	e := wantError(t, body, protocol.ClassBusinessRejection, "UNKNOWN_QUOTE")
	if e.Details["quoteNo"] != "Q-404" {
		t.Fatalf("details 应带上未知单号：%+v", e.Details)
	}
}

// TestDryRunDoesNotEmitEvent：预演只判定，不落状态也不发事件——否则一次
// 「只是问问」的调用就会触发 agent 开回合。
func TestDryRunDoesNotEmitEvent(t *testing.T) {
	srv, store := newTestServer(t)
	const body = `{"quoteNo":"Q-5","lines":[{"sku":"STD-1002","qty":12}]}`

	st, raw := postJSON(t, srv.URL+createDryRun, body, nil)
	if st != http.StatusOK {
		t.Fatalf("预演失败 %d %s", st, raw)
	}
	var report struct {
		DryRun struct {
			Status string `json:"status"`
		} `json:"dryRun"`
	}
	decodeInto(t, raw, &report)
	if report.DryRun.Status != statusVerified {
		t.Fatalf("预演结论 = %q，期望 %q", report.DryRun.Status, statusVerified)
	}
	if events := outbox(t, srv.URL); len(events) != 0 {
		t.Fatalf("预演不得产生事件：%+v", events)
	}
	if seq := store.lastSeq(); seq != 0 {
		t.Fatalf("预演不得入箱，lastSeq = %d", seq)
	}

	// 预演也不得落状态：真实建单之前，这张单查不到。
	st, raw = postJSON(t, srv.URL+getPath, `{"quoteNo":"Q-5"}`, nil)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("预演后不应存在该报价单，查询得到 %d %s", st, raw)
	}

	// 预演之后再真实执行，事件照发一条。
	st, raw = postJSON(t, srv.URL+createPath, body, idempotency("tenon:t/Q-5"))
	if st != http.StatusOK {
		t.Fatalf("真实建单失败 %d %s", st, raw)
	}
	if events := outbox(t, srv.URL); len(events) != 1 {
		t.Fatalf("真实建单后应有一条事件，得到 %d 条", len(events))
	}
}

// TestManifestSummariesOmitInputSchema：摘要级动作只有六个字段（D15 上下文成本），
// 且 quote.completed 必须出现在事件摘要里——agent 靠它决定要不要订阅。
func TestManifestSummariesOmitInputSchema(t *testing.T) {
	srv, _ := newTestServer(t)

	st, raw := getJSON(t, srv.URL+manifestPath)
	if st != http.StatusOK {
		t.Fatalf("读 manifest 失败 %d %s", st, raw)
	}
	if strings.Contains(raw, "inputSchema") {
		t.Fatalf("manifest 摘要不得含 inputSchema：%s", raw)
	}

	var mf struct {
		Tenon  string `json:"tenon"`
		Module struct {
			ID      string `json:"id"`
			Version string `json:"version"`
			Title   string `json:"title"`
		} `json:"module"`
		Actions []map[string]json.RawMessage `json:"actions"`
		Events  []struct {
			Type    string `json:"type"`
			Version string `json:"version"`
		} `json:"events"`
		Transports   []string `json:"transports"`
		Capabilities []string `json:"capabilities"`
	}
	decodeInto(t, raw, &mf)

	if mf.Tenon != protocol.Version {
		t.Fatalf("manifest tenon = %q，期望 %q", mf.Tenon, protocol.Version)
	}
	if mf.Module.ID != moduleID || mf.Module.Version != moduleVersion || mf.Module.Title == "" {
		t.Fatalf("module 摘要不对：%+v", mf.Module)
	}
	if len(mf.Actions) != 2 {
		t.Fatalf("动作摘要应有 2 条，得到 %d：%s", len(mf.Actions), raw)
	}

	// 摘要字段集合：六个必填字段之外，summaryTemplate 可选（0.2.0 加的字段）。
	// 多出别的字段就是 manifest 与 schema 不一致；少了必填字段同理。
	requiredKeys := []string{"id", "version", "title", "kind", "risk", "dryRun"}
	optionalKeys := map[string]bool{"summaryTemplate": true}
	kinds := make(map[string]string, len(mf.Actions))
	for _, summary := range mf.Actions {
		for _, k := range requiredKeys {
			if _, ok := summary[k]; !ok {
				t.Fatalf("动作摘要缺必填字段 %q：%v", k, summary)
			}
		}
		for k := range summary {
			if optionalKeys[k] {
				continue
			}
			known := false
			for _, req := range requiredKeys {
				if req == k {
					known = true
					break
				}
			}
			if !known {
				t.Fatalf("动作摘要出现契约外字段 %q：%v", k, summary)
			}
		}
		var id, kind string
		decodeInto(t, string(summary["id"]), &id)
		decodeInto(t, string(summary["kind"]), &kind)
		kinds[id] = kind
	}
	if kinds[actionCreateStandardPartQuote] != "write" || kinds[actionGetQuote] != "read" {
		t.Fatalf("动作读写分类不对：%v", kinds)
	}

	if len(mf.Events) != 1 || mf.Events[0].Type != eventQuoteCompleted {
		t.Fatalf("事件摘要应只有 %q，得到 %+v", eventQuoteCompleted, mf.Events)
	}
	if mf.Events[0].Version != module.DefaultEventVersion {
		t.Fatalf("事件摘要 version = %q，期望 %q", mf.Events[0].Version, module.DefaultEventVersion)
	}

	// 能力标签必须与实现自洽：有 dry-run 回调且动作声明 supported，才允许声明 dry-run。
	caps := make(map[string]bool, len(mf.Capabilities))
	for _, c := range mf.Capabilities {
		caps[c] = true
	}
	if !caps["dry-run"] || !caps["event-replay"] {
		t.Fatalf("能力标签不对：%v", mf.Capabilities)
	}
}

// TestConcurrentCreatesAndReads：内存 map 必须并发安全。本机不能跑 -race
// （CGO_ENABLED=0，无 gcc），所以这个用例只做行为级不变量：并发建单 + 并发读，
// 不得出现 5xx，且事件数与单数一致。
func TestConcurrentCreatesAndReads(t *testing.T) {
	srv, _ := newTestServer(t)
	const workers = 8

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			quoteNo := fmt.Sprintf("Q-C%02d", i)
			body := fmt.Sprintf(`{"quoteNo":%q,"lines":[{"sku":"STD-1001","qty":%d}]}`, quoteNo, i+1)

			st, raw, err := request(http.MethodPost, srv.URL+createPath, body,
				idempotency("tenon:t/"+quoteNo))
			if err != nil {
				t.Errorf("并发建单失败：%v", err)
				return
			}
			if st != http.StatusOK {
				t.Errorf("并发建单 %s 得到 %d %s", quoteNo, st, raw)
				return
			}
			st, raw, err = request(http.MethodPost, srv.URL+getPath,
				fmt.Sprintf(`{"quoteNo":%q}`, quoteNo), nil)
			if err != nil {
				t.Errorf("并发查询失败：%v", err)
				return
			}
			if st != http.StatusOK {
				t.Errorf("并发查询 %s 得到 %d %s", quoteNo, st, raw)
			}
		}(i)
	}
	wg.Wait()

	events := outbox(t, srv.URL)
	if len(events) != workers {
		t.Fatalf("%d 张不同的单应产生 %d 条事件，得到 %d 条", workers, workers, len(events))
	}
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		var payload quotePayload
		decodeInto(t, string(ev.Payload), &payload)
		if seen[payload.QuoteNo] {
			t.Fatalf("报价单 %s 产生了重复事件", payload.QuoteNo)
		}
		seen[payload.QuoteNo] = true
	}
	if len(seen) != workers {
		t.Fatalf("事件载荷只覆盖 %d 张单，期望 %d", len(seen), workers)
	}
}

// 模板里的每个占位符都必须真的在 inputSchema.properties 里：否则渲染必然退 R2，
// 卡片上永远读不到人话（P3 spec §4 的 R1）。
func assertTemplateArgs(t *testing.T, a module.Action) {
	t.Helper()
	if a.SummaryTemplate == "" {
		t.Fatalf("%s 没声明 summaryTemplate", a.ID)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(a.InputSchema, &schema); err != nil {
		t.Fatalf("%s 的 inputSchema 读不出：%v", a.ID, err)
	}
	for _, name := range module.TemplatePlaceholders(a.SummaryTemplate) {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("%s 的模板占位符 {%s} 不在 inputSchema.properties 里", a.ID, name)
		}
	}
}

func TestActionTemplatesUseDeclaredArgs(t *testing.T) {
	assertTemplateArgs(t, createStandardPartQuoteAction())
}
