package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearLLMEnv 把本包读的五个变量全部置空，让 ConfigFromEnv 的用例不受开发机
// 环境影响。t.Setenv 会在用例结束时自动还原。
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envBaseURL, envAPIKey, envKeyFile, envModel, envTimeout} {
		t.Setenv(k, "")
	}
}

// writeKeyFile 在临时目录写一个 key 文件并返回路径。用 os.WriteFile 而不是
// 在断言里手拼，是为了让「文件优先」与「裁空白」两个用例共用同一段准备代码。
func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写 key 文件失败：%v", err)
	}
	return path
}

// TestChatSendsJSONModeAndParsesContent 锁定请求体的关键字段与响应解包：
// temperature 必须是数值 0、response_format 必须是 json_object、model 原样传递、
// 角色顺序 system→user。
func TestChatSendsJSONModeAndParsesContent(t *testing.T) {
	var (
		gotBody   map[string]any
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCT     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"plan\":1}"}}]}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "deepseek-flash", Timeout: 5 * time.Second})
	out, err := c.ChatJSON(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != `{"plan":1}` {
		t.Fatalf("内容解析错误：%s", out)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("方法 = %q，期望 POST", gotMethod)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("路径 = %q，期望 /chat/completions", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("鉴权头 = %q，期望 Bearer k", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["model"] != "deepseek-flash" {
		t.Errorf("model = %v，期望原样传递", gotBody["model"])
	}

	// temperature 断言成 float64：解码成字符串 "0" 时类型断言会失败，
	// 于是这条断言同时锁住了「是数值」和「等于 0」。
	temp, ok := gotBody["temperature"].(float64)
	if !ok {
		t.Fatalf("temperature 必须是 JSON 数值，实际 %T(%v)", gotBody["temperature"], gotBody["temperature"])
	}
	if temp != 0 {
		t.Fatalf("temperature 必须为 0，实际 %v", temp)
	}

	rf, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 response_format：%v", gotBody)
	}
	if rf["type"] != "json_object" {
		t.Fatalf("response_format.type 必须为 json_object，实际 %v", rf["type"])
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages 必须是两条，实际 %v", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	second, _ := msgs[1].(map[string]any)
	if first["role"] != "system" || second["role"] != "user" {
		t.Fatalf("角色顺序必须是 system→user，实际 %v→%v", first["role"], second["role"])
	}
	if first["content"] != "sys" || second["content"] != "user" {
		t.Fatalf("消息内容不对：%v / %v", first["content"], second["content"])
	}
}

// TestChatSurfacesHTTPError 是计划里的第二个用例：非 2xx 必须返回错误，
// 上层据此判定 retryable。
func TestChatSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
	_, err := c.ChatJSON(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("429 必须返回错误（上层据此判定 retryable）")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("错误信息应含状态码 429，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("错误信息应含响应体内容，实际：%v", err)
	}
}

// TestChat500ErrorMentionsStatus 锁住「错误信息含状态码与响应体」这条约定。
func TestChat500ErrorMentionsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom upstream"}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
	_, err := c.ChatJSON(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("500 必须返回错误")
	}
	for _, want := range []string{"500", "boom upstream"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q，实际：%v", want, err)
		}
	}
}

// TestChatErrorBodyTruncated 确认错误信息只保留响应体前 500 字符：
// 既要有前缀，也不能把整段（可能几 MB）塞进日志。
func TestChatErrorBodyTruncated(t *testing.T) {
	long := strings.Repeat("x", 600) + "TAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, long)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
	_, err := c.ChatJSON(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("429 必须返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, strings.Repeat("x", 500)) {
		t.Errorf("错误信息应含响应体前 500 字符，实际长度 %d", len(msg))
	}
	if strings.Contains(msg, strings.Repeat("x", 501)) {
		t.Errorf("错误信息超过 500 字符上限，实际长度 %d", len(msg))
	}
	if strings.Contains(msg, "TAIL") {
		t.Errorf("错误信息不应包含被截断的尾部，实际：%v", msg)
	}
}

// TestChatEmptyChoicesReturnsError：choices 是空数组时报错，不 panic 也不返回空字节。
func TestChatEmptyChoicesReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
	out, err := c.ChatJSON(context.Background(), "s", "u")
	if err == nil {
		t.Fatalf("choices 为空必须返回错误，实际输出 %q", out)
	}
	if len(out) != 0 {
		t.Errorf("出错时不应返回内容，实际 %q", out)
	}
}

// TestChatInvalidJSONReturnsError：响应体不是合法 JSON（代理返回 HTML 等）时报错。
func TestChatInvalidJSONReturnsError(t *testing.T) {
	bodies := map[string]string{
		"纯文本":    "not json at all",
		"被截断":    `{"choices":[{"message":{"content":"x"`,
		"JSON数组": `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
			if _, err := c.ChatJSON(context.Background(), "s", "u"); err == nil {
				t.Fatalf("响应体 %q 不是合法对象，必须返回错误", body)
			}
		})
	}
}

// TestChatEmptyContentReturnsError：choices 非空但 content 为空，同样是坏响应。
func TestChatEmptyContentReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"   "}}]}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
	if _, err := c.ChatJSON(context.Background(), "s", "u"); err == nil {
		t.Fatal("content 为空白必须返回错误")
	}
}

// TestChatHonorsCanceledContext：ctx 已取消时立即失败，且错误链里仍能认出
// context.Canceled——上层靠 errors.Is 判定「不是模型的错，别重试」。
func TestChatHonorsCanceledContext(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Second})
	if _, err := c.ChatJSON(ctx, "s", "u"); err == nil {
		t.Fatal("ctx 已取消必须返回错误")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("错误链应保留 context.Canceled，实际：%v", err)
	}
	if hits != 0 {
		t.Errorf("ctx 已取消时不应真的发请求，实际命中 %d 次", hits)
	}
}

// TestChatTimeoutIsError：客户端超时也必须返回错误，且错误是 net.Error 的
// Timeout 形态（上层据此退避重试）。
func TestChatTimeoutIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 20 * time.Millisecond})
	_, err := c.ChatJSON(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("超时必须返回错误")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("超时错误应满足 net.Error 且 Timeout() 为真，实际：%v", err)
	}
}

// TestNewFillsDefaults：Timeout 为 0（zero value Config）时用缺省 60s，
// BaseURL 的尾斜杠被去掉——否则会拼出 //chat/completions。
func TestNewFillsDefaults(t *testing.T) {
	c := New(Config{})
	if c.hc.Timeout != DefaultTimeout {
		t.Errorf("零值 Timeout 应取缺省 %v，实际 %v", DefaultTimeout, c.hc.Timeout)
	}
	if got := New(Config{BaseURL: " https://api.example.com/ "}).baseURL; got != "https://api.example.com" {
		t.Errorf("BaseURL 应去空白与尾斜杠，实际 %q", got)
	}
}

// TestConfigFromEnvDefaults：只给 key，其余全部走缺省值。
func TestConfigFromEnvDefaults(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envAPIKey, "env-key")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q，期望缺省 %q", cfg.BaseURL, DefaultBaseURL)
	}
	if cfg.Model != DefaultModel {
		t.Errorf("Model = %q，期望缺省 %q", cfg.Model, DefaultModel)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v，期望缺省 %v", cfg.Timeout, DefaultTimeout)
	}
	if cfg.APIKey != "env-key" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
}

// TestConfigFromEnvReadsOverrides：四个变量都显式给值时以环境为准，
// 且 BaseURL 的尾斜杠会被去掉（设计 §4 说它不带尾斜杠）。
func TestConfigFromEnvReadsOverrides(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envBaseURL, " https://api.example.com/v1/ ")
	t.Setenv(envAPIKey, "k-1")
	t.Setenv(envModel, "deepseek-chat")
	t.Setenv(envTimeout, "90s")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Model != "deepseek-chat" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.Timeout != 90*time.Second {
		t.Errorf("Timeout = %v", cfg.Timeout)
	}
	if cfg.APIKey != "k-1" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
}

// TestConfigFromEnvFileWinsOverEnv：文件优先，且内容的首尾空白（常见是
// `echo key > file` 留下的换行）被裁掉。
func TestConfigFromEnvFileWinsOverEnv(t *testing.T) {
	clearLLMEnv(t)
	path := writeKeyFile(t, "  file-key\n\n")
	t.Setenv(envKeyFile, path)
	t.Setenv(envAPIKey, "env-key-should-lose")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.APIKey != "file-key" {
		t.Fatalf("APIKey = %q，期望 file-key（文件优先且裁空白）", cfg.APIKey)
	}
}

// TestConfigFromEnvFileNotDecoded：文件内容原样使用，**不做 hex / base64 解码**。
// 用纯 ASCII 的 "deadbeef" 是因为它既能被 hex 解码也能被看出没解码。
func TestConfigFromEnvFileNotDecoded(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envKeyFile, writeKeyFile(t, "deadbeef00\n"))

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.APIKey != "deadbeef00" {
		t.Fatalf("APIKey = %q，期望原样 deadbeef00（不得做 hex 解码）", cfg.APIKey)
	}
}

// TestConfigFromEnvMissingKeyIsError：两个来源都没有 key 时报错，
// 且不返回半成品配置——不允许静默降级。
func TestConfigFromEnvMissingKeyIsError(t *testing.T) {
	clearLLMEnv(t)

	cfg, err := ConfigFromEnv()
	if err == nil {
		t.Fatalf("缺 key 必须报错，实际返回 %+v", cfg)
	}
	if cfg != (Config{}) {
		t.Errorf("出错时应返回零值 Config，实际 %+v", cfg)
	}
	if !strings.Contains(err.Error(), envAPIKey) {
		t.Errorf("错误信息应提示 %s，实际：%v", envAPIKey, err)
	}
}

// TestConfigFromEnvUnreadableKeyFileIsError：配了 key 文件但读不到时报错，
// **不回落到环境变量**——否则配置错误会伪装成能跑。
func TestConfigFromEnvUnreadableKeyFileIsError(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envKeyFile, filepath.Join(t.TempDir(), "absent"))
	t.Setenv(envAPIKey, "env-key")

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("key 文件读不到必须报错，不得回落到环境变量")
	} else if !strings.Contains(err.Error(), envKeyFile) {
		t.Errorf("错误信息应提示 %s，实际：%v", envKeyFile, err)
	}
}

// TestConfigFromEnvBlankKeyFileIsError：文件存在但只有空白，等同于没有 key。
func TestConfigFromEnvBlankKeyFileIsError(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envKeyFile, writeKeyFile(t, " \n\t\n"))
	t.Setenv(envAPIKey, "env-key")

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("key 文件只有空白必须报错")
	}
}

// TestConfigFromEnvInvalidTimeoutIsError：非法或非正的时长都报错，
// 不允许把「没有超时」的客户端带上线。
func TestConfigFromEnvInvalidTimeoutIsError(t *testing.T) {
	bad := map[string]string{
		"缺单位": "60",
		"非数字": "abc",
		"零":   "0s",
		"负数":  "-5s",
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			clearLLMEnv(t)
			t.Setenv(envAPIKey, "k")
			t.Setenv(envTimeout, raw)

			if _, err := ConfigFromEnv(); err == nil {
				t.Fatalf("TENON_LLM_TIMEOUT=%q 必须报错", raw)
			}
		})
	}
}

// TestConfigFromEnvBlankTimeoutUsesDefault：变量存在但只有空白，视为没配，
// 走缺省 60s——不把「空变量」误判成配置错误。
func TestConfigFromEnvBlankTimeoutUsesDefault(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envAPIKey, "k")
	t.Setenv(envTimeout, "   ")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("空白超时应视为未配置，实际报错：%v", err)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Fatalf("Timeout = %v，期望缺省 %v", cfg.Timeout, DefaultTimeout)
	}
}
