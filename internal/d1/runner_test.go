package d1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

// 本文件全部用例都用桩服务 + 临时结果文件：不跑真 LLM、不连任何外部进程。
// agent 那一侧被压成「结果文件里写什么」，于是断言的是 D1 判据本身。

// stubWorld 是 A/B 两轮的桩世界：一个宿主（注册表 + 管理面）、三个模块
// （quote 带触发器，三个都提供 reset）以及一个结果文件。
type stubWorld struct {
	t *testing.T

	// 用例设定：A/B 两轮 agent 各自会写出什么。PlanA/PlanB 就是「规划出来的动作
	// 序列」，OutcomeA/OutcomeB 是结局。
	PlanA    []string
	PlanB    []string
	OutcomeA string
	OutcomeB string
	PlanIDA  string
	PlanIDB  string

	// SkipResultB 为真时 B 轮不写结果行：验证「等不到就 fail，不挂死」。
	SkipResultB bool
	// IgnoreDisable 为真时管理面返回 200 但注册表不真的把模块关掉：验证摘除
	// 没生效时 runner 不会把锅甩给 agent。
	IgnoreDisable bool

	ResultPath   string
	RoundTimeout time.Duration

	mu       sync.Mutex
	calls    []string
	registry map[string]bool
	quotes   []quoteRequest
	triggers int

	host        *httptest.Server
	quote       *httptest.Server
	inventory   *httptest.Server
	procurement *httptest.Server
}

func newStubWorld(t *testing.T) *stubWorld {
	t.Helper()
	w := &stubWorld{
		t:            t,
		OutcomeA:     outcomeNeedsHuman,
		OutcomeB:     outcomeNeedsHuman,
		PlanIDA:      "pln_a",
		PlanIDB:      "pln_b",
		ResultPath:   filepath.Join(t.TempDir(), "agent-results.jsonl"),
		RoundTimeout: 2 * time.Second,
		registry:     make(map[string]bool),
	}
	w.host = httptest.NewServer(http.HandlerFunc(w.serveHost))
	w.quote = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.serveModule(rw, r, moduleQuote)
	}))
	w.inventory = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.serveModule(rw, r, moduleInventory)
	}))
	w.procurement = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.serveModule(rw, r, procurementModule)
	}))
	t.Cleanup(func() {
		w.host.Close()
		w.quote.Close()
		w.inventory.Close()
		w.procurement.Close()
	})
	return w
}

// Config 是这一次运行的 runner 参数：地址都指向桩服务，轮询间隔压到毫秒级。
func (w *stubWorld) Config() Config {
	return Config{
		Host:         w.host.URL,
		Quote:        w.quote.URL,
		Inventory:    w.inventory.URL,
		Procurement:  w.procurement.URL,
		AgentResult:  w.ResultPath,
		RoundTimeout: w.RoundTimeout,
		PollInterval: 5 * time.Millisecond,
	}
}

// ---------- 桩宿主 ----------

func (w *stubWorld) serveHost(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case adminModulesPath:
		var req struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.t.Errorf("读管理面请求体失败：%v", err)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.t.Errorf("管理面请求体不是合法 JSON：%v", err)
			return
		}
		enabled := req.Enabled == nil || *req.Enabled
		w.record(fmt.Sprintf("admin:%s=%v", req.ID, enabled))
		w.mu.Lock()
		if !enabled && w.IgnoreDisable {
			// 模拟「管理面回了 200，但注册表根本没变」。
			enabled = w.registry[req.ID]
		}
		w.registry[req.ID] = enabled
		w.mu.Unlock()
		writeStubJSON(rw, http.StatusOK, w.modulesView())

	case modulesPath:
		w.record("modules")
		writeStubJSON(rw, http.StatusOK, w.modulesView())

	default:
		http.NotFound(rw, r)
	}
}

func (w *stubWorld) modulesView() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	mods := make([]map[string]any, 0, len(w.registry))
	for id, enabled := range w.registry {
		mods = append(mods, map[string]any{"id": id, "baseURL": "http://stub.invalid", "enabled": enabled})
	}
	return map[string]any{"modules": mods}
}

