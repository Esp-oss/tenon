package registrylog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// baseTime 固定写入时刻，让断言里的 at 是确定值（2023-11-14T22:13:20Z）。
var baseTime = time.Unix(1700000000, 0)

// newLog 在临时目录里起一条事件链，并在测试结束前关掉文件句柄：
// Windows 上还开着的句柄会让 t.TempDir 的清理失败。
func newLog(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry-events.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("打开事件链失败：%v", err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("关闭事件链失败：%v", err)
		}
	})
	return l, path
}

// readLines 读出文件里的非空行，顺带证明落盘形态是可逐行解析的 JSONL。
func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取事件链失败：%v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// decodeEntries 把落盘的每一行解析回记录，用于断言链上的 hash 关系。
func decodeEntries(t *testing.T, path string) []entry {
	t.Helper()
	lines := readLines(t, path)
	out := make([]entry, 0, len(lines))
	for i, line := range lines {
		var rec entry
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("第 %d 行不是合法记录：%v（%s）", i+1, err, line)
		}
		out = append(out, rec)
	}
	return out
}

func appendThree(t *testing.T, l *Log) {
	t.Helper()
	for _, ev := range []Event{
		{Event: EventRegistered, ModuleID: "inventory", DeclarationHash: "sha256:aaa"},
		{Event: EventLost, ModuleID: "inventory"},
		{Event: EventRecovered, ModuleID: "inventory"},
	} {
		if err := l.Append(ev, baseTime); err != nil {
			t.Fatalf("追加 %s 失败：%v", ev.Event, err)
		}
	}
}

// TestAppendAndVerifyChain 是计划 Task 5 的主用例：连写三条后链自洽；改掉中间一条即断。
func TestAppendAndVerifyChain(t *testing.T) {
	l, path := newLog(t)
	appendThree(t, l)

	if err := Verify(path); err != nil {
		t.Fatalf("刚写完的链应通过校验：%v", err)
	}

	// 篡改任一行 → 校验失败。这里把第二条的 lost 改成 recovered（内容变了、hash 没变）。
	lines := readLines(t, path)
	if got := strings.Replace(lines[1], `"lost"`, `"recovered"`, 1); got == lines[1] {
		t.Fatalf("第二行里没有可篡改的 lost：%s", lines[1])
	} else {
		lines[1] = got
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("回写被篡改的链失败：%v", err)
	}
	if err := Verify(path); err == nil {
		t.Fatal("篡改后必须校验失败")
	}
}

