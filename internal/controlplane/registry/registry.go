// Package registry 是控制平面的模块注册表，M0.5 阶段是纯内存态。
//
// 它只回答一个问题：现在有哪些模块、各自在哪、是否参与调度。消费方有四个：
//
//   - 注册/心跳端点（POST /tenon/registry/register|heartbeat）→ Upsert / Touch；
//   - GET /tenon/modules 与 agent 的能力发现 → Enabled()，只看得见「活着且没被人工摘掉」的；
//   - POST /tenon/admin/modules（仅 TENON_TEST_MODE=1）→ Upsert / SetEnabled；
//   - cmd/host 的后台清扫循环（每 HeartbeatInterval 一次）→ Sweep，把失联的判成 lost。
//
// 设计文档 §3 的 D1 靠「摘除模块」而不是「改 agent」：SetEnabled(id, false) 把条目
// 留在表里但让它从 Enabled() 里消失，于是同一事件下 agent 能看到的模块不同，而 agent
// 一行代码都不用动。
//
// 注册只有一条路径：模块自己 POST 申报。没有启动配置、没有 static 条目、没有环境变量
// （D5）——不心跳的模块就是不可见，没有例外。
//
// 三条约束写死在这里：
//   - 不持久化（D6）。进程重启即回到空表，模块在下一个心跳周期内收到 409 重申报后回来。
//   - 并发安全。HTTP 处理、后台清扫与测试并发读写，所有方法共用一把 sync.RWMutex。
//   - 事件链只记跃迁。表本身不落盘，但「谁在什么时候注册/失联/被摘除」要留痕：可选的
//     registrylog 哈希链在每次状态跃迁时写一条，心跳本身不写（见 logEvent）。
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"tenon.dev/internal/controlplane/registrylog"
)

// ErrNotFound 表示 SetEnabled 指定的模块从未登记过。
//
// 单独导出成哨兵值，是为了让调用方能用 errors.Is 区分「模块不存在」（配置/时序问题）
// 与「开关写入失败」（不该发生），而不是去匹配错误文本。
var ErrNotFound = errors.New("registry: 模块未登记")

// 模块存活状态。存活由心跳刷新，人工开关与它正交——两者都满足才可见。
const (
	StateAlive = "alive"
	StateLost  = "lost"

	// HeartbeatInterval 是模块心跳间隔，也是 host 后台清扫循环的节拍。
	//
	// host（判定失联）与模块侧（推送心跳）共用这一个常量：两边各写一个 5 * time.Second
	// 字面量，将来只改一处就会让「几个心跳算过期」的语义悄悄失准。
	HeartbeatInterval = 5 * time.Second

	// DefaultTTL 是心跳过期时间：3 个心跳周期。
	//
	// 允许连丢两次心跳才判 lost：单次丢包、一次 GC 停顿或调度抖动不该让模块从能力
	// 清单里消失（那会让 agent 的规划凭空少一个模块）；而连丢三次基本就是进程没了。
	DefaultTTL = 3 * HeartbeatInterval
)

// ModuleEntry 是注册表里的一行，也是 GET /tenon/modules 的响应元素。
// 字段标签即线上形状：{"id": ..., "baseURL": ..., "enabled": ...}。
type ModuleEntry struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Enabled bool   `json:"enabled"`
	// State 由心跳决定（alive/lost）；Enabled 由人工决定。两者都满足才参与调度。
	State string `json:"state"`
	// DeclarationHash 是模块申报的能力摘要哈希；心跳带它来比对，不一致就要求重申报。
	DeclarationHash string `json:"declarationHash,omitempty"`
	// LastSeen 是最后一次心跳时刻（host 单调时钟写入），用于 TTL 清扫。
	LastSeen time.Time `json:"lastSeen"`
	// Manifest 是模块申报的完整摘要，供诊断与后续按能力路由。
	Manifest json.RawMessage `json:"manifest,omitempty"`
}

// Registry 是并发安全的内存注册表。零值可直接用（内部表惰性初始化），
// 常规用法仍是 NewRegistry()。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]ModuleEntry
	// Log 是可选的注册事件链。nil 表示不记录（单测与不需要审计面的进程）。
	//
	// 字段是导出的，但**必须在并发使用之前**装配好（host 启动期，或直接走
	// NewRegistryWithLog）；注册表不会为它加第二把锁——跃迁与写事件本来就在同一把
	// 写锁下串行。
	Log *registrylog.Log
}

// NewRegistry 返回一个空注册表。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]ModuleEntry)}
}