// ---------- 桩模块 ----------

func (w *stubWorld) serveModule(rw http.ResponseWriter, r *http.Request, id string) {
	switch r.URL.Path {
	case adminResetPath:
		w.record("reset:" + id)
		writeStubJSON(rw, http.StatusOK, map[string]bool{"reset": true})

	case "/tenon/actions/" + quoteActionID:
		if id != moduleQuote {
			http.NotFound(rw, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.t.Errorf("读触发器请求体失败：%v", err)
			return
		}
		// 写动作的两个协议前提：版本头与幂等键（缺一个宿主/模块就该拒收）。
		if got := r.Header.Get(protocol.HeaderVersion); got != protocol.Version {
			w.t.Errorf("触发器请求的 %s = %q，期望 %q", protocol.HeaderVersion, got, protocol.Version)
		}
		if r.Header.Get(protocol.HeaderIdempotencyKey) == "" {
			w.t.Errorf("触发器请求缺少 %s", protocol.HeaderIdempotencyKey)
		}
		var req quoteRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.t.Errorf("触发器入参不是合法 JSON：%v", err)
			return
		}
		w.mu.Lock()
		w.quotes = append(w.quotes, req)
		w.triggers++
		n := w.triggers
		w.mu.Unlock()

		w.record("trigger:" + req.QuoteNo)
		writeStubJSON(rw, http.StatusOK, map[string]string{"quoteId": "q_0001", "status": "completed"})
		// 报价完成 → quote 写发件箱 → agent 开一回合。桩把它压成「往结果文件追加一行」。
		w.emitResult(n)

	default:
		http.NotFound(rw, r)
	}
}

// emitResult 模拟 agent 的一回合：把结局与规划序列追加成一行 JSON。
func (w *stubWorld) emitResult(n int) {
	plan, outcome, planID := w.PlanA, w.OutcomeA, w.PlanIDA
	if n > 1 {
		if w.SkipResultB {
			return
		}
		plan, outcome, planID = w.PlanB, w.OutcomeB, w.PlanIDB
	}
	raw, err := json.Marshal(agentResult{
		At:            time.Now().UTC().Format(time.RFC3339Nano),
		CorrelationID: fmt.Sprintf("cor:quote/stub-%d", n),
		Outcome:       outcome,
		PlanID:        planID,
		Actions:       plan,
		Detail:        fmt.Sprintf("桩世界的第 %d 回合", n),
	})
	if err != nil {
		w.t.Errorf("结果行编组失败：%v", err)
		return
	}
	f, err := os.OpenFile(w.ResultPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		w.t.Errorf("打开结果文件失败：%v", err)
		return
	}
	// 一行一条：写成一次 Write，读侧不会读到半行。
	if _, err := f.Write(append(raw, '\n')); err != nil {
		w.t.Errorf("写结果行失败：%v", err)
	}
	if err := f.Close(); err != nil {
		w.t.Errorf("关闭结果文件失败：%v", err)
	}
}

// ---------- 桩世界观测 ----------

func (w *stubWorld) record(entry string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, entry)
}

func (w *stubWorld) callLog() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls...)
}

func (w *stubWorld) sentQuotes() []quoteRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]quoteRequest(nil), w.quotes...)
}

func (w *stubWorld) triggerCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.triggers
}

