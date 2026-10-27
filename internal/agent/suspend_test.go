package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeAuditHost 只实现 POST /tenon/audit，可注入失败。
func fakeAuditHost(t *testing.T, failAt int) *httptest.Server {
	t.Helper()
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tenon/audit" {
			calls++
			if failAt > 0 && calls == failAt {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
}

func newSuspendFixture(t *testing.T, hostURL string) (*StateStore, Pending, Deps) {
	t.Helper()
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	deps := Deps{Host: hostURL, AgentID: "ag_test", OnBehalfOf: "user:alice", HTTP: http.DefaultClient}
	return store, samplePending(), deps
}

// I7 的三步全成功：pending 在盘、审计已写、标记已回写。
func TestSuspendWritesPendingThenAuditThenFlag(t *testing.T) {
	srv := fakeAuditHost(t, 0)
	defer srv.Close()
	store, p, deps := newSuspendFixture(t, srv.URL)

	if err := suspend(context.Background(), store, deps, p); err != nil {
		t.Fatalf("挂起失败：%v", err)
	}
	got, ok, _ := store.Load(p.PlanID)
	if !ok {
		t.Fatalf("pending 未落盘")
	}
	if !got.AuditSettled || got.DispatchedAuditID == "" {
		t.Fatalf("标记或审计 id 缺失：%+v", got)
	}
}

// 审计写失败 → 整个挂起失败，且**不留半截 pending**。
func TestSuspendFailsClosedWhenAuditFails(t *testing.T) {
	srv := fakeAuditHost(t, 1)
	defer srv.Close()
	store, p, deps := newSuspendFixture(t, srv.URL)

	if err := suspend(context.Background(), store, deps, p); err == nil {
		t.Fatalf("审计失败时挂起必须报错")
	}
	if _, ok, _ := store.Load(p.PlanID); ok {
		t.Fatalf("审计失败后不许留 pending")
	}
}

// 崩溃分流：pending 在、审计链查不到 → 归档 stale，绝不执行。
func TestSelfCheckArchivesStaleWhenAuditMissing(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	p.AuditSettled = false
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	// 固定时间：夹具的 ExpiresAt 是会到期的常量，用 time.Now() 会让用例过几小时自己变红。
	fixedNow, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	rep, err := selfCheck(context.Background(), store, auditProbeNotFound{})(fixedNow)
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	if rep.Stale != 1 {
		t.Fatalf("应归档 1 份 stale，得到 %+v", rep)
	}
	if _, ok, _ := store.Load(p.PlanID); ok {
		t.Fatalf("stale 之后不该还在 pending/")
	}
}

// 崩溃分流：审计查得到 → 补标记，继续等审批。
func TestSelfCheckSettlesAuditWhenPresent(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	p.AuditSettled = false
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	fixedNow, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	rep, err := selfCheck(context.Background(), store, auditProbeFound{})(fixedNow)
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	if rep.Settled != 1 {
		t.Fatalf("应补标记 1 份，得到 %+v", rep)
	}
	got, ok, _ := store.Load(p.PlanID)
	if !ok || !got.AuditSettled {
		t.Fatalf("标记未补上：%+v ok=%v", got, ok)
	}
}

// 坏文件必须被看见：List() 会跳过它，自检要单独把它数出来（只告警）。
func TestSelfCheckCountsCorruptFiles(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "pending", "pln_broken.json"), []byte("{半截"), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}
	fixedNow, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	rep, err := selfCheck(context.Background(), store, auditProbeFound{})(fixedNow)
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	if rep.Corrupt != 1 {
		t.Fatalf("应报 1 个坏文件，得到 %+v", rep)
	}
}
