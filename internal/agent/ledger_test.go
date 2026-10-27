package agent

import (
	"path/filepath"
	"testing"
)

func TestLedgerClaimOnce(t *testing.T) {
	l, err := NewLedger(filepath.Join(t.TempDir(), "processed.jsonl"))
	if err != nil {
		t.Fatalf("打开账本失败：%v", err)
	}
	if ok, err := l.Claim("req_1"); err != nil || !ok {
		t.Fatalf("首次 claim 应成功：ok=%v err=%v", ok, err)
	}
	if ok, _ := l.Claim("req_1"); ok {
		t.Fatalf("同一 requestId 第二次 claim 必须返回 false")
	}
}

// 写前标记：claimed 但没 done 的请求，重放时必须跳过（宁可丢一句，不可重复扣库存）。
func TestLedgerClaimedWithoutDoneSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed.jsonl")
	l, _ := NewLedger(path)
	if ok, _ := l.Claim("req_1"); !ok {
		t.Fatalf("首次 claim 应成功")
	}
	// 模拟崩溃：不写 done 就重开账本。
	again, err := NewLedger(path)
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	if ok, _ := again.Claim("req_1"); ok {
		t.Fatalf("重开后同一 requestId 仍必须跳过")
	}
}

func TestLedgerDoneRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed.jsonl")
	l, _ := NewLedger(path)
	l.Claim("req_1")
	if err := l.Done("req_1", "pln_9", "completed"); err != nil {
		t.Fatalf("写 done 失败：%v", err)
	}
	again, _ := NewLedger(path)
	if ok, _ := again.Claim("req_1"); ok {
		t.Fatalf("done 之后也必须跳过")
	}
}
