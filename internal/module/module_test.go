package module

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

const (
	fixtureModuleID = "demo"
	readActionID    = "demo.GetThing"
	writeActionID   = "demo.CreateThing"
	rejectName      = "boom"
)

// eventBody 是一份符合 domain-event.schema.json 的事件信封。
const eventBody = `{"eventId":"evt_01JBTEST0001","type":"quote.completed","version":"0.1.0",` +
	`"subject":"quote:Quote/Q-2026-0417","occurredAt":"2026-10-27T09:15:00Z","causationId":null,` +
	`"correlationId":"cor:quote/Q-2026-0417","payload":{"qty":12}}`

// TestMain 把环境变量的缺省值指到用例专属临时目录：即使某个用例忘了 t.Setenv，
// 审计文件也不会写进工作区（本任务只允许出现受版本控制的文件）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tenon-module-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时目录失败：%v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv(EnvAuditPath, filepath.Join(dir, "audit.jsonl"))
	_ = os.Setenv(EnvHMACSecretFile, filepath.Join(dir, "absent-secret.txt"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// prepareEnv 给用例准备独占的密钥文件与审计路径，并返回密钥原文（= 文件内容去掉首尾空白）。
func prepareEnv(t *testing.T) []byte {
	t.Helper()
	secret := []byte("test-secret-" + t.Name())
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "hmac.txt")
	if err := os.WriteFile(secretPath, append(append([]byte(nil), secret...), '\n'), 0o600); err != nil {
		t.Fatalf("写入密钥文件失败：%v", err)
	}
	t.Setenv(EnvHMACSecretFile, secretPath)
	t.Setenv(EnvAuditPath, filepath.Join(dir, "audit.jsonl"))
	return secret
}

// fixture 是一个最小可用的模块：一个 read、一个 write（带幂等声明），
// 计数器用来证明「重复提交没有产生第二次副作用」。
type fixture struct {
	module   *Module
	executed atomic.Int32
}

func newFixture() *fixture {
	f := &fixture{}
	f.module = &Module{
		ID:      fixtureModuleID,
		Title:   "演示模块",
		Version: "0.1.0",
		Actions: []Action{
			{
				ID:      readActionID,
				Version: "0.1.0",
				Title:   "读一个东西",
				Kind:    "read",
				InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {"name": {"type": "string"}}
}`),
				OutputSchema: json.RawMessage(`{"type":"object","required":["onHand"],"properties":{"onHand":{"type":"integer"}}}`),
				Risk:         Risk{Level: "low", Reversible: true},
				TimeoutMs:    30000,
				DryRun:       DryRunCapability{Supported: false},
			},
			{
				ID:      writeActionID,
				Version: "0.1.0",
				Title:   "写一个东西",
				Kind:    "write",
				InputSchema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {"name": {"type": "string"}}
}`),
				OutputSchema: json.RawMessage(`{"type":"object","required":["seq"],"properties":{"seq":{"type":"integer"}}}`),
				Risk:         Risk{Level: "medium", Reversible: true},
				Idempotency: &Idempotency{
					KeyFields:         []string{"name"},
					Scope:             "module",
					DuplicateBehavior: "return_first_result",
				},
				TimeoutMs: 30000,
				DryRun:    DryRunCapability{Supported: true},
			},
		},
		Events: []string{"demo.created"},
		Handle: func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
			var in struct {
				Name string `json:"name"`
			}
			if actionID == readActionID {
				// 未知字段被忽略是契约要求，所以这里不做任何严格模式处理。
				return json.RawMessage(`{"onHand":7}`), nil
			}
			if actionID != writeActionID {
				return nil, testError(protocol.ClassContractViolation, "UNKNOWN_ACTION", "测试模块不提供 "+actionID)
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, testError(protocol.ClassContractViolation, "SCHEMA_MISMATCH", err.Error())
			}
			if in.Name == rejectName {
				e := protocol.NewError(protocol.ClassBusinessRejection, "STOCK_INSUFFICIENT", "可用 3，需求 12")
				e.Details = map[string]any{"available": 3}
				return nil, &e
			}
			body, err := json.Marshal(map[string]any{"seq": f.executed.Add(1), "name": in.Name})
			if err != nil {
				return nil, testError(protocol.ClassContractViolation, "TEST_MARSHAL_FAILED", err.Error())
			}
			return body, nil
		},
		DryRun: func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
			if actionID != writeActionID {
				return nil, testError(protocol.ClassContractViolation, "DRY_RUN_UNSUPPORTED", "测试模块不预演 "+actionID)
			}
			return json.RawMessage(`{"status":"verified","diff":[{"entity":"demo:Thing/x","field":"onHand","from":0,"to":1}]}`), nil
		},
	}
	return f
}

func (f *fixture) actionIndex(t *testing.T, id string) int {
	t.Helper()
	for i := range f.module.Actions {
		if f.module.Actions[i].ID == id {
			return i
		}
	}
	t.Fatalf("测试夹具里没有 action %q", id)
	return -1
}

func testError(class protocol.ErrorClass, code, message string) *protocol.Error {
	e := protocol.NewError(class, code, message)
	return &e
}

func serve(t *testing.T, m *Module) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// do 发一个请求。body 为 nil 表示无请求体；extra 里的头覆盖默认头（值留空即置空该头）。
func do(t *testing.T, method, url string, body []byte, extra map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 失败：%v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp, raw
}

func doJSON(t *testing.T, method, url string, payload any, extra map[string]string) (*http.Response, []byte) {
	t.Helper()
	if payload == nil {
		return do(t, method, url, nil, extra)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编组请求体失败：%v", err)
	}
	return do(t, method, url, body, extra)
}

func idem(key string) map[string]string {
	return map[string]string{protocol.HeaderIdempotencyKey: key}
}

func decodeTenonError(t *testing.T, raw []byte) protocol.Error {
	t.Helper()
	var env protocol.ErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("错误响应不是 tenonError 信封：%v（原文 %s）", err, raw)
	}
	return env.TenonError
}

func eventBodyWithID(id string) []byte {
	return []byte(strings.Replace(eventBody, "evt_01JBTEST0001", id, 1))
}

// readAuditLines 读回模块侧审计 JSONL。
func readAuditLines(t *testing.T) []auditRecord {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv(EnvAuditPath))
	if err != nil {
		t.Fatalf("读取审计文件失败：%v", err)
	}
	var records []auditRecord
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("审计行不是合法 JSON：%v（原文 %s）", err, line)
		}
		records = append(records, rec)
	}
	return records
}

