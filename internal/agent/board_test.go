package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 公告板用例的时间基准：远期常量（§0.1.1）。
//
// 绝不用 time.Now() 造夹具，也绝不用 2026-10-27 那种会自然到期的时刻——host 的
// pendingBoard.List 会把过期条目直接剔掉，夹具一过期，用例就自己变红了。
const boardExpiresFuture = "2099-01-01T00:00:00Z"

// boardHost 是公告板用例的桩宿主：记下 POST /tenon/pending 的 payload，也记下
// **所有**被访问过的路径——后者用来证明 finishTerminal 没有顺手去写 result 段。
type boardHost struct {
	mu     sync.Mutex
	pushes []map[string]any
	paths  []string
}

func newBoardHost(t *testing.T) (*boardHost, *httptest.Server) {
	t.Helper()
	h := &boardHost{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.URL.Path)
		h.mu.Unlock()
		switch r.URL.Path {
		case "/tenon/session":
			// HostClient 第一次推送前必然先登录：没有这一条，push 会死在登录上，
			// 用例看起来像「没推」而不是「推失败」。
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "tok-board", "user": "agent:test", "roles": []string{"role:agent"},
			})
		case "/tenon/pending":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.mu.Lock()
			h.pushes = append(h.pushes, body)
			h.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *boardHost) takePushes() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.pushes...)
}

func (h *boardHost) takePaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

// 归档终态时推一条 state=终态 的公告板覆盖：否则已执行的单会在板上挂到过期。
func TestFinishTerminalPushesOverride(t *testing.T) {
	host, srv := newBoardHost(t)
	client := NewHostClient(srv.URL, "agent", "pw", http.DefaultClient)

	p := samplePending()
	if err := finishTerminal(context.Background(), client, p, StateDone); err != nil {
		t.Fatalf("推终态失败：%v", err)
	}

	pushes := host.takePushes()
	if len(pushes) != 1 {
		t.Fatalf("应推 1 条 state=done 的覆盖：%+v", pushes)
	}
	got := pushes[0]
	if got["state"] != string(StateDone) {
		t.Fatalf("state 应为 %q，得到 %+v", StateDone, got)
	}
	if got["planId"] != p.PlanID {
		t.Fatalf("planId 不对：%+v", got)
	}
	// 合法且在未来：原样透传（同一份 pending 每 tick 推出同一字节，upsert 幂等）。
	if got["expiresAt"] != boardExpiresFuture {
		t.Fatalf("expiresAt 应原样透传 %q，得到 %+v", boardExpiresFuture, got["expiresAt"])
	}
	// payload 必须映射成 host 的 pendingItem：明细字段叫 items，不是 labels。
	if _, ok := got["labels"]; ok {
		t.Fatalf("不许推 labels：host 的 pendingItem 只认 items，推 labels 等于一张没有明细的卡：%+v", got)
	}
	items, _ := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items 应有 1 行明细：%+v", got)
	}
	if row, _ := items[0].(map[string]any); row["label"] != p.Labels[0] {
		t.Fatalf("明细内容不对：%+v", items[0])
	}
	if got["by"] != p.By {
		t.Fatalf("by 应透传（host 会用 token 的 sub 覆写，但它仍是展示字段）：%+v", got["by"])
	}
	if roles, _ := got["requiredRoles"].([]any); len(roles) != 1 || roles[0] != p.RequiredRoles[0] {
		t.Fatalf("requiredRoles 不对：%+v", got["requiredRoles"])
	}
	if rid, _ := got["requestId"].(string); rid != "req_1" {
		t.Fatalf("requestId 应透传：%+v", got["requestId"])
	}

	// **最关键的一条**：绝不写 result 段。计划级 result 在归档之前就由 resume.go 写好了
	// （auditId 复用挂起时那条 dispatched），这里再写一次就会让同一 auditId 出现两条 result。
	for _, path := range host.takePaths() {
		if path == "/tenon/audit" {
			t.Fatalf("finishTerminal 不许碰审计（result 段已由 resume.go 写过）：%v", host.takePaths())
		}
	}
}

// PushTerminal 是给 cmd/agent 的导出出口（finishTerminal 未导出、Resume 又拿不到凭证）：
// 它必须**真的 POST** 一条 state=done 的覆盖，而不是「等价于推过了」。
func TestPushTerminal(t *testing.T) {
	host, srv := newBoardHost(t)
	client := NewHostClient(srv.URL, "agent", "pw", http.DefaultClient)
	p := samplePending()

	if err := PushTerminal(context.Background(), client, p, StateDone); err != nil {
		t.Fatalf("推终态失败：%v", err)
	}

	pushes := host.takePushes()
	if len(pushes) != 1 {
		t.Fatalf("应 POST 1 条公告板覆盖：%+v", pushes)
	}
	if got := pushes[0]["state"]; got != string(StateDone) {
		t.Fatalf("state 必须是 %q（否则卡片会继续冒充「等人批」），得到 %+v", StateDone, got)
	}
	if pushes[0]["planId"] != p.PlanID {
		t.Fatalf("覆盖必须带同一个 planId（公告板按它 upsert）：%+v", pushes[0])
	}
	// 导出包装不是另一个实现：仍然只推公告板，绝不写 result 段（那会让同一 auditId
	// 出现两条 result）。
	for _, path := range host.takePaths() {
		if path == "/tenon/audit" {
			t.Fatalf("PushTerminal 不许碰审计（result 段已由 resume.go 写过）：%v", host.takePaths())
		}
	}
}

