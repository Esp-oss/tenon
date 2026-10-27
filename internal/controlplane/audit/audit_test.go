package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTestStore 打开一个临时审计日志，并在用例结束时关闭它。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewAuditStore(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditStore：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// readLines 读出日志的原始行，用来断言存储形态（单行、紧凑）与手工篡改。
func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计日志：%v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestAuditChainAndTwoStages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	st, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Append(AuditRecord{AuditID: "aud_1", Stage: "dispatched", PlanID: "pln_1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(AuditRecord{AuditID: "aud_1", Stage: "result", PlanID: "pln_1"}); err != nil {
		t.Fatal(err)
	}

	recs, err := st.Query(Query{PlanID: "pln_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("期望 2 段，得到 %d", len(recs))
	}
	if recs[0].PrevHash != nil {
		t.Fatal("链首 prevHash 必须为 null")
	}
	if recs[1].PrevHash == nil || *recs[1].PrevHash != recs[0].Hash {
		t.Fatal("prevHash 未正确串联")
	}
	if err := st.VerifyChain(); err != nil {
		t.Fatalf("链校验失败：%v", err)
	}

	// 两段共用同一个 auditId，靠 stage 区分；at 由 Store 补当前时刻。
	if recs[0].AuditID != recs[1].AuditID || recs[0].Stage != StageDispatched || recs[1].Stage != StageResult {
		t.Fatalf("两段记录不成对：%+v / %+v", recs[0], recs[1])
	}
	if recs[0].At.IsZero() || recs[1].At.IsZero() {
		t.Fatal("at 未补写")
	}
	for i, rec := range recs {
		if !strings.HasPrefix(rec.Hash, "sha256:") || len(rec.Hash) != len("sha256:")+64 {
			t.Fatalf("第 %d 条 hash 形态不对：%q", i+1, rec.Hash)
		}
	}

	// 存储形态：一行一条、紧凑无空白——规范化后才可能是「同一内容同一摘要」。
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("期望 2 行，得到 %d", len(lines))
	}
	for i, line := range lines {
		if strings.ContainsAny(line, " \t\r") {
			t.Fatalf("第 %d 行含多余空白，不是规范化 JSON：%q", i+1, line)
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON：%v", i+1, err)
		}
	}
}

func TestAuditAppendFailureIsObservable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	st, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	st.FailNextAppend(errors.New("磁盘满")) // 测试注入钩子
	if err := st.Append(AuditRecord{AuditID: "aud_x", Stage: "dispatched"}); err == nil {
		t.Fatal("注入失败时 Append 必须返回错误——上层据此拒绝执行")
	}

	// 注入的失败不能留下半条记录，也不能污染链尾：下一次写入仍应是链首。
	recs, err := st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("注入失败后不应有记录，得到 %d 条", len(recs))
	}
	if err := st.Append(AuditRecord{AuditID: "aud_y", Stage: "dispatched"}); err != nil {
		t.Fatalf("注入只应影响一次 Append：%v", err)
	}
	recs, err = st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].PrevHash != nil {
		t.Fatalf("注入后的第一条记录应为链首：%+v", recs)
	}
	if err := st.VerifyChain(); err != nil {
		t.Fatalf("链校验失败：%v", err)
	}
}

func TestAuditChainDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	st, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	decisions := []string{
		`{"outcome":"auto","policyId":"pol_release","grantId":null,"approvedBy":null}`,
		`{"outcome":"deny","policyId":"pol_deduct","grantId":null,"approvedBy":null}`,
		`{"outcome":"approve","policyId":"pol_deduct","grantId":null,"approvedBy":"user:alice"}`,
	}
	for i, d := range decisions {
		rec := AuditRecord{
			AuditID:  fmt.Sprintf("aud_%d", i+1),
			Stage:    StageResult,
			PlanID:   "pln_1",
			Decision: json.RawMessage(d),
			Result:   json.RawMessage(`{"status":"ok","outputHash":null,"errorClass":null}`),
		}
		if err := st.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.VerifyChain(); err != nil {
		t.Fatalf("三条记录应能通过校验：%v", err)
	}

	// 手工篡改中间一条的 decision：deny → auto（越权被悄悄改写成自动放行）。
	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("期望 3 行，得到 %d", len(lines))
	}
	orig := strings.Join(lines, "\n") + "\n"
	tampered := strings.Replace(orig, `"outcome":"deny"`, `"outcome":"auto"`, 1)
	if tampered == orig {
		t.Fatal("篡改未生效：第 2 条 decision 里没有 deny")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	err = st.VerifyChain()
	if err == nil {
		t.Fatal("decision 被改过，VerifyChain 必须报错")
	}
	var ce *ChainError
	if !errors.As(err, &ce) {
		t.Fatalf("错误应可断言为 *ChainError，得到 %T：%v", err, err)
	}
	if ce.Index != 2 {
		t.Fatalf("断链位置应为第 2 条，得到第 %d 条（%v）", ce.Index, err)
	}
	if !strings.Contains(err.Error(), "第 2 条") || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("错误信息应指出第 2 条且说明 hash 不符：%v", err)
	}

	// 打开损坏的日志必须直接拒绝接链，而不是接着写出一条自洽但整体断裂的链。
	if _, err := NewAuditStore(path); err == nil {
		t.Fatal("既有日志已断链，NewAuditStore 必须拒绝")
	} else if !strings.Contains(err.Error(), "拒绝接链") {
		t.Fatalf("错误信息应说明拒绝接链：%v", err)
	}
}

func TestAuditChainDetectsDeletedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	st, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i := 0; i < 3; i++ {
		if err := st.Append(AuditRecord{AuditID: fmt.Sprintf("aud_d%d", i), Stage: StageResult, PlanID: "pln_1"}); err != nil {
			t.Fatal(err)
		}
	}
	lines := readLines(t, path)
	// 删掉中间那条：剩下的第二条 prevHash 指向已不存在的记录。
	kept := []string{lines[0], lines[2]}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = st.VerifyChain()
	var ce *ChainError
	if !errors.As(err, &ce) {
		t.Fatalf("删掉中间记录后应断链，得到 %v", err)
	}
	if ce.Index != 2 {
		t.Fatalf("断链位置应为第 2 条，得到第 %d 条", ce.Index)
	}
	if !strings.Contains(ce.Reason, "prevHash") {
		t.Fatalf("原因应指出 prevHash 接不上：%s", ce.Reason)
	}
}

func TestAuditQueryFilterLimitAndCursor(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 6; i++ {
		plan := "pln_a"
		if i%2 == 1 {
			plan = "pln_b"
		}
		rec := AuditRecord{AuditID: fmt.Sprintf("aud_q%d", i), Stage: StageResult, PlanID: plan}
		if err := st.Append(rec); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("全量应为 6 条，得到 %d", len(all))
	}

	onlyA, err := st.Query(Query{PlanID: "pln_a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 3 {
		t.Fatalf("pln_a 应为 3 条，得到 %d", len(onlyA))
	}
	for _, rec := range onlyA {
		if rec.PlanID != "pln_a" {
			t.Fatalf("planId 过滤失效：%+v", rec)
		}
	}

	limited, err := st.Query(Query{PlanID: "pln_a", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit 应截断到 2 条，得到 %d", len(limited))
	}
	if limited[0].AuditID != onlyA[0].AuditID || limited[1].AuditID != onlyA[1].AuditID {
		t.Fatalf("limit 应保留最早的记录：%+v", limited)
	}

	// Since 是写入顺序上的游标（先跳行，再按 planId 过滤）。
	fromFour, err := st.Query(Query{Since: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFour) != 2 || fromFour[0].AuditID != "aud_q4" {
		t.Fatalf("since=4 应返回最后两条：%+v", fromFour)
	}
	fromOnePlanA, err := st.Query(Query{Since: 1, PlanID: "pln_a"})
	if err != nil {
		t.Fatal(err)
	}
	// 先跳掉第 0 条（pln_a），剩下的 pln_a 是第 2、4 条。
	if len(fromOnePlanA) != 2 || fromOnePlanA[0].AuditID != "aud_q2" || fromOnePlanA[1].AuditID != "aud_q4" {
		t.Fatalf("since 与 planId 应叠加生效：%+v", fromOnePlanA)
	}

	pastTail, err := st.Query(Query{Since: 99})
	if err != nil {
		t.Fatalf("游标越过末尾不应报错：%v", err)
	}
	if len(pastTail) != 0 {
		t.Fatalf("游标越过末尾应返回空结果，得到 %d 条", len(pastTail))
	}

	if _, err := st.Query(Query{Since: -1}); err == nil {
		t.Fatal("负游标必须报错")
	}
	if _, err := st.Query(Query{Limit: -1}); err == nil {
		t.Fatal("负 limit 必须报错")
	}
}

// TestAuditConcurrentAppendKeepsChain 用 50 个 goroutine 各写一条，验证串行化后
// 既不丢写也不错链。本机跑不了 -race，所以这里靠「条数 + auditId 不重复 + 全链可校验」
// 三条一起作为不丢写、不交错写入的证据。
func TestAuditConcurrentAppendKeepsChain(t *testing.T) {
	const n = 50
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	st, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := AuditRecord{
				AuditID:  fmt.Sprintf("aud_c%02d", i),
				Stage:    StageDispatched,
				PlanID:   "pln_c",
				Decision: json.RawMessage(fmt.Sprintf(`{"outcome":"auto","policyId":null,"grantId":null,"approvedBy":null,"seq":%d}`, i)),
			}
			if err := st.Append(rec); err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Append 失败：%v", err)
	}

	if err := st.VerifyChain(); err != nil {
		t.Fatalf("并发写入后链断了：%v", err)
	}
	recs, err := st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("应写入 %d 条，实际 %d 条（丢写或被覆盖）", n, len(recs))
	}
	if lines := readLines(t, path); len(lines) != n {
		t.Fatalf("磁盘上应是 %d 行（每次追加独占一行），实际 %d 行", n, len(lines))
	}
	ids := make(map[string]bool, n)
	hashes := make(map[string]bool, n)
	for _, rec := range recs {
		if ids[rec.AuditID] {
			t.Fatalf("auditId 重复：%s", rec.AuditID)
		}
		ids[rec.AuditID] = true
		if hashes[rec.Hash] {
			t.Fatalf("hash 重复（两条记录摘要相同）：%s", rec.Hash)
		}
		hashes[rec.Hash] = true
	}
}

func TestAuditReopenContinuesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	first, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := first.Append(AuditRecord{AuditID: fmt.Sprintf("aud_r%d", i), Stage: StageDispatched, PlanID: "pln_r"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("关闭第一个 Store：%v", err)
	}

	// 进程重启：新的 Store 指向同一文件，必须先接上既有链尾再写。
	second, err := NewAuditStore(path)
	if err != nil {
		t.Fatalf("重启后打开审计日志：%v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Append(AuditRecord{AuditID: "aud_r2", Stage: StageResult, PlanID: "pln_r"}); err != nil {
		t.Fatalf("重启后追加：%v", err)
	}

	recs, err := second.Query(Query{PlanID: "pln_r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("重启后应共 3 条，得到 %d", len(recs))
	}
	if recs[0].PrevHash != nil {
		t.Fatal("链首 prevHash 必须为 null")
	}
	if recs[2].PrevHash == nil || *recs[2].PrevHash != recs[1].Hash {
		t.Fatalf("重启后未接上链尾：%+v", recs[2])
	}
	if err := second.VerifyChain(); err != nil {
		t.Fatalf("重启后链校验失败：%v", err)
	}

	// 再接一次，确认接链不是一次性的巧合。
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	if err := third.Append(AuditRecord{AuditID: "aud_r3", Stage: StageResult, PlanID: "pln_r"}); err != nil {
		t.Fatal(err)
	}
	recs, err = third.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || *recs[3].PrevHash != recs[2].Hash {
		t.Fatalf("第二次重启后未接上链尾：%+v", recs)
	}
	if err := third.VerifyChain(); err != nil {
		t.Fatalf("第二次重启后链校验失败：%v", err)
	}
}

// TestAuditHashIgnoresRawFormatting 说明 hash 只取决于语义内容：同一份记录用不同排版
// （键序、空白、数字写法）写进两条独立的链，摘要必须相同。
func TestAuditHashIgnoresRawFormatting(t *testing.T) {
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	mk := func(decision, actor, effects string) AuditRecord {
		return AuditRecord{
			AuditID:  "aud_fmt",
			Stage:    StageResult,
			At:       at,
			Actor:    json.RawMessage(actor),
			PlanID:   "pln_fmt",
			Action:   "inventory.DeductStock",
			Decision: json.RawMessage(decision),
			Effects:  json.RawMessage(effects),
		}
	}
	appended := func(rec AuditRecord) AuditRecord {
		st := openTestStore(t)
		if err := st.Append(rec); err != nil {
			t.Fatal(err)
		}
		recs, err := st.Query(Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 1 {
			t.Fatalf("期望 1 条，得到 %d", len(recs))
		}
		return recs[0]
	}

	a := appended(mk(
		`{"outcome":"auto","policyId":"pol_1","grantId":null,"approvedBy":null}`,
		`{"onBehalfOf":"user:alice","id":"agent:fulfillment"}`,
		`{"entities":["inventory:SKU/STD-1001"],"documentCount":1,"quantity":12.0}`,
	))
	b := appended(mk(
		"{\n  \"policyId\" : \"pol_1\",\n  \"approvedBy\": null,\n  \"grantId\": null,\n  \"outcome\": \"auto\"\n}",
		`{"id":"agent:fulfillment","onBehalfOf":"user:alice"}`,
		`{ "quantity" : 1.2e1 , "documentCount" : 1 , "entities" : [ "inventory:SKU/STD-1001" ] }`,
	))
	if a.Hash != b.Hash {
		t.Fatalf("排版不同的同一记录摘要不一致：\n%s\n%s", a.Hash, b.Hash)
	}
}

func TestAuditAppendRejectsUnusableRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	st, err := NewAuditStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Append(AuditRecord{Stage: StageDispatched}); err == nil {
		t.Fatal("缺 auditId 必须报错")
	}
	if err := st.Append(AuditRecord{AuditID: "aud_z", Stage: "dispatch"}); err == nil {
		t.Fatal("非法 stage 必须报错")
	}
	recs, err := st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("被拒的记录不应落盘，得到 %d 条", len(recs))
	}
	// 被拒的写入不能占掉链尾。
	if err := st.Append(AuditRecord{AuditID: "aud_ok", Stage: StageDispatched}); err != nil {
		t.Fatal(err)
	}
	recs, err = st.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].PrevHash != nil {
		t.Fatalf("被拒后写入的第一条应为链首：%+v", recs)
	}
}

// TestAuditWriteFailurePoisonsStore 覆盖真实 I/O 失败：内存链尾可能与磁盘不一致，
// 此时继续追加会写出断链，所以 Store 必须自我失效，要求重新打开并校验。
func TestAuditWriteFailurePoisonsStore(t *testing.T) {
	st := openTestStore(t)
	if err := st.Append(AuditRecord{AuditID: "aud_p0", Stage: StageDispatched}); err != nil {
		t.Fatal(err)
	}
	// 绕过 Close 直接关掉底层句柄，制造一次真实写失败。
	if err := st.f.Close(); err != nil {
		t.Fatalf("关闭底层文件：%v", err)
	}
	if err := st.Append(AuditRecord{AuditID: "aud_p1", Stage: StageDispatched}); err == nil {
		t.Fatal("底层文件已关闭，Append 必须报错")
	}
	err := st.Append(AuditRecord{AuditID: "aud_p2", Stage: StageDispatched})
	if err == nil || !strings.Contains(err.Error(), "失效") {
		t.Fatalf("写失败后 Store 必须失效并说明原因，得到 %v", err)
	}
}

func TestCanonicalJSON(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"键排序", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"去空白与嵌套", `{"b": [true, false, null], "a": 1}`, `{"a":1,"b":[true,false,null]}`},
		{"整数写法归一", `{"n":1000.0}`, `{"n":1000}`},
		{"指数写法归一", `{"n":1e3}`, `{"n":1000}`},
		{"小数", `{"n":1.5}`, `{"n":1.5}`},
		{"超过 21 位用指数", `{"n":1e21}`, `{"n":1e+21}`},
		{"小于 1e-6 用指数", `{"n":1e-7}`, `{"n":1e-7}`},
		{"1e-6 仍用小数", `{"n":0.000001}`, `{"n":0.000001}`},
		{"负零", `{"n":-0}`, `{"n":0}`},
		{"负指数", `{"n":-2.5e-8}`, `{"n":-2.5e-8}`},
		{"字符串必需转义", `{"s":"a\"b\\c\nd\te\u0001f"}`, `{"s":"a\"b\\c\nd\te\u0001f"}`},
		{"非 ASCII 原样", `{"s":"中文/é"}`, `{"s":"中文/é"}`},
		{"键按 UTF-16 码元序", `{"\uffff":1,"\ud83d\ude00":2}`, "{\"\U0001F600\":2,\"\uFFFF\":1}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalJSONString(tc.in)
			if err != nil {
				t.Fatalf("规范化 %s：%v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("规范化 %s\n得到 %s\n期望 %s", tc.in, got, tc.want)
			}
			// 规范化必须幂等：验链时会对已规范化的记录再算一次。
			again, err := canonicalJSONString(got)
			if err != nil {
				t.Fatalf("二次规范化 %s：%v", got, err)
			}
			if again != got {
				t.Fatalf("规范化不幂等：%s → %s", got, again)
			}
		})
	}

	for _, bad := range []string{`{"a":}`, `1 2`, `{"n":1e999}`, ``, `{"n":01}`} {
		if _, err := canonicalJSONString(bad); err == nil {
			t.Errorf("非法输入 %q 应报错", bad)
		}
	}
}

// canonicalJSONString 是测试侧的小包装：解析 → 规范化 → 转字符串。
func canonicalJSONString(raw string) (string, error) {
	v, err := decodeJSON([]byte(raw))
	if err != nil {
		return "", err
	}
	out, err := encodeCanonical(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
