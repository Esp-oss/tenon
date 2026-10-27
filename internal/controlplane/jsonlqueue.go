package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// jsonlQueue 是落盘的入站队列：一行一帧，ack 后重写存活部分。
//
// 为什么落盘：入站承载的是「人说过的话」。丢了不会有人报错，业务员只会以为提过。
// 出站 Intent 是界面提示，丢了可由公告板与 pending 重建，因此仍留内存实现。
type jsonlQueue struct {
	mu       sync.Mutex
	path     string
	capacity int
	seq      int64
	frames   []queuedFrame
	dropped  int64

	// broken 记住第一次落盘失败：队列已不可信，让 Close() 把它暴露出来。
	broken error

	// metaPath 存序号高位水位。它不存帧内容，只在主文件损坏时兜底序号，
	// 避免游标退回 0 导致持旧游标的客户端永久漏帧。
	metaPath string

	// failNext 是测试注入：下一次 rewrite 直接返回它。与 audit.Store.failNext 同一手法，
	// 用来验证「落盘失败时 HTTP 层必须说实话」这条路径，而不必真的把磁盘写满。
	failNext error
}

var _ Queue = (*jsonlQueue)(nil)

// newJSONLQueue 打开（或新建）落盘队列；capacity 非正时退回 maxIntentFrames。
func newJSONLQueue(path string, capacity int) (*jsonlQueue, error) {
	if capacity <= 0 {
		capacity = maxIntentFrames
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("controlplane: 创建入站队列目录失败：%w", err)
	}
	q := &jsonlQueue{path: path, capacity: capacity, frames: make([]queuedFrame, 0, capacity)}
	q.metaPath = path + ".meta"
	if err := q.load(); err != nil {
		return nil, err
	}
	return q, nil
}

// load 只读已 ack 之后仍存活的帧；末尾半行（崩溃留下）跳过而不是报错。
func (q *jsonlQueue) load() error {
	f, err := os.Open(q.path)
	if os.IsNotExist(err) {
		// 主文件不在、但 meta 可能在（运维手工清了队列文件）：水位仍要从 meta 恢复，
		// 否则序号回绕，持旧游标的客户端会永久漏帧。
		q.loadMeta()
		return nil
	}
	if err != nil {
		return fmt.Errorf("controlplane: 打开入站队列失败：%w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec queuedFrame
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // 半行/坏行：跳过，不阻塞后续帧
		}
		if rec.Seq > q.seq {
			q.seq = rec.Seq
		}
		q.frames = append(q.frames, rec)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	q.loadMeta()
	return nil
}

// loadMeta 读回水位。失败只告警不回退：主文件里仍可能有一份更大的 seq。
func (q *jsonlQueue) loadMeta() {
	raw, err := os.ReadFile(q.metaPath)
	if err != nil {
		return // 没有 meta 是正常情况（首次运行）
	}
	var meta struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		log.Printf("controlplane: 入站队列水位文件不可解析，忽略：%v", err)
		return
	}
	if meta.Seq > q.seq {
		q.seq = meta.Seq
	}
}

// saveMeta 落水位。先写临时文件再改名：半个 meta 比没有 meta 更坏。
func (q *jsonlQueue) saveMeta() {
	tmp := q.metaPath + ".tmp"
	raw, err := json.Marshal(map[string]int64{"seq": q.seq})
	if err != nil {
		return
	}
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		log.Printf("controlplane: 写入站队列水位失败：%v", err)
		return
	}
	if err := os.Rename(tmp, q.metaPath); err != nil {
		log.Printf("controlplane: 提交入站队列水位失败：%v", err)
	}
}

// rewrite 把存活帧重写回磁盘（ack 之后调用）。先写临时文件再改名，避免半个文件。
func (q *jsonlQueue) rewrite() error {
	tmp := q.path + ".tmp"
	// 任何失败路径都清掉半成品，避免下次启动把 .tmp 误当数据；
	// 成功路径下 os.Rename 之后 .tmp 已不存在，Stat 失败即自然跳过，不会误删。
	defer func() {
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.Remove(tmp)
		}
	}()
	// 测试注入：让落盘失败可复现，不必真把磁盘写满。
	if q.failNext != nil {
		err := q.failNext
		q.failNext = nil
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("controlplane: 写入站队列失败：%w", err)
	}
	w := bufio.NewWriter(f)
	for _, rec := range q.frames {
		raw, err := json.Marshal(rec)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}

// Push 入队一帧并返回分配的序号；队列满时丢最旧的、累加 dropped 并打一行告警。
// 落盘失败记入 broken，不吞；落盘成功后顺带刷新序号水位。
//
// 为什么每次都全量 rewrite 而不改成 append：文件末尾若留半行（崩溃），append 会把新帧
// 直接拼到坏行后面，产出一条更坏的行——新帧随之静默丢失，而日志里毫无异常。
// 全量 rewrite 走「临时文件 + rename」，顺带把坏行治好。别为省 IO 改回 append。
func (q *jsonlQueue) Push(frame json.RawMessage) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	q.frames = append(q.frames, queuedFrame{Seq: q.seq, Frame: frame})
	for len(q.frames) > q.capacity {
		q.frames = q.frames[1:]
		q.dropped++
		// 入站丢的是「用户说过的话」，与出站丢界面提示不是一个量级的事：必须留痕（spec §7）。
		log.Printf("controlplane: 入站队列已满（容量 %d），丢弃最旧一帧，累计 %d", q.capacity, q.dropped)
	}
	// 落盘失败不吞：调用方（POST 处理器）应当据此回 500，而不是假装入队成功。
	if err := q.rewrite(); err != nil {
		q.broken = err
	} else {
		// meta 只是主文件损坏时的兜底：帧本身已经落在主文件里了，不该让一次 push
		// 因为兜底失败而失败，所以 saveMeta 只告警、不设 broken。
		q.saveMeta()
	}
	return q.seq
}

// Since 返回序号严格大于 after 的帧，并给出下一次应当传入的游标与累计丢弃数。
// 没有新帧时游标回显 after（调用方可以继续用同一个游标轮询）。
func (q *jsonlQueue) Since(after int64) ([]queuedFrame, int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queuedFrame, 0, len(q.frames))
	cursor := after
	for _, rec := range q.frames {
		if rec.Seq <= after {
			continue
		}
		out = append(out, rec)
		cursor = rec.Seq
	}
	return out, cursor, q.dropped
}

// Ack 丢掉序号不大于 upto 的帧，返回实际确认到的序号（不超过已分配的最大序号）。
// 重复 ack 同一序号是幂等的。
func (q *jsonlQueue) Ack(upto int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 与 memoryQueue 保持同一契约：裁剪到已分配的最大序号，永远返回真实确认位置。
	// 两个实现的行为一旦分叉，同一段客户端代码在「落盘/内存」两种配置下就会拿到不同结果。
	if upto > q.seq {
		upto = q.seq
	}
	kept := make([]queuedFrame, 0, len(q.frames))
	for _, rec := range q.frames {
		if rec.Seq > upto {
			kept = append(kept, rec)
		}
	}
	q.frames = kept
	if err := q.rewrite(); err != nil {
		q.broken = err
	}
	return upto
}

// Highest 返回已分配的最大序号（0 表示还没入过队）。ack 省略 upto 时用它确认全部。
func (q *jsonlQueue) Highest() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.seq
}

// Broken 返回第一次落盘失败。Push 只回序号，这条是「已接受但没落盘」的唯一出口。
func (q *jsonlQueue) Broken() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.broken
}

// Close 暴露第一次落盘失败：队列已不可信时调用方必须知道。
func (q *jsonlQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.broken
}
