// Package registrylog 记录模块注册生命周期事件，写入独立的 append-only 哈希链
// registry-events.jsonl。
//
// 为什么不并进业务审计（spec D4）：audit-event.schema.json 根 required 有 15 个字段
// （planId/stepId/action/inputHash/idempotencyKey/…），而「模块什么时候注册、什么时候
// 失联」与它们无关；硬塞进同一个 schema 只会造出半真半假的审计记录。所以注册事件自己
// 成链，只回答「谁、在什么时候、因为什么跃迁」。
//
// 每行一条记录（spec §7）：
//
//	{"at":"2026-10-27T00:00:00Z","event":"registered","moduleId":"inventory",
//	 "declarationHash":"sha256:…","prevHash":null,"hash":"sha256:…"}
//
// hash = "sha256:" + hex(sha256(CanonicalJSON(记录去掉 hash 字段)))，prevHash 指向上一条
// 的 hash、链首为 null——与 internal/controlplane/audit 完全同形，任何第三方都能脱离本包
// 独立复算：hash 只取决于语义内容，与键序、空白、数字写法无关。
//
// 三条边界：
//   - 只管「怎么存、怎么串、怎么验」，不知道注册表的状态机。依赖方向是 registry →
//     registrylog，本包不 import registry（否则成环）。
//   - 只在跃迁时写（什么算跃迁由 registry 决定）。心跳成功但状态没变就不写——5s 一次的
//     心跳会把日志刷成噪声，而噪声里的断裂没人看得出来。
//   - 写失败只返回错误，不决定业务处置：注册/心跳的成败由注册表说了算，事件链是观测面
//     （spec §7 与计划 Task 3 的约定一致）。
package registrylog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"tenon.dev/internal/protocol"
)

// hashPrefix 是规范约定的摘要前缀，见 common.schema.json#/$defs/Sha256Hash。
const hashPrefix = "sha256:"

// 事件类型（spec §7）。registered / recovered 都表示「这条走完之后模块是 alive 的」，
// 区别只在于是首次登记还是从 lost 回来。
const (
	EventRegistered     = "registered"
	EventReRegistered   = "re-registered"
	EventLost           = "lost"
	EventRecovered      = "recovered"
	EventManualEnabled  = "manual-enabled"
	EventManualDisabled = "manual-disabled"
)

// Event 是一条待写入的注册事件。
//
// 调用方只描述「什么事发生在哪个模块上」：at 由 now 参数给出，prevHash 与 hash 由 Log 按
// 链算。DeclarationHash 可空（lost / manual-* 事件不一定有意义），空值不写进记录。
type Event struct {
	Event           string
	ModuleID        string
	DeclarationHash string
}

// entry 是一行的线上形状。字段名与 spec §7 一致；declarationHash 用 omitempty，因为
// 「没有声明哈希」与「声明哈希是空串」在语义上是同一件事。
type entry struct {
	At              string  `json:"at"`
	Event           string  `json:"event"`
	ModuleID        string  `json:"moduleId"`
	DeclarationHash string  `json:"declarationHash,omitempty"`
	PrevHash        *string `json:"prevHash"`
	Hash            string  `json:"hash"`
}

// ChainError 说明链断在第几行（物理行号，1 基）以及为什么。
//
// 行号按文件里的物理行算，不按「第几条记录」：人拿它去编辑器里定位，数的是行。
type ChainError struct {
	Line   int
	Reason string
}

// Error 输出「第几行、为什么断」。
func (e *ChainError) Error() string {
	return fmt.Sprintf("注册事件链第 %d 行断裂：%s", e.Line, e.Reason)
}

// Log 是一条 append-only 的注册事件日志。
//
// 进程内用 Mutex 串行化写入，因此同一个 *Log 上的 Append 是全序的，链不会分叉；多进程同时
// 写同一文件不在支持范围内。
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
	tail *string // 链尾 hash；nil 表示还没有记录（下一条的 prevHash 为 null）
	// broken 在真实写盘失败后置位：这一行可能只写了一半，内存链尾与磁盘不再一致，
	// 继续追加会把半行接成坏链。此时拒绝追加，让人重新打开（Open 会重新读末行）。
	broken error
}

