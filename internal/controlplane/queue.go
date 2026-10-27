package controlplane

import (
	"encoding/json"
	"sync"
)

// queuedFrame 是队列里的一帧：序号由队列分配，帧体原样保留。
//
// 序号不能塞进帧里：frames.schema.json 的三帧都是 additionalProperties=false 的封闭
// 结构，多一个 seq 字段就不是合法帧了，所以包一层。
type queuedFrame struct {
	Seq   int64           `json:"seq"`
	Frame json.RawMessage `json:"frame"`
}

// Queue 是可回放的帧队列：序号单调递增，Ack 掉已确认的前缀。
//
// 入站与出站各持一条：出站的 Ack 是累积游标，两个消费者共用一条队列会互相吞帧。
type Queue interface {
	Push(frame json.RawMessage) int64
	Since(after int64) (frames []queuedFrame, next int64, dropped int64)
	Ack(upto int64) int64
	Highest() int64
	// Broken 返回第一次落盘失败。Push 只回序号，落盘错误必须另有出口，
	// 否则「已接受但没落盘」会被伪装成「已持久化」。
	Broken() error
	Close() error
}

// memoryQueue 是有界内存队列：满则丢最旧并累加 dropped。重启即丢（出站用）。
type memoryQueue struct {
	mu       sync.Mutex
	capacity int
	seq      int64
	frames   []queuedFrame
	dropped  int64
}

var _ Queue = (*memoryQueue)(nil)

// Broken 对内存实现恒为 nil：它没有落盘这一步。
func (q *memoryQueue) Broken() error { return nil }

// newMemoryQueue 构造队列；capacity 非正时退回 maxIntentFrames。
func newMemoryQueue(capacity int) *memoryQueue {
	if capacity <= 0 {
		capacity = maxIntentFrames
	}
	return &memoryQueue{capacity: capacity, frames: make([]queuedFrame, 0, capacity)}
}

// Push 入队一帧并返回分配的序号；队列满时丢最旧的并累加 dropped。
func (q *memoryQueue) Push(frame json.RawMessage) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	q.frames = append(q.frames, queuedFrame{Seq: q.seq, Frame: frame})
	for len(q.frames) > q.capacity {
		q.frames = q.frames[1:]
		q.dropped++
	}
	return q.seq
}

// Since 返回序号严格大于 after 的帧，并给出下一次应当传入的游标与累计丢弃数。
// 没有新帧时游标回显 after（调用方可以继续用同一个游标轮询）。
func (q *memoryQueue) Since(after int64) ([]queuedFrame, int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queuedFrame, 0, len(q.frames))
	cursor := after
	for _, f := range q.frames {
		if f.Seq <= after {
			continue
		}
		out = append(out, f)
		cursor = f.Seq
	}
	return out, cursor, q.dropped
}

// Ack 丢掉序号不大于 upto 的帧，返回实际确认到的序号（不超过已分配的最大序号）。
// 重复 ack 同一序号是幂等的。
func (q *memoryQueue) Ack(upto int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if upto > q.seq {
		upto = q.seq
	}
	cut := 0
	for cut < len(q.frames) && q.frames[cut].Seq <= upto {
		cut++
	}
	if cut > 0 {
		q.frames = append(q.frames[:0:0], q.frames[cut:]...)
	}
	return upto
}

// Highest 返回已分配的最大序号（0 表示还没入过队）。ack 省略 upto 时用它确认全部。
func (q *memoryQueue) Highest() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.seq
}

// Close 释放队列占用的资源。内存队列没有资源要放，总是 nil。
func (q *memoryQueue) Close() error { return nil }
