package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// HeaderTicket 携带宿主签发的执行票据。见 design §5.1。
const HeaderTicket = "Tenon-Ticket"

// TicketPrefix 是票据的版本前缀；换签名方案时递增。
const TicketPrefix = "v1"

// TicketMaxTTL 是票据允许的最长有效期。票只能"当场用"，不能预支权限。
const TicketMaxTTL = 60 * time.Second

// 票据相关的错误码（design §5.3）。全部映射为 policy_denied（403）：
// 调用方不是"参数写错了"，而是"没有被授权做这件事"。
const (
	CodeTicketMissing        = "TICKET_MISSING"
	CodeTicketInvalid        = "TICKET_INVALID"
	CodeTicketUnknownKey     = "TICKET_UNKNOWN_KEY"
	CodeTicketWrongModule    = "TICKET_WRONG_MODULE"
	CodeTicketActionMismatch = "TICKET_ACTION_MISMATCH"
	CodeTicketExpired        = "TICKET_EXPIRED"
	CodeTicketArgsMismatch   = "TICKET_ARGS_MISMATCH"
	CodeTicketReused         = "TICKET_REUSED"
	CodeTicketNotApproved    = "TICKET_NOT_APPROVED"
)

// NewTicketError 构造票据类错误。分类固定 policy_denied、不可重试：
// 票据失败意味着计划过期或参数被改，重试无意义，只能重新规划。
func NewTicketError(code, message string) Error {
	return NewError(ClassPolicyDenied, code, message)
}

// Ticket 是票据载荷。字段名短，是因为整张票要塞进 HTTP 头。
type Ticket struct {
	V              int     `json:"v"`
	TicketID       string  `json:"tid"`
	KeyID          string  `json:"kid"`
	Module         string  `json:"mod"`
	Action         string  `json:"act"`
	ArgsHash       string  `json:"ah"`
	IdempotencyKey string  `json:"idem"`
	PlanID         string  `json:"pid"`
	StepID         string  `json:"sid"`
	AuditID        string  `json:"aud"`
	ApprovedBy     *string `json:"by"`
	GrantID        *string `json:"gid"`
	IssuedAt       int64   `json:"iat"`
	ExpiresAt      int64   `json:"exp"`
}

// 票据的错误原因集合。调用方据此区分"格式错"与"时间错"。
var (
	ErrTicketFormat     = errors.New("票据格式非法")
	ErrTicketInvalid    = errors.New("票据签名不匹配")
	ErrTicketUnknownKey = errors.New("票据签名密钥不在信任集")
	ErrTicketExpired    = errors.New("票据已过期或签发时间超前")
	ErrTicketDuration   = errors.New("票据有效期非法")
)

// KeyID 由公钥内容推导，不单独分配：换密钥即换 id，无法伪造映射。
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// SignTicket 签发一张票。kid 由私钥推导并写入载荷，调用方不必自填。
// 有效期必须满足 0 < exp-iat <= 60s，否则拒绝签发——签出去的长票等于长期授权。
func SignTicket(priv ed25519.PrivateKey, t Ticket) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("私钥长度 %d，期望 %d", len(priv), ed25519.PrivateKeySize)
	}
	if t.V == 0 {
		t.V = 1
	}
	if t.V != 1 {
		return "", fmt.Errorf("%w：不支持的版本 %d", ErrTicketFormat, t.V)
	}
	if err := t.validateShape(); err != nil {
		return "", err
	}
	ttl := time.Duration(t.ExpiresAt-t.IssuedAt) * time.Second
	if ttl <= 0 || ttl > TicketMaxTTL {
		return "", fmt.Errorf("%w：有效期 %s 不在 (0, %s] 内", ErrTicketDuration, ttl, TicketMaxTTL)
	}
	t.KeyID = KeyID(priv.Public().(ed25519.PublicKey))

	payload, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("序列化载荷：%w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	signed := TicketPrefix + "." + body
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(signed))), nil
}

