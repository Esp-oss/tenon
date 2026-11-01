package module

import (
	"net/http"
	"strconv"

	"tenon.dev/internal/protocol"
)

// serveAuditLocal 返回本模块的审计链片段，供宿主日切对账。
//
// 只读、只给本模块的记录。它的价值不在"模块说自己做过什么"——那本来就由模块
// 控制——而在于**它与宿主那一份不能同时被篡改**：要让两边同时说谎，
// 得同时进两个进程的磁盘。
func (h *handler) serveAuditLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if perr := checkVersion(r, false); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	limit := defaultEventPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > maxEventPage {
		limit = maxEventPage
	}
	// 与写入共用 auditMu：读的时候不能有半条记录正在追加，
	// 否则对账会看到一个"看起来链断"的中间态。
	h.m.auditMu.Lock()
	defer h.m.auditMu.Unlock()
	status, err := h.m.readChain(parseSince(r.URL.Query().Get("since")), limit)
	if err != nil {
		writeTenonError(w, protocol.NewError(protocol.ClassRetryable, "AUDIT_CHAIN_BROKEN",
			"模块侧审计链校验失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, status)
}