func countOf(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// indexOfNth 返回第 n 次（1-based）出现的下标，找不到返回 -1。
func indexOfNth(calls []string, want string, n int) int {
	seen := 0
	for i, c := range calls {
		if c == want {
			seen++
			if seen == n {
				return i
			}
		}
	}
	return -1
}

// indexOfPrefix 返回第一个带该前缀的下标（动作序列的 quoteNo 含时间戳，只能按前缀找）。
func indexOfPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func writeStubJSON(rw http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_, _ = rw.Write(raw)
}

// ---------- 判据 ----------

// 自适应：A 轮规划出 procurement.*，摘掉之后 B 轮不再规划它并转人工 → pass。
func TestD1PassesWhenAdaptive(t *testing.T) {
	w := newStubWorld(t)
	// A 轮结局是 needs_human 不影响结论：procurement 没有 :dry-run，agent 在预演
	// 闸门就 fail-closed 转人工，D1 判的是规划层而不是「执行完了没有」。
	w.PlanA = []string{"inventory.GetStock", "inventory.DeductStock", "procurement.CreatePurchaseRequest"}
	w.PlanB = []string{"inventory.GetStock", "inventory.DeductStock"}
	w.OutcomeA = outcomeNeedsHuman
	w.OutcomeB = outcomeNeedsHuman

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictPass {
		t.Fatalf("期望 pass，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !equalSeq(rep.Rounds.A.Actions, w.PlanA) {
		t.Fatalf("A 轮动作序列 = %v，期望 %v", rep.Rounds.A.Actions, w.PlanA)
	}
	if !equalSeq(rep.Rounds.B.Actions, w.PlanB) {
		t.Fatalf("B 轮动作序列 = %v，期望 %v", rep.Rounds.B.Actions, w.PlanB)
	}
	if rep.Rounds.A.PlanID != w.PlanIDA || rep.Rounds.B.PlanID != w.PlanIDB {
		t.Fatalf("planId = %q/%q，期望 %q/%q",
			rep.Rounds.A.PlanID, rep.Rounds.B.PlanID, w.PlanIDA, w.PlanIDB)
	}
	if rep.Rounds.B.Outcome != outcomeNeedsHuman {
		t.Fatalf("B 轮结局 = %q，期望 %q", rep.Rounds.B.Outcome, outcomeNeedsHuman)
	}
}

// 序列相同：摘掉模块后行为没变 → 说明 agent 没在用能力清单 → fail。
func TestD1FailsWhenPlansIdentical(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock"}
	w.PlanB = []string{"inventory.DeductStock"} // 摘掉模块后行为没变 → 说明没用清单
	w.OutcomeB = outcomeNeedsHuman

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "序列相同") {
		t.Fatalf("fail 理由必须点明「序列相同」，得到：%s", rep.Reason)
	}
}

// B 轮还在调被摘除的模块：序列虽然不同，但证明注册表过滤没生效 → fail 且点名。
func TestD1FailsWhenBRoundStillCallsRemovedModule(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock"}
	w.PlanB = []string{"procurement.CreatePurchaseRequest"}

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "procurement.CreatePurchaseRequest") {
		t.Fatalf("fail 理由必须点出仍是哪个动作，得到：%s", rep.Reason)
	}
	if !strings.Contains(rep.Reason, "仍在调用被摘除模块") {
		t.Fatalf("fail 理由必须说明违反了哪一条，得到：%s", rep.Reason)
	}
}

// B 轮崩了（failed）：不是转人工 → fail。
func TestD1FailsWhenBRoundFailed(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock", "procurement.CreatePurchaseRequest"}
	w.PlanB = []string{"inventory.DeductStock"}
	w.OutcomeB = outcomeFailed

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "failed") {
		t.Fatalf("fail 理由必须点明 B 轮结局是 failed，得到：%s", rep.Reason)
	}
}

// A 轮崩了：两轮对比没有意义 → fail，且不能把 A 轮的崩溃记成「成功对照」。
func TestD1FailsWhenARoundFailed(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock"}
	w.PlanB = []string{}
	w.OutcomeA = outcomeFailed

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "A 轮崩了") {
		t.Fatalf("fail 理由必须点明 A 轮崩了，得到：%s", rep.Reason)
	}
}

