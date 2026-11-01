package module

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenon.dev/internal/protocol"
)

// uiOnlyModule 是一个「只有页面、没有动作」的模块：Validate 要求 Handle 非 nil，
// 所以给一个不会被调用的空实现——这几条用例测的是路由与静态服务，不是业务动作。
func uiOnlyModule() *Module {
	return &Module{
		ID:      "inventory",
		Title:   "库存",
		Version: "0.1.0",
		// Validate 还要求至少声明一个动作：给一个最小只读动作，页面用例不会调到它。
		Actions: []Action{{
			ID:           "inventory.Ping",
			Version:      "0.1.0",
			Title:        "探针",
			Kind:         "read",
			InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Risk:         Risk{Level: "low", Reversible: true},
			TimeoutMs:    1000,
		}},
		Handle: func(context.Context, string, json.RawMessage) (json.RawMessage, *protocol.Error) {
			return nil, nil
		},
	}
}

// uiTestModule 起一个只有页面、没有动作的模块，用于测 / 与 /ui/ 的路由。
func uiTestModule(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	if dir != "" {
		t.Setenv(EnvModuleUIDir, dir)
	}
	mod := uiOnlyModule()
	if err := mod.Validate(); err != nil {
		t.Fatalf("模块定义不合法：%v", err)
	}
	srv := httptest.NewServer(mod.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// 页面目录没配时必须 503 并点名环境变量：把它伪装成 404 会让运维去查路由，方向就错了。
func TestUIServes503WhenDirMissing(t *testing.T) {
	srv := uiTestModule(t, filepath.Join(t.TempDir(), "nope"))
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("目录不可用时应 503，实际 %d", res.StatusCode)
	}
}

// 配好目录后 / 返回 index.html，/ui/x 返回目录内的同源资源。
func TestUIServesIndexAndAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>库存台</h1>"), 0o600); err != nil {
		t.Fatalf("写 index 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("写脚本失败：%v", err)
	}
	srv := uiTestModule(t, dir)

	for _, c := range []struct{ path, want string }{
		{"/", "<h1>库存台</h1>"},
		{"/ui/app.js", "console.log(1)"},
	} {
		res, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("%s 请求失败：%v", c.path, err)
		}
		buf := make([]byte, 128)
		n, _ := res.Body.Read(buf)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s 应 200，实际 %d", c.path, res.StatusCode)
		}
		if got := string(buf[:n]); got != c.want {
			t.Fatalf("%s 返回 %q，期望 %q", c.path, got, c.want)
		}
	}
}

// 页面只读：非 GET/HEAD 一律 405，且要带 Allow（与协议端点同一口径）。
func TestUIRejectsNonReadMethods(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写 index 失败：%v", err)
	}
	srv := uiTestModule(t, dir)

	res, err := http.Post(srv.URL+"/", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / 应 405，实际 %d", res.StatusCode)
	}
	if allow := res.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("405 必须带 Allow，实际 %q", allow)
	}
}

// 路径穿越必须挡在目录之外：/ui/../ 之类的请求不能读到页面根以外的文件。
func TestUIBlocksTraversal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写 index 失败：%v", err)
	}
	// 页面根之外放一个「秘密」文件：穿越成功就会读到它。
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("写秘密文件失败：%v", err)
	}
	srv := uiTestModule(t, dir)

	// Go 的 http 客户端会先把 ../ 规范化掉，所以直接打到 handler 上构造原始路径。
	req := httptest.NewRequest(http.MethodGet, "/ui/../secret.txt", nil)
	rec := httptest.NewRecorder()
	handlerOf(t, dir).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("路径穿越读到了目录外的文件：%s", rec.Body.String())
	}
	_ = srv
}

// handlerOf 造一个绑定了指定页面目录的模块 handler。
func handlerOf(t *testing.T, dir string) http.Handler {
	t.Helper()
	t.Setenv(EnvModuleUIDir, dir)
	mod := uiOnlyModule()
	if err := mod.Validate(); err != nil {
		t.Fatalf("模块定义不合法：%v", err)
	}
	return mod.Handler()
}
