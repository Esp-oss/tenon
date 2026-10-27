package conformance

// 宿主模式用例：C4/C5 只有在给了宿主地址（Options.HostTarget）时才能判定。
//
// 分工是刻意的：
//   - 判 PASS 用**真实宿主**（internal/controlplane）。审计链与 fail-closed 都是它的行为，
//     拿桩自证等于让判据跟着桩走。
//   - 判 FAIL 用**桩**。要构造的形态（第二段没落盘、策略一律放行）是真实宿主不应该有的，
//     只能靠桩造出来；它们证明这两条用例不是「永远 PASS 的摆设」。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"tenon.dev/internal/controlplane"
	"tenon.dev/internal/controlplane/policy"
	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/spec"
)

// ---------- 脚手架 ----------

// newRealHost 起一个真实的控制平面宿主。
//
// 策略里放一条 inventory.DeductStock → auto 的规则，目的不是「让测试好看」，而是让 C5 的
// 探针不是对着一台「什么都拒绝」的空壳跑：同一台宿主对规则命中的 plan 会给出 auto，
// 才说明 C5 看到的 deny 来自闸门判定。
func newRealHost(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := controlplane.New(controlplane.Config{
		Policies: []policy.PolicyRule{{
			PolicyID: "pol-conformance-auto",
			Priority: 10,
			Match:    policy.PolicyMatch{Module: "inventory", Actions: []string{"inventory.DeductStock"}},
			Outcome:  policy.OutcomeAuto,
		}},
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		Modules:   []registry.ModuleEntry{{ID: "inventory", BaseURL: "http://127.0.0.1:8081"}},
		// 装配期要求会话密钥；一致性套件不测登录，给个固定值即可。
		SessionSecret: []byte("conformance-secret"),
	})
	if err != nil {
		t.Fatalf("装配真实宿主失败：%v", err)
	}
	t.Cleanup(func() {
		// Windows 上不关审计句柄会锁住文件，下一次打开与链校验都会失败。
		if err := srv.Close(); err != nil {
			t.Errorf("关闭宿主审计存储失败：%v", err)
		}
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// newModuleServer 起一个合规的假模块（复用 conformance_test.go 里的 fake）：
// 宿主模式的报告里 C1/C8 仍应成立，不能让它们变成噪音。
func newModuleServer(t *testing.T) *httptest.Server {
	t.Helper()
	f := &fake{idemSeen: map[string][]byte{}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return ts
}

// mustSpec 加载 spec/0.1 的 schema 集合（C1 需要）。
func mustSpec(t *testing.T) *spec.Set {
	t.Helper()
	set, err := spec.Load(filepath.Join("..", "spec", "0.1", "schemas"))
	if err != nil {
		t.Fatalf("加载 schema 失败：%v", err)
	}
	return set
}

// statusOf 取某条用例的结论；第二个返回值表示报告里有没有这条。
func statusOf(rep Report, caseID string) (Status, bool) {
	for _, r := range rep.Results {
		if r.Case == caseID {
			return r.Status, true
		}
	}
	return "", false
}

// detailOf 取某条用例的 detail，让断言失败时能直接看到原因。
func detailOf(rep Report, caseID string) string {
	for _, r := range rep.Results {
		if r.Case == caseID {
			return r.Detail
		}
	}
	return "（报告里没有这条用例）"
}

// evidenceOf 取某条用例的证据列表。
func evidenceOf(rep Report, caseID string) []string {
	for _, r := range rep.Results {
		if r.Case == caseID {
			return r.Evidence
		}
	}
	return nil
}

// ---------- 真实宿主：C4/C5 判 PASS ----------

func TestHostModeMakesC4AndC5Pass(t *testing.T) {
	host := newRealHost(t)
	module := newModuleServer(t)

	rep := Run(context.Background(), Options{
		Target: module.URL, HostTarget: host.URL, Spec: mustSpec(t), ModuleID: "fakeinv",
	})

	for _, r := range rep.Results {
		if r.Status == Fail {
			t.Fatalf("%s 判 FAIL：%s", r.Case, r.Detail)
		}
	}
	if got, _ := statusOf(rep, "C1"); got != Pass {
		t.Fatalf("宿主模式下模块侧 C1 仍应 PASS，得到 %q（detail=%s）", got, detailOf(rep, "C1"))
	}
	if got, _ := statusOf(rep, "C4"); got != Pass {
		t.Fatalf("宿主模式下 C4 应 PASS，得到 %q（detail=%s）", got, detailOf(rep, "C4"))
	}
	if got, _ := statusOf(rep, "C5"); got != Pass {
		t.Fatalf("宿主模式下 C5 应 PASS，得到 %q（detail=%s）", got, detailOf(rep, "C5"))
	}
	if got, _ := statusOf(rep, "D1"); got != Skip {
		t.Fatalf("没有 LLM 规划器时 D1 永远 SKIP，得到 %q（detail=%s）", got, detailOf(rep, "D1"))
	}

	// PASS 必须带证据：C4 要能看见 hash 前缀，C5 要能看见「无命中默认拒绝」的理由码。
	c4Evidence := strings.Join(evidenceOf(rep, "C4"), " | ")
	if !strings.Contains(c4Evidence, hashScheme) {
		t.Errorf("C4 的证据里应能看到 hash 前缀 %s：%v", hashScheme, evidenceOf(rep, "C4"))
	}
	if !strings.Contains(c4Evidence, "prevHash") {
		t.Errorf("C4 的证据里应能看到 prevHash 链的判定：%v", evidenceOf(rep, "C4"))
	}
	c5Evidence := strings.Join(evidenceOf(rep, "C5"), " | ")
	if !strings.Contains(c5Evidence, reasonNoMatch) {
		t.Errorf("C5 的证据里应记录 %s：%v", reasonNoMatch, evidenceOf(rep, "C5"))
	}
	// 四个探针都要出现在证据里，否则「三种输入都 deny」可能只跑了一部分。
	for _, label := range []string{"①", "②", "③", "④"} {
		if !strings.Contains(c5Evidence, label) {
			t.Errorf("C5 的证据里缺少探针 %s：%v", label, evidenceOf(rep, "C5"))
		}
	}

	// 反证：同一台宿主对规则命中的 plan 给出 auto。没有这一步，C5 的 PASS 可能只是
	// 「策略端点一律拒绝」——那证明不了 fail-closed，只证明了一台拒绝一切的机器。
	raw, status, err := doJSON(context.Background(), host.Client(), http.MethodPost,
		host.URL+"/tenon/policies:evaluate",
		[]byte(`{"plan":{"planId":"pln_conformance_control","steps":[{"stepId":"s1","action":"inventory.DeductStock","args":{"qty":1}}]}}`),
		nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("对照探针请求失败：status=%d err=%v", status, err)
	}
	var control PolicyEvaluationView
	if err := json.Unmarshal(raw, &control); err != nil {
		t.Fatalf("对照探针响应不是 PolicyEvaluationResponse：%v（原文 %s）", err, clip(raw))
	}
	if control.Outcome == outcomeDeny {
		t.Fatalf("对照探针（规则命中的 inventory.DeductStock）也得到 deny：本用例无法区分「闸门判定」与「一律拒绝」")
	}
	if control.Outcome != "auto" {
		t.Errorf("对照探针应命中 auto 规则，得到 %q（decidedBy=%v）", control.Outcome, control.DecidedBy)
	}

	// 退出码语义不变：宿主模式只把 C4/C5 从 SKIP 变成 PASS，D1 仍是 SKIP，
	// 所以 --strict 下退出码仍是 2，非 strict 下是 0（无 FAIL）。
	if code := rep.ExitCode(false); code != 0 {
		t.Errorf("有 SKIP 但无 FAIL 时退出码应为 0，实际 %d", code)
	}
	if code := rep.ExitCode(true); code != 2 {
		t.Errorf("--strict 下仍有 SKIP（D1）时退出码应为 2，实际 %d", code)
	}
	pass, fail, skip := rep.Counts()
	t.Logf("宿主模式 pass=%d fail=%d skip=%d", pass, fail, skip)
	// 证据是报告要交付的内容，打进测试日志便于人工复核判据是否真的成立。
	t.Logf("C4 证据：%s", strings.Join(evidenceOf(rep, "C4"), "\n           "))
	t.Logf("C5 证据：%s", strings.Join(evidenceOf(rep, "C5"), "\n           "))
}

// ---------- 无宿主地址：C4/C5 必须仍是 SKIP（防回归）----------

func TestModuleModeKeepsC4AndC5Skip(t *testing.T) {
	module := newModuleServer(t)

	rep := Run(context.Background(), Options{
		Target: module.URL, Spec: mustSpec(t), ModuleID: "fakeinv",
	})

	for _, caseID := range []string{"C4", "C5", "D1"} {
		got, ok := statusOf(rep, caseID)
		if !ok || got != Skip {
			t.Fatalf("模块模式下 %s 应 SKIP，得到 %q（存在=%v，detail=%s）", caseID, got, ok, detailOf(rep, caseID))
		}
		if strings.TrimSpace(detailOf(rep, caseID)) == "" {
			t.Fatalf("%s 判 SKIP 必须写明原因（不能只给一个空结论）", caseID)
		}
	}
	// 空 HostTarget 不是失败：模块模式的退出码语义与新增宿主模式之前一致。
	if code := rep.ExitCode(false); code != 0 {
		t.Errorf("模块模式下有 SKIP 但无 FAIL，退出码应为 0，实际 %d", code)
	}
	if code := rep.ExitCode(true); code != 2 {
		t.Errorf("--strict 下 SKIP 的退出码应为 2，实际 %d", code)
	}
}

// ---------- 反向用例：坏掉的宿主必须判 FAIL，且理由具体 ----------

// hostStub 是只挂 C4/C5 所需请求形态的假宿主，行为由开关决定。
type hostStub struct {
	mu         sync.Mutex
	records    []map[string]any
	dropResult bool
	failOpen   bool
}

// newHostStub 起一个行为可配的桩宿主。
func newHostStub(t *testing.T, configure func(*hostStub)) *httptest.Server {
	t.Helper()
	s := &hostStub{}
	if configure != nil {
		configure(s)
	}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(ts.Close)
	return ts
}

// serve 按路径分派：审计的 POST/GET 与策略评估。
func (s *hostStub) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/tenon/audit" && r.Method == http.MethodPost:
		s.appendAudit(w, r)
	case r.URL.Path == "/tenon/audit" && r.Method == http.MethodGet:
		s.queryAudit(w, r)
	case r.URL.Path == "/tenon/policies:evaluate" && r.Method == http.MethodPost:
		s.evaluate(w)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"tenonError":{"class":"contract_violation","code":"UNKNOWN_ENDPOINT","message":"stub","retryable":false}}`)
	}
}

// appendAudit 追加一条审计；dropResult=true 时对第二段「假装写成功」但不落盘。
func (s *hostStub) appendAudit(w http.ResponseWriter, r *http.Request) {
	var rec struct {
		AuditID string `json:"auditId"`
		Stage   string `json:"stage"`
		PlanID  string `json:"planId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropResult && rec.Stage == stageResult {
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	var prev any
	if n := len(s.records); n > 0 {
		prev = s.records[n-1]["hash"]
	}
	s.records = append(s.records, map[string]any{
		"auditId":  rec.AuditID,
		"stage":    rec.Stage,
		"planId":   rec.PlanID,
		"hash":     hashScheme + fmt.Sprintf("%064x", len(s.records)),
		"prevHash": prev,
	})
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// queryAudit 按 planId 过滤并遵守 limit（与宿主同义：先过滤再截断）。
func (s *hostStub) queryAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	planID := q.Get("planId")
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	s.mu.Lock()
	out := make([]map[string]any, 0, len(s.records))
	for _, rec := range s.records {
		if planID != "" && rec["planId"] != planID {
			continue
		}
		out = append(out, rec)
	}
	s.mu.Unlock()
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"records": out, "nextCursor": strconv.Itoa(len(out))})
}