// 结果行一直不出现：超时判 fail，且 runner 自己不许挂死。
func TestD1FailsWhenResultNeverAppears(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock"}
	w.PlanB = []string{"inventory.DeductStock"}
	w.SkipResultB = true
	w.RoundTimeout = 150 * time.Millisecond

	start := time.Now()
	rep, err := Run(context.Background(), w.Config())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "没等到") {
		t.Fatalf("fail 理由必须说明是等结果行超时，得到：%s", rep.Reason)
	}
	// 上限给得很宽：只要没挂死就行（正常情况 ~150ms 返回）。
	if elapsed > 10*time.Second {
		t.Fatalf("等结果行没有按 --round-timeout 收尾，耗时 %v", elapsed)
	}
}

// 摘除没生效（管理面 200 但注册表仍是 enabled=true）：必须在 B 轮之前就判 fail，
// 否则「序列相同」的结论会把锅甩给 agent。
func TestD1FailsWhenRegistryKeepsModuleEnabled(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock", "procurement.CreatePurchaseRequest"}
	w.PlanB = []string{"inventory.DeductStock"}
	w.IgnoreDisable = true

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("期望 fail，得到 %s（%s）", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "摘除模块后注册表没有生效") {
		t.Fatalf("fail 理由必须点明摘除没生效，得到：%s", rep.Reason)
	}
	if got := w.triggerCount(); got != 1 {
		t.Fatalf("摘除没生效时不该再跑 B 轮，实际触发了 %d 次报价", got)
	}
}

// ---------- 流程 ----------

// reset 的调用顺序：两轮各自在触发之前都要 reset 全部模块，且 B 轮的 reset 在
// 摘除之后、B 轮触发之前。顺序错了，「两轮输入相同」的前提就不成立。
func TestD1ResetsEveryModuleBeforeEachRound(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.DeductStock", "procurement.CreatePurchaseRequest"}
	w.PlanB = []string{"inventory.DeductStock"}

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if rep.Verdict != VerdictPass {
		t.Fatalf("这一轮的判据应当 pass，得到 %s（%s）", rep.Verdict, rep.Reason)
	}

	calls := w.callLog()

	// A 轮：三个模块都登记且都是 enabled，各自 reset 一次。
	for _, id := range []string{moduleQuote, moduleInventory, procurementModule} {
		if got := countOf(calls, "admin:"+id+"=true"); got != 1 {
			t.Fatalf("模块 %s 的登记次数 = %d，期望 1（调用日志：%v）", id, got, calls)
		}
	}
	for _, id := range []string{moduleQuote, moduleInventory, procurementModule} {
		if got := countOf(calls, "reset:"+id); got != 2 {
			t.Fatalf("模块 %s 的 reset 次数 = %d，期望 2（A 与 B 轮各一次）：%v", id, got, calls)
		}
	}

	triggerA := indexOfPrefix(calls, "trigger:D1-A-")
	triggerB := indexOfPrefix(calls, "trigger:D1-B-")
	disable := indexOfNth(calls, "admin:procurement=false", 1)
	resetA := indexOfNth(calls, "reset:"+procurementModule, 1)
	resetB := indexOfNth(calls, "reset:"+procurementModule, 2)
	for name, idx := range map[string]int{
		"第一次 reset": resetA, "A 轮触发": triggerA,
		"摘除 procurement": disable, "第二次 reset": resetB, "B 轮触发": triggerB,
	} {
		if idx < 0 {
			t.Fatalf("调用日志里没有「%s」：%v", name, calls)
		}
	}
	if !(resetA < triggerA) {
		t.Fatalf("A 轮必须先 reset 再触发报价（reset=%d，trigger=%d）", resetA, triggerA)
	}
	if !(triggerA < disable) {
		t.Fatalf("摘除必须发生在 A 轮之后（triggerA=%d，disable=%d）", triggerA, disable)
	}
	if !(disable < resetB) {
		t.Fatalf("B 轮的 reset 必须在摘除之后（disable=%d，reset=%d）", disable, resetB)
	}
	if !(resetB < triggerB) {
		t.Fatalf("B 轮必须先 reset 再触发报价（reset=%d，trigger=%d）", resetB, triggerB)
	}

	// 两轮的业务输入相同、只有单号不同：否则「同一输入」的前提不成立。
	quotes := w.sentQuotes()
	if len(quotes) != 2 {
		t.Fatalf("触发次数 = %d，期望 2", len(quotes))
	}
	if quotes[0].QuoteNo == quotes[1].QuoteNo {
		t.Fatalf("两轮必须换单号（业务层幂等会把第二轮挡住），实际都是 %q", quotes[0].QuoteNo)
	}
	lineA, lineB := quotes[0].Lines, quotes[1].Lines
	if len(lineA) != 1 || len(lineB) != 1 {
		t.Fatalf("报价明细 = %v / %v，期望各一行", lineA, lineB)
	}
	if lineA[0] != lineB[0] {
		t.Fatalf("两轮明细必须完全相同（SKU/数量），实际 %+v / %+v", lineA[0], lineB[0])
	}
}