// 重推现存 pending：幂等 upsert，payload 带 state=awaiting_approval。
func TestRepushPendingBoard(t *testing.T) {
	host, srv := newBoardHost(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	p := samplePending()
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	n, err := RepushBoard(context.Background(), NewHostClient(srv.URL, "agent", "pw", http.DefaultClient), store)
	if err != nil || n != 1 {
		t.Fatalf("应推 1 条：n=%d err=%v", n, err)
	}
	pushes := host.takePushes()
	if len(pushes) != 1 {
		t.Fatalf("推送条数不对：%+v", pushes)
	}
	if pushes[0]["state"] != string(StateAwaitingApproval) {
		t.Fatalf("state 不对：%+v", pushes[0])
	}
	// expiresAt 必须是合法 RFC3339 且**晚于当前时刻**：host 的 List 会把过期条目直接剔除，
	// 推一个已经过去的时刻等于推了但立刻看不见。
	exp, perr := time.Parse(time.RFC3339, toString(pushes[0]["expiresAt"]))
	if perr != nil {
		t.Fatalf("expiresAt 不是 RFC3339：%v（%+v）", perr, pushes[0]["expiresAt"])
	}
	if !exp.After(time.Now().UTC()) {
		t.Fatalf("expiresAt 必须晚于当前时刻，得到 %s", exp)
	}
	// 推的是「现存的 pending」原样：不算、不改写、不归档。
	if _, ok, _ := store.Load(p.PlanID); !ok {
		t.Fatalf("重推不该动 pending 文件")
	}
}

// ① Archive 的两步之间失败会留同名双份（归档件已是 done，pending 件还在）。
// 重推会让板上挂一张**早已处理完的卡**，所以必须跳过，并顺手清掉残留。
func TestRepushPendingBoardSkipsArchivedTerminal(t *testing.T) {
	host, srv := newBoardHost(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	p := samplePending()
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := store.Archive(p.PlanID, StateDone); err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	// 手工复现崩溃现场：把 pending 件写回来（步骤 ① 写完归档件、② 删 pending 件之前挂掉）。
	if err := store.Save(p); err != nil {
		t.Fatalf("造残留失败：%v", err)
	}
	if _, ok, _ := store.Load(p.PlanID); !ok {
		t.Fatalf("前置条件不成立：残留 pending 没造出来")
	}

	n, err := RepushBoard(context.Background(), NewHostClient(srv.URL, "agent", "pw", http.DefaultClient), store)
	if err != nil || n != 0 {
		t.Fatalf("归档里已有 done，这个 planId 不许重推：n=%d err=%v", n, err)
	}
	if pushes := host.takePushes(); len(pushes) != 0 {
		t.Fatalf("不许推任何条目：%+v", pushes)
	}
	if _, ok, _ := store.Load(p.PlanID); ok {
		t.Fatalf("残留的 pending 件必须被清掉（否则每 tick 都要再看一遍这张已完成的卡）")
	}
	archived, aerr := store.LoadArchived(p.PlanID)
	if aerr != nil || archived.State != StateDone {
		t.Fatalf("归档件不该被动过：%+v err=%v", archived, aerr)
	}
}

// ② state=resuming 是崩溃标记（confirm 已被接受过）：按待批重推等于让人再批一次。
func TestRepushPendingBoardSkipsResuming(t *testing.T) {
	host, srv := newBoardHost(t)
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	p := samplePending()
	p.State = StateResuming
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	n, err := RepushBoard(context.Background(), NewHostClient(srv.URL, "agent", "pw", http.DefaultClient), store)
	if err != nil || n != 0 {
		t.Fatalf("resuming 不许重推：n=%d err=%v", n, err)
	}
	if pushes := host.takePushes(); len(pushes) != 0 {
		t.Fatalf("不许推任何条目：%+v", pushes)
	}
	// 也不许顺手归档/改写它：恢复路径要靠这个标记认出「这条 confirm 已被接受过」。
	got, ok, _ := store.Load(p.PlanID)
	if !ok || got.State != StateResuming {
		t.Fatalf("resuming 标记必须原样留着：ok=%v state=%q", ok, got.State)
	}
}

// expiresAt 缺失/非法时绝不能透传：host 对**解析不了**的条目不是剔除而是永久保留，
// 推一个空串等于推一张永不消失的卡。
func TestBoardPayloadFixesUnparsableExpiry(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	for _, raw := range []string{"", "not-a-time", "2026-10-27 09:30:03"} {
		p := samplePending()
		p.ExpiresAt = raw
		got := boardPayloadAt(p, StateAwaitingApproval, now)
		exp, err := time.Parse(time.RFC3339, toString(got["expiresAt"]))
		if err != nil {
			t.Fatalf("expiresAt %q 必须换成合法 RFC3339，得到 %v", raw, got["expiresAt"])
		}
		if want := now.Add(pendingTTL); !exp.Equal(want) {
			t.Fatalf("兜底窗口应为 now+pendingTTL（%s），得到 %s", want, exp)
		}
	}
}

// toString 把 payload 里的值读成字符串（map[string]any 里是 any）。
func toString(v any) string {
	s, _ := v.(string)
	return s
}