// 1. manifest 是摘要级：动作项只有六个字段，且原始 JSON 里不含 inputSchema。
func TestManifestIsSummaryOnly(t *testing.T) {
	prepareEnv(t)
	srv := serve(t, newFixture().module)

	resp, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	if got := resp.Header.Get(protocol.HeaderVersion); got != protocol.Version {
		t.Errorf("响应头 %s = %q，期望 %q", protocol.HeaderVersion, got, protocol.Version)
	}
	for _, forbidden := range []string{"inputSchema", "outputSchema", "idempotency"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("manifest 摘要不得包含 %q，原文：%s", forbidden, raw)
		}
	}

	var got struct {
		Tenon  string `json:"tenon"`
		Module struct {
			ID      string `json:"id"`
			Version string `json:"version"`
			Title   string `json:"title"`
		} `json:"module"`
		Actions      []map[string]json.RawMessage `json:"actions"`
		Events       []eventSummary               `json:"events"`
		Transports   []string                     `json:"transports"`
		Capabilities []string                     `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("manifest 不是合法 JSON：%v（原文 %s）", err, raw)
	}
	if got.Tenon != protocol.Version {
		t.Errorf("manifest.tenon = %q，期望 %q", got.Tenon, protocol.Version)
	}
	if got.Module.ID != fixtureModuleID || got.Module.Version != "0.1.0" || got.Module.Title == "" {
		t.Errorf("manifest.module 不完整：%+v", got.Module)
	}
	if len(got.Actions) != 2 {
		t.Fatalf("manifest.actions 长度 = %d，期望 2", len(got.Actions))
	}
	wantKeys := []string{"id", "version", "title", "kind", "risk", "dryRun"}
	for i, summary := range got.Actions {
		if len(summary) != len(wantKeys) {
			t.Fatalf("actions[%d] 有 %d 个字段 %v，期望恰好 %v", i, len(summary), keysOf(summary), wantKeys)
		}
		for _, key := range wantKeys {
			if _, ok := summary[key]; !ok {
				t.Errorf("actions[%d] 缺少字段 %q", i, key)
			}
		}
	}
	if len(got.Events) != 1 || got.Events[0].Type != "demo.created" || got.Events[0].Version == "" {
		t.Errorf("manifest.events = %+v，期望一条带 version 的 demo.created", got.Events)
	}
	if len(got.Transports) != 1 || got.Transports[0] != "http" {
		t.Errorf("manifest.transports = %v，期望 [\"http\"]", got.Transports)
	}
	if !contains(got.Capabilities, "dry-run") || !contains(got.Capabilities, "event-replay") {
		t.Errorf("manifest.capabilities = %v，期望同时含 dry-run 与 event-replay", got.Capabilities)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// 2. 写操作缺 Idempotency-Key → 400 contract_violation，且不执行。
func TestWriteRequiresIdempotencyKey(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)

	resp, raw := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID, map[string]any{"name": "alpha"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（原文 %s）", resp.StatusCode, raw)
	}
	got := decodeTenonError(t, raw)
	if got.Class != protocol.ClassContractViolation {
		t.Errorf("class = %q，期望 %q", got.Class, protocol.ClassContractViolation)
	}
	if got.Code != "MISSING_IDEMPOTENCY_KEY" {
		t.Errorf("code = %q，期望 MISSING_IDEMPOTENCY_KEY", got.Code)
	}
	if got.Retryable {
		t.Error("contract_violation 必须 retryable=false（errors.md §3）")
	}
	if n := f.executed.Load(); n != 0 {
		t.Errorf("业务回调被调用 %d 次，期望 0 次", n)
	}
}

// 3. 同键重复提交 → 两次响应体逐字节相同，业务计数器只 +1（真幂等）。
func TestWriteIsIdempotent(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)
	url := srv.URL + "/tenon/actions/" + writeActionID
	headers := idem("tenon:pln_01JB/s1")
	payload := map[string]any{"name": "alpha"}

	resp1, raw1 := doJSON(t, http.MethodPost, url, payload, headers)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("首次调用状态码 = %d，期望 200（原文 %s）", resp1.StatusCode, raw1)
	}
	resp2, raw2 := doJSON(t, http.MethodPost, url, payload, headers)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("重复调用状态码 = %d，期望 200（原文 %s）", resp2.StatusCode, raw2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Errorf("重复提交的响应体不同：%s vs %s", raw1, raw2)
	}
	if n := f.executed.Load(); n != 1 {
		t.Errorf("业务回调被调用 %d 次，期望 1 次（重复提交不得产生第二次副作用）", n)
	}

	// 同键不同参数是调用方 bug：必须拒绝，而不是静默按首次执行（semantics.md §5.2）。
	resp3, raw3 := doJSON(t, http.MethodPost, url, map[string]any{"name": "beta"}, headers)
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("同键异参状态码 = %d，期望 400（原文 %s）", resp3.StatusCode, raw3)
	}
	if got := decodeTenonError(t, raw3); got.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("code = %q，期望 IDEMPOTENCY_KEY_REUSED", got.Code)
	}
	if n := f.executed.Load(); n != 1 {
		t.Errorf("同键异参也被执行了，计数器 = %d，期望 1", n)
	}

	// 换一个键则是另一笔业务：必须真的执行。
	resp4, raw4 := doJSON(t, http.MethodPost, url, payload, idem("tenon:pln_01JB/s2"))
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("新键状态码 = %d，期望 200（原文 %s）", resp4.StatusCode, raw4)
	}
	if n := f.executed.Load(); n != 2 {
		t.Errorf("新键调用后计数器 = %d，期望 2", n)
	}
}

// 4. 未知字段必须被忽略（json.Decoder 不设 DisallowUnknownFields）。
func TestUnknownFieldsAreIgnored(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)

	resp, raw := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID, map[string]any{
		"name":           "alpha",
		"totallyUnknown": 1,
		"nested":         map[string]any{"deep": []any{1, 2, 3}},
	}, idem("tenon:pln_unknown/s1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte(`"seq":1`)) {
		t.Errorf("响应体 = %s，期望带上业务出参", raw)
	}

	respRead, rawRead := doJSON(t, http.MethodGet, srv.URL+"/tenon/actions/"+readActionID, nil, nil)
	if respRead.StatusCode != http.StatusOK {
		t.Fatalf("读动作定义状态码 = %d，期望 200（原文 %s）", respRead.StatusCode, rawRead)
	}
	if !bytes.Contains(rawRead, []byte("inputSchema")) {
		t.Errorf("GET /tenon/actions/{id} 必须返回完整 Action（含 inputSchema）：%s", rawRead)
	}
	respRun, rawRun := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+readActionID, map[string]any{
		"name":   "alpha",
		"extra!": "ignored",
	}, nil)
	if respRun.StatusCode != http.StatusOK {
		t.Fatalf("读动作执行状态码 = %d，期望 200（原文 %s）", respRun.StatusCode, rawRun)
	}
}

// 5. 模块不支持预演 → POST :dry-run 返回 404 且响应体为空（能力不存在，不是错误）。
func TestDryRunUnsupportedReturnsEmpty404(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	// 模块级：没有任何预演能力；动作级：声明 supported=false，与实现一致。
	f.module.DryRun = nil
	f.module.Actions[f.actionIndex(t, writeActionID)].DryRun = DryRunCapability{Supported: false}
	srv := serve(t, f.module)
	url := srv.URL + "/tenon/actions/" + writeActionID + ":dry-run"

	resp, raw := doJSON(t, http.MethodPost, url, map[string]any{"name": "alpha"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（原文 %q）", resp.StatusCode, raw)
	}
	if len(raw) != 0 {
		t.Fatalf("不支持预演时必须返回空响应体，实际 %q", raw)
	}

	// 未知 action 的预演仍是「动作不存在」：404 但带报文，便于调用方区分。
	respUnknown, rawUnknown := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/demo.Nope:dry-run", map[string]any{"name": "alpha"}, nil)
	if respUnknown.StatusCode != http.StatusNotFound {
		t.Fatalf("未知动作状态码 = %d，期望 404（原文 %s）", respUnknown.StatusCode, rawUnknown)
	}
	if got := decodeTenonError(t, rawUnknown); got.Code != "UNKNOWN_ACTION" {
		t.Errorf("code = %q，期望 UNKNOWN_ACTION", got.Code)
	}
}

// 5b. 支持预演的动作 → 200 + {"dryRun":{status:"verified",diff:[...]}}，且不改状态。
func TestDryRunSupportedReturnsReport(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)

	resp, raw := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID+":dry-run",
		map[string]any{"name": "alpha"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	var got struct {
		DryRun struct {
			Status string `json:"status"`
			Diff   []struct {
				Entity string `json:"entity"`
				Field  string `json:"field"`
				From   int    `json:"from"`
				To     int    `json:"to"`
			} `json:"diff"`
		} `json:"dryRun"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("预演响应不是合法 JSON：%v（原文 %s）", err, raw)
	}
	if got.DryRun.Status != "verified" {
		t.Errorf("dryRun.status = %q，期望 verified", got.DryRun.Status)
	}
	if len(got.DryRun.Diff) != 1 || got.DryRun.Diff[0].Field != "onHand" {
		t.Errorf("dryRun.diff = %+v，期望一条 onHand 变化", got.DryRun.Diff)
	}
	if n := f.executed.Load(); n != 0 {
		t.Errorf("预演产生了 %d 次副作用，期望 0", n)
	}

	// 声明 supported=false 的动作（本夹具的 read 动作）同样按能力不存在处理。
	respRead, rawRead := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+readActionID+":dry-run",
		map[string]any{"name": "alpha"}, nil)
	if respRead.StatusCode != http.StatusNotFound || len(rawRead) != 0 {
		t.Errorf("read 动作预演 = %d 且体长 %d，期望 404 且空体", respRead.StatusCode, len(rawRead))
	}
}