// 报告可反序列化、字段集合固定、两轮 actions 都在。
func TestReportJSONCarriesBothRounds(t *testing.T) {
	w := newStubWorld(t)
	w.PlanA = []string{"inventory.GetStock", "inventory.DeductStock", "procurement.CreatePurchaseRequest"}
	w.PlanB = []string{"inventory.GetStock", "inventory.DeductStock"}
	w.RoundTimeout = 2 * time.Second

	rep, err := Run(context.Background(), w.Config())
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("报告编组失败：%v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("报告不是合法 JSON：%v", err)
	}
	for _, key := range []string{"verdict", "reason", "rounds", "roundTimeout"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("报告缺少字段 %q：%s", key, raw)
		}
	}
	if len(top) != 4 {
		t.Fatalf("报告字段数 = %d，期望 4（%v）：%s", len(top), top, raw)
	}
	// roundTimeout 是秒数（可读、可复核），不是 time.Duration 的纳秒。
	if got := string(top["roundTimeout"]); got != "2" {
		t.Fatalf("roundTimeout = %s，期望 2（秒）", got)
	}

	var rounds map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["rounds"], &rounds); err != nil {
		t.Fatalf("rounds 不是合法 JSON：%v", err)
	}
	for _, label := range []string{"a", "b"} {
		round, ok := rounds[label]
		if !ok {
			t.Fatalf("报告缺少 rounds.%s：%s", label, raw)
		}
		for _, key := range []string{"outcome", "planId", "actions"} {
			if _, ok := round[key]; !ok {
				t.Fatalf("rounds.%s 缺少字段 %q：%s", label, key, raw)
			}
		}
		if len(round) != 3 {
			t.Fatalf("rounds.%s 字段数 = %d，期望 3：%s", label, len(round), raw)
		}
	}

	var decoded Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("报告反序列化失败：%v", err)
	}
	if decoded.Verdict != VerdictPass {
		t.Fatalf("反序列化后 verdict = %q，期望 pass（%s）", decoded.Verdict, decoded.Reason)
	}
	if !equalSeq(decoded.Rounds.A.Actions, w.PlanA) {
		t.Fatalf("A 轮 actions = %v，期望 %v", decoded.Rounds.A.Actions, w.PlanA)
	}
	if !equalSeq(decoded.Rounds.B.Actions, w.PlanB) {
		t.Fatalf("B 轮 actions = %v，期望 %v", decoded.Rounds.B.Actions, w.PlanB)
	}
	if decoded.RoundTimeout != 2 {
		t.Fatalf("反序列化后 roundTimeout = %v，期望 2", decoded.RoundTimeout)
	}
}

// 参数不合法时返回 error 而不是「fail 报告」：那不是 D1 的结论，是用法错误。
func TestRunRejectsMissingAgentResult(t *testing.T) {
	w := newStubWorld(t)
	cfg := w.Config()
	cfg.AgentResult = ""
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("缺 agent 结果文件路径必须报错")
	}

	cfg = w.Config()
	cfg.Procurement = "not a url"
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("模块地址不是合法 URL 必须报错")
	}
}
