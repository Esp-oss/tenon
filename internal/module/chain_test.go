package module

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

func chainModule(t *testing.T, path string) *Module {
	t.Helper()
	t.Setenv(EnvAuditPath, path)
	return &Module{ID: "demo", Title: "链演示", Version: "0.1.0"}
}

func (m *Module) appendForTest(t *testing.T, result string) {
	t.Helper()
	if err := m.appendAudit(auditRecord{
		At: "2026-10-31T00:00:00Z", Module: m.ID, Kind: "action",
		Action: "demo.Write", IdempotencyKey: "tenon:pln/s1", ArgsHash: "sha256:x",
		Result: result, AuditID: "aud_1", PlanID: "pln", StepID: "s1", TicketID: "tkt_1",
	}); err != nil {
		t.Fatalf("追加审计失败：%v", err)
	}
}

// 新文件：没有历史，就不写创世行；首条记录从空 prev 起链。
func TestChainStartsCleanOnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	m := chainModule(t, path)
	m.appendForTest(t, "ok")

	st, err := m.readChain(0, 100)
	if err != nil {
		t.Fatalf("读链失败：%v", err)
	}
	if st.Legacy != 0 || len(st.Records) != 1 || st.HeadHash == "" {
		t.Fatalf("空文件首条不应有 legacy，实际 legacy=%d records=%d head=%q",
			st.Legacy, len(st.Records), st.HeadHash)
	}
}

// 历史文件：先写创世行记住行数，旧记录不参与链校验——否则升级即"链断"。
func TestChainSeedsGenesisForExistingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	old := `{"at":"2026-10-01T00:00:00Z","module":"demo","kind":"action","action":"demo.Write","result":"ok"}` + "\n" +
		`{"at":"2026-10-01T00:00:01Z","module":"demo","kind":"action","action":"demo.Write","result":"ok"}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("写历史日志：%v", err)
	}
	m := chainModule(t, path)
	m.appendForTest(t, "ok")

	st, err := m.readChain(0, 100)
	if err != nil {
		t.Fatalf("带历史的链不该报错：%v", err)
	}
	if st.Legacy != 2 {
		t.Fatalf("应识别出 2 条历史记录，实际 %d", st.Legacy)
	}
	if len(st.Records) != 4 { // 2 历史 + 创世 + 1 新
		t.Fatalf("应有 4 条记录（含创世），实际 %d", len(st.Records))
	}
}

// 删掉中间一行：后续记录的 prevHash 接不上，必须报出来。
func TestChainDetectsDeletedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	m := chainModule(t, path)
	m.appendForTest(t, "ok")
	m.appendForTest(t, "ok")
	m.appendForTest(t, "ok")

	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("应有 3 行，实际 %d", len(lines))
	}
	writeLines(t, path, []string{lines[0], lines[2]})

	if _, err := m.readChain(0, 100); err == nil {
		t.Fatal("删掉中间一行后必须报链断，实际没有报错")
	}
}

// 改一行内容：哈希对不上，必须报出来。
func TestChainDetectsEditedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	m := chainModule(t, path)
	m.appendForTest(t, "ok")
	m.appendForTest(t, "ok")

	lines := readLines(t, path)
	lines[0] = strings.Replace(lines[0], `"result":"ok"`, `"result":"error"`, 1)
	writeLines(t, path, lines)

	if _, err := m.readChain(0, 100); err == nil {
		t.Fatal("改动一行内容后必须报哈希不符，实际没有报错")
	}
}

// 端点必须能给出 head 与记录：宿主对账只认这个响应，不接受"你自己翻文件"。
func TestAuditLocalEndpointReturnsHead(t *testing.T) {
	e := newTicketEnv(t)
	ticket := e.ticket(t, fixtureModuleID, writeActionID, []byte(ticketBody), "tenon:pln_t/s1", 30*time.Second)
	if status, raw := e.post(t, ticketBody, "tenon:pln_t/s1", ticket); status != 200 {
		t.Fatalf("先要有一笔成功写入，得到 %d：%s", status, raw)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/tenon/audit/local", nil)
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var got struct {
		HeadHash string `json:"headHash"`
		HeadSeq  int64  `json:"headSeq"`
		Records  []struct {
			AuditID string `json:"auditId"`
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, raw)
	}
	if got.HeadHash == "" || got.HeadSeq != 1 {
		t.Fatalf("head 不完整：%+v", got)
	}
	if len(got.Records) != 1 || got.Records[0].AuditID != "aud_t" {
		t.Fatalf("记录里应带票据的 auditId：%+v", got.Records)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志：%v", err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写日志：%v", err)
	}
}
