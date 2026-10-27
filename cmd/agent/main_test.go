package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenon.dev/internal/agent"
	"tenon.dev/internal/spec"
)

// clearAgentEnv 清空本进程读取的全部环境变量，让每个用例从同一个起点开始。
func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envHost, envSkillDir, envAgentID, envOnBehalfOf, envPollInterval,
		envOnce, envCursorFile, envSpecDir, envResultFile,
		envStateDir, envAgentUser, envAgentPassword, envRetentionDays,
	} {
		t.Setenv(name, "")
	}
}

func TestAgentRefusesToStartWithoutLLMKey(t *testing.T) {
	// 缺 key 必须启动即失败，不得静默降级（设计 §5）。
	// 两个来源都清空：文件优先于环境变量，只清一个测不出东西。
	t.Setenv("TENON_LLM_API_KEY", "")
	t.Setenv("TENON_LLM_API_KEY_FILE", "")

	if err := run(context.Background()); err == nil {
		t.Fatal("缺 key 必须启动即失败，不得静默降级")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
	t.Setenv(envOnBehalfOf, "user:alice")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("缺省配置应当可用，得到错误：%v", err)
	}
	if cfg.host != defaultHost {
		t.Fatalf("host 缺省值不对：%q", cfg.host)
	}
	if cfg.skillDir != defaultSkillDir {
		t.Fatalf("skill 目录缺省值不对：%q", cfg.skillDir)
	}
	if cfg.agentID != defaultAgentID {
		t.Fatalf("agent id 缺省值不对：%q", cfg.agentID)
	}
	if cfg.pollInterval != defaultPollInterval {
		t.Fatalf("轮询间隔缺省值不对：%v", cfg.pollInterval)
	}
	if cfg.once {
		t.Fatal("TENON_ONCE 缺省必须是假")
	}
	if cfg.specDir != spec.DefaultDir {
		t.Fatalf("schema 目录缺省值不对：%q", cfg.specDir)
	}
}

func TestLoadConfigRequiresOnBehalfOf(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")

	// 不提供默认代行身份：编一个身份等于让审计记录归属于不存在的人。
	if _, err := loadConfig(); err == nil {
		t.Fatal("缺 TENON_ON_BEHALF_OF 必须启动即失败")
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "host 不是 URL", env: map[string]string{envHost: "not a url"}},
		{name: "host scheme 不对", env: map[string]string{envHost: "ftp://127.0.0.1:8080"}},
		{name: "轮询间隔非法", env: map[string]string{envPollInterval: "一秒"}},
		{name: "轮询间隔为零", env: map[string]string{envPollInterval: "0s"}},
		{name: "once 取值无法识别", env: map[string]string{envOnce: "maybe"}},
		{name: "agent id 词法非法", env: map[string]string{envAgentID: "9 bad id"}},
		{name: "委托人词法非法", env: map[string]string{envOnBehalfOf: "alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEnv(t)
			t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
			t.Setenv(envOnBehalfOf, "user:alice")
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if _, err := loadConfig(); err == nil {
				t.Fatalf("%s：必须报错", tc.name)
			}
		})
	}
}

func TestCursorStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursors.json")

	store, err := loadCursors(path)
	if err != nil {
		t.Fatalf("文件不存在应当是空游标，得到错误：%v", err)
	}
	if got := store.get("inventory"); got != "" {
		t.Fatalf("空游标应当返回空串，得到 %q", got)
	}

	store.set("inventory", "cur_out_3")
	store.set("quote", "cur_out_7")
	store.set("empty", "")
	if err := store.save(path); err != nil {
		t.Fatalf("写游标失败：%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("游标文件没有落盘：%v", err)
	}

	again, err := loadCursors(path)
	if err != nil {
		t.Fatalf("重新加载游标失败：%v", err)
	}
	if again.get("inventory") != "cur_out_3" || again.get("quote") != "cur_out_7" {
		t.Fatalf("游标往返不一致：%+v", again.entries)
	}
	if _, ok := again.entries["empty"]; ok {
		t.Fatal("空游标不该写盘（回显空串等于从头读）")
	}
}

