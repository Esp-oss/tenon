// Package llm 是 Tenon agent 的 LLM 客户端：调 OpenAI 兼容的 /chat/completions，
// 拿回「一个 JSON 对象」的文本，交给调用方做 schema 校验。
//
// 三条硬性约束（docs/superpowers/specs/2026-10-27-tenon-m05-host-agent-design.md §4）：
//   - temperature 恒为 0：D1 的可重复性依赖它（R7）；
//   - 请求带 response_format={"type":"json_object"}：要求模型只吐一个 JSON 对象；
//   - 缺 key 时 ConfigFromEnv 直接报错，**不允许静默降级**——agent 必须启动即失败，
//     否则它会在没有模型的情况下假装「已规划」，那是最坏的失败方式。
//
// 职责边界：本包只做传输与解包。schema 校验在调用方（internal/spec 的
// plan.schema.json），重试与错误分类在 internal/agent——本包不替上层决定
// 「这次失败要不要重试」，但用 %w 保留了原始 error，上层可用 errors.Is /
// errors.As 自行判定超时与网络故障。
//
// 本包只依赖标准库。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// 本包读取的环境变量名，与设计 §4 的环境变量表一一对应。
const (
	envBaseURL = "TENON_LLM_BASE_URL"
	envAPIKey  = "TENON_LLM_API_KEY"
	envKeyFile = "TENON_LLM_API_KEY_FILE"
	envModel   = "TENON_LLM_MODEL"
	envTimeout = "TENON_LLM_TIMEOUT"
)

// 缺省值：与设计 §4 的表一致，改这里就等于改文档承诺的默认行为。
const (
	DefaultBaseURL = "https://api.deepseek.com"
	DefaultModel   = "deepseek-flash"
	DefaultTimeout = 60 * time.Second
)

// errBodyLimit 是错误信息里响应体的最大保留字符数（按 rune 计，不切坏汉字）。
const errBodyLimit = 500

// maxResponseBytes 是响应体的大小上限。规划响应在几 KB 量级；定一个正常不会
// 踩到的 8 MiB，只为防止坏端点把进程内存打爆（超限是显式错误，不静默截断）。
const maxResponseBytes = 8 << 20

// Config 是客户端的静态配置。BaseURL 形如 https://api.deepseek.com：
// 不带 /v1、也不带尾斜杠——路径由 ChatJSON 自己拼。
type Config struct {
	BaseURL string        // 形如 https://api.deepseek.com（不带 /v1，也不带尾斜杠）
	APIKey  string        // key 本身：文件来源已裁空白，不做 hex/base64 解码
	Model   string        // 例如 deepseek-flash
	Timeout time.Duration // 单次请求的整体超时；<= 0 时 New 用 DefaultTimeout
}

// ConfigFromEnv 从环境变量组装配置；缺 key 时返回错误（**不静默降级**）。
//
// 读取规则：
//   - TENON_LLM_BASE_URL	缺省 https://api.deepseek.com，去首尾空白与尾斜杠；
//   - TENON_LLM_API_KEY_FILE	非空时**优先**读文件：内容去首尾空白后整段当作 key，
//     **不做 hex / base64 解码**（文件里就是 key 本身）；
//   - TENON_LLM_API_KEY	没有 key 文件时用它；
//   - TENON_LLM_MODEL	缺省 deepseek-flash；
//   - TENON_LLM_TIMEOUT	缺省 60s，用 time.ParseDuration 解析，必须为正。
//
// 只要 key 文件被配置就必须读成功：读失败或内容为空都返回错误，不会回落到
// 环境变量——那会把「配置错」伪装成「能跑」。
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envBaseURL)), "/"),
		Model:   strings.TrimSpace(os.Getenv(envModel)),
		Timeout: DefaultTimeout,
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}

	// 显式配了超时才覆盖缺省值；空字符串等同于没配。
	if raw := strings.TrimSpace(os.Getenv(envTimeout)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("llm: %s=%q 不是合法时长（例：90s、2m）：%w", envTimeout, raw, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("llm: %s=%q 必须为正数（不接受无超时）", envTimeout, raw)
		}
		cfg.Timeout = d
	}

	key, err := apiKeyFromEnv()
	if err != nil {
		return Config{}, err
	}
	cfg.APIKey = key
	return cfg, nil
}