// 6. 事件验签：正确签名 → 200 且可读回；错误签名与超容差 → 401；读不到密钥 → 一律拒。
func TestEventSignatureVerification(t *testing.T) {
	secret := prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)
	now := time.Now().UTC()

	countEvents := func() int {
		t.Helper()
		resp, raw := doJSON(t, http.MethodGet, srv.URL+"/tenon/events", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("补课状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
		}
		var page struct {
			Events     []json.RawMessage `json:"events"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatalf("补课响应不是合法 JSON：%v（原文 %s）", err, raw)
		}
		if page.NextCursor == "" {
			t.Errorf("补课响应缺少 nextCursor：%s", raw)
		}
		return len(page.Events)
	}

	t.Run("正确签名可投递并可读回", func(t *testing.T) {
		body := eventBodyWithID("evt_01JBSIGN0001")
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
			protocol.HeaderSignature: protocol.Sign(secret, now.Unix(), body),
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
		}
		respPage, rawPage := doJSON(t, http.MethodGet, srv.URL+"/tenon/events", nil, nil)
		var page struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(rawPage, &page); err != nil {
			t.Fatalf("补课响应不是合法 JSON：%v", err)
		}
		if respPage.StatusCode != http.StatusOK || len(page.Events) != 1 {
			t.Fatalf("补课返回 %d 条事件，期望 1 条", len(page.Events))
		}
		if !bytes.Equal(bytes.TrimSpace(page.Events[0]), body) {
			t.Errorf("读回的事件信封被改写：%s，期望 %s", page.Events[0], body)
		}
	})

	t.Run("错误签名被拒", func(t *testing.T) {
		body := eventBodyWithID("evt_01JBSIGN0002")
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
			protocol.HeaderSignature: protocol.Sign([]byte("not-the-secret"), now.Unix(), body),
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（原文 %s）", resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "SIGNATURE_INVALID" {
			t.Errorf("code = %q，期望 SIGNATURE_INVALID", got.Code)
		}
		if n := countEvents(); n != 1 {
			t.Errorf("验签失败的事件入库了：事件数 = %d，期望 1", n)
		}
	})

	t.Run("时间戳超容差被拒", func(t *testing.T) {
		body := eventBodyWithID("evt_01JBSIGN0003")
		stale := now.Add(-2 * protocol.SignatureTolerance)
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
			protocol.HeaderSignature: protocol.Sign(secret, stale.Unix(), body),
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（原文 %s）", resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "SIGNATURE_EXPIRED" {
			t.Errorf("code = %q，期望 SIGNATURE_EXPIRED", got.Code)
		}
		if n := countEvents(); n != 1 {
			t.Errorf("超容差的事件入库了：事件数 = %d，期望 1", n)
		}
	})

	t.Run("缺签名头被拒", func(t *testing.T) {
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", eventBodyWithID("evt_01JBSIGN0004"), nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（原文 %s）", resp.StatusCode, raw)
		}
	})

	t.Run("读不到密钥一律拒", func(t *testing.T) {
		t.Setenv(EnvHMACSecretFile, filepath.Join(t.TempDir(), "absent.txt"))
		body := eventBodyWithID("evt_01JBSIGN0005")
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
			protocol.HeaderSignature: protocol.Sign(secret, now.Unix(), body),
		})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("状态码 = %d，期望 503（fail-closed，原文 %s）", resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "SIGNATURE_SECRET_UNAVAILABLE" {
			t.Errorf("code = %q，期望 SIGNATURE_SECRET_UNAVAILABLE", got.Code)
		}
		if n := countEvents(); n != 1 {
			t.Errorf("无密钥时事件仍入库：事件数 = %d，期望 1", n)
		}
	})
}

// 7. 事件去重：同一 eventId 投两次 → 事件表里只有一条，审计也只有一行。
func TestEventDeduplication(t *testing.T) {
	secret := prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)
	body := eventBodyWithID("evt_01JBDEDUP001")
	signature := protocol.Sign(secret, time.Now().UTC().Unix(), body)

	for i := 0; i < 2; i++ {
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
			protocol.HeaderSignature: signature,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次投递状态码 = %d，期望 200（原文 %s）", i+1, resp.StatusCode, raw)
		}
		var got struct {
			Duplicate bool `json:"duplicate"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("投递响应不是合法 JSON：%v（原文 %s）", err, raw)
		}
		if want := i == 1; got.Duplicate != want {
			t.Errorf("第 %d 次投递 duplicate = %v，期望 %v", i+1, got.Duplicate, want)
		}
	}

	resp, raw := doJSON(t, http.MethodGet, srv.URL+"/tenon/events", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("补课状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	var page struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("补课响应不是合法 JSON：%v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("事件表里有 %d 条，期望 1 条（按 eventId 去重）", len(page.Events))
	}

	var eventLines int
	for _, rec := range readAuditLines(t) {
		if rec.Kind == "event" && rec.EventID == "evt_01JBDEDUP001" {
			eventLines++
		}
	}
	if eventLines != 1 {
		t.Errorf("审计里该事件有 %d 行，期望 1 行", eventLines)
	}

	// 另一个 eventId → 事件表与审计都要加一条。
	other := eventBodyWithID("evt_01JBDEDUP002")
	respOther, rawOther := do(t, http.MethodPost, srv.URL+"/tenon/events", other, map[string]string{
		protocol.HeaderSignature: protocol.Sign(secret, time.Now().UTC().Unix(), other),
	})
	if respOther.StatusCode != http.StatusOK {
		t.Fatalf("新事件状态码 = %d，期望 200（原文 %s）", respOther.StatusCode, rawOther)
	}
	_, rawAfter := doJSON(t, http.MethodGet, srv.URL+"/tenon/events", nil, nil)
	if err := json.Unmarshal(rawAfter, &page); err != nil {
		t.Fatalf("补课响应不是合法 JSON：%v", err)
	}
	if len(page.Events) != 2 {
		t.Errorf("事件表里有 %d 条，期望 2 条", len(page.Events))
	}
}

// 8. 业务拒绝 → 422，class=business_rejection、retryable=false，并落审计 error 行。
func TestBusinessRejectionMapping(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)

	resp, raw := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID,
		map[string]any{"name": rejectName}, idem("tenon:pln_01JB/s9"))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422（原文 %s）", resp.StatusCode, raw)
	}
	got := decodeTenonError(t, raw)
	if got.Class != protocol.ClassBusinessRejection {
		t.Errorf("class = %q，期望 %q", got.Class, protocol.ClassBusinessRejection)
	}
	if got.Code != "STOCK_INSUFFICIENT" {
		t.Errorf("code = %q，期望 STOCK_INSUFFICIENT", got.Code)
	}
	if got.Retryable {
		t.Error("business_rejection 必须 retryable=false（errors.md §3）")
	}
	if available, ok := got.Details["available"]; !ok || available != float64(3) {
		t.Errorf("details = %v，期望含 available=3", got.Details)
	}
	if n := f.executed.Load(); n != 0 {
		t.Errorf("业务拒绝前不应有成功副作用，计数器 = %d", n)
	}

	var found bool
	for _, rec := range readAuditLines(t) {
		if rec.Kind == "action" && rec.Action == writeActionID {
			found = true
			if rec.Result != "error" || rec.ErrorCode != "STOCK_INSUFFICIENT" {
				t.Errorf("审计行 = %+v，期望 result=error、errorCode=STOCK_INSUFFICIENT", rec)
			}
		}
	}
	if !found {
		t.Error("业务拒绝没有留下模块侧审计行")
	}
}

// 9. 未知 action：GET 与 POST 都是 404 + contract_violation/UNKNOWN_ACTION。
func TestUnknownAction(t *testing.T) {
	prepareEnv(t)
	srv := serve(t, newFixture().module)

	for _, tc := range []struct {
		method string
		path   string
		body   any
		extra  map[string]string
	}{
		{http.MethodGet, "/tenon/actions/demo.Nope", nil, nil},
		{http.MethodPost, "/tenon/actions/demo.Nope", map[string]any{"name": "x"}, idem("tenon:pln_01JB/s1")},
	} {
		resp, raw := doJSON(t, tc.method, srv.URL+tc.path, tc.body, tc.extra)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s 状态码 = %d，期望 404（原文 %s）", tc.method, tc.path, resp.StatusCode, raw)
		}
		got := decodeTenonError(t, raw)
		if got.Class != protocol.ClassContractViolation || got.Code != "UNKNOWN_ACTION" {
			t.Errorf("%s %s 的错误 = %+v，期望 contract_violation/UNKNOWN_ACTION", tc.method, tc.path, got)
		}
	}
}

// 10. 版本协商：执行端点强制要求 Tenon-Version；只读端点按 versioning.md §4.3 宽松。
func TestVersionHeaderEnforcement(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)
	url := srv.URL + "/tenon/actions/" + writeActionID

	t.Run("执行端点缺版本头即拒", func(t *testing.T) {
		resp, raw := doJSON(t, http.MethodPost, url, map[string]any{"name": "alpha"},
			map[string]string{protocol.HeaderVersion: "", protocol.HeaderIdempotencyKey: "tenon:pln_v/s1"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400（原文 %s）", resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "VERSION_HEADER_MISSING" {
			t.Errorf("code = %q，期望 VERSION_HEADER_MISSING", got.Code)
		}
		if n := f.executed.Load(); n != 0 {
			t.Errorf("版本不符仍执行了业务，计数器 = %d，期望 0", n)
		}
	})

	t.Run("major 不同即拒", func(t *testing.T) {
		resp, raw := doJSON(t, http.MethodPost, url, map[string]any{"name": "alpha"},
			map[string]string{protocol.HeaderVersion: "1.0", protocol.HeaderIdempotencyKey: "tenon:pln_v/s2"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400（原文 %s）", resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "PROTOCOL_VERSION_UNSUPPORTED" {
			t.Errorf("code = %q，期望 PROTOCOL_VERSION_UNSUPPORTED", got.Code)
		}
	})

	t.Run("对端最低版本过高即拒", func(t *testing.T) {
		resp, raw := doJSON(t, http.MethodPost, url, map[string]any{"name": "alpha"}, map[string]string{
			protocol.HeaderMinVersion:     "0.2",
			protocol.HeaderIdempotencyKey: "tenon:pln_v/s3",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400（原文 %s）", resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "PROTOCOL_MIN_VERSION_TOO_HIGH" {
			t.Errorf("code = %q，期望 PROTOCOL_MIN_VERSION_TOO_HIGH", got.Code)
		}
	})

	t.Run("只读端点缺版本头仍可用", func(t *testing.T) {
		resp, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil,
			map[string]string{protocol.HeaderVersion: ""})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（versioning.md §4.3：缺版本按 0.1，原文 %s）", resp.StatusCode, raw)
		}
	})
}