// NewRegistryWithLog 返回一个把注册事件写进 path 的注册表——host 的接线入口。
//
// 为什么由注册表自己开日志，而不是让装配方先 Open 再塞进来：注册表是唯一知道「什么算
// 跃迁」的地方，日志跟它一起构造就不会出现「表在跳、日志没人接」的静默状态（spec D7 对
// 「静默不可用」零容忍）。打开失败必须让它失败：宁可起不来，也不要在没有事件链的情况下
// 跑起来冒充有审计面。
//
// 返回的 *Registry 持有文件句柄，进程退出前应调用 Close。
func NewRegistryWithLog(path string) (*Registry, error) {
	l, err := registrylog.Open(path)
	if err != nil {
		return nil, err
	}
	r := NewRegistry()
	r.Log = l
	return r, nil
}

// Close 关闭事件链文件；没接日志时是空操作。表本身不落盘，关掉之后注册/心跳照常工作，
// 只是不再有事件记录。
func (r *Registry) Close() error {
	if r.Log == nil {
		return nil
	}
	return r.Log.Close()
}

// logEvent 把一条跃迁写进事件链。调用方必须已持有写锁。
//
// 在锁内写文件是刻意的：这样日志顺序与跃迁顺序一致，两个并发跃迁不会把事件写反。代价是
// 跃迁期间写锁被 I/O 占着——但跃迁本来就稀疏（注册一次、失联一次、人工开关一次），而
// 5s 一次的心跳路径根本不写日志。
//
// 写失败只记一行 stderr，不改变方法的返回值：事件链是观测面，注册/心跳的成败由注册表
// 说了算（spec §7）。链坏了要人去修，不该让模块因此注册不上。
func (r *Registry) logEvent(ev registrylog.Event) {
	if r.Log == nil {
		return
	}
	if err := r.Log.Append(ev, time.Now()); err != nil {
		log.Printf("registry: 写入注册事件失败（%s/%s）：%v", ev.Event, ev.ModuleID, err)
	}
}

// Upsert 登记一个新模块，或更新已登记模块的地址。
//
// 语义刻意做窄，避免同一结构体既当入参又当出参带来的歧义：
//   - 从未登记的 id → 新建并置为 enabled（新增即参与调度）；
//   - 已登记的 id → 只覆盖 BaseURL，保留既有开关状态；
//   - 入参的 Enabled 一律忽略，空 ID 一律忽略。
//
// 为什么忽略 Enabled：零值 false 与「显式禁用」在 Go 里无法区分，若照抄入参，
// Upsert(ModuleEntry{ID: x, BaseURL: u}) 会意外把一个已启用的模块关掉。开关的唯一
// 入口是 SetEnabled——那里对未知 id 报错而不是静默造条目。
//
// 空 ID 被丢弃也同样是有意的：注册表不存无名条目，否则 GET /tenon/modules 会吐
// 出 id 为空的记录。校验归 HTTP 层（应当回 400），这里只保证表本身干净。
//
// 事件链上，这是唯一会「让模块从无到有」的入口：新建记 registered，声明哈希变了记
// re-registered（spec §7），哈希没变就什么都不记——重复注册不该把日志刷成噪声。
// 顺带一提，注册本身就是「现在是 alive 的证据」，所以一个 lost 的模块以相同哈希重新注册
// 只记 recovered，不额外记一条「复活」。
func (r *Registry) Upsert(e ModuleEntry) {
	if e.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]ModuleEntry)
	}
	now := time.Now()
	if cur, ok := r.entries[e.ID]; ok {
		kind := ""
		switch {
		case cur.DeclarationHash != e.DeclarationHash:
			kind = registrylog.EventReRegistered
		case cur.State != "" && cur.State != StateAlive:
			kind = registrylog.EventRecovered
		}
		cur.BaseURL = e.BaseURL
		cur.DeclarationHash = e.DeclarationHash
		cur.Manifest = e.Manifest
		// 注册即视为刚心跳过：否则模块刚起来还要等一个 TTL 才可见。
		cur.State = StateAlive
		cur.LastSeen = now
		r.entries[e.ID] = cur
		if kind != "" {
			r.logEvent(registrylog.Event{Event: kind, ModuleID: e.ID, DeclarationHash: cur.DeclarationHash})
		}
		return
	}
	e.Enabled = true
	e.State = StateAlive
	e.LastSeen = now
	r.entries[e.ID] = e
	r.logEvent(registrylog.Event{Event: registrylog.EventRegistered, ModuleID: e.ID, DeclarationHash: e.DeclarationHash})
}

