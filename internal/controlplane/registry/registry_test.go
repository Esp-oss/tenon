package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
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
	if !slices.Equal(first, second) {
		t.Fatalf("两次 List 结果不一致：%+v vs %+v", first, second)
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

// TestEmptyRegistryJSON 锁住空注册表序列化成 []（而不是 null），
// 因为 GET /tenon/modules 直接编码 List()。
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
	const want = `[{"id":"inventory","baseURL":"http://i:8081","enabled":true}]`
	if string(b) != want {
		t.Fatalf("响应形状不对：%s，期望 %s", b, want)
	}
}

// TestParseModules 覆盖 TENON_MODULES 的正常与各类坏输入。
// wantErr 里的每一段都必须出现在错误信息里——「指出哪一项」是硬要求。
func TestParseModules(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []ModuleEntry
		wantErr []string
	}{
		{
			name: "正常两项",
			in:   "inventory=http://i:8081,procurement=http://p:8082",
			want: []ModuleEntry{
				{ID: "inventory", BaseURL: "http://i:8081", Enabled: true},
				{ID: "procurement", BaseURL: "http://p:8082", Enabled: true},
			},
		},
		{
			name: "空串即无模块",
			in:   "",
			want: nil,
		},
		{
			name: "只有逗号与空白",
			in:   " , ,  ",
			want: nil,
		},
		{
			name: "多余空格与尾逗号",
			in:   "  inventory = http://i:8081 ,  procurement=http://p:8082 , ",
			want: []ModuleEntry{
				{ID: "inventory", BaseURL: "http://i:8081", Enabled: true},
				{ID: "procurement", BaseURL: "http://p:8082", Enabled: true},
			},
		},
		{
			name: "地址末尾斜杠去掉",
			in:   "inventory=http://i:8081/",
			want: []ModuleEntry{{ID: "inventory", BaseURL: "http://i:8081", Enabled: true}},
		},
		{
			name: "保留路径前缀",
			in:   "inventory=https://gw.example.com/inventory",
			want: []ModuleEntry{{ID: "inventory", BaseURL: "https://gw.example.com/inventory", Enabled: true}},
		},
		{
			name:    "缺等号",
			in:      "inventory",
			wantErr: []string{"第 1 项", "inventory", "'='"},
		},
		{
			name:    "id 为空",
			in:      "=http://i:8081",
			wantErr: []string{"第 1 项", "id 为空"},
		},
		{
			name:    "地址为空",
			in:      "inventory=",
			wantErr: []string{"第 1 项", "地址为空"},
		},
		{
			name:    "非法 URL 指出第 2 项",
			in:      "inventory=http://i:8081,procurement=://bad",
			wantErr: []string{"第 2 项", "procurement=://bad", "不是合法 URL"},
		},
		{
			name:    "scheme 不是 http",
			in:      "inventory=ftp://i:8081",
			wantErr: []string{"第 1 项", "scheme"},
		},
		{
			name:    "缺主机名",
			in:      "inventory=http://",
			wantErr: []string{"第 1 项", "缺少主机名"},
		},
		{
			name:    "带查询串",
			in:      "inventory=http://i:8081?x=1",
			wantErr: []string{"第 1 项", "查询串"},
		},
		{
			name:    "id 重复",
			in:      "inventory=http://i:8081, inventory=http://i:9091",
			wantErr: []string{"第 2 项", "重复", "inventory"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseModules(tc.in)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ParseModules(%q) 应报错，却得到 %+v", tc.in, got)
				}
				for _, frag := range tc.wantErr {
					if !strings.Contains(err.Error(), frag) {
						t.Fatalf("错误信息缺少 %q：%v", frag, err)
					}
				}
				if got != nil {
					t.Fatalf("出错时应返回 nil 切片，得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseModules(%q) 不该报错：%v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseModules(%q) = %+v，期望 %+v", tc.in, got, tc.want)
			}
			for _, e := range got {
				if e.ID == "" || e.BaseURL == "" || !e.Enabled {
					t.Fatalf("解析出的条目不应是零值：%+v", e)
				}
			}
		})
	}
}

// TestParseModulesFeedsRegistry 把启动路径与 D1 的 B 轮串起来：
// 解析 → 登记 → 摘除 → agent 侧可见清单缩小。
func TestParseModulesFeedsRegistry(t *testing.T) {
	entries, err := ParseModules("procurement=http://p:8082,inventory=http://i:8081")
	if err != nil {
		t.Fatalf("ParseModules: %v", err)
	}
	r := NewRegistry()
	for _, e := range entries {
		r.Upsert(e)
	}
	if got := ids(r.Enabled()); !slices.Equal(got, []string{"inventory", "procurement"}) {
		t.Fatalf("登记后 Enabled() = %v", got)
	}

	// D1 的 B 轮：摘掉 procurement，agent 就只剩 inventory。
	if err := r.SetEnabled("procurement", false); err != nil {
		t.Fatalf("摘除失败：%v", err)
	}
	if got := ids(r.Enabled()); !slices.Equal(got, []string{"inventory"}) {
		t.Fatalf("摘除后 Enabled() = %v", got)
	}
	if got := len(r.List()); got != 2 {
		t.Fatalf("摘除后管理面仍应看到 2 条，得到 %d", got)
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