// apiKeyFromEnv 实现「文件优先」。文件路径存在即只认文件内容：
// 空内容与读失败都是错误，绝不回落到 TENON_LLM_API_KEY。
func apiKeyFromEnv() (string, error) {
	if path := strings.TrimSpace(os.Getenv(envKeyFile)); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("llm: 读取 %s=%q 失败：%w", envKeyFile, path, err)
		}
		// 只裁空白：key 文件常带一个换行；不猜测它是否还有别的编码。
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", fmt.Errorf("llm: %s=%q 的内容为空", envKeyFile, path)
		}
		return key, nil
	}

	key := strings.TrimSpace(os.Getenv(envAPIKey))
	if key == "" {
		return "", fmt.Errorf("llm: 缺少 API key，请设置 %s 或 %s（不允许静默降级：agent 必须启动即失败）", envAPIKey, envKeyFile)
	}
	return key, nil
}

// Client 是 OpenAI 兼容端点的最小客户端。零值不可用，请用 New 构造。
// 它内部只有一个 *http.Client 与若干只读字符串，可被多个 goroutine 并发使用。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	hc      *http.Client
}

// New 构造客户端。Timeout <= 0 时用 DefaultTimeout（宁可有超时，也不要挂死）；
// BaseURL 的空白与尾斜杠会被去掉，避免拼出 //chat/completions。
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		hc:      &http.Client{Timeout: timeout},
	}
}

// chatMessage 是一条消息。本包只用 system / user 两个角色：
// 规划不需要多轮对话历史，一次请求里放一个 system 约束加一个 user 事实。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseFormat 让模型只输出一个 JSON 对象（不支持该字段的端点会忽略它，
// 此时由调用方的 schema 校验兜底）。
type responseFormat struct {
	Type string `json:"type"`
}

// chatRequest 是请求体的完成形态。Temperature 用数值类型且恒为 0：
// 线上必须是 JSON 数字 0，不是字符串 "0"——后者会被一部分兼容端点判为坏请求。
type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature"`
	ResponseFormat responseFormat `json:"response_format"`
}

// chatResponse 只解出本包关心的那一条路径：choices[0].message.content。
type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

// ChatJSON 调一次 POST {BaseURL}/chat/completions，返回
// choices[0].message.content 的**原始字节**——不做 schema 校验、不剥 Markdown
// 围栏、不做业务字段解释，这些都是调用方的事。
//
// 返回错误的情形：请求体编码失败、URL 构造失败、传输层失败（超时 / DNS /
// 连接被拒）、非 2xx（错误信息含状态码与响应体前 500 字符）、响应不是合法
// JSON、choices 为空、content 为空。这些错误都用 %w 包住了原始 error，
// 上层可据 errors.Is(err, context.Canceled/DeadlineExceeded) 与 net.Error
// 判定是否 retryable；本函数自己**不做重试**。
func (c *Client) ChatJSON(ctx context.Context, system, user string) ([]byte, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature:    0,
		ResponseFormat: responseFormat{Type: "json_object"},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: 编码请求体失败：%w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: 构造请求失败（url=%q）：%w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		// 超时与网络错误都从这里出去；不像状态码那样自带分类，交给上层。
		return nil, fmt.Errorf("llm: 调用 %s 失败：%w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("llm: 读取响应体失败（状态 %d）：%w", resp.StatusCode, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("llm: 响应体超过 %d 字节上限（状态 %d）", maxResponseBytes, resp.StatusCode)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("llm: %s 返回 %d %s，响应体：%s",
			endpoint, resp.StatusCode, http.StatusText(resp.StatusCode), snippet(raw, errBodyLimit))
	}

	// 非 JSON 与空 choices 属于「端点/代理坏了」，不是「模型给了坏计划」：
	// 在这里就把它们区分出来，上层才不会把网关故障当成 schema 不符去重试。
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("llm: 响应不是合法 JSON（状态 %d）：%w", resp.StatusCode, err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("llm: 响应 choices 为空（状态 %d），响应体：%s",
			resp.StatusCode, snippet(raw, errBodyLimit))
	}

	content := parsed.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("llm: choices[0].message.content 为空（状态 %d）", resp.StatusCode)
	}
	return []byte(content), nil
}

// snippet 取响应体前 limit 个字符，用于错误信息。按 rune 截断是为了不把
// 多字节汉字切半个；按需遍历而不是整体转 []rune，是因为 body 可能很大。
func snippet(body []byte, limit int) string {
	s := strings.TrimSpace(string(body))
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	seen := 0
	for i := range s {
		if seen == limit {
			return s[:i]
		}
		seen++
	}
	return s
}
