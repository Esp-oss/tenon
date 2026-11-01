package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenon.dev/internal/controlplane/registrylog"
)

// ids 抽出 ID 序列，便于断言顺序。
func ids(entries []ModuleEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// strictlySorted 判断 ids 是否严格升序，顺带排除了重复。
func strictlySorted(list []string) bool {
	for i := 1; i < len(list); i++ {
		if list[i-1] >= list[i] {
			return false
		}
	}
	return true
}

// TestRegistryUpsertListEnabled 是计划 Task 4 的用例：登记两个、摘除一个之后，
// List 仍看得见两条（条目保留），Enabled 只剩一条（摘除生效）。
func TestRegistryUpsertListEnabled(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})
	r.Upsert(ModuleEntry{ID: "procurement", BaseURL: "http://p:8082"})
	if got := r.List(); len(got) != 2 {
		t.Fatalf("期望 2 个，得到 %d", len(got))
	}

	if err := r.SetEnabled("procurement", false); err != nil {
		t.Fatalf("摘除已登记模块不该报错：%v", err)
	}
	if list := r.List(); len(list) != 2 {
		t.Fatalf("摘除应保留条目但标记 disabled，List 得到 %d 条：%+v", len(list), list)
	}
	enabled := r.Enabled()
	if len(enabled) != 1 || enabled[0].ID != "inventory" {
		t.Fatalf("Enabled() 只应返回 enabled 的模块：%+v", enabled)
	}
	if enabled[0].BaseURL != "http://i:8081" {
		t.Fatalf("Enabled() 的地址不对：%+v", enabled[0])
	}

	// 摘除是可逆的：重新启用后回到 Enabled()。
	if err := r.SetEnabled("procurement", true); err != nil {
		t.Fatalf("重新启用失败：%v", err)
	}
	if got := ids(r.Enabled()); !slices.Equal(got, []string{"inventory", "procurement"}) {
		t.Fatalf("重新启用后 Enabled() = %v", got)
	}
}

// TestUpsertSemantics 固定 Upsert 的两条窄语义：重复登记只换地址、不动开关；
// 首次登记一律 enabled（入参 Enabled 的零值不能被当成「禁用」）。
func TestUpsertSemantics(t *testing.T) {
	t.Run("重复 Upsert 只换地址", func(t *testing.T) {
		r := NewRegistry()
		r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})
		if err := r.SetEnabled("inventory", false); err != nil {
			t.Fatalf("摘除失败：%v", err)
		}
		r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:9091"})

		got := r.List()
		if len(got) != 1 {
			t.Fatalf("重复登记不应产生第二条：%+v", got)
		}
		if got[0].BaseURL != "http://i:9091" {
			t.Fatalf("地址未更新：%q", got[0].BaseURL)
		}
		if got[0].Enabled {
			t.Fatal("重复 Upsert 不应把已摘除的模块重新启用")
		}
	})

	t.Run("首次登记即启用", func(t *testing.T) {
		r := NewRegistry()
		r.Upsert(ModuleEntry{ID: "quote", BaseURL: "http://q:8083", Enabled: false})
		if got := ids(r.Enabled()); !slices.Equal(got, []string{"quote"}) {
			t.Fatalf("首次登记的模块应参与调度，Enabled() = %v", got)
		}
	})

	t.Run("空 id 不入表", func(t *testing.T) {
		r := NewRegistry()
		r.Upsert(ModuleEntry{ID: "", BaseURL: "http://nowhere"})
		r.Upsert(ModuleEntry{BaseURL: "http://nowhere"})
		if got := r.List(); len(got) != 0 {
			t.Fatalf("无名条目不应进表：%+v", got)
		}
	})

	t.Run("零值可用", func(t *testing.T) {
		var r Registry
		r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})
		if got := len(r.List()); got != 1 {
			t.Fatalf("零值 Registry 应可用，List 得到 %d 条", got)
		}
	})
}

