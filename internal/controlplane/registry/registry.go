// Package registry 是控制平面的模块注册表，M0.5 阶段是纯内存态。
//
// 它只回答一个问题：现在有哪些模块、各自在哪、是否参与调度。消费方有四个：
//
//   - GET /tenon/modules（管理面）→ List()，看得见全部，含已摘除的；
//   - agent 的能力发现 → Enabled()，只看得见还在场的模块；
//   - POST /tenon/admin/modules（仅 TENON_TEST_MODE=1）→ Upsert / SetEnabled；
//   - cmd/host 启动时的 TENON_MODULES → ParseModules() 后逐个 Upsert。
//
// 设计文档 §3 的 D1 靠「摘除模块」而不是「改 agent」：SetEnabled(id, false) 把条目
// 留在表里但让它从 Enabled() 里消失，于是同一事件下 agent 能看到的模块不同，而 agent
// 一行代码都不用动。
//
// 两条约束写死在这里：
//   - 不持久化。M0.5 明确是内存态，进程重启即回到启动配置。
//   - 并发安全。HTTP 处理与测试并发读写，所有方法共用一把 sync.RWMutex。
package registry

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound 表示 SetEnabled 指定的模块从未登记过。
//
// 单独导出成哨兵值，是为了让调用方能用 errors.Is 区分「模块不存在」（配置/时序问题）
// 与「开关写入失败」（不该发生），而不是去匹配错误文本。
var ErrNotFound = errors.New("registry: 模块未登记")

// ModuleEntry 是注册表里的一行，也是 GET /tenon/modules 的响应元素。
// 字段标签即线上形状：{"id": ..., "baseURL": ..., "enabled": ...}。
type ModuleEntry struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Enabled bool   `json:"enabled"`
}

// Registry 是并发安全的内存注册表。零值可直接用（内部表惰性初始化），
// 常规用法仍是 NewRegistry()。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]ModuleEntry
}

// NewRegistry 返回一个空注册表。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]ModuleEntry)}
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
func (r *Registry) Upsert(e ModuleEntry) {
	if e.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]ModuleEntry)
	}
	if cur, ok := r.entries[e.ID]; ok {
		cur.BaseURL = e.BaseURL
		r.entries[e.ID] = cur
		return
	}
	e.Enabled = true
	r.entries[e.ID] = e
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
		if onlyEnabled && !e.Enabled {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ParseModules 解析启动配置 TENON_MODULES 的 id=url,id=url 形式，返回一份待
// Upsert 的条目（全部 enabled，登记即参与调度）。
//
// 容错边界都是刻意选的，宁可少猜也不要猜错：
//   - 空串、只有逗号和空白 → 无模块，返回空切片且不报错（「没配」是合法状态）；
//   - 空项（连续逗号、首尾逗号）跳过，不算错误；
//   - 每项、id、url 三段的两端空白都裁掉，" a = http://x " 等价于 "a=http://x"；
//   - 地址末尾的单个 '/' 去掉，避免后续拼 /tenon/... 时出现双斜杠；
//   - 其余一律报错并指明「第几项」与原始文本：缺 '='、id 为空、地址为空/非法/
//     scheme 不是 http(s)/缺主机名/带查询串或锚点、id 重复。
//
// 最后一条（id 重复）报错而不是后者覆盖前者：配置里同名两次基本是笔误，静默覆盖
// 只会让排查变成猜谜。要求「部分可用」的调用方应当自己降级，而不是让解析器吞错。
func ParseModules(s string) ([]ModuleEntry, error) {
	out := make([]ModuleEntry, 0, strings.Count(s, ",")+1)
	seen := make(map[string]int, cap(out))
	for i, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pos := i + 1 // 报错用 1-based 序号，与配置文本里数第几段对齐
		id, rawURL, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("TENON_MODULES 第 %d 项 %q 缺少 '='", pos, item)
		}
		id = strings.TrimSpace(id)
		rawURL = strings.TrimSpace(rawURL)
		if id == "" {
			return nil, fmt.Errorf("TENON_MODULES 第 %d 项 %q 的模块 id 为空", pos, item)
		}
		if first, dup := seen[id]; dup {
			return nil, fmt.Errorf("TENON_MODULES 第 %d 项 %q 与第 %d 项重复了 id %q", pos, item, first, id)
		}
		base, err := parseBaseURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("TENON_MODULES 第 %d 项 %q：%w", pos, item, err)
		}
		seen[id] = pos
		out = append(out, ModuleEntry{ID: id, BaseURL: base, Enabled: true})
	}
	return out, nil
}

// parseBaseURL 校验并规范化一个模块基地址。
//
// 只接受 http/https：模块是进程间 HTTP 调用，别的 scheme 一定是用错了。
// 允许带路径前缀（网关部署时模块挂在 /inventory 下），但不允许查询串或锚点——
// 基地址随后要往上拼 /tenon/actions/... ，带这类尾巴拼出来必然是错的。
func parseBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("模块地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("模块地址 %q 不是合法 URL：%w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("模块地址 %q 的 scheme 必须是 http 或 https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("模块地址 %q 缺少主机名", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("模块地址 %q 不应带查询串或锚点", raw)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
