package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	return pub, priv
}

func okTicket(now time.Time) Ticket {
	by := "user_li"
	gid := "gr-dev-fulfillment"
	return Ticket{
		V:              1,
		TicketID:       "tkt_0001",
		Module:         "inventory",
		Action:         "inventory.ReserveStock",
		ArgsHash:       "sha256:" + strings.Repeat("ab", 32),
		IdempotencyKey: "tenon:plan_01/s2",
		PlanID:         "plan_01",
		StepID:         "s2",
		AuditID:        "aud_0001",
		ApprovedBy:     &by,
		GrantID:        &gid,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(30 * time.Second).Unix(),
	}
}

func TestTicketRoundTrip(t *testing.T) {
	now := time.Unix(1793400000, 0)
	pub, priv := newKey(t)
	want := okTicket(now)

	token, err := SignTicket(priv, want)
	if err != nil {
		t.Fatalf("签票失败：%v", err)
	}
	if len(token) >= 1024 {
		t.Fatalf("票据长度 %d 字节，放不进 HTTP 头", len(token))
	}
	if !strings.HasPrefix(token, TicketPrefix+".") {
		t.Fatalf("票据前缀错误：%q", token)
	}

	keys := KeySet{KeyID(pub): pub}
	got, err := VerifyTicket(keys, token, now, SignatureTolerance)
	if err != nil {
		t.Fatalf("验票失败：%v", err)
	}
	if got.TicketID != want.TicketID || got.Module != want.Module || got.ArgsHash != want.ArgsHash {
		t.Fatalf("往返不一致：%+v", got)
	}
	if got.KeyID != KeyID(pub) {
		t.Fatalf("kid = %q，期望 %q", got.KeyID, KeyID(pub))
	}
	if got.ApprovedBy == nil || *got.ApprovedBy != "user_li" {
		t.Fatalf("approvedBy 丢失：%+v", got.ApprovedBy)
	}
}

func TestTicketTamperedPayload(t *testing.T) {
	now := time.Unix(1793400000, 0)
	pub, priv := newKey(t)
	token, err := SignTicket(priv, okTicket(now))
	if err != nil {
		t.Fatalf("签票失败：%v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("票据段数 = %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解码 payload 失败：%v", err)
	}
	// 把动作换成另一个，签名保持不变
	forged := strings.Replace(string(raw), "inventory.ReserveStock", "inventory.DeductStock", 1)
	if forged == string(raw) {
		t.Fatal("测试前提失败：payload 里没有预期动作名")
	}
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(forged))
	bad := strings.Join(parts, ".")

	if _, err := VerifyTicket(KeySet{KeyID(pub): pub}, bad, now, SignatureTolerance); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("改 payload 后应验签失败，得到 %v", err)
	}
}

func TestTicketWrongKey(t *testing.T) {
	now := time.Unix(1793400000, 0)
	pub, priv := newKey(t)
	otherPub, _ := newKey(t)
	token, err := SignTicket(priv, okTicket(now))
	if err != nil {
		t.Fatalf("签票失败：%v", err)
	}
	// 信任集里换成另一把公钥（kid 仍是原 kid），签名必然不匹配
	if _, err := VerifyTicket(KeySet{KeyID(pub): otherPub}, token, now, SignatureTolerance); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("错误公钥应验签失败，得到 %v", err)
	}
}

func TestTicketExpired(t *testing.T) {
	now := time.Unix(1793400000, 0)
	pub, priv := newKey(t)
	ticket := okTicket(now)
	ticket.ExpiresAt = now.Add(-1 * time.Second).Unix()
	if _, err := SignTicket(priv, ticket); !errors.Is(err, ErrTicketDuration) {
		t.Fatalf("签票时就该拒绝已过期的票，得到 %v", err)
	}

	// 签一张合法票，然后在过期后验
	ticket = okTicket(now)
	token, err := SignTicket(priv, ticket)
	if err != nil {
		t.Fatalf("签票失败：%v", err)
	}
	if _, err := VerifyTicket(KeySet{KeyID(pub): pub}, token, now.Add(31*time.Second), SignatureTolerance); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("过期票应被拒，得到 %v", err)
	}
}

