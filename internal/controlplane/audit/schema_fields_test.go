package audit

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestNewSchemaFieldsRoundTrip 锁住「审计记录必须能承载 schema 必填字段」。
//
// 背景：初版 AuditRecord 缺 trigger / inputHash / idempotencyKey / tokenUsage，
// 调用方写进去会被 json 静默丢弃 —— 落库记录因此不是 schema 完整的 AuditEvent，
// 而静默丢字段比报错更难发现。这条用例是那次修复的回归护栏。
func TestNewSchemaFieldsRoundTrip(t *testing.T) {
	st, err := NewAuditStore(filepath.Join(t.TempDir(), "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上必须关文件，否则 TempDir 清理会因占用失败
	t.Cleanup(func() { _ = st.Close() })
	rec := AuditRecord{
		AuditID:        "aud_x",
		Stage:          "dispatched",
		Actor:          json.RawMessage(`{"type":"agent","id":"ag"}`),
		Trigger:        json.RawMessage(`{"eventId":"evt_1234"}`),
		InputHash:      "sha256:abc",
		IdempotencyKey: "tenon:pln/s1",
		TokenUsage:     json.RawMessage(`{"in":1,"out":2}`),
	}
	if err := st.Append(rec); err != nil {
		t.Fatal(err)
	}
	got, err := st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(got))
	}
	if got[0].InputHash != "sha256:abc" || got[0].IdempotencyKey != "tenon:pln/s1" {
		t.Fatalf("标量字段被丢弃：%+v", got[0])
	}
	if len(got[0].Trigger) == 0 || len(got[0].TokenUsage) == 0 {
		t.Fatal("trigger / tokenUsage 被丢弃")
	}
}