// TestSetEnabledUnknownID 锁住「不存在就报错、绝不静默创建」。
func TestSetEnabledUnknownID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{name: "从未登记", id: "procurement"},
		{name: "空 id", id: ""},
		{name: "大小写不同算两个 id", id: "Inventory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})

			err := r.SetEnabled(tc.id, false)
			if err == nil {
				t.Fatalf("SetEnabled(%q) 应报错", tc.id)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("期望 errors.Is(err, ErrNotFound)，得到 %v", err)
			}
			if !strings.Contains(err.Error(), tc.id) && tc.id != "" {
				t.Fatalf("错误信息应指出是哪个 id：%v", err)
			}
			if got := ids(r.List()); !slices.Equal(got, []string{"inventory"}) {
				t.Fatalf("SetEnabled 不得静默创建条目，List = %v", got)
			}
		})
	}

	// 幂等：把已启用的模块再启用一次不算错。
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})
	for i := 0; i < 3; i++ {
		if err := r.SetEnabled("inventory", true); err != nil {
			t.Fatalf("第 %d 次重复启用报错：%v", i+1, err)
		}
	}
	if got := len(r.Enabled()); got != 1 {
		t.Fatalf("重复启用后 Enabled() = %d 条", got)
	}
}

// TestListOrderStableAndSorted 锁住「顺序稳定 + 按 ID 字典序 + 返回副本」。
func TestListOrderStableAndSorted(t *testing.T) {
	r := NewRegistry()
	// 故意乱序登记，让 map 的随机遍历顺序暴露出来。
	for _, id := range []string{"quote", "inventory", "procurement"} {
		r.Upsert(ModuleEntry{ID: id, BaseURL: "http://" + id + ":8081"})
	}
	want := []string{"inventory", "procurement", "quote"}

	first := r.List()
	if got := ids(first); !slices.Equal(got, want) {
		t.Fatalf("List 应按 ID 字典序：%v，期望 %v", got, want)
	}
	second := r.List()
	// ModuleEntry 含 json.RawMessage（slice），不可比较——用「字段逐一比」而不是 ==。
	if len(first) != len(second) {
		t.Fatalf("两次 List 长度不一致：%d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].BaseURL != second[i].BaseURL || first[i].Enabled != second[i].Enabled {
			t.Fatalf("两次 List 结果不一致：%+v vs %+v", first[i], second[i])
		}
	}
	// 摘除一项后剩下的顺序仍稳定（不能依赖插入顺序）。
	if err := r.SetEnabled("inventory", false); err != nil {
		t.Fatalf("摘除失败：%v", err)
	}
	if got := ids(r.Enabled()); !slices.Equal(got, []string{"procurement", "quote"}) {
		t.Fatalf("Enabled() 顺序：%v", got)
	}

	// 返回值是副本：改它不会写穿注册表。
	first[0].BaseURL = "http://tampered"
	if got := r.List()[0].BaseURL; got == "http://tampered" {
		t.Fatalf("List 返回了内部可写引用：%q", got)
	}
}

// TestEmptyRegistryJSON 锁住空注册表序列化成 []（而不是 null）：
// GET /tenon/modules 直接编码 Enabled()，测试面的 admin 端点编码 List()，两个都不能是 null。
func TestEmptyRegistryJSON(t *testing.T) {
	r := NewRegistry()
	for name, entries := range map[string][]ModuleEntry{
		"List":    r.List(),
		"Enabled": r.Enabled(),
	} {
		b, err := json.Marshal(entries)
		if err != nil {
			t.Fatalf("%s 序列化失败：%v", name, err)
		}
		if string(b) != "[]" {
			t.Fatalf("空注册表的 %s 应序列化成 []，得到 %s", name, b)
		}
	}

	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://i:8081"})
	b, err := json.Marshal(r.List())
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	// 线上形状 = id/baseURL/enabled + 存活信息。不逐字比字符串（lastSeen 是动态值），
	// 而是断言「契约字段都在、且空声明的哈希不出现」。
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("响应不是合法 JSON：%s", b)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(got))
	}
	e := got[0]
	if e["id"] != "inventory" || e["baseURL"] != "http://i:8081" || e["enabled"] != true {
		t.Fatalf("契约字段不对：%s", b)
	}
	if e["state"] != "alive" {
		t.Fatalf("注册后 state 应为 alive：%s", b)
	}
	if _, ok := e["declarationHash"]; ok {
		t.Fatalf("空声明哈希不应出现在线上形状里：%s", b)
	}
}