// TestVerifyReportsLineNumber 锁住「错误里给行号」：审计要看的是第几行，不是「链坏了」。
func TestVerifyReportsLineNumber(t *testing.T) {
	l, path := newLog(t)
	appendThree(t, l)
	if err := l.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	lines := readLines(t, path)
	lines[2] = strings.Replace(lines[2], `"recovered"`, `"lost"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("回写失败：%v", err)
	}

	err := Verify(path)
	var chainErr *ChainError
	if !errors.As(err, &chainErr) {
		t.Fatalf("期望 *ChainError，得到 %v", err)
	}
	// 行号按物理行算：改的是第三条，就该说第 3 行。
	if chainErr.Line != 3 {
		t.Fatalf("行号应为 3，得到 %d（%v）", chainErr.Line, chainErr)
	}
	if !strings.Contains(err.Error(), "第 3 行") {
		t.Fatalf("错误文本里应点明行号：%v", err)
	}
	if !strings.Contains(chainErr.Reason, "hash") {
		t.Fatalf("原因应指出 hash 不符：%v", chainErr)
	}
}

// TestVerifyDetectsDeletedRecord 覆盖另一类断裂：记录被删 → prevHash 接不上。
func TestVerifyDetectsDeletedRecord(t *testing.T) {
	l, path := newLog(t)
	appendThree(t, l)
	if err := l.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	lines := readLines(t, path)
	// 删掉第一条：新首行的 prevHash 仍指向那条已经不在的 hash。
	if err := os.WriteFile(path, []byte(strings.Join(lines[1:], "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("回写失败：%v", err)
	}

	err := Verify(path)
	var chainErr *ChainError
	if !errors.As(err, &chainErr) {
		t.Fatalf("期望 *ChainError，得到 %v", err)
	}
	if chainErr.Line != 1 {
		t.Fatalf("断裂应报在第 1 行，得到 %d（%v）", chainErr.Line, chainErr)
	}
	if !strings.Contains(chainErr.Reason, "prevHash") {
		t.Fatalf("原因应指出 prevHash 断链：%v", chainErr)
	}
}

// TestAppendWritesCanonicalRecord 锁住落盘形态：键按 JCS 排序、无多余空白、at 折成 UTC、
// 链首 prevHash 为 null。第三方要能照着这一行的字节独立复算 hash。
func TestAppendWritesCanonicalRecord(t *testing.T) {
	l, path := newLog(t)
	// 用 +08:00 的时刻写入，落盘必须是同一瞬间的 UTC 表示。
	beijing := time.Date(2026, 10, 27, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if err := l.Append(Event{Event: EventRegistered, ModuleID: "inventory", DeclarationHash: "sha256:aaa"}, beijing); err != nil {
		t.Fatalf("追加失败：%v", err)
	}

	line := readLines(t, path)[0]
	if !json.Valid([]byte(line)) {
		t.Fatalf("落盘行不是合法 JSON：%s", line)
	}
	// 键序：at < declarationHash < event < hash < moduleId < prevHash（UTF-16 码元序）。
	if !strings.HasPrefix(line, `{"at":"2026-10-27T00:00:00Z","declarationHash":"sha256:aaa","event":"registered","hash":"sha256:`) {
		t.Fatalf("行首形状不对：%s", line)
	}
	if !strings.HasSuffix(line, `","moduleId":"inventory","prevHash":null}`) {
		t.Fatalf("行尾形状不对（链首 prevHash 应为 null）：%s", line)
	}
	if strings.ContainsAny(line, " \t") {
		t.Fatalf("规范 JSON 不该有空白：%q", line)
	}
}

// TestOpenResumesChainFromTail 锁住「Open 从末行 hash 接链」：重启后写出的第四条必须
// 接在第三条的 hash 上，否则重启就断链。
func TestOpenResumesChainFromTail(t *testing.T) {
	l, path := newLog(t)
	appendThree(t, l)
	if err := l.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	l2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败：%v", err)
	}
	t.Cleanup(func() {
		if err := l2.Close(); err != nil {
			t.Errorf("关闭失败：%v", err)
		}
	})
	if err := l2.Append(Event{Event: EventManualDisabled, ModuleID: "inventory"}, baseTime); err != nil {
		t.Fatalf("接链追加失败：%v", err)
	}

	if err := Verify(path); err != nil {
		t.Fatalf("重启接链后应通过校验：%v", err)
	}
	entries := decodeEntries(t, path)
	if len(entries) != 4 {
		t.Fatalf("应有 4 条记录，得到 %d", len(entries))
	}
	if entries[0].PrevHash != nil {
		t.Fatalf("链首 prevHash 应为 null，得到 %v", *entries[0].PrevHash)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].PrevHash == nil || *entries[i].PrevHash != entries[i-1].Hash {
			t.Fatalf("第 %d 条的 prevHash 没接上第 %d 条：%+v", i+1, i, entries[i])
		}
	}
}

// TestOpenRejectsBrokenTail：末行坏了还接着写，会把「链从某处断了」掩盖成「一切正常」，
// 所以 Open 必须拒绝。
func TestOpenRejectsBrokenTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry-events.jsonl")
	// 半截行：像写到一半崩掉的样子。
	if err := os.WriteFile(path, []byte(`{"at":"2023-11-14T22:13:20Z","event":"registered"`+"\n"), 0o600); err != nil {
		t.Fatalf("准备坏文件失败：%v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("末行无法解析时 Open 应拒绝接链")
	}

	// 末行能解析但没有 hash 同样拒绝。
	noHash := filepath.Join(t.TempDir(), "registry-events.jsonl")
	if err := os.WriteFile(noHash, []byte(`{"at":"2023-11-14T22:13:20Z","event":"registered","moduleId":"inventory","prevHash":null}`+"\n"), 0o600); err != nil {
		t.Fatalf("准备坏文件失败：%v", err)
	}
	if _, err := Open(noHash); err == nil {
		t.Fatal("末行缺少 hash 时 Open 应拒绝接链")
	}
}

// TestAppendRejectsEmptyFields：没有事件类型或没有模块 id 的记录没有诊断价值，写进去只会
// 让审计面变成噪声。
func TestAppendRejectsEmptyFields(t *testing.T) {
	l, path := newLog(t)
	if err := l.Append(Event{ModuleID: "inventory"}, baseTime); err == nil {
		t.Fatal("缺少 event 类型应报错")
	}
	if err := l.Append(Event{Event: EventRegistered}, baseTime); err == nil {
		t.Fatal("缺少 moduleId 应报错")
	}
	if got := len(readLines(t, path)); got != 0 {
		t.Fatalf("被拒的记录不应落盘，得到 %d 行", got)
	}
}