// evaluate 返回策略评估：failOpen=true 时一律放行（模拟 fail-open 宿主）。
func (s *hostStub) evaluate(w http.ResponseWriter) {
	if s.failOpen {
		_, _ = io.WriteString(w, `{"outcome":"auto","decidedBy":["pol-stub-fail-open"],"reasons":["POLICY_AUTO_APPROVED"]}`)
		return
	}
	_, _ = io.WriteString(w, `{"outcome":"deny","decidedBy":[],"reasons":["NO_POLICY_MATCH_DEFAULT_DENY"]}`)
}

// TestHostModeC4FailsWhenResultSegmentMissing：第二段没落盘时 C4 必须 FAIL，且 detail 指名
// 缺的是哪一段——运维要能一眼定位，而不是看到「审计不对」再去翻日志。
func TestHostModeC4FailsWhenResultSegmentMissing(t *testing.T) {
	stub := newHostStub(t, func(s *hostStub) { s.dropResult = true })
	module := newModuleServer(t)

	rep := Run(context.Background(), Options{
		Target: module.URL, HostTarget: stub.URL, Spec: mustSpec(t), ModuleID: "fakeinv",
	})

	got, _ := statusOf(rep, "C4")
	if got != Fail {
		t.Fatalf("第二段没落盘应判 C4 FAIL，得到 %q（detail=%s）", got, detailOf(rep, "C4"))
	}
	detail := detailOf(rep, "C4")
	if !strings.Contains(detail, "第二段") || !strings.Contains(detail, "stage=result") {
		t.Fatalf("C4 的 detail 必须指出缺的是第二段（stage=result），实际：%s", detail)
	}
	if code := rep.ExitCode(false); code != 1 {
		t.Errorf("有 FAIL 时退出码应为 1，实际 %d", code)
	}
}

