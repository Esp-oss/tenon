package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledgerEntry 是 processed.jsonl 的一行。
type ledgerEntry struct {
	RequestID string `json:"requestId"`
	Phase     string `json:"phase"` // claimed | done
	PlanID    string `json:"planId,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	At        string `json:"at"`
}

// Ledger 是 requestId 去重账本，write-ahead 语义：
// 处理前写 claimed（fsync），处理完写 done。重放时见 claimed 无 done 即跳过——
// 因为重新规划会生成新 planId → 新幂等键 → 模块幂等表挡不住，后果是重复扣库存。
type Ledger struct {
	mu   sync.Mutex
	path string
	seen map[string]string // requestId -> phase
}

// NewLedger 打开（或新建）账本并把已有条目读进内存。
func NewLedger(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("agent: 建账本目录失败：%w", err)
	}
	l := &Ledger{path: path, seen: map[string]string{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var e ledgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // 半行跳过，不阻塞
		}
		l.seen[e.RequestID] = e.Phase
	}
	return l, sc.Err()
}

// Claim 尝试占用一个 requestId：返回 false 表示它已被处理过（或正在处理中）。
func (l *Ledger) Claim(requestID string) (bool, error) {
	if requestID == "" {
		return true, nil // 事件回合没有 requestId，不走账本
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[requestID]; ok {
		return false, nil
	}
	if err := l.append(ledgerEntry{RequestID: requestID, Phase: "claimed", At: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return false, err
	}
	l.seen[requestID] = "claimed"
	return true, nil
}

// Done 记录处理完毕。
func (l *Ledger) Done(requestID, planID, outcome string) error {
	if requestID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.append(ledgerEntry{
		RequestID: requestID, Phase: "done", PlanID: planID, Outcome: outcome,
		At: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	l.seen[requestID] = "done"
	return nil
}

func (l *Ledger) append(e ledgerEntry) error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	// claimed 必须落盘才算数：它挡的是重复副作用，不能只活在页缓存里。
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