func TestPollIntervalIsHonoured(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
	t.Setenv(envOnBehalfOf, "user:alice")
	t.Setenv(envPollInterval, "250ms")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig 失败：%v", err)
	}
	if cfg.pollInterval != 250*time.Millisecond {
		t.Fatalf("TENON_POLL_INTERVAL 没有生效：%v", cfg.pollInterval)
	}
}

// TestInboundConfigDefaults 锁住入站装配的四个缺省值：它们是「起进程就跑得起来」的前提，
// 改一个数字就该有用例红。保留期还必须为正数——负数会让 PruneArchived 删掉全部归档件。
func TestInboundConfigDefaults(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
	t.Setenv(envOnBehalfOf, "user:alice")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig 失败：%v", err)
	}
	if cfg.stateDir != defaultStateDir {
		t.Fatalf("状态目录缺省值不对：%q", cfg.stateDir)
	}
	if cfg.agentUser != defaultAgentUser || cfg.agentPass != defaultAgentPass {
		t.Fatalf("agent 服务账号缺省值不对：%q/%q", cfg.agentUser, cfg.agentPass)
	}
	if cfg.retainDays != defaultRetainDays {
		t.Fatalf("保留期缺省值不对：%d", cfg.retainDays)
	}

	t.Setenv(envRetentionDays, "-1")
	if _, err := loadConfig(); err == nil {
		t.Fatal("负的保留期必须报错（它会让归档件被全量删除）")
	}
	t.Setenv(envRetentionDays, "三十")
	if _, err := loadConfig(); err == nil {
		t.Fatal("保留期不是数字必须报错")
	}
}

// ---------- 入站 tick 的用例 ----------

// countingLLM 是桩模型：本文件的用例只关心调度（谁被调用、几次、是否重放），
// 所以它默认返回错误——规划失败不影响「帧有没有被处理、有没有被 ack」这些断言。
type countingLLM struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (l *countingLLM) ChatJSON(context.Context, string, string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return nil, l.err
}

func (l *countingLLM) Calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// fakeInboundHost 扮演宿主：入站 poll/ack、公告板、注册表、闸门与审计。
//
// 它刻意**不**按 ack 删帧：用例要模拟「宿主重放」与「进程重启后从旧水位重读」，
// 由用例自己决定下一次 poll 该看到哪些帧。
type fakeInboundHost struct {
	*httptest.Server
	mu      sync.Mutex
	frames  []agent.QueuedFrame
	seq     int64
	acks    []int64
	pushes  []map[string]any
	actions []string
}

func newFakeInboundHost(t *testing.T) *fakeInboundHost {
	t.Helper()
	h := &fakeInboundHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Server.Close)
	return h
}

// pushFrame 追加一帧（序号递增）。同一份帧体推两次 = 宿主把同一件事又送了一遍。
func (h *fakeInboundHost) pushFrame(body string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.frames = append(h.frames, agent.QueuedFrame{Seq: h.seq, Frame: json.RawMessage(body)})
	return h.seq
}

func (h *fakeInboundHost) acksSeen() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.acks...)
}

func (h *fakeInboundHost) pushesSeen() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.pushes...)
}

