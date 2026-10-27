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
	if err := q.load(); err != nil {
		return nil, err
	}
	return q, nil
}

// load 只读已 ack 之后仍存活的帧；末尾半行（崩溃留下）跳过而不是报错。
func (q *jsonlQueue) load() error {
	f, err := os.Open(q.path)
	if os.IsNotExist(err) {
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
	return sc.Err()
}

// rewrite 把存活帧重写回磁盘（ack 之后调用）。先写临时文件再改名，避免半个文件。
func (q *jsonlQueue) rewrite() error {
	tmp := q.path + ".tmp"
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
// 落盘失败记入 broken，不吞。
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

// Close 暴露第一次落盘失败：队列已不可信时调用方必须知道。
func (q *jsonlQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.broken
}