// TestConcurrentUpsertAndList 交给 -race 判定数据竞争。
// 读侧疯狂 List/Enabled，写侧同时新增与切换开关。
func TestConcurrentUpsertAndList(t *testing.T) {
	const (
		writers    = 4
		readers    = 4
		iterations = 100
		seedCount  = 8
	)
	r := NewRegistry()
	for i := 0; i < seedCount; i++ {
		r.Upsert(ModuleEntry{ID: fmt.Sprintf("seed-%02d", i), BaseURL: fmt.Sprintf("http://seed-%02d:8081", i)})
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				r.Upsert(ModuleEntry{ID: fmt.Sprintf("w%d-%03d", w, i), BaseURL: "http://h:8081"})
				if err := r.SetEnabled(fmt.Sprintf("seed-%02d", i%seedCount), (w+i)%2 == 0); err != nil {
					t.Errorf("SetEnabled 失败的种子模块：%v", err)
					return
				}
			}
		}(w)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				// 顺序必须是先 Enabled 后 List：条目只增不删，所以后取的全集
				// 一定包含先取的子集。反过来取则可能漏掉中途新增的模块。
				enabled := r.Enabled()
				list := r.List()
				if !strictlySorted(ids(enabled)) || !strictlySorted(ids(list)) {
					t.Errorf("快照未按 ID 严格升序：enabled=%v list=%v", ids(enabled), ids(list))
					return
				}
				all := make(map[string]struct{}, len(list))
				for _, e := range list {
					all[e.ID] = struct{}{}
				}
				for _, e := range enabled {
					if !e.Enabled {
						t.Errorf("Enabled() 里出现了未启用的条目：%+v", e)
						return
					}
					if _, ok := all[e.ID]; !ok {
						t.Errorf("Enabled() 的 %q 不在 List() 里", e.ID)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	if got, want := len(r.List()), seedCount+writers*iterations; got != want {
		t.Fatalf("并发结束后条目数 = %d，期望 %d", got, want)
	}
}
func TestRegistryLifecycle(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://127.0.0.1:8081", DeclarationHash: "sha256:aaa"})

	if got := len(r.Enabled()); got != 1 {
		t.Fatalf("注册后应可见，得到 %d", got)
	}

	ok, reason := r.Touch("inventory", "sha256:aaa")
	if !ok || reason != "" {
		t.Fatalf("心跳应成功，得到 ok=%v reason=%q", ok, reason)
	}

	ok, reason = r.Touch("inventory", "sha256:bbb")
	if ok || reason != "declaration-changed" {
		t.Fatalf("哈希变化应要求重申报，得到 ok=%v reason=%q", ok, reason)
	}

	ok, reason = r.Touch("ghost", "sha256:aaa")
	if ok || reason != "unknown-module" {
		t.Fatalf("未知模块应要求重申报，得到 ok=%v reason=%q", ok, reason)
	}
}

func TestSweepMarksLostAfterTTL(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	r.DebugSetLastSeen("inventory", time.Now().Add(-time.Minute))

	lost := r.Sweep(time.Now())
	if len(lost) != 1 || lost[0] != "inventory" {
		t.Fatalf("应标记 inventory 为 lost，得到 %v", lost)
	}
	if got := len(r.Enabled()); got != 0 {
		t.Fatalf("lost 模块不应可见，得到 %d", got)
	}
}

func TestManualDisableIsSticky(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	if err := r.SetEnabled("inventory", false); err != nil {
		t.Fatalf("disable 失败：%v", err)
	}
	if ok, _ := r.Touch("inventory", "sha256:aaa"); !ok {
		t.Fatal("心跳本身应成功")
	}
	if got := len(r.Enabled()); got != 0 {
		t.Fatalf("人工 disable 必须粘性，得到可见 %d", got)
	}
}

// logEvents 读出事件链里的「事件/模块」序列。
//
// 断「只在跃迁时写」用序列比数行数直观：多记一次心跳就会立刻在序列里多出一个
// registered/inventory，而不是只差一个数字。
func logEvents(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取事件链失败：%v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Event    string `json:"event"`
			ModuleID string `json:"moduleId"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("事件链里有一行不是合法记录：%v（%s）", err, line)
		}
		out = append(out, rec.Event+"/"+rec.ModuleID)
	}
	return out
}

// openLoggedRegistry 起一个接了事件链的注册表。
//
// 测试结束前必须关掉文件句柄：Windows 上还开着的句柄会让 t.TempDir 的清理失败，
// 于是「用例全绿、清理报错」。
func openLoggedRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry-events.jsonl")
	r, err := NewRegistryWithLog(path)
	if err != nil {
		t.Fatalf("打开注册事件链失败：%v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("关闭注册事件链失败：%v", err)
		}
	})
	return r, path
}

// TestRegistryLogsOnlyTransitions 是计划 Task 5 的核心用例：心跳不写，跃迁才写。
func TestRegistryLogsOnlyTransitions(t *testing.T) {
	r, path := openLoggedRegistry(t)
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://127.0.0.1:8081", DeclarationHash: "sha256:aaa"})
	if got := logEvents(t, path); !slices.Equal(got, []string{"registered/inventory"}) {
		t.Fatalf("首次登记应只有一条 registered，得到 %v", got)
	}

	// 连续 3 次成功心跳：状态没变，链上不能多出任何记录。
	for i := 0; i < 3; i++ {
		if ok, reason := r.Touch("inventory", "sha256:aaa"); !ok {
			t.Fatalf("第 %d 次心跳应成功，得到 reason=%q", i+1, reason)
		}
	}
	if got := logEvents(t, path); !slices.Equal(got, []string{"registered/inventory"}) {
		t.Fatalf("三次成功心跳只应产生 1 条 registered，得到 %v", got)
	}

	// TTL 过期 → lost；重复清扫不重复记同一次失联。
	r.DebugSetLastSeen("inventory", time.Now().Add(-time.Minute))
	if lost := r.Sweep(time.Now()); !slices.Equal(lost, []string{"inventory"}) {
		t.Fatalf("应标记 inventory 为 lost，得到 %v", lost)
	}
	if lost := r.Sweep(time.Now()); len(lost) != 0 {
		t.Fatalf("已是 lost 的模块不应再被标记：%v", lost)
	}

	// lost → 心跳回来 → recovered。
	if ok, reason := r.Touch("inventory", "sha256:aaa"); !ok {
		t.Fatalf("复活心跳应成功，得到 reason=%q", reason)
	}

	want := []string{"registered/inventory", "lost/inventory", "recovered/inventory"}
	if got := logEvents(t, path); !slices.Equal(got, want) {
		t.Fatalf("事件序列 = %v，期望 %v", got, want)
	}
	if err := registrylog.Verify(path); err != nil {
		t.Fatalf("事件链应自洽：%v", err)
	}
}

// TestRegistryLogsReRegisteredOnHashChange 锁住「re-registered 只在声明变时记」。
func TestRegistryLogsReRegisteredOnHashChange(t *testing.T) {
	r, path := openLoggedRegistry(t)
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	// 同一份声明重复注册：哈希没变，不该记。
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	if got := logEvents(t, path); !slices.Equal(got, []string{"registered/inventory"}) {
		t.Fatalf("同哈希重复注册不应多记，得到 %v", got)
	}

	// 模块升级（baseURL 变也算新声明）→ 哈希变 → re-registered。
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://y", DeclarationHash: "sha256:bbb"})
	want := []string{"registered/inventory", "re-registered/inventory"}
	if got := logEvents(t, path); !slices.Equal(got, want) {
		t.Fatalf("事件序列 = %v，期望 %v", got, want)
	}
	if err := registrylog.Verify(path); err != nil {
		t.Fatalf("事件链应自洽：%v", err)
	}
}

// TestRegistryLogsRecoveredOnReRegister：失联的模块带着同一份声明重新注册（比如它没收到
// 409 而是自己重启了）时，状态确实从 lost 回到 alive，链上就必须留一条 recovered——
// 否则日志会一直宣称它失联。
func TestRegistryLogsRecoveredOnReRegister(t *testing.T) {
	r, path := openLoggedRegistry(t)
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	r.DebugSetLastSeen("inventory", time.Now().Add(-time.Minute))
	r.Sweep(time.Now())
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})

	want := []string{"registered/inventory", "lost/inventory", "recovered/inventory"}
	if got := logEvents(t, path); !slices.Equal(got, want) {
		t.Fatalf("事件序列 = %v，期望 %v", got, want)
	}
}

// TestRegistryLogsManualToggle：人工开关也是跃迁，且幂等重复设置不该被记成两次决策。
func TestRegistryLogsManualToggle(t *testing.T) {
	r, path := openLoggedRegistry(t)
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	if err := r.SetEnabled("inventory", false); err != nil {
		t.Fatalf("摘除失败：%v", err)
	}
	if err := r.SetEnabled("inventory", false); err != nil {
		t.Fatalf("重复摘除应幂等：%v", err)
	}
	if err := r.SetEnabled("inventory", true); err != nil {
		t.Fatalf("启用失败：%v", err)
	}

	want := []string{"registered/inventory", "manual-disabled/inventory", "manual-enabled/inventory"}
	if got := logEvents(t, path); !slices.Equal(got, want) {
		t.Fatalf("事件序列 = %v，期望 %v", got, want)
	}
	if err := registrylog.Verify(path); err != nil {
		t.Fatalf("事件链应自洽：%v", err)
	}
}

// TestRegistryLogFailureDoesNotChangeBusinessResult：观测面坏了不是业务面坏了的理由
// （spec §7）。事件一条都写不进去时，注册/心跳/清扫/开关仍必须按语义工作。
func TestRegistryLogFailureDoesNotChangeBusinessResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry-events.jsonl")
	r, err := NewRegistryWithLog(path)
	if err != nil {
		t.Fatalf("打开注册事件链失败：%v", err)
	}
	// 关掉日志句柄，模拟「事件链写不进去」（磁盘满、权限变化都是这个后果）。
	if err := r.Close(); err != nil {
		t.Fatalf("关闭事件链失败：%v", err)
	}

	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	if got := len(r.Enabled()); got != 1 {
		t.Fatalf("注册仍应生效，得到可见 %d", got)
	}
	if ok, reason := r.Touch("inventory", "sha256:aaa"); !ok {
		t.Fatalf("心跳仍应成功，得到 reason=%q", reason)
	}
	if err := r.SetEnabled("inventory", false); err != nil {
		t.Fatalf("摘除仍应成功：%v", err)
	}
	if got := len(r.Enabled()); got != 0 {
		t.Fatalf("摘除仍应生效，得到可见 %d", got)
	}
	if lost := r.Sweep(time.Now().Add(2 * DefaultTTL)); len(lost) != 1 {
		t.Fatalf("清扫仍应把失联模块判成 lost：%v", lost)
	}

	// 事件一条也没落盘；空链本身是自洽的，所以 Verify 通过。
	if got := logEvents(t, path); len(got) != 0 {
		t.Fatalf("日志已失效，不应写出任何事件：%v", got)
	}
	if err := registrylog.Verify(path); err != nil {
		t.Fatalf("空链应通过校验：%v", err)
	}
}

// TestOnRegisteredFiresAfterWriteOutsideLock 固定注册钩子的两条约定：
//   - 在**写锁之外**回调：钩子里读注册表必须能拿到锁（sync.RWMutex 不可重入，若 Upsert
//     还持着写锁就回调，这里会死锁——用例超时即失败）；
//   - 在**写表之后**回调：钩子看到的必须是落表后的条目（Enabled/State/LastSeen 已定），
//     否则补校验会对着一个半成品状态做判断。
func TestOnRegisteredFiresAfterWriteOutsideLock(t *testing.T) {
	r := NewRegistry()
	var seen []ModuleEntry
	r.OnRegistered(func(e ModuleEntry) {
		seen = append(seen, e)
		// 死锁探针：读锁与写锁互斥。
		if list := r.List(); len(list) != 1 || list[0].ID != e.ID {
			t.Errorf("钩子里读到的注册表不对：%+v", list)
		}
		if on := r.Enabled(); len(on) != 1 {
			t.Errorf("钩子里应能看见刚注册的模块：%+v", on)
		}
	})

	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	if len(seen) != 1 {
		t.Fatalf("注册应触发一次回调，得到 %d 次", len(seen))
	}
	if !seen[0].Enabled || seen[0].State != StateAlive || seen[0].LastSeen.IsZero() {
		t.Fatalf("回调应拿到落表后的条目（enabled/alive/lastSeen 已定）：%+v", seen[0])
	}

	// 重复注册同样触发（模块重启后重申报要能再补校验一次），但不影响条目状态。
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://y", DeclarationHash: "sha256:aaa"})
	if len(seen) != 2 || seen[1].BaseURL != "http://y" {
		t.Fatalf("重复注册应触发回调并带上新地址：%+v", seen)
	}

	// 空 ID 不登记，也就不该回调。
	r.Upsert(ModuleEntry{ID: "", BaseURL: "http://z"})
	if len(seen) != 2 {
		t.Fatalf("空 ID 不该触发回调，得到 %d 次", len(seen))
	}
}

// TestOnRegisteredPanicDoesNotBreakRegistration：钩子是观察者/派生逻辑，一个写坏的钩子
// 不能把注册变成 500（注册已经落表，模块重试也改变不了结果）。
func TestOnRegisteredPanicDoesNotBreakRegistration(t *testing.T) {
	r := NewRegistry()
	called := false
	r.OnRegistered(func(ModuleEntry) { panic("钩子写坏了") })
	r.OnRegistered(func(ModuleEntry) { called = true })

	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x"})

	if got := len(r.Enabled()); got != 1 {
		t.Fatalf("注册必须仍然生效，得到可见 %d", got)
	}
	if !called {
		t.Fatal("前面的钩子 panic 不该阻止后面的钩子")
	}
}

// TestLogEventWritesRuleDropped 固定「业务规则补校验失败」的事件写法：rule-dropped 落在
// 注册事件链上（不是业务审计），带上 ruleFile/policyId/reason，且链自洽。
func TestLogEventWritesRuleDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry-events.jsonl")
	r, err := NewRegistryWithLog(path)
	if err != nil {
		t.Fatalf("打开注册事件链失败：%v", err)
	}
	t.Cleanup(func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("关闭事件链失败：%v", cerr)
		}
	})

	r.Upsert(ModuleEntry{ID: "procurement", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	r.LogEvent(registrylog.Event{
		Event:    registrylog.EventRuleDropped,
		ModuleID: "procurement",
		RuleFile: "skills/s1/approval.json",
		PolicyID: "biz-small",
		Reason:   "拉取动作定义返回 404",
	})

	events := logRecords(t, path)
	if len(events) != 2 {
		t.Fatalf("期望 registered + rule-dropped 两条，得到 %+v", events)
	}
	last := events[len(events)-1]
	if last["event"] != registrylog.EventRuleDropped ||
		last["ruleFile"] != "skills/s1/approval.json" ||
		last["policyId"] != "biz-small" ||
		last["reason"] != "拉取动作定义返回 404" {
		t.Fatalf("rule-dropped 事件字段不对：%+v", last)
	}
	if err := registrylog.Verify(path); err != nil {
		t.Fatalf("事件链应通过校验：%v", err)
	}
}

// logRecords 把事件链读成按字段可断言的形状（logEvents 只看 event/moduleId）。
func logRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取事件链失败：%v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("事件行不是合法 JSON（%s）：%v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// 管理面 Upsert 不携带声明（空哈希）时，必须保留已知声明：
// 否则 Touch 会因「登记哈希为空」接受任意值，声明校验被静默旁路一个 TTL。
func TestUpsertWithoutDeclarationPreservesHash(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://a:8081", DeclarationHash: "sha256:aaa"})
	// 模拟 POST /tenon/admin/modules（只带 id 与地址）
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://a:8081"})

	if ok, reason := r.Touch("inventory", "sha256:aaa"); !ok {
		t.Fatalf("原声明的心跳应通过，得到 ok=%v reason=%q", ok, reason)
	}
	if ok, reason := r.Touch("inventory", "sha256:bbb"); ok || reason != "declaration-changed" {
		t.Fatalf("换声明仍必须被拒，得到 ok=%v reason=%q", ok, reason)
	}

	// 显式携带新声明时仍要更新（重申报路径）。
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://a:8081", DeclarationHash: "sha256:bbb"})
	if ok, reason := r.Touch("inventory", "sha256:bbb"); !ok {
		t.Fatalf("重申报后新哈希心跳应通过，得到 ok=%v reason=%q", ok, reason)
	}
}
