package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// origin 凭证是「事件 id → 这次请求是谁说的」的凭据，只在计划生命周期内被查。
// 过期的必须修剪，否则每说一句话就多一条，长期运行会线性增长。
func TestOriginStoreCompactsExpiredRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests-index.jsonl")
	st, err := newOriginStore(path)
	if err != nil {
		t.Fatalf("打开登记表失败：%v", err)
	}

	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	st.retention = time.Hour

	old := now.Add(-2 * time.Hour).Format(time.RFC3339)
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	for _, rec := range []originRecord{
		{EventID: "evt_old", RequestID: "req_old", By: "user:alice", At: old},
		{EventID: "evt_fresh", RequestID: "req_fresh", By: "user:alice", At: fresh},
	} {
		if err := st.Register(rec); err != nil {
			t.Fatalf("登记失败：%v", err)
		}
	}
	st.compactNow()

	if st.IsRequest("evt_old") {
		t.Fatalf("超过保留期的凭证应被修剪")
	}
	if !st.IsRequest("evt_fresh") {
		t.Fatalf("保留期内的凭证不该丢")
	}

	// 文件也必须被重写：只删内存等于下次重启又把旧账读回来。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读登记表失败：%v", err)
	}
	if strings.Contains(string(raw), "evt_old") {
		t.Fatalf("修剪后文件里不该还有 evt_old：%s", raw)
	}
	if !strings.Contains(string(raw), "evt_fresh") {
		t.Fatalf("文件里应保留 evt_fresh：%s", raw)
	}

	// 重启后结论一致。
	again, err := newOriginStore(path)
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	if again.IsRequest("evt_old") || !again.IsRequest("evt_fresh") {
		t.Fatalf("重启后登记表与文件不一致：old=%v fresh=%v",
			again.IsRequest("evt_old"), again.IsRequest("evt_fresh"))
	}
}

// 超期但**仍挂在公告板上**的请求，凭证一条都不能丢：
// 丢了会让续跑时的策略把 request 判成 event，绕过「对话发起必须审批」。
func TestOriginStoreKeepsProtectedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests-index.jsonl")
	st, err := newOriginStore(path)
	if err != nil {
		t.Fatalf("打开登记表失败：%v", err)
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	st.retention = time.Minute
	st.keep = func(rec originRecord) bool { return rec.RequestID == "req_pending" }

	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	for _, rec := range []originRecord{
		{EventID: "evt_pending", RequestID: "req_pending", By: "user:alice", At: old},
		{EventID: "evt_done", RequestID: "req_done", By: "user:alice", At: old},
	} {
		if err := st.Register(rec); err != nil {
			t.Fatalf("登记失败：%v", err)
		}
	}
	st.compactNow()

	if !st.IsRequest("evt_pending") {
		t.Fatalf("被公告板引用的凭证超期也不许丢")
	}
	if st.IsRequest("evt_done") {
		t.Fatalf("未被引用的过期凭证应被修剪")
	}
}

// 硬上限：保留期内被灌爆时，从最旧的未引用记录开始丢，不能让内存无界增长。
func TestOriginStoreCapsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests-index.jsonl")
	st, err := newOriginStore(path)
	if err != nil {
		t.Fatalf("打开登记表失败：%v", err)
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	st.retention = 24 * time.Hour
	st.maxEntries = 2

	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if err := st.Register(originRecord{
			EventID: "evt_" + string(rune('a'+i)), RequestID: "req", By: "user:alice", At: at,
		}); err != nil {
			t.Fatalf("登记失败：%v", err)
		}
	}
	st.compactNow()
	if got := len(st.byEvent); got != 2 {
		t.Fatalf("上限 2 时应只保留 2 条，得到 %d", got)
	}
	if st.IsRequest("evt_a") || st.IsRequest("evt_b") {
		t.Fatalf("应丢最旧的两条，实际保留了：%v", st.byEvent)
	}
	if !st.IsRequest("evt_c") || !st.IsRequest("evt_d") {
		t.Fatalf("最新的两条应保留：%v", st.byEvent)
	}
}

// 公告板的 referencesRequest 是上面那个 keep 回调的实现：同一 requestId 才算引用。
func TestPendingBoardReferencesRequest(t *testing.T) {
	b := newPendingBoard()
	req := "req_abc"
	b.Upsert(pendingItem{PlanID: "pln_1", RequestID: req, State: pendingStateAwaitingApproval})
	if !b.referencesRequest(req) {
		t.Fatalf("公告板上的 requestId 应被判为引用")
	}
	if b.referencesRequest("req_other") || b.referencesRequest("") {
		t.Fatalf("只有完全相同的 requestId 才算引用（空串永远不算）")
	}
}
