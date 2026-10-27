package controlplane

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// uiPrefix 是静态资源在 URL 上的前缀：页面本体走 `/`，它引用的资源走 `/ui/…`。
const uiPrefix = "/ui/"

// serveStatic 只服务 `GET /` 与其下的静态资源（`/ui/…`），页面内容留给 P3。
//
// 三处刻意的判断：
//   - 未配置 UI 目录（TENON_UI_DIR 为空）或该目录不存在 → 503 并写明原因。
//     「页面没配」是可诊断的部署问题，伪装成 404 会让运维去查路由，查错了方向。
//   - 已配置的目录里确实没有这个文件 → 404：这才是真正的「路径不存在」。
//   - 只接 GET/HEAD，其余方法 405（与其它端点一致，不把方法错误降级成 404）。
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}
	if strings.TrimSpace(s.uiDir) == "" {
		http.Error(w, "未配置 UI 目录（TENON_UI_DIR）", http.StatusServiceUnavailable)
		return
	}
	// 先判目录再判文件：目录忘了建与页面忘了放是两种部署事故，
	// 混成同一个 404 就分不出该修哪一边。
	info, err := os.Stat(s.uiDir)
	if err != nil {
		http.Error(w, "UI 目录不可用（TENON_UI_DIR="+s.uiDir+"）："+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !info.IsDir() {
		http.Error(w, "UI 目录不是目录（TENON_UI_DIR="+s.uiDir+"）", http.StatusServiceUnavailable)
		return
	}
	clean, ok := uiPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.uiDir, clean)
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

// uiPath 把请求路径映射成 uiDir 下的相对路径；第二个返回值为 false 表示越界。
//
// 这里多做一步 Clean 之外的事是**防目录穿越**：filepath.Clean 只做归一化，
// `/ui/../secret` 归一化后仍是 `../secret`，直接拼进目录就读到了 uiDir 之外。
// filepath.IsLocal 恰好是这条规则——绝对路径、盘符路径、含 `..` 逃出当前目录的路径
// 一律拒绝——所以只放行 uiDir 之内的相对路径。
func uiPath(urlPath string) (string, bool) {
	rel := urlPath
	if strings.HasPrefix(rel, uiPrefix) {
		// 剥掉 `/ui/` 前缀再拼目录：URL 上的前缀是路由用的，不是磁盘上的一层目录。
		rel = strings.TrimPrefix(rel, uiPrefix)
	} else {
		rel = strings.TrimPrefix(rel, "/")
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == string(filepath.Separator) {
		rel = "index.html"
	}
	if !filepath.IsLocal(rel) {
		return "", false
	}
	return rel, true
}