// actionsSeen 是模块执行端点收到的调用路径：用例据此证明「批了真的执行了」，
// 而不是只看归档状态（归档只说明恢复流程走完了）。
func (h *fakeInboundHost) actionsSeen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.actions...)
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *fakeInboundHost) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/tenon/session":
		writeTestJSON(w, map[string]any{"token": "tok-test", "user": "agent:test", "roles": []string{"role:agent"}})

	case "/tenon/requests/poll":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		h.mu.Lock()
		out := make([]agent.QueuedFrame, 0, len(h.frames))
		next := after
		for _, f := range h.frames {
			if f.Seq > after {
				out = append(out, f)
			}
			if f.Seq > next {
				next = f.Seq
			}
		}
		h.mu.Unlock()
		writeTestJSON(w, map[string]any{"frames": out, "nextCursor": next})

	case "/tenon/requests/ack":
		var body struct {
			Upto int64 `json:"upto"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.acks = append(h.acks, body.Upto)
		h.mu.Unlock()
		writeTestJSON(w, map[string]any{"ok": true})

	case "/tenon/actions/inventory.GetStock", "/tenon/actions/inventory.DeductStock":
		// 模块执行端点：恢复路径真的会走到这里（2xx 即 verdictOK，响应体形状无要求）。
		h.mu.Lock()
		h.actions = append(h.actions, r.URL.Path)
		h.mu.Unlock()
		writeTestJSON(w, map[string]any{"ok": true})

	case "/tenon/pending":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.pushes = append(h.pushes, body)
		h.mu.Unlock()
		writeTestJSON(w, map[string]any{"ok": true})

	case "/tenon/modules":
		// 模块就是这台桩服务自己：能力发现与闸门都由同一个 handler 应答。
		writeTestJSON(w, map[string]any{"modules": []map[string]any{
			{"id": "inventory", "baseURL": h.Server.URL, "enabled": true},
		}})

	case "/.well-known/tenon":
		writeTestJSON(w, map[string]any{
			"tenon":  "0.1",
			"module": map[string]any{"id": "inventory", "version": "0.1.0"},
			"actions": []map[string]any{
				{"id": "inventory.DeductStock", "kind": "write"},
				{"id": "inventory.GetStock", "kind": "read"},
			},
			"events": []any{},
		})

	case "/tenon/policies:evaluate":
		writeTestJSON(w, map[string]any{"outcome": "auto", "decidedBy": []string{"pol-test"},
			"approvers": []string{"role:warehouse_manager"}, "reasons": []string{"TEST_RULE"}})

	case "/tenon/grants:evaluate":
		writeTestJSON(w, map[string]any{"allowed": true, "matchedGrantId": "gr-test001"})

	case "/tenon/audit":
		if r.Method == http.MethodGet {
			writeTestJSON(w, map[string]any{"records": []any{}, "nextCursor": "0"})
			return
		}
		writeTestJSON(w, map[string]any{"ok": true})

	case "/tenon/intents":
		writeTestJSON(w, map[string]any{"ok": true, "seq": 1})

	case "/tenon/events":
		// 第 4 段（事件轮询）会读每个 enabled 模块的发件箱；本文件的用例只走人发起的路径。
		writeTestJSON(w, map[string]any{"events": []any{}, "nextCursor": ""})

	default:
		http.NotFound(w, r)
	}
}

// inboundEnv 是一套装好的 tick 环境：假宿主 + 真状态目录 + 真账本 + 桩模型。
type inboundEnv struct {
	t        *testing.T
	host     *fakeInboundHost
	stateDir string
	store    *agent.StateStore
	ledger   *agent.Ledger
	llm      *countingLLM
	deps     agent.Deps
	steps    []string
}

func newInboundEnv(t *testing.T) *inboundEnv {
	t.Helper()
	stateDir := t.TempDir()
	store, err := agent.NewStateStore(stateDir)
	if err != nil {
		t.Fatalf("建状态目录失败：%v", err)
	}
	ledger, err := agent.NewLedger(filepath.Join(stateDir, "processed.jsonl"))
	if err != nil {
		t.Fatalf("开账本失败：%v", err)
	}
	specSet, err := spec.Load(filepath.Join("..", "..", spec.DefaultDir))
	if err != nil {
		t.Fatalf("加载 schema 失败：%v", err)
	}
	llm := &countingLLM{err: errors.New("桩模型故意失败：本用例只关心调度")}
	host := newFakeInboundHost(t)
	env := &inboundEnv{t: t, host: host, stateDir: stateDir, store: store, ledger: ledger, llm: llm}
	env.deps = agent.Deps{
		Host:       host.URL,
		AgentID:    "ag_test",
		OnBehalfOf: "user:alice",
		Planner:    &agent.Planner{LLM: llm, Spec: specSet},
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		Skills:     agent.EmptySkills(),
		State:      store,
	}
	return env
}

// poller 组装被测的 poller（steps 记录器指向 env.steps）。
func (e *inboundEnv) poller() *poller {
	e.steps = nil
	return &poller{
		cfg:     config{host: e.host.URL, stateDir: e.stateDir, retainDays: defaultRetainDays},
		deps:    e.deps,
		client:  e.deps.HTTP,
		cursors: &cursorStore{entries: map[string]string{}},
		plans:   newPlanObserver(e.llm),
		hostc:   agent.NewHostClient(e.host.URL, "agent", "agent-dev", e.deps.HTTP),
		store:   e.store,
		ledger:  e.ledger,
		steps:   &e.steps,
	}
}

// tick 跑一轮并断言不返回错误（入站/公告板的瞬时故障只记日志，不该让 tick 红）。
func (e *inboundEnv) tick(p *poller) {
	e.t.Helper()
	if _, err := p.pollOnce(context.Background()); err != nil {
		e.t.Fatalf("tick 失败：%v", err)
	}
}

// ledgerLines 读账本文件的行（断言「只 claim 了一次」这类事实）。
func (e *inboundEnv) ledgerLines() []string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "processed.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("读账本失败：%v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func containsStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

// 四段顺序是唯一的调度契约（入站 → 自检 → 公告板 → 事件），必须有用例把它钉住：
// 顺序一换，公告板就会推出一张「批了但还没执行」的卡，自检也会在旧状态上做判断。
func TestPollerTickOrder(t *testing.T) {
	env := newInboundEnv(t)
	env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_1","planId":"pln_missing",
		"decision":"approve","patch":null,"by":"user:bob","at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	env.tick(p)

	want := []string{"poll", "ack", "selfcheck", "board", "events"}
	if strings.Join(p.Steps(), ",") != strings.Join(want, ",") {
		t.Fatalf("tick 顺序不符：want=%v got=%v", want, p.Steps())
	}
}

// 同一 requestId 第二次不会被重跑：账本里的 done 挡住它，规划（花钱的那一步）不再发生。
func TestRequestTurnIsNotRerunForSameRequestID(t *testing.T) {
	env := newInboundEnv(t)
	frame := `{"type":"tenon.request","requestId":"req_DEDUPE1","syntheticEventId":"evt_0001",
		"text":"扣 12 件","by":"user:alice","at":"2026-10-27T09:00:00Z"}`
	env.host.pushFrame(frame)

	p := env.poller()
	env.tick(p)
	first := env.llm.Calls()
	if first == 0 {
		t.Fatal("第一条 request 应当真的跑了回合（规划被调用）")
	}

	// 宿主把同一件事又送了一遍（重放/重启后重读）：换一个序号，帧体不变。
	env.host.pushFrame(frame)
	env.tick(p)

	if got := env.llm.Calls(); got != first {
		t.Fatalf("同一 requestId 第二次不该重跑：规划次数 %d → %d", first, got)
	}
	lines := env.ledgerLines()
	if len(lines) != 2 {
		t.Fatalf("账本应当只有 claimed+done 两行，得到 %d 行：%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"phase":"claimed"`) || !strings.Contains(lines[1], `"phase":"done"`) {
		t.Fatalf("账本顺序不对（claimed 必须先落盘）：%v", lines)
	}
	if acks := env.host.acksSeen(); len(acks) != 2 {
		t.Fatalf("两帧都该被 ack，得到 %v", acks)
	}
}