func (t Ticket) validateShape() error {
	missing := make([]string, 0, 9)
	for name, v := range map[string]string{
		"tid": t.TicketID, "mod": t.Module, "act": t.Action,
		"ah": t.ArgsHash, "idem": t.IdempotencyKey,
		"pid": t.PlanID, "sid": t.StepID, "aud": t.AuditID,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// map 遍历无序，排序后再报，测试输出才稳定
		sortStrings(missing)
		return fmt.Errorf("%w：缺少字段 %s", ErrTicketFormat, strings.Join(missing, ","))
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ParseTicket 拆票并返回载荷。不验签，只解析——验签走 VerifyTicket。
func ParseTicket(token string) (Ticket, string, error) {
	var zero Ticket
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return zero, "", fmt.Errorf("%w：段数 %d，期望 3", ErrTicketFormat, len(parts))
	}
	if parts[0] != TicketPrefix {
		return zero, "", fmt.Errorf("%w：前缀 %q", ErrTicketFormat, parts[0])
	}
	if parts[1] == "" || parts[2] == "" {
		return zero, "", fmt.Errorf("%w：载荷或签名段为空", ErrTicketFormat)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return zero, "", fmt.Errorf("%w：载荷不是 base64url：%v", ErrTicketFormat, err)
	}
	var t Ticket
	if err := json.Unmarshal(raw, &t); err != nil {
		return zero, "", fmt.Errorf("%w：载荷不是合法 JSON：%v", ErrTicketFormat, err)
	}
	if t.V != 1 {
		return zero, "", fmt.Errorf("%w：不支持的版本 %d", ErrTicketFormat, t.V)
	}
	return t, parts[1], nil
}

// VerifyTicket 校验票据。步骤顺序与 design §5.3 一致：
// 格式 → kid → 签名 → 时间。参数层面的比对（mod/act/ah）由调用方做，
// 因为只有调用方知道"这次请求打的是哪个模块、哪个动作"。
func VerifyTicket(keys KeySet, token string, now time.Time, tolerance time.Duration) (Ticket, error) {
	t, body, err := ParseTicket(token)
	if err != nil {
		return Ticket{}, err
	}
	pub, ok := keys[t.KeyID]
	if !ok {
		return Ticket{}, fmt.Errorf("%w：kid %q", ErrTicketUnknownKey, t.KeyID)
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Ticket{}, fmt.Errorf("%w：签名段非法", ErrTicketFormat)
	}
	if !ed25519.Verify(pub, []byte(TicketPrefix+"."+body), sig) {
		return Ticket{}, ErrTicketInvalid
	}
	if now.Unix() > t.ExpiresAt {
		return Ticket{}, fmt.Errorf("%w：exp=%d now=%d", ErrTicketExpired, t.ExpiresAt, now.Unix())
	}
	if t.IssuedAt > now.Add(tolerance).Unix() {
		return Ticket{}, fmt.Errorf("%w：iat=%d 超前于 now=%d", ErrTicketExpired, t.IssuedAt, now.Unix())
	}
	if ttl := time.Duration(t.ExpiresAt-t.IssuedAt) * time.Second; ttl <= 0 || ttl > TicketMaxTTL {
		return Ticket{}, fmt.Errorf("%w：有效期 %s", ErrTicketDuration, ttl)
	}
	return t, nil
}

// ArgsHash 是入参的摘要，绑定**规范化之后的请求体字节**。
//
// 规范化 = 去掉 JSON 里不重要的空白（`json.Compact`），键序与值一个字节都不动。
// 为什么不能直接哈希原始字节：同一份参数在链路上会经历一次落盘再读回——挂起时写进
// pending/*.json 的是紧凑形式，而模型给的原始输出常带缩进。字节一变，摘要就变，症状是
// 「人批准过了，签票却说参数不一致」（2026-10-31 本地演示实测：approvedArgsHash 与
// requestedArgsHash 不同，批准后无法执行）。空白不是参数的一部分，不该参与身份判定。
//
// 不做「解析成 map 再序列化」：那会按字典序重排键，等于把**另一个**不变量混进来，
// 而且 struct 字段顺序、数字格式都会漂移，反而更难对齐。
func ArgsHash(body []byte) string {
	sum := sha256.Sum256(canonicalJSON(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalJSON 去掉无关空白。非法 JSON 原样返回：哈希函数不该替调用方判断合法性，
// 那种错误应该在解析处报出来，而不是在这里变成「摘要算不出来」。
func canonicalJSON(body []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return body
	}
	return buf.Bytes()
}

// KeySet 是模块侧的信任公钥集：kid → 公钥。多把并存以支持轮换。
type KeySet map[string]ed25519.PublicKey

// ParseKeySet 解析 {"<kid>":"<base64 公钥>"}。公钥长度不对即报错，不静默丢弃。
func ParseKeySet(data []byte) (KeySet, error) {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("信任集不是 JSON 对象：%w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("信任集为空")
	}
	set := make(KeySet, len(raw))
	for kid, b64 := range raw {
		if kid == "" {
			return nil, errors.New("信任集含空 kid")
		}
		pub, err := decodePublicKey(b64)
		if err != nil {
			return nil, fmt.Errorf("kid %q：%w", kid, err)
		}
		if KeyID(pub) != kid {
			return nil, fmt.Errorf("kid %q 与公钥指纹 %q 不符", kid, KeyID(pub))
		}
		set[kid] = pub
	}
	return set, nil
}

func decodePublicKey(b64 string) (ed25519.PublicKey, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(b64); err == nil {
			if len(raw) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("公钥长度 %d，期望 %d", len(raw), ed25519.PublicKeySize)
			}
			return ed25519.PublicKey(raw), nil
		}
	}
	return nil, errors.New("公钥不是 base64")
}

// LoadKeySetFile 读取信任集文件。模块缺这个文件时必须拒绝启动，而不是"没有密钥就放行"。
func LoadKeySetFile(path string) (KeySet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取信任集 %s：%w", path, err)
	}
	return ParseKeySet(data)
}
