package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeInboundHost 提供 session / requests poll / ack / pending。
func fakeInboundHost(t *testing.T, frames string) (*httptest.Server, *int32) {
	t.Helper()
	var logins int32
	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/session", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&logins, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "tok-" + "1", "user": "agent:test", "roles": []string{"role:agent"},
		})
	})
	mux.HandleFunc("/tenon/requests/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"tenonError":{"code":"AUTH"}}`))
			return
		}
		_, _ = w.Write([]byte(frames))
	})
	mux.HandleFunc("/tenon/requests/ack", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"acked":1}`))
	})
	mux.HandleFunc("/tenon/pending", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return httptest.NewServer(mux), &logins
}

func TestHostClientLoginThenPoll(t *testing.T) {
	srv, logins := fakeInboundHost(t, `{"frames":[{"seq":1,"frame":{"type":"tenon.request","requestId":"req_1"}}],"nextCursor":1,"dropped":0}`)
	defer srv.Close()
	c := NewHostClient(srv.URL, "agent", "agent-dev", http.DefaultClient)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("登录失败：%v", err)
	}
	frames, next, err := c.PollInbound(context.Background(), 0)
	if err != nil {
		t.Fatalf("poll 失败：%v", err)
	}
	if len(frames) != 1 || next != 1 || frames[0].Seq != 1 {
		t.Fatalf("poll 结果不符：%+v next=%d", frames, next)
	}
	if atomic.LoadInt32(logins) != 1 {
		t.Fatalf("应只登录 1 次，得到 %d", atomic.LoadInt32(logins))
	}
}

// 401 → 重登一次并重试；并发 401 只允许一次登录在飞。
func TestHostClientReLoginsOnUnauthorized(t *testing.T) {
	var logins int32
	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/session", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&logins, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + itoa(int(n))})
	})
	mux.HandleFunc("/tenon/requests/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"frames":[],"nextCursor":0,"dropped":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHostClient(srv.URL, "agent", "agent-dev", http.DefaultClient)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("首次登录失败：%v", err)
	}
	if _, _, err := c.PollInbound(context.Background(), 0); err != nil {
		t.Fatalf("重登后 poll 应成功：%v", err)
	}
	if got := atomic.LoadInt32(&logins); got != 2 {
		t.Fatalf("应登录 2 次（首次 + 一次重登），得到 %d", got)
	}
}

// 一批 401 同时到达时只允许一次重登在飞：其余调用等它，共享同一个新 token。
//
// 计划给的两个用例只覆盖串行（一次 401 → 一次重登）；单飞的价值只在并发时存在，
// 不并发地打一次就等于没验证过它。这里把窗口钉成确定性的：重登请求要等
// 全部 8 个并发调用都撞上 401 之后才应答。
func TestHostClientConcurrentUnauthorizedSingleFlight(t *testing.T) {
	const callers = 8
	var logins, poll401 int32
	all401 := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/session", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&logins, 1)
		if n > 1 {
			// 重登：等到所有并发调用都已拿到 401，再留一点时间让它们就位等待。
			<-all401
			time.Sleep(50 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + itoa(int(n))})
	})
	mux.HandleFunc("/tenon/requests/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			if atomic.AddInt32(&poll401, 1) == callers {
				once.Do(func() { close(all401) })
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"frames":[],"nextCursor":0,"dropped":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHostClient(srv.URL, "agent", "agent-dev", http.DefaultClient)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("首次登录失败：%v", err)
	}

	start := make(chan struct{})
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = c.PollInbound(context.Background(), 0)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发 poll 失败：%v", i, err)
		}
	}
	if got := atomic.LoadInt32(&logins); got != 2 {
		t.Fatalf("一批 401 只该触发 1 次重登（首次登录 + 重登 = 2），得到 %d", got)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