// TestAppendAfterCloseFails：关闭后继续写是编程错误，必须报错而不是写进已失效的句柄。
func TestAppendAfterCloseFails(t *testing.T) {
	l, _ := newLog(t)
	if err := l.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if err := l.Append(Event{Event: EventRegistered, ModuleID: "inventory"}, baseTime); err == nil {
		t.Fatal("关闭后 Append 应报错")
	}
	// 重复关闭是幂等的：清理路径不该因为关两次而报错。
	if err := l.Close(); err != nil {
		t.Fatalf("重复关闭应幂等：%v", err)
	}
}

// TestAppendWriteFailureKeepsChainState 锁住计划里的那条硬要求：写失败返回错误，且不改
// 内存态（链尾不动）。半行可能已经落盘，所以之后必须拒绝继续追加。
func TestAppendWriteFailureKeepsChainState(t *testing.T) {
	l, path := newLog(t)
	if err := l.Append(Event{Event: EventRegistered, ModuleID: "inventory"}, baseTime); err != nil {
		t.Fatalf("首次追加失败：%v", err)
	}

	l.mu.Lock()
	tailBefore := *l.tail
	l.mu.Unlock()

	// 把写句柄换成只读句柄：磁盘满、权限变化、句柄被外力收走都是这个样子。
	ro, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("打开只读句柄失败：%v", err)
	}
	l.mu.Lock()
	old := l.f
	l.f = ro
	l.mu.Unlock()
	if err := old.Close(); err != nil {
		t.Fatalf("关掉原句柄失败：%v", err)
	}

	if err := l.Append(Event{Event: EventLost, ModuleID: "inventory"}, baseTime); err == nil {
		t.Fatal("写失败应返回错误")
	}

	l.mu.Lock()
	tailAfter, broken := l.tail, l.broken
	l.mu.Unlock()
	if tailAfter == nil || *tailAfter != tailBefore {
		t.Fatalf("写失败不得推进内存链尾：期望 %s，得到 %s", tailBefore, describeHash(tailAfter))
	}
	if broken == nil {
		t.Fatal("写失败后应置 broken，拒绝继续追加")
	}
	if err := l.Append(Event{Event: EventRecovered, ModuleID: "inventory"}, baseTime); err == nil {
		t.Fatal("失效后应拒绝继续追加")
	}
	// 只读句柄一个字节也没写进去，既有链必须仍然完好。
	if err := Verify(path); err != nil {
		t.Fatalf("既有链应完好：%v", err)
	}
}

// TestVerifyEmptyAndMissingFile：空链合法（还没发生过跃迁），文件不存在则报错——
// 「验不了」不等于「校验通过」。
func TestVerifyEmptyAndMissingFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "registry-events.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("准备空文件失败：%v", err)
	}
	if err := Verify(empty); err != nil {
		t.Fatalf("空链应通过校验：%v", err)
	}

	if err := Verify(filepath.Join(t.TempDir(), "not-there.jsonl")); err == nil {
		t.Fatal("文件不存在时 Verify 应报错")
	}
}

// TestVerifyToleratesBlankLines：写入总以换行结尾，切分产物里的空串不该算断链；
// 手写工具顺手多敲的空行同理。
func TestVerifyToleratesBlankLines(t *testing.T) {
	l, path := newLog(t)
	appendThree(t, l)
	if err := l.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	lines := readLines(t, path)
	body := strings.Join(lines, "\n\n") + "\n\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("回写失败：%v", err)
	}
	if err := Verify(path); err != nil {
		t.Fatalf("空行不该让链断裂：%v", err)
	}
}

// TestConcurrentAppendProducesValidChain 交给 -race 判定数据竞争，同时断言并发追加之后
// 整个文件仍是一条自洽的链（每行的 prevHash 都接得上，hash 都对得上）。
func TestConcurrentAppendProducesValidChain(t *testing.T) {
	l, path := newLog(t)
	const (
		workers = 8
		perWork = 20
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWork; i++ {
				ev := Event{Event: EventLost, ModuleID: "inventory"}
				if i%2 == 0 {
					ev = Event{Event: EventRecovered, ModuleID: "inventory"}
				}
				if err := l.Append(ev, baseTime); err != nil {
					t.Errorf("并发追加失败：%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := Verify(path); err != nil {
		t.Fatalf("并发追加后链应自洽：%v", err)
	}
	if got, want := len(readLines(t, path)), workers*perWork; got != want {
		t.Fatalf("记录数 = %d，期望 %d（每次 Append 恰好一行，不丢不重）", got, want)
	}
	// 事件类型只该出现这两种；不排序地比对次数，避免把链序误当成断言点。
	entries := decodeEntries(t, path)
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		kinds = append(kinds, e.Event)
	}
	slices.Sort(kinds)
	want := slices.Repeat([]string{EventLost, EventRecovered}, workers*perWork/2)
	slices.Sort(want)
	if !slices.Equal(kinds, want) {
		t.Fatalf("事件类型分布不对：%v", kinds)
	}
}