// 可重试错误**不 ack**：帧留在宿主那边，下一 tick 重放。游标也不能前进——
// 前进了就等于把没处理完的帧永久跳过（宿主只按 seq 回放）。
func TestRetryableConfirmIsNotAcked(t *testing.T) {
	env := newInboundEnv(t)
	// 归档件坏了 → Resume 读归档失败 → 可重试（"这一次没问成"）。
	archived := filepath.Join(env.stateDir, "pending", "archived", "pln_broken.json")
	if err := os.WriteFile(archived, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("准备坏归档件失败：%v", err)
	}
	env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_2","planId":"pln_broken",
		"decision":"approve","patch":null,"by":"user:bob","at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	env.tick(p)

	if acks := env.host.acksSeen(); len(acks) != 0 {
		t.Fatalf("可重试错误不该 ack（否则这一帧再也不会重放），得到 ack=%v", acks)
	}
	if p.inbound != 0 {
		t.Fatalf("未 ack 的帧不能让游标前进，得到 inbound=%d", p.inbound)
	}
	if containsStep(p.Steps(), "ack") {
		t.Fatalf("没有 ack 就不该记 ack 阶段：%v", p.Steps())
	}
}

// 确定性拒绝**要 ack**：重放一万次还是同一个结论，留着只会把日志刷满。
func TestDeterministicConfirmRejectionIsAcked(t *testing.T) {
	env := newInboundEnv(t)
	env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_3","planId":"pln_nosuchplan",
		"decision":"approve","patch":null,"by":"user:bob","at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	env.tick(p)

	acks := env.host.acksSeen()
	if len(acks) != 1 || acks[0] != 1 {
		t.Fatalf("确定性拒绝必须 ack 掉，得到 acks=%v", acks)
	}
	if p.inbound != 1 {
		t.Fatalf("ack 之后游标应当前进到 1，得到 %d", p.inbound)
	}
}