// 11. 运维端点：healthz 永远 200；readyz 按密钥与审计可写决定 200/503。
func TestHealthAndReady(t *testing.T) {
	prepareEnv(t)
	srv := serve(t, newFixture().module)

	resp, raw := doJSON(t, http.MethodGet, srv.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != "ok" {
		t.Fatalf("healthz = %d/%q，期望 200/ok", resp.StatusCode, raw)
	}

	// 密钥不可读 → 503 并说明缺什么。
	t.Setenv(EnvHMACSecretFile, filepath.Join(t.TempDir(), "absent.txt"))
	resp, raw = doJSON(t, http.MethodGet, srv.URL+"/readyz", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d，期望 503（原文 %s）", resp.StatusCode, raw)
	}
	var notReady struct {
		Ready   bool     `json:"ready"`
		Missing []string `json:"missing"`
	}
	if err := json.Unmarshal(raw, &notReady); err != nil {
		t.Fatalf("readyz 响应不是合法 JSON：%v（原文 %s）", err, raw)
	}
	if notReady.Ready || !contains(notReady.Missing, "hmac_secret") {
		t.Errorf("readyz 报文 = %+v，期望 ready=false 且 missing 含 hmac_secret", notReady)
	}

	// 依赖齐备 → 200。
	prepareEnv(t)
	resp, raw = doJSON(t, http.MethodGet, srv.URL+"/readyz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
}

// 12. 模块侧审计：写操作追加一行，字段齐备且 argsHash 是入参的 sha256。
func TestWriteAuditLine(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)
	payload := map[string]any{"name": "alpha"}
	requestBody, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编组请求体失败：%v", err)
	}

	resp, raw := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID, payload,
		idem("tenon:pln_audit/s1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}

	lines := readAuditLines(t)
	if len(lines) != 1 {
		t.Fatalf("审计行数 = %d，期望 1（读操作不写审计）", len(lines))
	}
	rec := lines[0]
	if rec.Module != fixtureModuleID {
		t.Errorf("module = %q，期望 %q", rec.Module, fixtureModuleID)
	}
	if rec.Action != writeActionID || rec.Kind != "action" {
		t.Errorf("action/kind = %q/%q，期望 %q/action", rec.Action, rec.Kind, writeActionID)
	}
	if rec.IdempotencyKey != "tenon:pln_audit/s1" {
		t.Errorf("idempotencyKey = %q", rec.IdempotencyKey)
	}
	if rec.Result != "ok" || rec.ErrorCode != "" {
		t.Errorf("result/errorCode = %q/%q，期望 ok/空", rec.Result, rec.ErrorCode)
	}
	sum := sha256.Sum256(requestBody)
	if want := "sha256:" + hex.EncodeToString(sum[:]); rec.ArgsHash != want {
		t.Errorf("argsHash = %q，期望 %q", rec.ArgsHash, want)
	}
	if _, err := time.Parse(time.RFC3339, rec.At); err != nil {
		t.Errorf("at = %q 不是 RFC3339：%v", rec.At, err)
	}
}

// 13. 补课/重放：游标自洽、分页不重不漏、非法游标与 limit 被拒。
func TestEventReplayPagination(t *testing.T) {
	secret := prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)

	ids := []string{"evt_01JBPAGE0001", "evt_01JBPAGE0002", "evt_01JBPAGE0003"}
	for _, id := range ids {
		body := eventBodyWithID(id)
		resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
			protocol.HeaderSignature: protocol.Sign(secret, time.Now().UTC().Unix(), body),
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("投递 %s 状态码 = %d，期望 200（原文 %s）", id, resp.StatusCode, raw)
		}
	}

	type page struct {
		Events     []json.RawMessage `json:"events"`
		NextCursor string            `json:"nextCursor"`
	}
	fetch := func(query string) page {
		t.Helper()
		resp, raw := doJSON(t, http.MethodGet, srv.URL+"/tenon/events"+query, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("补课 %s 状态码 = %d，期望 200（原文 %s）", query, resp.StatusCode, raw)
		}
		var got page
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("补课响应不是合法 JSON：%v", err)
		}
		return got
	}

	first := fetch("?limit=2")
	if len(first.Events) != 2 {
		t.Fatalf("第一页 %d 条，期望 2 条", len(first.Events))
	}
	if first.NextCursor == "" {
		t.Fatal("第一页缺少 nextCursor")
	}
	second := fetch("?since=" + first.NextCursor + "&limit=2")
	if len(second.Events) != 1 {
		t.Fatalf("第二页 %d 条，期望 1 条", len(second.Events))
	}
	if !bytes.Contains(second.Events[0], []byte(ids[2])) {
		t.Errorf("第二页内容 = %s，期望最后一条事件", second.Events[0])
	}
	third := fetch("?since=" + second.NextCursor + "&limit=2")
	if len(third.Events) != 0 {
		t.Errorf("翻到底后仍有 %d 条事件", len(third.Events))
	}
	if third.NextCursor != second.NextCursor {
		t.Errorf("空页游标 = %q，期望保持不变 %q", third.NextCursor, second.NextCursor)
	}
	// 同一个 since 重复取结果一致（天然幂等）。
	again := fetch("?since=" + first.NextCursor + "&limit=2")
	if len(again.Events) != len(second.Events) || again.NextCursor != second.NextCursor {
		t.Errorf("同一个游标两次结果不同：%+v vs %+v", again, second)
	}

	for _, bad := range []string{"?since=not-a-cursor", "?limit=0", "?limit=abc"} {
		resp, raw := doJSON(t, http.MethodGet, srv.URL+"/tenon/events"+bad, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400（原文 %s）", bad, resp.StatusCode, raw)
			continue
		}
		want := "INVALID_CURSOR"
		if strings.HasPrefix(bad, "?limit") {
			want = "INVALID_LIMIT"
		}
		if got := decodeTenonError(t, raw); got.Code != want {
			t.Errorf("%s 的 code = %q，期望 %q", bad, got.Code, want)
		}
	}
}

// 14. 启动自检：C1 的跨文档约束（keyFields ⊆ required）必须在启动时就失败。
func TestValidateRejectsInconsistentDefinitions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fixture)
		want   string
	}{
		{
			name: "keyFields 不在 required 里",
			mutate: func(f *fixture) {
				f.module.Actions[1].Idempotency = &Idempotency{
					KeyFields:         []string{"name", "absentField"},
					Scope:             "module",
					DuplicateBehavior: "return_first_result",
				}
			},
			want: "keyFields",
		},
		{
			name: "read 动作声明了 idempotency",
			mutate: func(f *fixture) {
				f.module.Actions[0].Idempotency = &Idempotency{
					KeyFields:         []string{"name"},
					Scope:             "module",
					DuplicateBehavior: "return_first_result",
				}
			},
			want: "idempotency",
		},
		{
			name:   "声明了预演却没有回调",
			mutate: func(f *fixture) { f.module.DryRun = nil },
			want:   "dryRun",
		},
		{
			name: "补偿动作不存在",
			mutate: func(f *fixture) {
				f.module.Actions[1].Compensation = &Compensation{
					Action: "demo.MissingAction",
					ArgMap: map[string]string{"name": "$in.name"},
				}
			},
			want: "补偿动作",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.mutate(f)
			err := f.module.Validate()
			if err == nil {
				t.Fatal("期望 Validate 失败，实际通过")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), tc.want)
			}
		})
	}

	// 反过来：健康定义必须通过自检，且 TimeoutMs=0 被显式化为协议兜底值。
	f := newFixture()
	f.module.Actions[1].TimeoutMs = 0
	if err := f.module.Validate(); err != nil {
		t.Fatalf("合法定义自检失败：%v", err)
	}
	if got := f.module.Actions[1].TimeoutMs; got != DefaultTimeoutMs {
		t.Errorf("timeoutMs = %d，期望兜底为 %d", got, DefaultTimeoutMs)
	}
}

// 16. TENON_MODULE_ID 覆盖 manifest.module.id 与审计的 module 字段（interfaces.md §6）。
func TestModuleIDOverride(t *testing.T) {
	prepareEnv(t)
	t.Setenv(EnvModuleID, "inventory-shadow")
	f := newFixture()
	srv := serve(t, f.module)

	_, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil, nil)
	var got struct {
		Module struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"module"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("manifest 不是合法 JSON：%v（原文 %s）", err, raw)
	}
	if got.Module.ID != "inventory-shadow" {
		t.Errorf("manifest.module.id = %q，期望被 TENON_MODULE_ID 覆盖", got.Module.ID)
	}
	if got.Module.Version != "0.1.0" {
		t.Errorf("manifest.module.version = %q，期望 0.1.0", got.Module.Version)
	}

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID,
		map[string]any{"name": "alpha"}, idem("tenon:pln_override/s1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, body)
	}
	lines := readAuditLines(t)
	if len(lines) != 1 {
		t.Fatalf("审计行数 = %d，期望 1", len(lines))
	}
	if lines[0].Module != "inventory-shadow" {
		t.Errorf("审计 module = %q，期望随 TENON_MODULE_ID 一起被覆盖", lines[0].Module)
	}
}

