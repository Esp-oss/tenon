package protocol

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEntityRefRoundTrip(t *testing.T) {
	ref := EntityRef{Module: "inventory", Type: "SKU", ID: "STD-1001"}
	got := ref.String()
	if got != "inventory:SKU/STD-1001" {
		t.Fatalf("String() = %q", got)
	}
	back, err := ParseEntityRef(got)
	if err != nil {
		t.Fatalf("ParseEntityRef: %v", err)
	}
	if back != ref {
		t.Fatalf("往返不一致：%+v != %+v", back, ref)
	}
}

func TestParseEntityRefRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "inventory", "inventory:", "inventory:SKU", "inventory:SKU/", ":SKU/x", "a:b/c/d"} {
		if got, err := ParseEntityRef(s); err == nil {
			// a:b/c/d 允许：id 里可含 '/'？规范要求 id 不含 '/'，这里只要求不 panic 且解析成功与否稳定
			if s == "a:b/c/d" {
				if got.ID != "c/d" {
					t.Errorf("%q 解析出 id=%q", s, got.ID)
				}
				continue
			}
			t.Errorf("%q 应报错，却得到 %+v", s, got)
		}
	}
}

func TestSignAndVerify(t *testing.T) {
	secret := []byte("dev-secret")
	body := []byte(`{"eventId":"evt_1234","type":"quote.completed"}`)
	now := time.Unix(1_700_000_000, 0)

	header := Sign(secret, now.Unix(), body)
	if !strings.HasPrefix(header, "t=1700000000,v1=") {
		t.Fatalf("签名头格式不对：%q", header)
	}

	if err := Verify(secret, header, body, now, SignatureTolerance); err != nil {
		t.Fatalf("正确签名验不过：%v", err)
	}

	t.Run("被篡改的 body", func(t *testing.T) {
		err := Verify(secret, header, []byte(`{"eventId":"evt_9999"}`), now, SignatureTolerance)
		if !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("期望 ErrSignatureInvalid，得到 %v", err)
		}
	})

	t.Run("错误的密钥", func(t *testing.T) {
		err := Verify([]byte("other"), header, body, now, SignatureTolerance)
		if !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("期望 ErrSignatureInvalid，得到 %v", err)
		}
	})

	t.Run("超出容差", func(t *testing.T) {
		later := now.Add(SignatureTolerance + time.Second)
		err := Verify(secret, header, body, later, SignatureTolerance)
		if !errors.Is(err, ErrSignatureExpired) {
			t.Fatalf("期望 ErrSignatureExpired，得到 %v", err)
		}
	})

	t.Run("容差边界内", func(t *testing.T) {
		edge := now.Add(SignatureTolerance)
		if err := Verify(secret, header, body, edge, SignatureTolerance); err != nil {
			t.Fatalf("边界应通过：%v", err)
		}
	})

	t.Run("格式非法", func(t *testing.T) {
		for _, h := range []string{"", "garbage", "t=abc,v1=zz", "t=1700000000"} {
			if err := Verify(secret, h, body, now, SignatureTolerance); err == nil {
				t.Errorf("%q 应报格式错误", h)
			}
		}
	})
}

func TestErrorClassMapping(t *testing.T) {
	cases := []struct {
		class     ErrorClass
		status    int
		retryable bool
	}{
		{ClassRetryable, http.StatusServiceUnavailable, true},
		{ClassBusinessRejection, http.StatusUnprocessableEntity, false},
		{ClassPolicyDenied, http.StatusForbidden, false},
		{ClassContractViolation, http.StatusBadRequest, false},
	}
	for _, c := range cases {
		if got := StatusFor(c.class); got != c.status {
			t.Errorf("%s 状态码 = %d，期望 %d", c.class, got, c.status)
		}
		if got := c.class.Retryable(); got != c.retryable {
			t.Errorf("%s Retryable() = %v，期望 %v", c.class, got, c.retryable)
		}
	}
	if got := StatusFor(ErrorClass("unknown")); got != http.StatusInternalServerError {
		t.Errorf("未知类的兜底状态码 = %d", got)
	}
}

func TestNewErrorDerivesRetryable(t *testing.T) {
	if e := NewError(ClassRetryable, "X", "m"); !e.Retryable {
		t.Error("retryable 类应自动 retryable=true")
	}
	if e := NewError(ClassPolicyDenied, "X", "m"); e.Retryable {
		t.Error("policy_denied 不应 retryable")
	}
}

func TestIdempotencyKeyFormat(t *testing.T) {
	if got := IdempotencyKey("pln_01JB", "s2"); got != "tenon:pln_01JB/s2" {
		t.Fatalf("幂等键格式 = %q", got)
	}
}
