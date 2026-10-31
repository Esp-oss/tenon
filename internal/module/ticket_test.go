package module

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

// ticketEnv 是一个 enforce 模式的模块：所有写请求都必须带合法票据。
type ticketEnv struct {
	srv      *httptest.Server
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	executed *atomic.Int32
}

func newTicketEnv(t *testing.T) *ticketEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥：%v", err)
	}
	executed := &atomic.Int32{}
	m := &Module{
		ID:         fixtureModuleID,
		Title:      "票据演示模块",
		Version:    "0.1.0",
		TicketMode: TicketModeEnforce,
		TrustedKeys: protocol.KeySet{
			protocol.KeyID(pub): pub,
		},
		Actions: []Action{
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
				OutputSchema: json.RawMessage(`{"type":"object","required":["seq"]}`),
				Risk:         Risk{Level: "medium", Reversible: true},
				TimeoutMs:    30000,
				Idempotency:  &Idempotency{KeyFields: []string{"name"}, Scope: "module", DuplicateBehavior: "return_first_result"},
			},
		},
		Handle: func(ctx context.Context, actionID string, args json.RawMessage) (json.RawMessage, *protocol.Error) {
			executed.Add(1)
			return json.RawMessage(`{"seq":1}`), nil
		},
	}
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return &ticketEnv{srv: srv, priv: priv, pub: pub, executed: executed}
}

func (e *ticketEnv) ticket(t *testing.T, module, action string, body []byte, idem string, ttl time.Duration) string {
	t.Helper()
	now := time.Now().UTC()
	tk := protocol.Ticket{
		TicketID:       "tkt_" + action + "_" + idem + "_" + now.Format("150405.000000000"),
		Module:         module,
		Action:         action,
		ArgsHash:       protocol.ArgsHash(body),
		IdempotencyKey: idem,
		PlanID:         "pln_t",
		StepID:         "s1",
		AuditID:        "aud_t",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(ttl).Unix(),
	}
	token, err := protocol.SignTicket(e.priv, tk)
	if err != nil {
		t.Fatalf("签票：%v", err)
	}
	return token
}

func (e *ticketEnv) post(t *testing.T, body, idem, ticket string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/tenon/actions/"+writeActionID, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	if idem != "" {
		req.Header.Set(protocol.HeaderIdempotencyKey, idem)
	}
	if ticket != "" {
		req.Header.Set(protocol.HeaderTicket, ticket)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

const ticketBody = `{"name":"alpha"}`

// 1. 无票：403，且业务侧一次都没被调用——看 executed 而不是看响应码。
func TestTicketMissingRejectsWrite(t *testing.T) {
	e := newTicketEnv(t)
	status, raw := e.post(t, ticketBody, "tenon:pln_t/s1", "")
	if status != http.StatusForbidden || !strings.Contains(string(raw), protocol.CodeTicketMissing) {
		t.Fatalf("无票应 403 TICKET_MISSING，得到 %d：%s", status, raw)
	}
	if got := e.executed.Load(); got != 0 {
		t.Fatalf("被拒的请求仍然执行了业务逻辑 %d 次", got)
	}
}

// 2. 改参：票是给 name=alpha 签的，发 name=beta。
func TestTicketArgsMismatchRejectsWrite(t *testing.T) {
	e := newTicketEnv(t)
	ticket := e.ticket(t, fixtureModuleID, writeActionID, []byte(ticketBody), "tenon:pln_t/s1", 30*time.Second)
	status, raw := e.post(t, `{"name":"beta"}`, "tenon:pln_t/s1", ticket)
	if status != http.StatusForbidden || !strings.Contains(string(raw), protocol.CodeTicketArgsMismatch) {
		t.Fatalf("改参应 403 TICKET_ARGS_MISMATCH，得到 %d：%s", status, raw)
	}
	if got := e.executed.Load(); got != 0 {
		t.Fatalf("改参请求仍执行了业务逻辑 %d 次", got)
	}
}

// 3. 换幂等键重放同一张票：只成功一次。
func TestTicketReplayWithNewIdempotencyKeyRejected(t *testing.T) {
	e := newTicketEnv(t)
	ticket := e.ticket(t, fixtureModuleID, writeActionID, []byte(ticketBody), "tenon:pln_t/s1", 30*time.Second)
	if status, raw := e.post(t, ticketBody, "tenon:pln_t/s1", ticket); status != http.StatusOK {
		t.Fatalf("首次应成功，得到 %d：%s", status, raw)
	}
	status, raw := e.post(t, ticketBody, "tenon:pln_t/s1-again", ticket)
	if status != http.StatusForbidden || !strings.Contains(string(raw), protocol.CodeTicketReused) {
		t.Fatalf("换键重放应 403 TICKET_REUSED，得到 %d：%s", status, raw)
	}
	if got := e.executed.Load(); got != 1 {
		t.Fatalf("业务只应执行 1 次，实际 %d 次", got)
	}
}

// 4. 过期票：403，业务不执行。
func TestTicketExpiredRejected(t *testing.T) {
	e := newTicketEnv(t)
	// 签一张 1 秒有效的票，等它过期；签票端不允许 exp<=iat，所以只能等。
	ticket := e.ticket(t, fixtureModuleID, writeActionID, []byte(ticketBody), "tenon:pln_t/s1", time.Second)
	time.Sleep(2100 * time.Millisecond)
	status, raw := e.post(t, ticketBody, "tenon:pln_t/s1", ticket)
	if status != http.StatusForbidden || !strings.Contains(string(raw), protocol.CodeTicketExpired) {
		t.Fatalf("过期票应 403 TICKET_EXPIRED，得到 %d：%s", status, raw)
	}
	if got := e.executed.Load(); got != 0 {
		t.Fatalf("过期票仍执行了业务 %d 次", got)
	}
}

// 5. 跨模块用票：拿别的模块的票打本模块。
func TestTicketWrongModuleRejected(t *testing.T) {
	e := newTicketEnv(t)
	ticket := e.ticket(t, "procurement", writeActionID, []byte(ticketBody), "tenon:pln_t/s1", 30*time.Second)
	status, raw := e.post(t, ticketBody, "tenon:pln_t/s1", ticket)
	if status != http.StatusForbidden || !strings.Contains(string(raw), protocol.CodeTicketWrongModule) {
		t.Fatalf("跨模块用票应 403 TICKET_WRONG_MODULE，得到 %d：%s", status, raw)
	}
	if got := e.executed.Load(); got != 0 {
		t.Fatalf("跨模块用票仍执行了业务 %d 次", got)
	}
}

// 6. 同票同键重发（传输层重试）：返回首次结果，业务只执行一次。
func TestTicketRetryWithSameKeyReturnsFirstResult(t *testing.T) {
	e := newTicketEnv(t)
	ticket := e.ticket(t, fixtureModuleID, writeActionID, []byte(ticketBody), "tenon:pln_t/s1", 30*time.Second)
	_, first := e.post(t, ticketBody, "tenon:pln_t/s1", ticket)
	status, second := e.post(t, ticketBody, "tenon:pln_t/s1", ticket)
	if status != http.StatusOK {
		t.Fatalf("同键重试应 200（幂等优先），得到 %d：%s", status, second)
	}
	if string(first) != string(second) {
		t.Fatalf("重试必须返回首次结果：%s / %s", first, second)
	}
	if got := e.executed.Load(); got != 1 {
		t.Fatalf("业务只应执行 1 次，实际 %d 次", got)
	}
}
