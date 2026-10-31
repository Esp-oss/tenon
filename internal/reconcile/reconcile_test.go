package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// hostLine 造一条宿主审计行。result 为空表示"还没写结果"。
func hostLine(auditID, action string, inputHash string, result string) string {
	rec := map[string]any{
		"auditId": auditID, "stage": "dispatched", "at": "2026-10-31T00:00:00Z",
		"actor": map[string]any{"type": "agent", "id": "ag"}, "planId": "pln_1",
		"stepId": "s1", "action": action, "inputHash": inputHash,
		"idempotencyKey": "tenon:pln_1/s1", "hash": "sha256:x", "prevHash": nil,
	}
	line, _ := json.Marshal(rec)
	out := string(line) + "\n"
	if result != "" {
		res := map[string]any{
			"auditId": auditID, "stage": "result", "at": "2026-10-31T00:00:01Z",
			"actor": map[string]any{"type": "agent", "id": "ag"}, "planId": "pln_1",
			"stepId": "s1", "action": action, "result": map[string]any{"ok": result == "ok"},
			"hash": "sha256:y", "prevHash": "sha256:x",
		}
		b, _ := json.Marshal(res)
		out += string(b) + "\n"
	}
	return out
}

func writeHost(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host-audit.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写宿主审计：%v", err)
	}
	return path
}

// moduleStub 模拟模块的 /tenon/audit/local。
func moduleStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReconcileClean(t *testing.T) {
	host := writeHost(t, hostLine("aud_1", "inventory.DeductStock", "sha256:a", "ok"))
	mod := moduleStub(t, 200, `{"module":"inventory","headSeq":1,"headHash":"sha256:h","records":[
		{"kind":"action","action":"inventory.DeductStock","auditId":"aud_1","argsHash":"sha256:a","result":"ok"}]}`)

	rep, err := Run(context.Background(), Options{HostAuditPath: host,
		Modules: []ModuleSource{{ID: "inventory", BaseURL: mod.URL}}})
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if len(rep.Findings) != 0 || rep.ExitCode() != 0 {
		t.Fatalf("两侧一致时不应有差异，实际 %+v（exit=%d）", rep.Findings, rep.ExitCode())
	}
	if len(rep.Heads) != 1 || rep.Heads[0].HeadHash == "" {
		t.Fatalf("应记录模块 head：%+v", rep.Heads)
	}
}

// 模块有、宿主没有 = 绕过。这是对账存在的理由，退出码必须非 0。
func TestReconcileModuleOnlyExitsNonZero(t *testing.T) {
	host := writeHost(t, "")
	mod := moduleStub(t, 200, `{"module":"inventory","headSeq":1,"headHash":"sha256:h","records":[
		{"kind":"action","action":"inventory.DeductStock","auditId":"aud_9","argsHash":"sha256:a","result":"ok"}]}`)

	rep, err := Run(context.Background(), Options{HostAuditPath: host,
		Modules: []ModuleSource{{ID: "inventory", BaseURL: mod.URL}}})
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if rep.Counts[ClassModuleOnly] != 1 || rep.ExitCode() != 2 {
		t.Fatalf("应报 module_only 且退出码 2，实际 %+v exit=%d", rep.Counts, rep.ExitCode())
	}
}

// 模块拒绝给出链（自己说链断了）：必须原样上报，不能当成"没记录"。
func TestReconcileChainBrokenExitsNonZero(t *testing.T) {
	host := writeHost(t, hostLine("aud_1", "inventory.DeductStock", "sha256:a", "ok"))
	mod := moduleStub(t, 503, `{"tenonError":{"class":"retryable","code":"AUDIT_CHAIN_BROKEN","message":"第 3 行内容与哈希不符"}}`)

	rep, err := Run(context.Background(), Options{HostAuditPath: host,
		Modules: []ModuleSource{{ID: "inventory", BaseURL: mod.URL}}})
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if rep.Counts[ClassChainBroken] != 1 || rep.ExitCode() != 3 {
		t.Fatalf("应报 chain_broken 且退出码 3，实际 %+v exit=%d", rep.Counts, rep.ExitCode())
	}
	if rep.Counts[ClassHostOnly] != 0 {
		t.Fatalf("链断时不应把同一笔再报成 host_only：%+v", rep.Counts)
	}
}

// 宿主已签发、结果没写 = 预期内（重试未落地），不能报成篡改。
func TestReconcileHostOnlyUnknownIsExpected(t *testing.T) {
	host := writeHost(t, hostLine("aud_1", "inventory.DeductStock", "sha256:a", ""))
	mod := moduleStub(t, 200, `{"module":"inventory","headSeq":0,"headHash":"","records":[]}`)

	rep, _ := Run(context.Background(), Options{HostAuditPath: host,
		Modules: []ModuleSource{{ID: "inventory", BaseURL: mod.URL}}})
	if rep.Counts[ClassHostOnlyUnknown] != 1 || rep.Counts[ClassHostOnly] != 0 {
		t.Fatalf("缺结果的尝试应归为 host_only_unknown：%+v", rep.Counts)
	}
	if rep.ExitCode() != 1 {
		t.Fatalf("host_only_unknown 不应升级为绕过（exit=%d）", rep.ExitCode())
	}
}

// 升级前的老记录只有幂等键：能对上就不追究，但要标 legacy_gap。
func TestReconcileLegacyGap(t *testing.T) {
	host := writeHost(t, hostLine("aud_1", "inventory.DeductStock", "sha256:a", "ok"))
	mod := moduleStub(t, 200, `{"module":"inventory","headSeq":3,"headHash":"sha256:h","legacyRecords":2,"records":[
		{"kind":"action","action":"inventory.DeductStock","idempotencyKey":"tenon:pln_1/s1","argsHash":"sha256:a","result":"ok","legacy":true}]}`)

	rep, _ := Run(context.Background(), Options{HostAuditPath: host,
		Modules: []ModuleSource{{ID: "inventory", BaseURL: mod.URL}}})
	if rep.Counts[ClassLegacyGap] != 1 {
		t.Fatalf("应按幂等键对上的老记录标 legacy_gap：%+v", rep.Counts)
	}
}
