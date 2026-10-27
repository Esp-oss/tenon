package controlplane

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 未配 TENON_UI_DIR 必须是 503：这是可诊断的部署问题，
// 伪装成 404 会让运维去查路由，查错方向（static.go 的注释）。
func TestStaticUnavailableWithoutUIDir(t *testing.T) {
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未配 UI 目录应 503，得到 %d", resp.StatusCode)
	}
}

// 配了目录但目录不存在，同样是 503（部署事故要能区分「忘配」与「忘建」）。
func TestStaticUnavailableWhenDirMissing(t *testing.T) {
	s := newTestServer(t, Config{
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		UIDir:     filepath.Join(t.TempDir(), "no-such-ui"),
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("目录不存在应 503，得到 %d", resp.StatusCode)
	}
}

// 目录里的真实文件要能取到；`/` 映射到 index.html（uiPath 的约定）。
func TestStaticServesIndexAndAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>t</title>"), 0o644); err != nil {
		t.Fatalf("写 index.html 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatalf("写 app.js 失败：%v", err)
	}
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"), UIDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/ui/app.js"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s 失败：%v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 应 200，得到 %d", path, resp.StatusCode)
		}
	}
}

// 目录穿越必须被拒（filepath.IsLocal 那条规则）：读到 uiDir 之外等于把仓库里
// 任意文件挂到公网。
func TestStaticRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o644); err != nil {
		t.Skipf("上级目录不可写，跳过越界用例：%v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"), UIDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/ui/../secret.txt", nil)
	if err != nil {
		t.Fatalf("造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET 越界路径失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("越界路径应 404（不是 200、也不是 500），得到 %d", resp.StatusCode)
	}
}

// 只接 GET/HEAD：其它方法 405，不把方法错误降级成 404。
func TestStaticRejectsNonGet(t *testing.T) {
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"), UIDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST / 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / 应 405，得到 %d", resp.StatusCode)
	}
}
