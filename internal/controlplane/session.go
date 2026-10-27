package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"tenon.dev/internal/protocol"
)

// User 是 dev 账号一行。文件名与字段都带 dev 语义：这不是真认证。
type User struct {
	User     string   `json:"user"`
	Password string   `json:"password"`
	Roles    []string `json:"roles"`
}

// Claims 是 token 的有效载荷。
type Claims struct {
	Subject string   `json:"sub"`
	Roles   []string `json:"roles"`
	Exp     int64    `json:"exp"`
}

// 错误值：调用方（HTTP 处理器）据此决定 401 还是 500。
var (
	ErrTokenInvalid = errors.New("token 无效")
	ErrTokenExpired = errors.New("token 已过期")
)

// LoadUsers 读取 dev 账号表。空文件视为「没有任何账号」（全部登录失败），不是错误。
func LoadUsers(path string) ([]User, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("controlplane: 读取账号文件失败：%w", err)
	}
	var users []User
	if err := json.Unmarshal(raw, &users); err != nil {
		return nil, fmt.Errorf("controlplane: 账号文件不是 JSON 数组：%w", err)
	}
	return users, nil
}

// SignToken 签发无状态 token：base64url(payload).base64url(HMAC-SHA256(payload))。
func SignToken(secret []byte, subject string, roles []string, now time.Time, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("controlplane: 会话密钥为空")
	}
	payload, err := json.Marshal(Claims{Subject: subject, Roles: roles, Exp: now.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// VerifyToken 校验签名与过期时间。任何一步失败都返回错误，不做「降级放行」。
func VerifyToken(secret []byte, token string, now time.Time) (Claims, error) {
	var out Claims
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return out, ErrTokenInvalid
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return out, ErrTokenInvalid
	}
	sig, err := enc.DecodeString(parts[1])
	if err != nil {
		return out, ErrTokenInvalid
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return out, ErrTokenInvalid
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, ErrTokenInvalid
	}
	if now.Unix() >= out.Exp {
		return out, ErrTokenExpired
	}
	return out, nil
}

// bearerClaims 从 Authorization 头取并校验 token。失败时已写好 401 响应。
func (s *Server) bearerClaims(w http.ResponseWriter, r *http.Request) (Claims, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		writeTenonErrorStatus(w, http.StatusUnauthorized, *newError(protocol.ClassPolicyDenied,
			"AUTH_REQUIRED", "缺少 Authorization: Bearer <token>"))
		return Claims{}, false
	}
	claims, err := VerifyToken(s.sessionSecret, strings.TrimSpace(raw[len(prefix):]), time.Now().UTC())
	if err != nil {
		writeTenonErrorStatus(w, http.StatusUnauthorized, *newError(protocol.ClassPolicyDenied,
			"AUTH_INVALID", err.Error()))
		return Claims{}, false
	}
	return claims, true
}

// serveSession 处理 POST /tenon/session：口令换 token。
func (s *Server) serveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var in struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {\"user\":…,\"password\":…}："+err.Error()))
		return
	}
	for _, u := range s.users {
		if u.User != in.User || u.Password != in.Password {
			continue
		}
		subject := "user:" + u.User
		tok, err := SignToken(s.sessionSecret, subject, u.Roles, time.Now().UTC(), sessionTTL)
		if err != nil {
			writeTenonErrorStatus(w, http.StatusInternalServerError,
				*newError(protocol.ClassContractViolation, "TOKEN_SIGN_FAILED", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token": tok,
			"user":  subject,
			"roles": u.Roles,
			"exp":   time.Now().UTC().Add(sessionTTL).Unix(),
		})
		return
	}
	// 不区分「用户不存在」与「口令错」：dev 环境也不送可枚举的信息。
	writeTenonErrorStatus(w, http.StatusUnauthorized,
		*newError(protocol.ClassPolicyDenied, "AUTH_INVALID", "账号或口令不正确"))
}