// 空 requestId 必须在进账本前拒掉：Claim("") 恒返回 true 且不写盘 = 静默关掉去重。
func TestEmptyRequestIDRejectedBeforeLedger(t *testing.T) {
	env := newInboundEnv(t)
	env.host.pushFrame(`{"type":"tenon.request","text":"扣 12 件","by":"user:alice"}`)

	p := env.poller()
	env.tick(p)

	if lines := env.ledgerLines(); len(lines) != 0 {
		t.Fatalf("缺 requestId 的帧不该写账本，得到 %v", lines)
	}
	if env.llm.Calls() != 0 {
		t.Fatal("缺 requestId 的帧不该开回合")
	}
	if acks := env.host.acksSeen(); len(acks) != 0 {
		t.Fatalf("报文有问题的帧要暴露出来（不 ack，让宿主重放并留日志），得到 acks=%v", acks)
	}
}

// Claim 返回 false 时按 phase 分流：claimed（上次中断）是**唯一会主动丢用户输入**的地方，
// 必须显式告警 + 推一条「请重新发起」的公告板条目，让人看得见。
func TestClaimedPhaseWarnsOnBoardInsteadOfSilentSkip(t *testing.T) {
	env := newInboundEnv(t)
	// 账本逐行等于「上一次进程留下的现场」：先写盘再用 NewLedger 读进来，
	// 直接写文件不会更新已经载入的内存表。
	claimed := `{"requestId":"req_CRASHED1","phase":"claimed","at":"2026-10-27T09:00:00Z"}` + "\n"
	ledgerPath := filepath.Join(env.stateDir, "processed.jsonl")
	if err := os.WriteFile(ledgerPath, []byte(claimed), 0o600); err != nil {
		t.Fatalf("准备账本失败：%v", err)
	}
	reloaded, err := agent.NewLedger(ledgerPath)
	if err != nil {
		t.Fatalf("重新载入账本失败：%v", err)
	}
	env.ledger = reloaded
	env.host.pushFrame(`{"type":"tenon.request","requestId":"req_CRASHED1","syntheticEventId":"evt_0009",
		"text":"扣 12 件","by":"user:alice","at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	env.tick(p)

	if env.llm.Calls() != 0 {
		t.Fatal("上次处理中断的 request 不该重跑（重复扣库存比丢一句请求更贵）")
	}
	pushes := env.host.pushesSeen()
	if len(pushes) != 1 {
		t.Fatalf("claimed 分流必须推一条公告板条目（否则用户看不见自己的请求被丢了），得到 %v", pushes)
	}
	if pushes[0]["planId"] != "req-req_CRASHED1" {
		t.Fatalf("公告板条目应指向那条 request，得到 %v", pushes[0])
	}
	if pushes[0]["state"] != "aborted" {
		t.Fatalf("公告板条目应标成 aborted，得到 %v", pushes[0]["state"])
	}
	if acks := env.host.acksSeen(); len(acks) != 1 {
		t.Fatalf("分流完成后要 ack（不留坏帧每 tick 重来），得到 acks=%v", acks)
	}
}

// 终态覆盖：Resume 返回 nil 后必须从归档件取**真实**终态推上去（reject 也是 nil，
// 写死 done 会把一张被驳回的卡标成已执行）。
func TestRejectPushesTerminalOverrideFromArchive(t *testing.T) {
	env := newInboundEnv(t)
	pending := agent.Pending{
		PlanID:         "pln_reject1",
		State:          agent.StateAwaitingApproval,
		TriggerEventID: "evt_0002",
		Origin:         "request",
		By:             "user:alice",
		Plan:           agent.Plan{PlanID: "pln_reject1"},
		Summary:        "扣 12 件",
		RequiredRoles:  []string{"role:warehouse_manager"},
		CreatedAt:      "2026-10-27T09:00:00Z",
		ExpiresAt:      "2099-01-01T00:00:00Z",
		AuditSettled:   true,
	}
	if err := env.store.Save(pending); err != nil {
		t.Fatalf("准备 pending 失败：%v", err)
	}
	env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_4","planId":"pln_reject1",
		"decision":"reject","patch":null,"by":"user:bob","at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	env.tick(p)

	archived, err := env.store.LoadArchived("pln_reject1")
	if err != nil {
		t.Fatalf("reject 之后归档里应当有 pln_reject1：%v", err)
	}
	if archived.State != agent.StateRejected {
		t.Fatalf("归档状态应当是 rejected，得到 %q", archived.State)
	}
	pushes := env.host.pushesSeen()
	if len(pushes) == 0 {
		t.Fatal("终态覆盖没有推出去：板上会一直挂着一张早就处理完的卡")
	}
	last := pushes[len(pushes)-1]
	if last["planId"] != "pln_reject1" || last["state"] != "rejected" {
		t.Fatalf("终态覆盖必须是 rejected（不能写死 done），得到 %v", last)
	}
	if acks := env.host.acksSeen(); len(acks) != 1 {
		t.Fatalf("处理完毕应当 ack，得到 acks=%v", acks)
	}
}

// Deps.State 为 nil 时不许调 SelfCheck（store.CountCorrupt() 会解引用 nil）。
func TestNilStateStoreSkipsSelfCheck(t *testing.T) {
	env := newInboundEnv(t)
	env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_5","planId":"pln_x",
		"decision":"approve","patch":null,"by":"user:bob","at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	p.store = nil
	p.deps.State = nil // 与装配约定一致：没有 store 就没有挂起通道
	env.tick(p)        // 不得 panic

	if containsStep(p.Steps(), "selfcheck") {
		t.Fatalf("没有挂起通道时不该跑自检：%v", p.Steps())
	}
	want := []string{"poll", "ack", "board", "events"}
	if strings.Join(p.Steps(), ",") != strings.Join(want, ",") {
		t.Fatalf("其余三段必须照跑：want=%v got=%v", want, p.Steps())
	}
}

// ---------- tenon.confirm 的审批人角色来自帧 ----------

// seedApprovalPending 造一份「等人批」的 pending：计划一步，要求 requiredRoles 里的角色。
//
// dispatchedAuditId / auditSettled 必须齐（与真实挂起同形）：缺了它们，同一 tick 里的
// 自检会把它当成「审计缺失」归档成 stale——那是另一个用例的事实，会盖掉这里的断言。
func seedApprovalPending(t *testing.T, env *inboundEnv, planID string, required []string) {
	t.Helper()
	p := agent.Pending{
		PlanID:         planID,
		State:          agent.StateAwaitingApproval,
		TriggerEventID: "evt_roles0001",
		Subject:        json.RawMessage(`"sku:StockItem/STD-1002"`),
		Origin:         "request",
		By:             "user:alice",
		Plan: agent.Plan{
			PlanID:  planID,
			Trigger: agent.PlanTrigger{EventID: "evt_roles0001", CorrelationID: "user:alice"},
			Steps: []agent.PlanStep{{
				StepID: "s1",
				Action: "inventory.GetStock",
				Args:   json.RawMessage(`{"sku":"STD-1002"}`),
				Kind:   "read",
			}},
		},
		Summary:           "查一下 STD-1002 的库存",
		RequiredRoles:     required,
		AuditSettled:      true,
		DispatchedAuditID: "aud_" + planID,
		CreatedAt:         "2026-10-27T09:00:00Z",
		ExpiresAt:         "2099-01-01T00:00:00Z",
	}
	if err := env.store.Save(p); err != nil {
		t.Fatalf("准备 pending 失败：%v", err)
	}
}

// assertApproveDenied 断言「角色校验 fail-closed」：没归档、pending 原样还在、一步都没执行。
// 确定性拒绝（403）必须 ack 掉——重放一万次还是同一个结论。
func (e *inboundEnv) assertApproveDenied(planID string) {
	e.t.Helper()
	if _, err := e.store.LoadArchived(planID); err == nil {
		e.t.Fatal("角色不匹配时不能归档：一旦离开 pending，换个有权限的人也批不了")
	}
	pending, ok, err := e.store.Load(planID)
	if err != nil || !ok {
		e.t.Fatalf("角色不匹配不该动 pending：ok=%v err=%v", ok, err)
	}
	if pending.State != agent.StateAwaitingApproval {
		e.t.Fatalf("pending 必须留在待批状态，得到 %q", pending.State)
	}
	if actions := e.host.actionsSeen(); len(actions) != 0 {
		e.t.Fatalf("403 时一步都不许执行，得到 %v", actions)
	}
	if acks := e.host.acksSeen(); len(acks) != 1 {
		e.t.Fatalf("确定性拒绝（403）必须 ack 掉，得到 %v", acks)
	}
}

// 宿主把提交者 token 的角色写进帧之后，approve 必须真的执行：帧里的 roles 是 agent
// 唯一的审批人角色来源（既不是登录身份，也不是客户端自报）。
func TestConfirmRolesFromFrame(t *testing.T) {
	env := newInboundEnv(t)
	seedApprovalPending(t, env, "pln_roles1", []string{"role:warehouse_manager"})
	env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_roles1","planId":"pln_roles1",
		"decision":"approve","patch":null,"by":"user:bob","roles":["role:warehouse_manager"],
		"at":"2026-10-27T09:00:00Z"}`)

	p := env.poller()
	env.tick(p)

	archived, err := env.store.LoadArchived("pln_roles1")
	if err != nil {
		t.Fatalf("approve 执行完必须归档（帧里的角色没有生效）：%v", err)
	}
	if archived.State != agent.StateDone {
		t.Fatalf("归档状态应当是 done，得到 %q", archived.State)
	}
	actions := env.host.actionsSeen()
	if len(actions) != 1 || !strings.HasSuffix(actions[0], "/tenon/actions/inventory.GetStock") {
		t.Fatalf("角色校验通过后必须真的执行那一步，得到 %v", actions)
	}
	if acks := env.host.acksSeen(); len(acks) != 1 {
		t.Fatalf("处理完毕应当 ack，得到 %v", acks)
	}
	pushes := env.host.pushesSeen()
	if len(pushes) == 0 || pushes[len(pushes)-1]["state"] != "done" {
		t.Fatalf("终态覆盖应推 done（板上那张卡才会落地），得到 %v", pushes)
	}
}