// 15. Content-Type：头存在却不是 JSON → 400；缺头按 JSON 处理。
func TestContentTypeEnforcement(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)
	url := srv.URL + "/tenon/actions/" + writeActionID

	resp, raw := doJSON(t, http.MethodPost, url, map[string]any{"name": "alpha"}, map[string]string{
		"Content-Type":                "text/plain",
		protocol.HeaderIdempotencyKey: "tenon:pln_ct/s1",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（原文 %s）", resp.StatusCode, raw)
	}
	if got := decodeTenonError(t, raw); got.Code != "UNSUPPORTED_CONTENT_TYPE" {
		t.Errorf("code = %q，期望 UNSUPPORTED_CONTENT_TYPE", got.Code)
	}
	if n := f.executed.Load(); n != 0 {
		t.Errorf("content-type 不符仍执行了业务，计数器 = %d，期望 0", n)
	}

	resp, raw = doJSON(t, http.MethodPost, url, map[string]any{"name": "alpha"}, map[string]string{
		"Content-Type":                "application/json; charset=utf-8",
		protocol.HeaderIdempotencyKey: "tenon:pln_ct/s2",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带 charset 的 application/json 状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
}

// eventsPage 是 GET /tenon/events 的响应形状（in 与 out 两个方向一致）。
type eventsPage struct {
	Events     []json.RawMessage `json:"events"`
	NextCursor string            `json:"nextCursor"`
}

// getEvents 取一页事件；非 200 即终止用例。
func getEvents(t *testing.T, url string) eventsPage {
	t.Helper()
	resp, raw := doJSON(t, http.MethodGet, url, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d，期望 200（原文 %s）", url, resp.StatusCode, raw)
	}
	var page eventsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("事件响应不是合法 JSON：%v（原文 %s）", err, raw)
	}
	return page
}

// 16. 发件箱方向：direction=out 读模块自产事件；缺省 in 仍是宿主投递进来的事件。
func TestOutboxDirection(t *testing.T) {
	secret := prepareEnv(t)
	f := newFixture()
	if seq := f.module.EmitEvent(Event{EventID: "evt_1", Type: "quote.completed"}); seq != 1 {
		t.Fatalf("首条发件箱事件序号 = %d，期望 1", seq)
	}
	srv := serve(t, f.module)

	// 同一模块也接受宿主投递，用来证明两张事件表互不串味。
	body := eventBodyWithID("evt_01JBDIR0001")
	resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, map[string]string{
		protocol.HeaderSignature: protocol.Sign(secret, time.Now().UTC().Unix(), body),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("宿主投递状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}

	out := getEvents(t, srv.URL+"/tenon/events?direction=out")
	if len(out.Events) != 1 {
		t.Fatalf("out 方向 %d 条，期望 1 条", len(out.Events))
	}
	if !bytes.Contains(out.Events[0], []byte(`"quote.completed"`)) {
		t.Fatalf("out 方向未返回模块自有事件：%s", out.Events[0])
	}
	if bytes.Contains(out.Events[0], []byte("evt_01JBDIR0001")) {
		t.Errorf("out 方向混入了宿主投递的事件：%s", out.Events[0])
	}
	// 信封的必填字段一个都不能少（domain-event.schema.json 的 required）。
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(out.Events[0], &envelope); err != nil {
		t.Fatalf("事件信封不是合法 JSON：%v", err)
	}
	for _, field := range []string{"eventId", "type", "version", "subject", "occurredAt", "causationId", "correlationId", "payload"} {
		if _, ok := envelope[field]; !ok {
			t.Errorf("事件信封缺少 %q 字段：%s", field, out.Events[0])
		}
	}

	in := getEvents(t, srv.URL+"/tenon/events") // 缺省方向
	if len(in.Events) != 1 {
		t.Fatalf("缺省方向 %d 条，期望 1 条宿主投递的事件", len(in.Events))
	}
	if !bytes.Contains(in.Events[0], []byte("evt_01JBDIR0001")) {
		t.Errorf("缺省方向返回的不是宿主投递的事件：%s", in.Events[0])
	}
	if bytes.Contains(in.Events[0], []byte(`"evt_1"`)) {
		t.Errorf("缺省方向应为 in，不应看到模块自有事件（eventId=evt_1）：%s", in.Events[0])
	}
	explicitIn := getEvents(t, srv.URL+"/tenon/events?direction=in")
	if len(explicitIn.Events) != 1 || explicitIn.NextCursor != in.NextCursor {
		t.Errorf("显式 direction=in 与缺省行为不一致：%+v vs %+v", explicitIn, in)
	}
}

// 17. 发件箱游标推进：先取 1 条，再取只拿到新增的那条；两套游标互不干扰。
func TestOutboxCursorAdvance(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	for _, id := range []string{"evt_out_0001", "evt_out_0002", "evt_out_0003"} {
		if seq := f.module.EmitEvent(Event{
			EventID: id,
			Type:    "quote.completed",
			Payload: json.RawMessage(`{"quoteNo":"` + id + `"}`),
		}); seq == 0 {
			t.Fatalf("EmitEvent(%s) 被拒绝", id)
		}
	}
	srv := serve(t, f.module)

	first := getEvents(t, srv.URL+"/tenon/events?direction=out&limit=1")
	if len(first.Events) != 1 || !bytes.Contains(first.Events[0], []byte("evt_out_0001")) {
		t.Fatalf("第一页 = %+v，期望只有 evt_out_0001", first)
	}
	second := getEvents(t, srv.URL+"/tenon/events?direction=out&since="+first.NextCursor+"&limit=1")
	if len(second.Events) != 1 || !bytes.Contains(second.Events[0], []byte("evt_out_0002")) {
		t.Fatalf("推进后的那一页 = %+v，期望只有 evt_out_0002（游标必须真的前进）", second)
	}
	if second.NextCursor == first.NextCursor {
		t.Errorf("两次翻页的 nextCursor 相同 = %q，游标没有前进", first.NextCursor)
	}
	// 同一个游标重复取结果一致（天然幂等）。
	again := getEvents(t, srv.URL+"/tenon/events?direction=out&since="+first.NextCursor+"&limit=1")
	if len(again.Events) != len(second.Events) || again.NextCursor != second.NextCursor {
		t.Errorf("同一游标两次结果不同：%+v vs %+v", again, second)
	}

	// 读 out 不会消耗 in 的位点，反之亦然。
	inPage := getEvents(t, srv.URL+"/tenon/events")
	if len(inPage.Events) != 0 {
		t.Errorf("in 方向 %d 条，期望 0（发件箱不串进收件箱）", len(inPage.Events))
	}
	inAgain := getEvents(t, srv.URL+"/tenon/events?since="+inPage.NextCursor)
	if inAgain.NextCursor != inPage.NextCursor {
		t.Errorf("读 out 之后 in 的游标变了：%q → %q", inPage.NextCursor, inAgain.NextCursor)
	}

	// 方向不符的游标必须被拒，而不是被当成同一张表的序号静默跳过一段事件。
	for _, bad := range []string{
		srv.URL + "/tenon/events?direction=out&since=" + inPage.NextCursor,
		srv.URL + "/tenon/events?direction=in&since=" + first.NextCursor,
		srv.URL + "/tenon/events?direction=out&since=" + cursorPrefix + "MA", // 0.1 的无标签游标只属于 in
	} {
		resp, raw := doJSON(t, http.MethodGet, bad, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400（原文 %s）", bad, resp.StatusCode, raw)
			continue
		}
		if got := decodeTenonError(t, raw); got.Code != "INVALID_CURSOR" {
			t.Errorf("%s 的 code = %q，期望 INVALID_CURSOR", bad, got.Code)
		}
	}
	// 0.1 的无标签游标在 in 方向仍然可用（向后兼容）。
	if page := getEvents(t, srv.URL+"/tenon/events?since="+cursorPrefix+"MA"); len(page.Events) != 0 {
		t.Errorf("以 0.1 游标读 in 方向 = %+v，期望空页", page)
	}
}

// 18. 同一 eventId 重复 EmitEvent 的语义：幂等去重；同 id 不同信封则拒。
func TestEmitEventDeduplicatesByEventID(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	srv := serve(t, f.module)

	firstEvent := Event{EventID: "evt_dup_0001", Type: "quote.completed", Payload: json.RawMessage(`{"n":1}`)}
	firstSeq := f.module.EmitEvent(firstEvent)
	if firstSeq != 1 {
		t.Fatalf("首条事件序号 = %d，期望 1", firstSeq)
	}
	// 同 eventId 且同内容：不追加，返回首次序号。跨秒重复也必须幂等——框架补的
	// occurredAt 精确到秒，若按信封字节比就会被误判成「内容不同」。
	time.Sleep(1100 * time.Millisecond)
	if seq := f.module.EmitEvent(firstEvent); seq != firstSeq {
		t.Fatalf("重复入箱的返回值 = %d，期望首次序号 %d", seq, firstSeq)
	}
	// 同 eventId 但信封不同：拒（同一个 eventId 不可能对应两个事实），首个信封保留。
	conflict := firstEvent
	conflict.Payload = json.RawMessage(`{"n":2}`)
	if seq := f.module.EmitEvent(conflict); seq != 0 {
		t.Fatalf("eventId 冲突但信封不同，返回值 = %d，期望 0（拒绝）", seq)
	}
	// 不可入箱的事件一个都不许进箱。
	for _, bad := range []Event{
		{}, // 缺 eventId
		{EventID: "evt_bad_0001", Type: "NotAnEventType"},
		{EventID: "evt_bad_0002", Type: "quote.completed", Payload: json.RawMessage(`[1,2]`)},
		{EventID: "evt_bad_0003", Type: "quote.completed", Subject: json.RawMessage(`{`)},
	} {
		if seq := f.module.EmitEvent(bad); seq != 0 {
			t.Errorf("EmitEvent(%+v) = %d，期望 0（不可入箱）", bad, seq)
		}
	}
	// 被拒与重复都不消耗序号：下一条合法事件接着 2；调用方给的字段原样保留。
	if seq := f.module.EmitEvent(Event{
		EventID:       "evt_dup_0002",
		Type:          "quote.completed",
		Version:       "1.2.3",
		Subject:       json.RawMessage(`"quote:Quote/Q-2026-0417"`),
		CorrelationID: "cor:quote/Q-2026-0417",
	}); seq != 2 {
		t.Fatalf("第二条合法事件序号 = %d，期望 2", seq)
	}

	page := getEvents(t, srv.URL+"/tenon/events?direction=out")
	if len(page.Events) != 2 {
		t.Fatalf("发件箱 %d 条，期望 2 条", len(page.Events))
	}
	if !bytes.Contains(page.Events[0], []byte(`"n":1`)) || bytes.Contains(page.Events[0], []byte(`"n":2`)) {
		t.Errorf("首个信封被后来的同 id 事件覆盖了：%s", page.Events[0])
	}
	if !bytes.Contains(page.Events[0], []byte("evt_dup_0001")) || !bytes.Contains(page.Events[1], []byte("evt_dup_0002")) {
		t.Errorf("发件箱顺序 = %s | %s，期望按入箱顺序", page.Events[0], page.Events[1])
	}
	for _, want := range []string{`"version":"1.2.3"`, `"subject":"quote:Quote/Q-2026-0417"`, `"correlationId":"cor:quote/Q-2026-0417"`} {
		if !bytes.Contains(page.Events[1], []byte(want)) {
			t.Errorf("调用方给的字段没原样出现在信封里：缺 %s（原文 %s）", want, page.Events[1])
		}
	}
}

// 19. 非法 direction → 400 + tenonError；合法方向的其余参数仍按各自错误码报。
func TestInvalidDirection(t *testing.T) {
	prepareEnv(t)
	srv := serve(t, newFixture().module)

	for _, query := range []string{"?direction=sideways", "?direction=OUT", "?direction=out%20in"} {
		resp, raw := doJSON(t, http.MethodGet, srv.URL+"/tenon/events"+query, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400（原文 %s）", query, resp.StatusCode, raw)
			continue
		}
		got := decodeTenonError(t, raw)
		if got.Class != protocol.ClassContractViolation || got.Code != "INVALID_DIRECTION" {
			t.Errorf("%s 的错误 = %+v，期望 contract_violation/INVALID_DIRECTION", query, got)
		}
		if got.Retryable {
			t.Errorf("%s 的 retryable = true，契约违反不应可重试", query)
		}
	}

	for _, tc := range []struct {
		query string
		code  string
	}{
		{"?direction=out&limit=0", "INVALID_LIMIT"},
		{"?direction=in&since=not-a-cursor", "INVALID_CURSOR"},
	} {
		resp, raw := doJSON(t, http.MethodGet, srv.URL+"/tenon/events"+tc.query, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400（原文 %s）", tc.query, resp.StatusCode, raw)
			continue
		}
		if got := decodeTenonError(t, raw); got.Code != tc.code {
			t.Errorf("%s 的 code = %q，期望 %q", tc.query, got.Code, tc.code)
		}
	}
}

// 20. EmitEvent 在 Handle 回调内部调用不得自死锁：写动作执行期间 mu 已被持有。
func TestEmitEventFromHandleDoesNotDeadlock(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	var m *Module
	m = &Module{
		ID:      fixtureModuleID,
		Title:   "回调内发事件",
		Version: "0.1.0",
		Actions: append([]Action(nil), f.module.Actions...),
		Events:  []string{"demo.created"},
		DryRun:  f.module.DryRun,
		Handle: func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
			if seq := m.EmitEvent(Event{
				EventID: "evt_from_handle",
				Type:    "demo.created",
				Payload: json.RawMessage(`{"ok":true}`),
			}); seq == 0 {
				return nil, testError(protocol.ClassContractViolation, "EMIT_REJECTED", "发件箱拒绝了事件")
			}
			return json.RawMessage(`{"seq":1}`), nil
		},
	}
	srv := serve(t, m)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/tenon/actions/"+writeActionID,
		strings.NewReader(`{"name":"alpha"}`))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	req.Header.Set(protocol.HeaderIdempotencyKey, "tenon:pln_outbox/s1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("写动作在回调内发事件时请求失败（超时即说明自死锁）：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	page := getEvents(t, srv.URL+"/tenon/events?direction=out")
	if len(page.Events) != 1 || !bytes.Contains(page.Events[0], []byte("evt_from_handle")) {
		t.Fatalf("回调内发出的事件没进发件箱：%+v", page)
	}
}

// 21. 并发 EmitEvent：序号不重号、不丢事件（-race 在本机不可用，这里锁语义等价性）。
func TestEmitEventConcurrent(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	const workers, perWorker = 8, 25
	total := workers * perWorker

	seqs := make(chan int64, total)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				seqs <- f.module.EmitEvent(Event{
					EventID: fmt.Sprintf("evt_conc_%02d_%02d", w, i),
					Type:    "quote.completed",
				})
			}
		}(w)
	}
	wg.Wait()
	close(seqs)

	seen := make(map[int64]struct{}, total)
	for seq := range seqs {
		if seq <= 0 {
			t.Fatalf("并发入箱被拒绝：seq = %d", seq)
		}
		if _, dup := seen[seq]; dup {
			t.Fatalf("序号 %d 被分配了两次", seq)
		}
		seen[seq] = struct{}{}
	}
	if len(seen) != total {
		t.Fatalf("分配了 %d 个序号，期望 %d 个", len(seen), total)
	}

	srv := serve(t, f.module)
	page := getEvents(t, srv.URL+"/tenon/events?direction=out&limit=500")
	if len(page.Events) != total {
		t.Fatalf("发件箱 %d 条，期望 %d 条", len(page.Events), total)
	}
	ids := make(map[string]struct{}, total)
	for _, ev := range page.Events {
		var probe struct {
			EventID string `json:"eventId"`
		}
		if err := json.Unmarshal(ev, &probe); err != nil {
			t.Fatalf("事件信封不是合法 JSON：%v（原文 %s）", err, ev)
		}
		ids[probe.EventID] = struct{}{}
	}
	if len(ids) != total {
		t.Fatalf("发件箱只有 %d 个不同的 eventId，期望 %d 个", len(ids), total)
	}
}

