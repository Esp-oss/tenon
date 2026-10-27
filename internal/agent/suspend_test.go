package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// 导出的 SelfCheck 是 cmd/agent 的入口：它必须把 Corrupt 原样带出来——坏文件对
// List() 不可见，这是「盘上有读不了的 pending」唯一的告警出口。
func TestSelfCheckExported(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	// 这一份是审计已收尾的活条目：自检不该碰 probe，Kept 才是它该去的地方。
	p.AuditSettled = true
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "pending", "pln_broken.json"), []byte("{半截"), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}

	rep, err := SelfCheck(context.Background(), store, auditProbeFound{})
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	// 调用方读的就是这些字段（类型本身未导出，字段全导出）。
	if rep.Corrupt != 1 {
		t.Fatalf("Corrupt 必须能被调用方读到（坏文件唯一的告警出口），得到 %d（%+v）", rep.Corrupt, rep)
	}
	if rep.Kept != 1 {
		t.Fatalf("未过期的活条目应原样保留，得到 %d（%+v）", rep.Kept, rep)
	}
	if rep.Stale+rep.Expired+rep.Settled != 0 {
		t.Fatalf("这份 pending 不该被归档/补标记，得到 %+v", rep)
	}
	// 时间由包装自己取（当前时刻）：夹具的 ExpiresAt 是远期常量，所以它不会被判过期。
	if _, ok, _ := store.Load(p.PlanID); !ok {
		t.Fatal("Kept 的 pending 必须还在盘上")
	}
}

// auditStub 是审计查询的桩宿主：按 since/limit 分页（**故意每页只给 perPage 条**），
// 并且可注入非 200。
//
// 每页条数由桩控制（而不是造 500+ 条记录）：probe 只认 nextCursor，桩逼出「目标在
// 第二页之后」的真实路径就够——那正是「不许拿第一页当整条链」的回归点。
type auditStub struct {
	mu      sync.Mutex
	ids     []string
	perPage int
	status  int
	calls   int
}

func newAuditStub(t *testing.T, ids []string, perPage, status int) (*auditStub, *httptest.Server) {
	t.Helper()
	s := &auditStub{ids: ids, perPage: perPage, status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tenon/session":
			// HostClient 第一次查询前必然先登录（token 只活在内存里）。
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-audit"})
			return
		case "/tenon/audit":
		default:
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()

		if s.status != http.StatusOK {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(`{"tenonError":{"class":"retryable","code":"AUDIT_READ_FAILED","message":"注入的失败"}}`))
			return
		}
		since, err := strconv.Atoi(r.URL.Query().Get("since"))
		if err != nil || since < 0 || since > len(s.ids) {
			since = len(s.ids)
		}
		end := since + s.perPage
		if s.perPage <= 0 || end > len(s.ids) {
			end = len(s.ids)
		}
		records := make([]map[string]any, 0, end-since)
		for _, id := range s.ids[since:end] {
			records = append(records, map[string]any{"auditId": id, "stage": "dispatched"})
		}
		// nextCursor 的语义与宿主 serveAuditQuery 一致：本页扫到的**写入顺序位置**。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"records":    records,
			"nextCursor": strconv.Itoa(end),
		})
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *auditStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// 审计探测：GET /tenon/audit 的真实参数只有 planId / since / limit（没有 auditId），
// 所以「在不在」只能翻页比对——找到 true、翻到底没有 false、非 200 是 error。
func TestHostAuditProbe(t *testing.T) {
	t.Run("找到", func(t *testing.T) {
		stub, srv := newAuditStub(t, []string{"aud_a", "aud_b", "aud_c"}, 1, http.StatusOK)
		probe := HostAuditProbe(NewHostClient(srv.URL, "agent", "pw", http.DefaultClient))

		found, err := probe.Has(context.Background(), "aud_c")
		if err != nil || !found {
			t.Fatalf("aud_c 在链上，应返回 true：found=%v err=%v", found, err)
		}
		if got := stub.callCount(); got < 3 {
			t.Fatalf("目标在第三页，必须翻页才找得到——只查了 %d 次，说明把第一页当成整条链了", got)
		}
	})

	t.Run("不存在", func(t *testing.T) {
		stub, srv := newAuditStub(t, []string{"aud_a", "aud_b"}, 1, http.StatusOK)
		probe := HostAuditProbe(NewHostClient(srv.URL, "agent", "pw", http.DefaultClient))

		found, err := probe.Has(context.Background(), "aud_missing")
		if err != nil {
			t.Fatalf("翻到底之后「没有」不是错误：%v", err)
		}
		if found {
			t.Fatal("链上没有 aud_missing，不许报 true")
		}
		// 两页 + 一页空结果收尾：翻到游标不再前进才算答完。
		if got := stub.callCount(); got < 3 {
			t.Fatalf("必须翻到游标不前进才回答 false，只查了 %d 次", got)
		}
	})

	t.Run("非 200 是错误", func(t *testing.T) {
		stub, srv := newAuditStub(t, nil, 1, http.StatusServiceUnavailable)
		probe := HostAuditProbe(NewHostClient(srv.URL, "agent", "pw", http.DefaultClient))

		found, err := probe.Has(context.Background(), "aud_a")
		if err == nil {
			t.Fatalf("非 200 必须报错（「查不到」不等于「不存在」），found=%v calls=%d", found, stub.callCount())
		}
		if !strings.Contains(err.Error(), "503") {
			t.Fatalf("错误里要带上状态码，得到 %v", err)
		}
	})

	t.Run("没有宿主客户端", func(t *testing.T) {
		if _, err := HostAuditProbe(nil).Has(context.Background(), "aud_a"); err == nil {
			t.Fatal("没有 HostClient 必须报错，不能静默答 false")
		}
	})
}