// Open 打开或创建 path 上的事件链，并把链尾接到既有记录之后。
//
// 只读末一行取 hash，不整链回放：Open 在 host 启动路径上，而「链是否完整」是
// Verify（验收脚本、事后核查）的职责，不该让每次启动都 O(n) 扫一遍。
// 末行读不懂或没有 hash 时返回错误而不是就地重写——那是链已损坏的信号，静默续写会把
// 「从某处断了」掩盖成「一切正常」。
//
// 返回的 *Log 持有文件句柄，进程退出前应调用 Close；Append 失败后须 Close 再 Open 才能
// 继续写（见 broken）。
func Open(path string) (*Log, error) {
	if path == "" {
		return nil, fmt.Errorf("注册事件日志路径不能为空")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开注册事件日志失败：%w", err)
	}
	tail, err := tailHash(path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Log{path: path, f: f, tail: tail}, nil
}

// Path 返回日志文件路径，供调用方在启动日志里点明事件链写在哪。
func (l *Log) Path() string { return l.path }

// Close 关闭底层文件句柄；之后再 Append 会报错。重复调用是幂等的。
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Append 把一条事件追加到链尾，返回 nil 表示已经落盘。
//
// 记忆点：只有落盘成功才推进内存链尾。写失败时 l.tail 原样不动，所以调用方看到错误时
// 内存态与磁盘态仍然一致（对不上就置 broken，拒绝继续写）。
//
// now 为零值时取当前时刻；写入前统一折算成 UTC，否则同一时刻在不同时区会写出不同字符串，
// 链就没法跨机器比对了。
func (l *Log) Append(ev Event, now time.Time) error {
	if ev.Event == "" {
		return fmt.Errorf("注册事件缺少 event 类型")
	}
	if ev.ModuleID == "" {
		return fmt.Errorf("注册事件缺少 moduleId")
	}
	if now.IsZero() {
		now = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return fmt.Errorf("注册事件日志已关闭，拒绝追加")
	}
	if l.broken != nil {
		return fmt.Errorf("注册事件日志已失效（上次写入失败：%v），须重新打开并校验链", l.broken)
	}

	rec := entry{
		At:              now.UTC().Format(time.RFC3339Nano),
		Event:           ev.Event,
		ModuleID:        ev.ModuleID,
		DeclarationHash: ev.DeclarationHash,
		PrevHash:        l.tail,
	}
	hash, err := hashOfEntry(rec)
	if err != nil {
		return fmt.Errorf("注册事件无法规范化（%s/%s）：%w", ev.Event, ev.ModuleID, err)
	}
	rec.Hash = hash
	line, err := lineOfEntry(rec)
	if err != nil {
		return fmt.Errorf("注册事件无法规范化（%s/%s）：%w", ev.Event, ev.ModuleID, err)
	}

	// 单次 Write 追加整行：O_APPEND 下这段字节要么整行落到文件尾，要么报错，不会插进
	// 别的记录中间。
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := l.f.Write(buf); err != nil {
		l.broken = err
		return fmt.Errorf("写入注册事件失败（%s/%s）：%w", ev.Event, ev.ModuleID, err)
	}
	// 不在返回前 fsync 的话，「日志写了」与「日志在盘上」就是两件事：崩溃后链尾丢了，
	// 下一次 Open 会从更早的 hash 接起，中间那些事件等于从没发生过。
	if err := l.f.Sync(); err != nil {
		l.broken = err
		return fmt.Errorf("注册事件落盘失败（%s/%s，fsync）：%w", ev.Event, ev.ModuleID, err)
	}

	tail := rec.Hash
	l.tail = &tail
	return nil
}

// Verify 按写入顺序回放 path 上的事件链，任何一处接不上或对不上就返回 *ChainError。
//
// 两种断裂都会被抓到：记录内容被改（重算 hash 与记录里的 hash 不符）与记录被删/被插
// （prevHash 接不上前一条）。空行被跳过——正常写入总以换行结尾，末尾的空串是切分产物。
func Verify(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取注册事件日志失败：%w", err)
	}
	var prev *string
	return forEachLine(raw, func(line int, data []byte) error {
		var rec entry
		if err := json.Unmarshal(data, &rec); err != nil {
			return &ChainError{Line: line, Reason: "记录无法解析：" + err.Error()}
		}
		if !sameHash(rec.PrevHash, prev) {
			return &ChainError{
				Line:   line,
				Reason: fmt.Sprintf("prevHash 未接上一条记录（期望 %s，实际 %s）", describeHash(prev), describeHash(rec.PrevHash)),
			}
		}
		want, err := hashOfEntry(rec)
		if err != nil {
			return &ChainError{Line: line, Reason: "记录无法重新规范化：" + err.Error()}
		}
		if rec.Hash != want {
			return &ChainError{
				Line:   line,
				Reason: fmt.Sprintf("记录内容与自身 hash 不符（重算 %s，记录里是 %s）", want, describeHash(&rec.Hash)),
			}
		}
		h := rec.Hash
		prev = &h
		return nil
	})
}

// forEachLine 按物理行号（1 基）遍历非空行。
func forEachLine(raw []byte, fn func(line int, data []byte) error) error {
	for i, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if err := fn(i+1, trimmed); err != nil {
			return err
		}
	}
	return nil
}

// tailHash 读末一条非空记录的 hash，作为新链的起点；文件为空时返回 nil（链首）。
func tailHash(path string) (*string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取注册事件日志失败：%w", err)
	}
	lines := bytes.Split(raw, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec entry
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("注册事件日志第 %d 行无法解析，拒绝接链：%w", i+1, err)
		}
		if rec.Hash == "" {
			return nil, fmt.Errorf("注册事件日志第 %d 行缺少 hash，拒绝在其后追加", i+1)
		}
		hash := rec.Hash
		return &hash, nil
	}
	return nil, nil
}

// lineOfEntry 把记录编成一行规范 JSON（含 hash 字段，供落盘）。
func lineOfEntry(rec entry) ([]byte, error) {
	tree, err := treeOfEntry(rec)
	if err != nil {
		return nil, err
	}
	return protocol.CanonicalJSON(tree)
}

// hashOfEntry 重算一条记录的 hash：hash 不进摘要输入，否则自指。
func hashOfEntry(rec entry) (string, error) {
	tree, err := treeOfEntry(rec)
	if err != nil {
		return "", err
	}
	delete(tree, "hash")
	return hashOfTree(tree)
}

// hashOfTree 对规范化后的 JSON 取 SHA-256，写成 sha256:<hex>。
func hashOfTree(v any) (string, error) {
	canon, err := protocol.CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hashPrefix + hex.EncodeToString(sum[:]), nil
}

// treeOfEntry 把记录编成 JSON 树（map[string]any）：字段名由 struct tag 决定，取值只有
// 字符串与 null，所以重放时的规范化结果只取决于语义内容，与当初的排版无关。
func treeOfEntry(rec entry) (map[string]any, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return tree, nil
}

func sameHash(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func describeHash(h *string) string {
	switch {
	case h == nil:
		return "null"
	case *h == "":
		return "空"
	default:
		return *h
	}
}