// 22. 模块重启（状态全在内存里，序号从 1 重新开始）后，旧的 out 游标不许饿死消费方。
func TestOutboxCursorAfterRestart(t *testing.T) {
	prepareEnv(t)
	old := newFixture()
	for _, id := range []string{"evt_out_0001", "evt_out_0002", "evt_out_0003"} {
		old.module.EmitEvent(Event{EventID: id, Type: "quote.completed"})
	}
	oldSrv := serve(t, old.module)
	stale := getEvents(t, oldSrv.URL+"/tenon/events?direction=out&limit=2").NextCursor
	if stale == "" {
		t.Fatal("旧实例没有给出游标")
	}

	// 重启后的同一模块：事件重新从 1 开始编号。
	fresh := newFixture()
	fresh.module.EmitEvent(Event{EventID: "evt_out_0004", Type: "quote.completed"})
	freshSrv := serve(t, fresh.module)

	page := getEvents(t, freshSrv.URL+"/tenon/events?direction=out&since="+stale)
	if len(page.Events) != 1 || !bytes.Contains(page.Events[0], []byte("evt_out_0004")) {
		t.Fatalf("用重启前的游标读 = %+v，期望整段重放而不是永远空页", page)
	}
}

// resetModule 是测试态重置的夹具：业务状态是一个计数器，Reset 把它归零，
// 用来证明 POST /tenon/admin/reset 既清了框架缓存、也调了业务回调。
type resetModule struct {
	module *Module
	calls  atomic.Int32
}

func newResetModule() *resetModule {
	base := newFixture() // 复用同一份动作清单（read + 带幂等声明的 write）
	r := &resetModule{}
	r.module = &Module{
		ID:       fixtureModuleID,
		Title:    "可重置的演示模块",
		Version:  "0.1.0",
		Actions:  append([]Action(nil), base.module.Actions...),
		Events:   []string{"demo.created"},
		DryRun:   base.module.DryRun,
		TestMode: true,
		Handle: func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
			if actionID == readActionID {
				return json.RawMessage(`{"onHand":7}`), nil
			}
			if actionID != writeActionID {
				return nil, testError(protocol.ClassContractViolation, "UNKNOWN_ACTION", "测试模块不提供 "+actionID)
			}
			return json.RawMessage(fmt.Sprintf(`{"seq":%d}`, r.calls.Add(1))), nil
		},
		Reset: func() { r.calls.Store(0) },
	}
	return r
}

