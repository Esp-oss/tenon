package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONLQueueRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("打开队列失败：%v", err)
	}
	defer q.Close()

	if seq := q.Push(json.RawMessage(`{"type":"tenon.request","n":1}`)); seq != 1 {
		t.Fatalf("首个序号应为 1，得到 %d", seq)
	}
	q.Push(json.RawMessage(`{"type":"tenon.request","n":2}`))

	frames, next, dropped := q.Since(0)
	if len(frames) != 2 || next != 2 || dropped != 0 {
		t.Fatalf("回放不符：len=%d next=%d dropped=%d", len(frames), next, dropped)
	}
	if q.Ack(1) != 1 {
		t.Fatalf("ack 返回值不对")
	}
	frames, _, _ = q.Since(0)
	if len(frames) != 1 || frames[0].Seq != 2 {
		t.Fatalf("ack 后应只剩 seq=2，得到 %+v", frames)
	}
}

// 重启是入站队列存在的理由：用户说过的话不能因为进程重启就消失。
func TestJSONLQueueSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	q.Push(json.RawMessage(`{"n":1}`))
	q.Push(json.RawMessage(`{"n":2}`))
	q.Ack(1)
	if err := q.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	again, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	defer again.Close()
	frames, next, _ := again.Since(1)
	if len(frames) != 1 || frames[0].Seq != 2 || next != 2 {
		t.Fatalf("重开后续号/游标不对：%+v next=%d", frames, next)
	}
	if seq := again.Push(json.RawMessage(`{"n":3}`)); seq != 3 {
		t.Fatalf("重开后序号应从 3 继续，得到 %d", seq)
	}
}

func TestJSONLQueueDropsOldestWhenFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 2)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	defer q.Close()
	q.Push(json.RawMessage(`{"n":1}`))
	q.Push(json.RawMessage(`{"n":2}`))
	q.Push(json.RawMessage(`{"n":3}`))
	frames, _, dropped := q.Since(0)
	if len(frames) != 2 || dropped != 1 || frames[0].Seq != 2 {
		t.Fatalf("容量 2 时应丢最旧：len=%d dropped=%d first=%d", len(frames), dropped, frames[0].Seq)
	}
}

// 断电留下的半行不该让整个队列读不出来。
func TestJSONLQueueIgnoresTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbound.jsonl")
	q, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	q.Push(json.RawMessage(`{"n":1}`))
	q.Close()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if _, err := f.WriteString(`{"seq":2,"fra`); err != nil {
		t.Fatalf("写半行失败：%v", err)
	}
	f.Close()

	again, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("半行应被忽略而不是报错：%v", err)
	}
	defer again.Close()
	if again.Highest() != 1 {
		t.Fatalf("半行不该被计入序号，最高应为 1，得到 %d", again.Highest())
	}
}
