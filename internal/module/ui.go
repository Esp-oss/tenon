package module

import (
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"tenon.dev/internal/protocol"
)

// EnvModuleUIDir 指定模块自带的业务页面目录。
//
// 未设置时按 ui/<moduleID> 找（相对进程工作目录）；两处都没有就是「页面没配」，
// 按 503 明说原因——把它伪装成 404 会让运维去查路由，方向就错了
// （与宿主侧 GET / 的口径一致，interfaces.md §4）。
const EnvModuleUIDir = "TENON_MODULE_UI_DIR"

// uiRoot 解析出这个模块的页面目录。
func (m *Module) uiRoot() string {
	if dir := strings.TrimSpace(os.Getenv(EnvModuleUIDir)); dir != "" {
		return dir
	}
	return filepath.Join("ui", m.id())
}

// serveUI 提供「业务系统页面」：只读视图 + 走去 agent 的动作入口。
//
// 为什么页面挂在模块上而不是宿主上：页面读的是**本模块**的状态，而模块的状态只通过
// 动作协议暴露。放在宿主里就得让宿主替页面转发动作调用——那等于在协议之外又开一条
// 读路径，页面显示的值与 agent 读到的值会漂移。
//
// 页面不参与 Tenon-Version 协商，也不要求 token：它是人看的静态资源。所有**写操作**
// 仍然只能由宿主签票后执行，页面自己没有任何写权限。
func (h *handler) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}

	root := h.m.uiRoot()
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		writeTenonErrorStatus(w, http.StatusServiceUnavailable,
			protocol.NewError(protocol.ClassRetryable, "UI_NOT_CONFIGURED",
				"模块页面目录不可用（"+EnvModuleUIDir+" 未设置或目录不存在）："+root))
		return
	}

	rel := strings.TrimPrefix(r.URL.Path, "/ui/")
	if r.URL.Path == "/" {
		rel = "index.html"
	}
	// 先 Clean 再拼：阻止 /ui/../../etc/passwd 这类穿越。Clean 之后仍以 .. 开头的
	// 一律拒绝（Clean 会把 "a/../../b" 收敛成 "../b"，这里就是最后一道闸）。
	clean := path.Clean("/" + rel)
	if strings.HasPrefix(clean, "/..") {
		writeTenonErrorStatus(w, http.StatusNotFound,
			protocol.NewError(protocol.ClassContractViolation, "UNKNOWN_ENDPOINT", "模块页面不存在"))
		return
	}
	full := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))

	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		writeTenonErrorStatus(w, http.StatusNotFound,
			protocol.NewError(protocol.ClassContractViolation, "UNKNOWN_ENDPOINT", "模块页面不存在"))
		return
	}

	f, err := os.Open(full)
	if err != nil {
		writeTenonErrorStatus(w, http.StatusNotFound,
			protocol.NewError(protocol.ClassContractViolation, "UNKNOWN_ENDPOINT", "模块页面不存在"))
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", contentTypeOf(full))
	// 页面是演示用的活数据视图：不让浏览器拿旧版本（改了页面却看不到变化最难排查）。
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.Copy(w, f)
}

func contentTypeOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}