// postReset 调一次测试态重置端点并断言 200，返回响应体。
func postReset(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	resp, raw := doJSON(t, http.MethodPost, srv.URL+adminResetPath, map[string]any{}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s 状态码 = %d，期望 200（原文 %s）", adminResetPath, resp.StatusCode, raw)
	}
	return raw
}

// outboxInvariant 校验发件箱与它的去重索引自洽：序号连续（无重号、无断号）、
// 索引表与事件表逐项对应、序号计数与条数一致。任何一个不成立都意味着 reset 与
// EmitEvent 交错时留下了半清状态。
func outboxInvariant(m *Module) error {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	if int64(len(m.outbox)) != m.outSeq {
		return fmt.Errorf("outSeq = %d，发件箱 %d 条", m.outSeq, len(m.outbox))
	}
	if len(m.outboxIdx) != len(m.outbox) {
		return fmt.Errorf("outboxIdx %d 项，发件箱 %d 条", len(m.outboxIdx), len(m.outbox))
	}
	for i, ev := range m.outbox {
		if ev.seq != int64(i+1) {
			return fmt.Errorf("发件箱第 %d 条序号 = %d，期望 %d（重号或断号）", i, ev.seq, i+1)
		}
		if idx, ok := m.outboxIdx[ev.eventID]; !ok || idx != i {
			return fmt.Errorf("outboxIdx[%s] = %d（存在=%v），期望 %d", ev.eventID, idx, ok, i)
		}
	}
	return nil
}

// outboxLen 读发件箱当前条数（持 outMu，可与 EmitEvent/reset 并发调用）。
func outboxLen(m *Module) int {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	return len(m.outbox)
}