// SetEnabled 打开或关闭一个已登记模块。
//
// ID 不存在时返回包裹了 ErrNotFound 的错误，且不创建条目——注册只能走 Upsert，
// 否则一个拼错的 id 会悄悄变成一个「幽灵模块」。重复设置成同一状态是幂等的，
// 返回 nil。
func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.entries[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if cur.Enabled != enabled {
		cur.Enabled = enabled
		r.entries[id] = cur
		kind := registrylog.EventManualDisabled
		if enabled {
			kind = registrylog.EventManualEnabled
		}
		// 只有真的变了才写：重复点两次开关是同一个决策，链上留两条会让人以为发生了两次。
		r.logEvent(registrylog.Event{Event: kind, ModuleID: id, DeclarationHash: cur.DeclarationHash})
	}
	return nil
}

// List 返回全部条目（含已摘除的），按 ID 字典序。
//
// 两处刻意的实现选择：
//   - 排序：map 遍历顺序随机，不排序的话同一状态的两次调用会得到不同顺序，
//     HTTP 响应无法比对、测试也无法稳定断言。
//   - 副本：返回值是深拷贝的切片，调用方随便改（比如给自己加字段）都写不穿注册表。
//
// 空注册表返回非 nil 的空切片，保证 GET /tenon/modules 序列化成 [] 而不是 null。
func (r *Registry) List() []ModuleEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot(false)
}

// Enabled 只返回参与调度的模块，同样按 ID 排序；空结果同样是非 nil 空切片。
//
// agent 的能力发现读这个而不是 List：D1 的 B 轮把 procurement 摘掉之后，agent 拿到
// 的清单里就只剩 quote/inventory，于是规划只会产出「扣 3 + 无力补足」。
func (r *Registry) Enabled() []ModuleEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot(true)
}

// snapshot 复制一份并按 ID 排序。调用方必须已持有读锁。
func (r *Registry) snapshot(onlyEnabled bool) []ModuleEntry {
	out := make([]ModuleEntry, 0, len(r.entries))
	for _, e := range r.entries {
		// 可见 = 人工开着 && 心跳还活着。空 State 视为 alive，兼容未经过 Upsert 的旧条目。
		if onlyEnabled && (!e.Enabled || (e.State != "" && e.State != StateAlive)) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Touch 记录一次心跳。
//
// 返回 (true, "") 表示存活已刷新；返回 (false, reason) 表示模块必须先重申报：
//   - "unknown-module"：host 不认识这个 id（通常是 host 重启后注册表为空）；
//   - "declaration-changed"：申报的声明哈希与登记的不一致（模块升级了或者发错了）。
//
// 注意：Touch 不改变 Enabled——人工摘除是粘性的，心跳救不回来。
//
// 事件链上只有 lost → alive 这一次跃迁记 recovered；已经 alive 的心跳成功什么都不写，
// 否则 3 个模块 × 每 5s 就能把日志刷成几万行，真正的断裂埋在噪声里。
func (r *Registry) Touch(id, hash string) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.entries[id]
	if !ok {
		return false, "unknown-module"
	}
	if cur.DeclarationHash != "" && hash != cur.DeclarationHash {
		return false, "declaration-changed"
	}
	wasLost := cur.State != "" && cur.State != StateAlive
	cur.State = StateAlive
	cur.LastSeen = time.Now()
	r.entries[id] = cur
	if wasLost {
		r.logEvent(registrylog.Event{Event: registrylog.EventRecovered, ModuleID: id, DeclarationHash: cur.DeclarationHash})
	}
	return true, ""
}

// Sweep 把超过 TTL 没心跳的模块标记为 lost，返回被标记的 id（按字典序）。
// 只改存活状态，不动 Enabled——人工开关的语义不受 TTL 影响。
//
// 每个转 lost 的模块写一条事件；已经是 lost 的跳过，所以清扫循环每 5s 跑一次也不会
// 重复记同一次失联。
func (r *Registry) Sweep(now time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lost []string
	for id, e := range r.entries {
		if e.State == StateLost {
			continue
		}
		if e.LastSeen.IsZero() || now.Sub(e.LastSeen) > DefaultTTL {
			e.State = StateLost
			r.entries[id] = e
			lost = append(lost, id)
		}
	}
	sort.Strings(lost)
	// 排序之后才写事件：map 遍历顺序随机，日志顺序也该稳定，否则同样的状态两次清扫会
	// 写出顺序不同的链（链本身仍然有效，但人没法比对）。
	for _, id := range lost {
		r.logEvent(registrylog.Event{Event: registrylog.EventLost, ModuleID: id, DeclarationHash: r.entries[id].DeclarationHash})
	}
	return lost
}

// DebugSetLastSeen 只给测试用：把 LastSeen 拨到指定时刻，便于断言 TTL 行为。
// 生产代码不该调用它——时间只能由心跳写入。
func (r *Registry) DebugSetLastSeen(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.entries[id]
	if !ok {
		return
	}
	cur.LastSeen = at
	r.entries[id] = cur
}
