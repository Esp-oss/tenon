// Package protocol 是 Tenon 线格式在 Go 侧的公共词汇表：
// 版本常量、实体寻址、错误分类与状态码映射、HMAC 签名、幂等键格式。
//
// 它只依赖标准库，且**不含任何业务逻辑**——业务在各模块里。
// 与规范的关系：字段与取值域以 spec/0.1/schemas/*.json 为唯一真源，
// 本包只做「线上要用到的那些」的编解码，不做完整校验（校验在 internal/spec）。
package protocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
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

// CanonicalJSON 输出 RFC 8785（JCS）形式的紧凑 JSON：键按 UTF-16 码元序排序、无任何
// 多余空白、字符串只做必需转义、数字按 ECMAScript Number::toString 书写。同一份语义内容
// 只有一种字节形态，所以 hash 可以被任何第三方独立复算。
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, x)
	case float64:
		// 调用方常直接用 encoding/json 反序列化（得到 float64）；转成 json.Number 再走同一条
		// 最短表示逻辑，避免两条数字格式化路径分叉。
		num, err := canonicalNumber(json.Number(strconv.FormatFloat(x, 'g', -1, 64)))
		if err != nil {
			return err
		}
		buf.WriteString(num)
	case json.Number:
		num, err := canonicalNumber(x)
		if err != nil {
			return err
		}
		buf.WriteString(num)
	case []any:
		buf.WriteByte('[')
		for i, elem := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("JSON 规范化：不支持的取值类型 %T", v)
	}
	return nil
}

// writeCanonicalString 按 RFC 8785 §3.2.2.2 写字符串：只做必需转义，控制字符用最短形式
// （\b \t \n \f \r，其余 \u00xx 小写十六进制），非 ASCII 字符按 UTF-8 原样输出。
func writeCanonicalString(buf *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[(r>>4)&0xf])
				buf.WriteByte(hexDigits[r&0xf])
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// utf16Less 按 UTF-16 码元序列比较字符串，这是 RFC 8785 §3.2.3 规定的键序；它与按 UTF-8
// 字节序只在「BMP 末尾字符（U+E000..U+FFFF）与代理对（非 BMP 字符）混排」时分歧。
func utf16Less(a, b string) bool {
	au, bu := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

// canonicalNumber 按 ECMAScript Number::toString（RFC 8785 §3.2.2.3）写数字：同一个数值
// 无论原文写成 1e3、1000 还是 1000.0，规范化后都是 1000；-0 写成 0。
//
// 解析不成 float64 或超出其表示范围（含 ±Inf）一律报错，不静默截断：静默截断会让两条
// 不同的记录算出同一个摘要，链就失去了「改一处即断」的性质。
func canonicalNumber(n json.Number) (string, error) {
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return "", fmt.Errorf("数字 %q 无法表示为 float64：%w", n.String(), err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("数字 %q 超出 float64 表示范围", n.String())
	}
	if f == 0 {
		return "0", nil
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// 'e' 格式给出最短往返有效数字，形如 d[.ddd]e±XX，满足 d.ddd × 10^exp = f。
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(sci, "e")
	exp, err := strconv.Atoi(expText)
	if err != nil {
		return "", fmt.Errorf("数字 %q 的指数部分无法解析：%w", n.String(), err)
	}
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	// 与 ECMA-262 的 (s, k, n) 同构：值 = 0.<digits> × 10^n。
	n10 := exp + 1
	var out string
	switch {
	case k <= n10 && n10 <= 21:
		out = digits + strings.Repeat("0", n10-k)
	case 0 < n10 && n10 <= 21:
		out = digits[:n10] + "." + digits[n10:]
	case -6 < n10 && n10 <= 0:
		out = "0." + strings.Repeat("0", -n10) + digits
	default:
		e := n10 - 1
		sign := "+"
		if e < 0 {
			sign, e = "-", -e
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(e)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(e)
		}
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}