// 23. 测试态重置（计划 Task 8 第一例）：业务种子回滚 + 幂等表清空。
func TestResetRestoresSeedAndClearsCaches(t *testing.T) {
	prepareEnv(t)
	r := newResetModule()
	srv := serve(t, r.module)
	actionURL := srv.URL + "/tenon/actions/" + writeActionID

	for i := 0; i < 2; i++ {
		resp, raw := doJSON(t, http.MethodPost, actionURL, map[string]any{"name": "alpha"},
			idem(fmt.Sprintf("tenon:pln_reset/s%d", i)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次写动作状态码 = %d，期望 200（原文 %s）", i+1, resp.StatusCode, raw)
		}
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("写动作执行了 %d 次，期望 2", got)
	}

	// 非 POST 不是「清一下」的入口：405 + Allow，而不是悄悄接受或 404。
	resp, raw := doJSON(t, http.MethodGet, srv.URL+adminResetPath, nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET reset 状态码 = %d，期望 405（原文 %s）", resp.StatusCode, raw)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		t.Errorf("GET reset 的 Allow = %q，期望 POST", allow)
	}

	if body := postReset(t, srv); string(body) != `{"reset":true}` {
		t.Errorf("reset 响应体 = %s，期望 {\"reset\":true}", body)
	}
	if got := r.calls.Load(); got != 0 {
		t.Fatalf("reset 后业务计数 = %d，期望 0（Reset 回调没被调用）", got)
	}

	// 幂等表被清空：同一个键换参数也应当重新执行，而不是撞 IDEMPOTENCY_KEY_REUSED，
	// 更不是把 reset 前留存的首次结果回放出来。
	resp, raw = doJSON(t, http.MethodPost, actionURL, map[string]any{"name": "beta"},
		idem("tenon:pln_reset/s0"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset 后复用幂等键状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	if got := r.calls.Load(); got != 1 {
		t.Fatalf("reset 后写动作计数 = %d，期望 1（幂等表没被清空，命中了旧记录）", got)
	}
}

// 24. 测试态重置（计划 Task 8 第二例）：TestMode=false 时该路径就是一条不存在的
// 路径——404，不是 405 也不是 403，且一个字节的状态都不许被清掉。
func TestResetAbsentWhenTestModeOff(t *testing.T) {
	prepareEnv(t)
	f := newFixture() // TestMode 缺省 false
	if seq := f.module.EmitEvent(Event{EventID: "evt_keep_0001", Type: "quote.completed"}); seq != 1 {
		t.Fatalf("预置事件入箱失败：seq = %d", seq)
	}
	srv := serve(t, f.module)

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut} {
		resp, raw := doJSON(t, method, srv.URL+adminResetPath, map[string]any{}, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s reset 状态码 = %d，期望 404（原文 %s）", method, resp.StatusCode, raw)
		}
		if got := decodeTenonError(t, raw); got.Code != "UNKNOWN_ENDPOINT" {
			t.Errorf("%s reset 的错误码 = %q，期望 UNKNOWN_ENDPOINT", method, got.Code)
		}
	}
	if page := getEvents(t, srv.URL+"/tenon/events?direction=out"); len(page.Events) != 1 {
		t.Fatalf("非测试态下 reset 改动了发件箱：%+v", page)
	}
}

// 25. reset 清发件箱：事件、outSeq、去重索引与 out 游标一起归零，新事件从 1 重新
// 编号；reset 前的旧游标不许饿死消费方（与进程重启同一套语义）。
func TestResetClearsOutboxAndSeq(t *testing.T) {
	prepareEnv(t)
	r := newResetModule()
	srv := serve(t, r.module)
	for _, id := range []string{"evt_before_1", "evt_before_2"} {
		if seq := r.module.EmitEvent(Event{EventID: id, Type: "quote.completed"}); seq == 0 {
			t.Fatalf("EmitEvent(%s) 被拒绝", id)
		}
	}
	old := getEvents(t, srv.URL+"/tenon/events?direction=out&limit=2")
	if len(old.Events) != 2 {
		t.Fatalf("reset 前发件箱 %d 条，期望 2 条", len(old.Events))
	}

	postReset(t, srv)

	after := getEvents(t, srv.URL+"/tenon/events?direction=out")
	if len(after.Events) != 0 {
		t.Fatalf("reset 后发件箱 %d 条，期望 0 条", len(after.Events))
	}
	if want := encodeCursor(directionOut, 0); after.NextCursor != want {
		t.Errorf("reset 后 out 游标 = %q，期望归零 %q", after.NextCursor, want)
	}
	// outSeq 归零：下一条事件的序号必须从 1 重新开始。
	if seq := r.module.EmitEvent(Event{EventID: "evt_after_1", Type: "quote.completed"}); seq != 1 {
		t.Fatalf("reset 后首条事件序号 = %d，期望 1", seq)
	}
	// reset 前发出的旧游标指向序号 2，而新事件是序号 1：超过当前最新序号的游标
	// 必须整段重放，而不是静默空页。
	page := getEvents(t, srv.URL+"/tenon/events?direction=out&since="+old.NextCursor)
	if len(page.Events) != 1 || !bytes.Contains(page.Events[0], []byte("evt_after_1")) {
		t.Fatalf("用 reset 前的游标读 = %+v，期望拿到 reset 后的新事件", page)
	}
	// 去重索引也归零：reset 前的指纹不再留存，同一 eventId 可以重新入箱。
	if seq := r.module.EmitEvent(Event{EventID: "evt_before_1", Type: "quote.completed"}); seq != 2 {
		t.Fatalf("reset 后复用旧 eventId 的序号 = %d，期望 2（去重索引没清）", seq)
	}
}

// 26. reset 清收件箱：宿主投递的事件、eventId 去重表与 in 游标一起归零；
// Reset 回调为 nil 时框架状态照清。
func TestResetClearsInbox(t *testing.T) {
	secret := prepareEnv(t)
	r := newResetModule()
	r.module.Reset = nil // 业务回调可有可无，框架状态不依赖它
	srv := serve(t, r.module)

	body := eventBodyWithID("evt_01JBRESETIN1")
	sign := func() map[string]string {
		return map[string]string{protocol.HeaderSignature: protocol.Sign(secret, time.Now().UTC().Unix(), body)}
	}
	if resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, sign()); resp.StatusCode != http.StatusOK {
		t.Fatalf("宿主投递状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	// 顺带放一条 out 事件：两张表必须一起清。
	r.module.EmitEvent(Event{EventID: "evt_in_side_out", Type: "quote.completed"})

	if in := getEvents(t, srv.URL+"/tenon/events"); len(in.Events) != 1 {
		t.Fatalf("reset 前收件箱 %d 条，期望 1 条", len(in.Events))
	}

	postReset(t, srv)

	in := getEvents(t, srv.URL+"/tenon/events")
	if len(in.Events) != 0 {
		t.Fatalf("reset 后收件箱 %d 条，期望 0 条", len(in.Events))
	}
	if want := encodeCursor(directionIn, 0); in.NextCursor != want {
		t.Errorf("reset 后 in 游标 = %q，期望归零 %q", in.NextCursor, want)
	}
	if out := getEvents(t, srv.URL+"/tenon/events?direction=out"); len(out.Events) != 0 {
		t.Fatalf("reset 后发件箱 %d 条，期望 0 条", len(out.Events))
	}
	// eventId 去重表被清空：同一个事件可以重新投递（duplicate=false）。
	resp, raw := do(t, http.MethodPost, srv.URL+"/tenon/events", body, sign())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset 后重新投递状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}
	var ack struct {
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatalf("投递回执不是合法 JSON：%v（原文 %s）", err, raw)
	}
	if ack.Duplicate {
		t.Errorf("reset 后同一 eventId 仍被判为重复：去重表没清")
	}
}

// 27. reset 与并发 EmitEvent 交错（8×25）：不 panic、不发重号、不留半清状态。
func TestResetUnderConcurrentEmitEvent(t *testing.T) {
	prepareEnv(t)
	r := newResetModule()
	m := r.module
	srv := serve(t, m)
	const workers, perWorker = 8, 25
	total := workers * perWorker

	seqs := make(chan int64, total)
	var emit sync.WaitGroup
	for w := 0; w < workers; w++ {
		emit.Add(1)
		go func(w int) {
			defer emit.Done()
			for i := 0; i < perWorker; i++ {
				// 稍作停顿，保证重置循环与「进行中的入箱」真的重叠：200 次入箱本来
				// 一个 HTTP 往返就跑完了，那样交错无从谈起，用例只会自我安慰。
				time.Sleep(time.Millisecond)
				seqs <- m.EmitEvent(Event{
					EventID: fmt.Sprintf("evt_reset_%02d_%02d", w, i),
					Type:    "quote.completed",
				})
			}
		}(w)
	}

	// 重置循环走真实端点，与入箱并发；只要入箱还在进行就继续重置，保证这两件事
	// 真的重叠而不是先后跑完。goroutine 里不能用 t.Fatalf（它只终止当前 goroutine），
	// 错误经 channel 回到主线断言。
	var resets, wiped atomic.Int32
	resetErr := make(chan string, 1)
	stop := make(chan struct{})
	var resetWG sync.WaitGroup
	resetWG.Add(1)
	go func() {
		defer resetWG.Done()
		defer close(resetErr)
		client := &http.Client{Timeout: 5 * time.Second}
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 观测：这次 reset 会抹掉多少条活事件（用于事后证明两者确实交错了）。
			if outboxLen(m) > 0 {
				wiped.Add(1)
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+adminResetPath, strings.NewReader("{}"))
			if err != nil {
				resetErr <- "构造 reset 请求失败：" + err.Error()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				resetErr <- "reset 请求失败：" + err.Error()
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				resetErr <- fmt.Sprintf("reset 状态码 = %d", resp.StatusCode)
				return
			}
			resets.Add(1)
			// 任何时刻的可观测状态都必须是自洽的：清表只发生在 outMu 的同一个
			// 临界区里，所以「事件表已清、索引表还在」这种半清状态不可达。
			if err := outboxInvariant(m); err != nil {
				resetErr <- "reset 与 EmitEvent 交错后" + err.Error()
				return
			}
		}
	}()

	emit.Wait()
	close(stop)
	close(seqs)
	resetWG.Wait()
	for msg := range resetErr {
		t.Fatal(msg)
	}
	t.Logf("并发窗口内完成 %d 次 reset，其中 %d 次抹掉了尚未被读取的活事件", resets.Load(), wiped.Load())
	if resets.Load() < 2 {
		t.Fatalf("并发窗口内只完成 %d 次 reset，重置与入箱没有重叠（用例本身失效）", resets.Load())
	}
	if wiped.Load() == 0 {
		t.Fatalf("没有一次 reset 抹掉活事件，重置与入箱没有真正交错（用例本身失效）")
	}

	seen := 0
	for seq := range seqs {
		if seq <= 0 {
			t.Fatalf("并发入箱被拒绝：seq = %d", seq)
		}
		seen++
	}
	if seen != total {
		t.Fatalf("收集到 %d 个序号，期望 %d 个", seen, total)
	}
	if err := outboxInvariant(m); err != nil {
		t.Fatalf("并发结束后%s", err)
	}

	// 收尾：再 reset 一次，状态必须干净且从 1 重新编号。
	postReset(t, srv)
	if err := outboxInvariant(m); err != nil {
		t.Fatalf("reset 之后%s", err)
	}
	if page := getEvents(t, srv.URL+"/tenon/events?direction=out"); len(page.Events) != 0 {
		t.Fatalf("reset 后发件箱 %d 条，期望 0 条", len(page.Events))
	}
	if seq := m.EmitEvent(Event{EventID: "evt_reset_final", Type: "quote.completed"}); seq != 1 {
		t.Fatalf("reset 后首条事件序号 = %d，期望 1", seq)
	}
}

// 28. Reset 回调在 m.mu 下、outMu 已释放时执行：回调里发事件必须立刻成功，
// 而不是在不可重入的 outMu 上自死锁；事件落在清空之后的新编号上（序号 1）。
func TestResetCallbackMayEmitEvent(t *testing.T) {
	prepareEnv(t)
	r := newResetModule()
	m := r.module
	seqCh := make(chan int64, 1)
	var once sync.Once
	m.Reset = func() {
		// once 只是防重入：这个用例只 reset 一次，但回调被调用两次也不该 panic。
		once.Do(func() {
			seqCh <- m.EmitEvent(Event{EventID: "evt_during_reset", Type: "quote.completed"})
		})
	}
	srv := serve(t, m)
	// reset 前先塞一条事件：回调里发的那条必须拿到清空后的序号 1，而不是 2。
	if seq := m.EmitEvent(Event{EventID: "evt_before_callback", Type: "quote.completed"}); seq != 1 {
		t.Fatalf("reset 前的事件序号 = %d，期望 1", seq)
	}

	// 用带超时的客户端调 reset：回调自死锁时这里必须超时失败，而不是把用例挂死。
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, srv.URL+adminResetPath, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("构造 reset 请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("reset 请求失败（超时即说明 Reset 回调被 outMu 卡住）：%v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取 reset 响应失败：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset 状态码 = %d，期望 200（原文 %s）", resp.StatusCode, raw)
	}

	select {
	case seq := <-seqCh:
		if seq != 1 {
			t.Fatalf("Reset 回调里的事件序号 = %d，期望 1（应落在清空后的新表里）", seq)
		}
	case <-time.After(time.Second):
		t.Fatal("Reset 回调没有执行：reset 端点没调用业务回调")
	}
	page := getEvents(t, srv.URL+"/tenon/events?direction=out")
	if len(page.Events) != 1 || !bytes.Contains(page.Events[0], []byte("evt_during_reset")) {
		t.Fatalf("发件箱 = %+v，期望只有回调里发出的 evt_during_reset", page)
	}
}

// 模板是模块声明的一部分：必须出现在摘要线里（agent 只读这条线），
// 空模板不出现（omitempty）——老模块的 manifest 不该多一个空字段。
func TestManifestCarriesSummaryTemplate(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	f.module.Actions[1].SummaryTemplate = "写一个 {name}"
	srv := serve(t, f.module)

	_, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil, nil)
	if !bytes.Contains(raw, []byte(`"summaryTemplate":"写一个 {name}"`)) {
		t.Fatalf("摘要线没带模板：%s", raw)
	}

	empty := newFixture()
	srv2 := serve(t, empty.module)
	_, raw2 := doJSON(t, http.MethodGet, srv2.URL+"/.well-known/tenon", nil, nil)
	if bytes.Contains(raw2, []byte("summaryTemplate")) {
		t.Fatalf("空模板不该出现：%s", raw2)
	}
}

// 必填字段是模块声明的一部分：摘要线必须带出，空则不出现。
func TestManifestCarriesRequiredArgs(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	f.module.Actions[1].RequiredArgs = []string{"name"}
	srv := serve(t, f.module)

	_, raw := doJSON(t, http.MethodGet, srv.URL+"/.well-known/tenon", nil, nil)
	if !bytes.Contains(raw, []byte(`"requiredArgs":["name"]`)) {
		t.Fatalf("摘要线没带 requiredArgs：%s", raw)
	}

	empty := newFixture()
	srv2 := serve(t, empty.module)
	_, raw2 := doJSON(t, http.MethodGet, srv2.URL+"/.well-known/tenon", nil, nil)
	if bytes.Contains(raw2, []byte("requiredArgs")) {
		t.Fatalf("空声明不该出现：%s", raw2)
	}
}

// 声明与契约不一致是装配错误：requiredArgs 必须在 inputSchema.required 里。
func TestValidateRejectsRequiredArgsOutsideSchema(t *testing.T) {
	prepareEnv(t)
	f := newFixture()
	f.module.Actions[1].RequiredArgs = []string{"nope"}
	err := f.module.Validate()
	if err == nil {
		t.Fatal("requiredArgs 不在 inputSchema.required 里时 Validate 必须失败")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("错误里要点名字段，得到：%v", err)
	}
}