// 客户端自报的角色不能变成审批权限：agent 只认帧里的 roles 这一个字段。
//
// 宿主已经先把自报的角色删掉（见 internal/controlplane 的 TestConfirmFrameRoles），
// 这里守的是 agent 侧**没有第二条准入通道**：不看登录 claims（服务账号自己是
// role:agent）、不看别的帧字段、缺角色时不给默认角色。
func TestConfirmClientRolesIgnored(t *testing.T) {
	t.Run("帧里只有提交者自己的低权限角色", func(t *testing.T) {
		env := newInboundEnv(t)
		seedApprovalPending(t, env, "pln_clerk1", []string{"role:warehouse_manager"})
		// 这帧的 roles 已经是宿主按 token 换出来的 role:clerk；另外再塞两处「看起来像
		// 角色」的字段，agent 一个都不许读（读了就等于把客户端自报又捡回来）。
		env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_clerk1","planId":"pln_clerk1",
			"decision":"approve","patch":null,"by":"user:alice","roles":["role:clerk"],
			"claims":{"roles":["role:warehouse_manager"]},
			"approverRoles":["role:warehouse_manager"],"at":"2026-10-27T09:00:00Z"}`)

		p := env.poller()
		env.tick(p)

		env.assertApproveDenied("pln_clerk1")
	})

	t.Run("帧里没有 roles", func(t *testing.T) {
		env := newInboundEnv(t)
		seedApprovalPending(t, env, "pln_noroles1", []string{"role:warehouse_manager"})
		// 缺 roles 就是空集：不得回落到登录身份。假宿主的 /tenon/session 明确回了
		// role:agent——若有人把「登录者的角色」当审批人角色用，这里就会放行。
		env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_noroles1","planId":"pln_noroles1",
			"decision":"approve","patch":null,"by":"user:alice","at":"2026-10-27T09:00:00Z"}`)

		p := env.poller()
		env.tick(p)

		env.assertApproveDenied("pln_noroles1")
	})

	t.Run("其它自报字段既不授予也不剥夺", func(t *testing.T) {
		env := newInboundEnv(t)
		seedApprovalPending(t, env, "pln_junk1", []string{"role:warehouse_manager"})
		// 帧里的 roles 才是唯一来源：夹带的字段既不能额外授予（上面那条用例）、
		// 也不能把 token 换来的角色「取交集」掉——合并/裁剪都是另一种准入语义。
		env.host.pushFrame(`{"type":"tenon.confirm","confirmId":"cfm_junk1","planId":"pln_junk1",
			"decision":"approve","patch":null,"by":"user:bob","roles":["role:warehouse_manager"],
			"claims":{"roles":["role:clerk"]},"approverRoles":["role:clerk"],
			"at":"2026-10-27T09:00:00Z"}`)

		p := env.poller()
		env.tick(p)

		archived, err := env.store.LoadArchived("pln_junk1")
		if err != nil {
			t.Fatalf("帧里的 roles 必须生效（夹带字段不许裁掉它）：%v", err)
		}
		if archived.State != agent.StateDone {
			t.Fatalf("归档状态应当是 done，得到 %q", archived.State)
		}
		if actions := env.host.actionsSeen(); len(actions) != 1 {
			t.Fatalf("角色生效后必须真的执行那一步，得到 %v", actions)
		}
	})
}
