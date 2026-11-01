package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

// keygen 的产物必须能真的签票、验票，而不是"文件写出来了就算成功"。
func TestKeygenProducesUsablePair(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "host.key")
	pubPath := filepath.Join(dir, "trusted.json")

	if code, err := runKeygen([]string{"--out", keyPath, "--pub", pubPath}); err != nil || code != 0 {
		t.Fatalf("runKeygen = (%d, %v)", code, err)
	}

	keys, err := protocol.LoadKeySetFile(pubPath)
	if err != nil {
		t.Fatalf("加载信任集：%v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("信任集条目 = %d，期望 1", len(keys))
	}

	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读私钥：%v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("私钥不是 PKCS#8 PEM：%+v", block)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("解析私钥：%v", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("私钥类型 = %T", parsed)
	}

	now := time.Unix(1793400000, 0)
	by := "user_li"
	ticket := protocol.Ticket{
		TicketID:       "tkt_keygen",
		Module:         "inventory",
		Action:         "inventory.ReserveStock",
		ArgsHash:       protocol.ArgsHash([]byte(`{"qty":8}`)),
		IdempotencyKey: "tenon:plan_01/s1",
		PlanID:         "plan_01",
		StepID:         "s1",
		AuditID:        "aud_01",
		ApprovedBy:     &by,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(30 * time.Second).Unix(),
	}
	token, err := protocol.SignTicket(priv, ticket)
	if err != nil {
		t.Fatalf("签票：%v", err)
	}
	got, err := protocol.VerifyTicket(keys, token, now, protocol.SignatureTolerance)
	if err != nil {
		t.Fatalf("用 keygen 产出的公钥验票失败：%v", err)
	}
	if got.TicketID != ticket.TicketID {
		t.Fatalf("往返不一致：%+v", got)
	}
}

func TestKeygenRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "host.key")
	pubPath := filepath.Join(dir, "trusted.json")

	if code, err := runKeygen([]string{"--out", keyPath, "--pub", pubPath}); err != nil || code != 0 {
		t.Fatalf("首次生成失败：(%d, %v)", code, err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读私钥：%v", err)
	}

	code, err := runKeygen([]string{"--out", keyPath, "--pub", pubPath})
	if err == nil || code != 2 {
		t.Fatalf("覆盖应被拒：(%d, %v)", code, err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读私钥：%v", err)
	}
	if string(before) != string(after) {
		t.Fatal("被拒绝的覆盖仍然改动了私钥文件")
	}
}