// TestHostModeC5FailsWhenPolicyFailsOpen：策略端点一律放行时 C5 必须 FAIL，并指出是哪个探针
// 被放行——fail-open 是最难在线上发现的一种缺陷（看起来一切正常）。
func TestHostModeC5FailsWhenPolicyFailsOpen(t *testing.T) {
	stub := newHostStub(t, func(s *hostStub) { s.failOpen = true })
	module := newModuleServer(t)

	rep := Run(context.Background(), Options{
		Target: module.URL, HostTarget: stub.URL, Spec: mustSpec(t), ModuleID: "fakeinv",
	})

	got, _ := statusOf(rep, "C5")
	if got != Fail {
		t.Fatalf("策略端点一律放行应判 C5 FAIL，得到 %q（detail=%s）", got, detailOf(rep, "C5"))
	}
	detail := detailOf(rep, "C5")
	if !strings.Contains(detail, "①") || !strings.Contains(detail, "deny") {
		t.Fatalf("C5 的 detail 必须指出被放行的探针与期望的处置，实际：%s", detail)
	}
}

// TestHostModeFailsWhenHostUnreachable：给了宿主地址但宿主不可达是 FAIL，不是 SKIP。
// 「你让我测宿主，宿主没应答」不能退化成「这条没判」。
func TestHostModeFailsWhenHostUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	module := newModuleServer(t)
	rep := Run(context.Background(), Options{
		Target: module.URL, HostTarget: deadURL, Spec: mustSpec(t), ModuleID: "fakeinv",
	})

	for _, caseID := range []string{"C4", "C5"} {
		got, _ := statusOf(rep, caseID)
		if got != Fail {
			t.Fatalf("宿主不可达时 %s 应判 FAIL（不能静默 SKIP），得到 %q（detail=%s）", caseID, got, detailOf(rep, caseID))
		}
	}
	if code := rep.ExitCode(false); code != 1 {
		t.Errorf("有 FAIL 时退出码应为 1，实际 %d", code)
	}
}