func TestTicketIssuedInFuture(t *testing.T) {
	now := time.Unix(1793400000, 0)
	pub, priv := newKey(t)
	ticket := okTicket(now)
	ticket.IssuedAt = now.Add(301 * time.Second).Unix()
	ticket.ExpiresAt = ticket.IssuedAt + 30

	// 签票端不拦未来时间（时钟由宿主负责），验票端要拦
	token, err := SignTicket(priv, ticket)
	if err != nil {
		t.Fatalf("签票失败：%v", err)
	}
	if _, err := VerifyTicket(KeySet{KeyID(pub): pub}, token, now, SignatureTolerance); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("签发时间超前容差应被拒，得到 %v", err)
	}
}

func TestTicketRejectsLongTTL(t *testing.T) {
	now := time.Unix(1793400000, 0)
	_, priv := newKey(t)
	ticket := okTicket(now)
	ticket.ExpiresAt = now.Add(61 * time.Second).Unix()
	if _, err := SignTicket(priv, ticket); !errors.Is(err, ErrTicketDuration) {
		t.Fatalf("TTL 超 60s 应拒绝签发，得到 %v", err)
	}
}

func TestTicketUnknownKey(t *testing.T) {
	now := time.Unix(1793400000, 0)
	pub, priv := newKey(t)
	token, err := SignTicket(priv, okTicket(now))
	if err != nil {
		t.Fatalf("签票失败：%v", err)
	}
	otherPub, _ := newKey(t)
	if _, err := VerifyTicket(KeySet{KeyID(otherPub): otherPub}, token, now, SignatureTolerance); !errors.Is(err, ErrTicketUnknownKey) {
		t.Fatalf("未知 kid 应返回 ErrTicketUnknownKey，得到 %v", err)
	}
	_ = pub
}

func TestKeySetRoundTrip(t *testing.T) {
	pub, _ := newKey(t)
	kid := KeyID(pub)
	raw := `{"` + kid + `":"` + base64.StdEncoding.EncodeToString(pub) + `"}`

	set, err := ParseKeySet([]byte(raw))
	if err != nil {
		t.Fatalf("解析信任集失败：%v", err)
	}
	if len(set) != 1 {
		t.Fatalf("信任集条目数 = %d", len(set))
	}
	if _, ok := set[kid]; !ok {
		t.Fatalf("信任集缺少 kid %q", kid)
	}

	if _, err := ParseKeySet([]byte(`{"bad":"not-base64"}`)); err == nil {
		t.Fatal("非法公钥应报错")
	}
	if _, err := ParseKeySet([]byte(`{"short":"` + base64.StdEncoding.EncodeToString([]byte("abc")) + `"}`)); err == nil {
		t.Fatal("长度不对的公钥应报错")
	}
}

func TestArgsHashIsStable(t *testing.T) {
	body := []byte(`{"sku":"M8","qty":8}`)
	got := ArgsHash(body)
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("argsHash 前缀错误：%q", got)
	}
	if got != ArgsHash([]byte(`{"sku":"M8","qty":8}`)) {
		t.Fatal("同一字节串必须得到同一 hash")
	}
	if got == ArgsHash([]byte(`{"sku":"M8","qty":9}`)) {
		t.Fatal("不同字节串不应得到同一 hash")
	}
}

// 空白不是参数的一部分：模型给的带缩进 JSON 与落盘读回后的紧凑形式必须同 hash。
//
// 2026-10-31 本地演示实测：挂起时快照对「带换行缩进」的原始字节求摘要，恢复执行时
// 请求体已经是紧凑形式，于是 approvedArgsHash ≠ requestedArgsHash，人批准过的计划
// 被宿主自己拒成 TICKET_NOT_APPROVED。
func TestArgsHashIgnoresWhitespace(t *testing.T) {
	pretty := []byte("{\n  \"sku\": \"M8\",\n  \"qty\": 8\n}\n")
	compact := []byte(`{"sku":"M8","qty":8}`)
	if ArgsHash(pretty) != ArgsHash(compact) {
		t.Fatalf("只差空白的同一份参数必须同 hash：%s vs %s", ArgsHash(pretty), ArgsHash(compact))
	}
	// 键序仍然参与身份判定：顺序不同 = 不同字节 = 不同摘要（不做「解析再序列化」）。
	reordered := []byte(`{"qty":8,"sku":"M8"}`)
	if ArgsHash(reordered) == ArgsHash(compact) {
		t.Fatal("键序不同不应被规范化成同一个摘要（那会引入另一套不变量）")
	}
}
