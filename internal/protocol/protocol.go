// Package protocol 是 Tenon 线格式在 Go 侧的公共词汇表：
// 版本常量、实体寻址、错误分类与状态码映射、HMAC 签名、幂等键格式。
//
// 它只依赖标准库，且**不含任何业务逻辑**——业务在各模块里。
// 与规范的关系：字段与取值域以 spec/0.1/schemas/*.json 为唯一真源，
// 本包只做「线上要用到的那些」的编解码，不做完整校验（校验在 internal/spec）。
package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Version 是本实现支持的协议版本，必须与 manifest 的 tenon 字段一致。
const Version = "0.1"

// 请求/响应头。见 docs/interfaces.md §3.1。
const (
	HeaderVersion        = "Tenon-Version"
	HeaderMinVersion     = "Tenon-Min-Version"
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderGrant          = "X-Tenon-Grant"
	HeaderSignature      = "X-Tenon-Signature"
)

// 签名容差：超出即拒，防重放。
const SignatureTolerance = 300 * time.Second

// EntityRef 是跨契约统一的实体引用（spec/0.1/schemas/common.schema.json#/$defs/EntityRef）。
type EntityRef struct {
	Module string `json:"module"`
	Type   string `json:"type"`
	ID     string `json:"id"`
}

// String 输出规范里的字符串简写：module:Type/id。
func (e EntityRef) String() string { return e.Module + ":" + e.Type + "/" + e.ID }

// ParseEntityRef 解析 module:Type/id。注意 module 与 Type 都不得含 ':' 或 '/'。
func ParseEntityRef(s string) (EntityRef, error) {
	colon := strings.Index(s, ":")
	if colon <= 0 {
		return EntityRef{}, fmt.Errorf("entity ref %q 缺少 module 前缀", s)
	}
	slash := strings.Index(s[colon+1:], "/")
	if slash <= 0 {
		return EntityRef{}, fmt.Errorf("entity ref %q 缺少 Type/id 段", s)
	}
	slash += colon + 1
	module, typ, id := s[:colon], s[colon+1:slash], s[slash+1:]
	if id == "" {
		return EntityRef{}, fmt.Errorf("entity ref %q 的 id 为空", s)
	}
	return EntityRef{Module: module, Type: typ, ID: id}, nil
}

// ErrorClass 是四类错误的分类（spec/0.1/errors.md）。
type ErrorClass string

const (
	// ClassRetryable 表示瞬时故障，可退避重试。
	ClassRetryable ErrorClass = "retryable"
	// ClassBusinessRejection 表示请求合法但业务上不可执行，agent 应重新规划。
	ClassBusinessRejection ErrorClass = "business_rejection"
	// ClassPolicyDenied 表示身份可信但无权做，只能转人工。
	ClassPolicyDenied ErrorClass = "policy_denied"
	// ClassContractViolation 表示报文不符契约，属接入方或协议 bug。
	ClassContractViolation ErrorClass = "contract_violation"
)

// Retryable 返回该类错误是否可重试。
func (c ErrorClass) Retryable() bool { return c == ClassRetryable }

// StatusFor 按 spec/0.1/errors.md §5 的建议映射到 HTTP 状态码。
func StatusFor(c ErrorClass) int {
	switch c {
	case ClassRetryable:
		return http.StatusServiceUnavailable
	case ClassBusinessRejection:
		return http.StatusUnprocessableEntity
	case ClassPolicyDenied:
		return http.StatusForbidden
	case ClassContractViolation:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// Error 是 tenonError 的响应体内容。
type Error struct {
	Class     ErrorClass     `json:"class"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	Retryable bool           `json:"retryable"`
}

// ErrorEnvelope 是错误响应的外层包裹（spec/0.1/schemas/error.schema.json）。
type ErrorEnvelope struct {
	TenonError Error `json:"tenonError"`
}

// NewError 构造错误；retryable 由 class 推导，避免调用方填错。
func NewError(class ErrorClass, code, message string) Error {
	return Error{Class: class, Code: code, Message: message, Retryable: class.Retryable()}
}

// IdempotencyKey 按规范约定构造幂等键：tenon:<planId>/<stepId>。
func IdempotencyKey(planID, stepID string) string {
	return "tenon:" + planID + "/" + stepID
}

// Sign 计算 X-Tenon-Signature 的值：t=<unix>,v1=<hex>，其中
// v1 = HMAC-SHA256(secret, "<t>." + rawBody)。
func Sign(secret []byte, timestamp int64, body []byte) string {
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hmacHex(secret, timestamp, body)
}

func hmacHex(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ErrSignature 是验签失败的原因集合，便于区分「格式错」与「被篡改」。
var (
	ErrSignatureFormat  = errors.New("签名头格式非法")
	ErrSignatureExpired = errors.New("签名时间戳超出容差")
	ErrSignatureInvalid = errors.New("签名不匹配")
)

// Verify 校验签名头。now 用于注入以便测试；tolerance 通常取 SignatureTolerance。
// body 必须是**原始字节**，不能先反序列化再序列化。
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	if header == "" {
		return ErrSignatureFormat
	}
	var tRaw, v1 string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return ErrSignatureFormat
		}
		switch kv[0] {
		case "t":
			tRaw = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if tRaw == "" || v1 == "" {
		return ErrSignatureFormat
	}
	ts, err := strconv.ParseInt(tRaw, 10, 64)
	if err != nil {
		return ErrSignatureFormat
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew < -tolerance || skew > tolerance {
		return ErrSignatureExpired
	}
	want := hmacHex(secret, ts, body)
	got, err := hex.DecodeString(v1)
	if err != nil {
		return ErrSignatureFormat
	}
	wantBytes, err := hex.DecodeString(want)
	if err != nil {
		return ErrSignatureFormat
	}
	if !hmac.Equal(got, wantBytes) {
		return ErrSignatureInvalid
	}
	return nil
}
