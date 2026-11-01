package module

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

const (
	// testRegistrySecret 是假宿主与推送器共享的密钥材料。
	testRegistrySecret = "registry-test-secret"
	// testManifest 是一份最小的合法能力摘要（键序刻意不是字典序：
	// 声明哈希必须先规范化再求值，直接对原始字节求 sha256 会得到另一个值）。
	testManifest = `{"tenon":"0.1","module":{"id":"inventory","version":"1.0.0","title":"库存"},` +
		`"actions":[],"events":[],"transports":["http"],"capabilities":[]}`
	// testInterval 是测试用的心跳间隔。比 5s 小两个数量级，用例才可能在毫秒级
	// 观察到一个完整周期；它只经由 ClientConfig 注入，不改变生产默认值。
	testInterval = 50 * time.Millisecond
)

// call 是一次收到的请求。断言「注册几次、心跳几次、签名与报文对不对」都靠它。
type call struct {
	path string
	body []byte
	sig  string
}

// fakeHost 是 httptest 假宿主：记录每一次请求，并允许用例按需改写回执。
type fakeHost struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []call
	respond func(call) (int, []byte)
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败：%v", err)
		}
		got := call{path: r.URL.Path, body: body, sig: r.Header.Get(protocol.HeaderSignature)}

		h.mu.Lock()
		h.calls = append(h.calls, got)
		respond := h.respond
		h.mu.Unlock()

		// 默认回执：注册 200 {"accepted":true}、心跳 200 {"alive":true}。推送器只看
		// 状态码与 409 的 reason，所以一个通用回执就够；需要 409/5xx 的用例自己接管。
		status, payload := http.StatusOK, []byte(`{"accepted":true}`)
		if respond != nil {
			if s, p := respond(got); s != 0 {
				status, payload = s, p
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// setRespond 接管回执。回调在锁外执行（它常要读计数器），所以这里只换函数指针。
func (h *fakeHost) setRespond(fn func(call) (int, []byte)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.respond = fn
}

// snapshot 复制一份已收到的请求，避免断言时与后台推送循环争锁。
func (h *fakeHost) snapshot() []call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]call(nil), h.calls...)
}

// waitFor 轮询等待条件成立。推送循环在后台跑，「注册发生过」只能等它发生，
// 睡固定时长不是等——它会让用例在慢机器上假绿。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func countOf(calls []call, path string) int {
	n := 0
	for _, c := range calls {
		if c.path == path {
			n++
		}
	}
	return n
}

// hashOf 按规范独立算一遍声明哈希：sha256: + hex(sha256(JCS(manifest)))。
//
// 刻意不复用生产代码里的 declarationHash：这里要证明的是「同一个算法在两侧得出同一个
// 值」，用同一个函数算两遍只能证明它自己等于自己。
func hashOf(t *testing.T, manifest []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(manifest, &v); err != nil {
		t.Fatalf("manifest 不是合法 JSON：%v", err)
	}
	canon, err := protocol.CanonicalJSON(v)
	if err != nil {
		t.Fatalf("manifest 无法规范化：%v", err)
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decodeRegister 把注册报文解析成独立的匿名结构：用它而不是生产结构体，才能查出
// 「生产侧的 json 标签写错了、报文与 host 期望的字段名对不上」这类错误。
func decodeRegister(t *testing.T, body []byte) (id, baseURL string, manifest json.RawMessage, hash string) {
	t.Helper()
	var got struct {
		Module struct {
			ID       string          `json:"id"`
			BaseURL  string          `json:"baseURL"`
			Manifest json.RawMessage `json:"manifest"`
		} `json:"module"`
		DeclarationHash string `json:"declarationHash"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("注册报文不是预期的 {module:{id,baseURL,manifest},declarationHash}：%v（原文 %s）", err, body)
	}
	return got.Module.ID, got.Module.BaseURL, got.Module.Manifest, got.DeclarationHash
}

// newTestClient 装配一个指向假宿主的推送器（不启动）。
func newTestClient(host string) *RegistryClient {
	return NewRegistryClient(ClientConfig{
		Host:     host,
		ModuleID: "inventory",
		BaseURL:  "http://127.0.0.1:8081",
		Manifest: []byte(testManifest),
		Secret:   []byte(testRegistrySecret),
		Interval: testInterval,
	})
}

// TestRegistryClientRegistersOnStart 覆盖 (a)：启动即注册一次，且只在启动时注册。
func TestRegistryClientRegistersOnStart(t *testing.T) {
	fake := newFakeHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTestClient(fake.srv.URL + "/").Start(ctx)

	waitFor(t, "启动后应发一次注册", func() bool {
		return countOf(fake.snapshot(), registryRegisterPath) >= 1
	})
	first := fake.snapshot()[0]
	if first.path != registryRegisterPath {
		t.Fatalf("首个请求应是注册，实际是 %s", first.path)
	}
	// 报文形状与内容：id/baseURL 原样上报，manifest 随报文同行，哈希与这份 manifest 一致。
	id, baseURL, manifest, hash := decodeRegister(t, first.body)
	if id != "inventory" || baseURL != "http://127.0.0.1:8081" {
		t.Fatalf("注册报文应上报 id=inventory、baseURL=http://127.0.0.1:8081，得到 id=%q baseURL=%q", id, baseURL)
	}
	if !json.Valid(manifest) {
		t.Fatalf("注册报文里的 manifest 不是合法 JSON：%s", manifest)
	}
	if want := hashOf(t, manifest); hash != want {
		t.Fatalf("注册报文带的哈希应与它带来的 manifest 对得上：得到 %s，期望 %s", hash, want)
	}

	// 心跳发生之后，注册次数仍必须是 1：注册是「申报」，不是每个周期都重来一遍。
	waitFor(t, "注册后应开始心跳", func() bool {
		return countOf(fake.snapshot(), registryHeartbeatPath) >= 1
	})
	if got := countOf(fake.snapshot(), registryRegisterPath); got != 1 {
		t.Fatalf("心跳正常时不应重复注册，注册次数 %d", got)
	}
}

// TestRegistryClientHeartbeatsOnInterval 覆盖 (b)：心跳按 Interval 周期触发。
func TestRegistryClientHeartbeatsOnInterval(t *testing.T) {
	fake := newFakeHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTestClient(fake.srv.URL).Start(ctx)

	// 至少 3 次心跳才说明它是「周期」而不是「一次」。50ms 间隔下约 150ms 到齐。
	waitFor(t, "应持续按周期心跳", func() bool {
		return countOf(fake.snapshot(), registryHeartbeatPath) >= 3
	})

	var beat call
	for _, c := range fake.snapshot() {
		if c.path == registryHeartbeatPath {
			beat = c
			break
		}
	}
	var got struct {
		ModuleID        string `json:"moduleId"`
		DeclarationHash string `json:"declarationHash"`
	}
	if err := json.Unmarshal(beat.body, &got); err != nil {
		t.Fatalf("心跳报文不是预期的 {moduleId,declarationHash}：%v（原文 %s）", err, beat.body)
	}
	if got.ModuleID != "inventory" {
		t.Fatalf("心跳应带 moduleId=inventory，得到 %q", got.ModuleID)
	}
	// 心跳带的是**规范化后**的声明哈希（不是原始字节的 sha256）：host 会拿它和注册时
	// 登记的哈希逐字比较，两侧算法必须同一个。
	if want := hashOf(t, []byte(testManifest)); got.DeclarationHash != want {
		t.Fatalf("心跳的声明哈希 = %s，期望 %s", got.DeclarationHash, want)
	}
	// 心跳不带完整声明（D3）：省流量，且完整声明只在注册时给过。
	if strings.Contains(string(beat.body), `"manifest"`) {
		t.Fatalf("心跳不应携带完整 manifest：%s", beat.body)
	}
}

// TestRegistryClientReRegistersOn409 覆盖 (c)：收到 409 立即重申报，不等到下一个周期。
func TestRegistryClientReRegistersOn409(t *testing.T) {
	fake := newFakeHost(t)
	var beats atomic.Int32
	fake.setRespond(func(c call) (int, []byte) {
		if c.path == registryHeartbeatPath && beats.Add(1) <= 2 {
			return http.StatusConflict, []byte(`{"reRegister":true,"reason":"unknown-module"}`)
		}
		return 0, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTestClient(fake.srv.URL).Start(ctx)

	waitFor(t, "两次 409 之后应重申报两次（启动 1 次 + 重申报 2 次）", func() bool {
		return countOf(fake.snapshot(), registryRegisterPath) >= 3
	})

	// 请求是串行发出的，所以顺序本身就能证明「立即」：409 心跳的**下一个**请求必须是
	// 注册。若推送器只是等下个周期，这里会先出现又一次心跳。
	calls := fake.snapshot()
	want := []string{registryRegisterPath, registryHeartbeatPath, registryRegisterPath,
		registryHeartbeatPath, registryRegisterPath}
	for i, path := range want {
		if i >= len(calls) {
			t.Fatalf("请求序列太短（%d 条），期望至少前 %d 条为 %v", len(calls), len(want), want)
		}
		if calls[i].path != path {
			t.Fatalf("第 %d 个请求应是 %s，实际是 %s（完整序列见下）\n%s",
				i+1, path, calls[i].path, pathsOf(calls))
		}
	}
}

// pathsOf 把收到的请求路径拼成一行，失败时贴出来比列表更好读。
func pathsOf(calls []call) string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.path)
	}
	return strings.Join(out, " → ")
}

// TestRegistryClientSignsEveryRequest 覆盖 (d)：签名头存在，且能被 protocol.Verify 验过。
func TestRegistryClientSignsEveryRequest(t *testing.T) {
	fake := newFakeHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTestClient(fake.srv.URL).Start(ctx)

	waitFor(t, "应有注册与心跳可供验签", func() bool {
		calls := fake.snapshot()
		return countOf(calls, registryRegisterPath) >= 1 && countOf(calls, registryHeartbeatPath) >= 2
	})
	cancel()

	calls := fake.snapshot()
	for i, c := range calls {
		if c.sig == "" {
			t.Fatalf("第 %d 个请求（%s）缺少 %s 头", i+1, c.path, protocol.HeaderSignature)
		}
		// 验签必须基于**原始字节**：先反序列化再序列化会让签名必然对不上，
		// 而这条断言正好能抓住那种「看起来发了签名、其实覆盖的是另一串字节」的实现。
		if err := protocol.Verify([]byte(testRegistrySecret), c.sig, c.body, time.Now(), protocol.SignatureTolerance); err != nil {
			t.Fatalf("第 %d 个请求（%s）的签名验不过：%v", i+1, c.path, err)
		}
	}
	// 反面断言：换一把密钥必须验不过。否则上面那条「验过了」可能只是 Verify 没在验。
	if err := protocol.Verify([]byte("not-the-secret"), calls[0].sig, calls[0].body, time.Now(), protocol.SignatureTolerance); err == nil {
		t.Fatal("换一把密钥也应验签失败，说明这条断言确实在校验签名")
	}
}

// TestRegistryClientStopsAfterContextCancel 覆盖 (e)：ctx 取消后不再有任何请求。
func TestRegistryClientStopsAfterContextCancel(t *testing.T) {
	fake := newFakeHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	newTestClient(fake.srv.URL).Start(ctx)

	waitFor(t, "取消前应已有注册与心跳", func() bool {
		calls := fake.snapshot()
		return countOf(calls, registryRegisterPath) >= 1 && countOf(calls, registryHeartbeatPath) >= 2
	})
	cancel()

	// 先给在途请求一点收尾时间，再取基线：否则「取消瞬间那条请求」可能落在两次快照
	// 之间，把「已经停了」误报成「还在发」。
	time.Sleep(100 * time.Millisecond)
	base := len(fake.snapshot())
	// 4 个 interval 的空窗：如果循环没退出，这里至少会多出 4 条心跳。
	time.Sleep(4 * testInterval)
	if got := len(fake.snapshot()); got != base {
		t.Fatalf("ctx 取消后不应再发请求：%d 条 → %d 条", base, got)
	}
}

// TestRegistryClientRetriesAfterServerError 补一条：非 2xx 只记日志，按间隔重试，
// 且**不阻塞**调用方（Start 已经在第一次失败时就返回了）。
func TestRegistryClientRetriesAfterServerError(t *testing.T) {
	fake := newFakeHost(t)
	var attempts atomic.Int32
	fake.setRespond(func(c call) (int, []byte) {
		// 前两次注册都失败（host 还没起好），之后成功。
		if c.path == registryRegisterPath && attempts.Add(1) <= 2 {
			return http.StatusServiceUnavailable, []byte(`{"tenonError":{"code":"NOT_READY"}}`)
		}
		return 0, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTestClient(fake.srv.URL).Start(ctx)

	waitFor(t, "注册失败后应按间隔重试并最终成功", func() bool {
		return countOf(fake.snapshot(), registryRegisterPath) >= 3
	})
	// 注册成功之后才开始心跳：没注册上就发心跳只会换回一个 409 unknown-module。
	waitFor(t, "注册成功后才开始心跳", func() bool {
		return countOf(fake.snapshot(), registryHeartbeatPath) >= 1
	})
}

// TestDeclarationHashIsCanonical 是声明哈希的单元断言：键序、空白、数值写法都不能
// 改变结果——host 侧用的是同一份 JCS 规范化，两侧对不上就会永远注册失败。
func TestDeclarationHashIsCanonical(t *testing.T) {
	a, err := declarationHash([]byte(`{"b":1,"a":2,"t":"库存"}`))
	if err != nil {
		t.Fatalf("算声明哈希失败：%v", err)
	}
	b, err := declarationHash([]byte("{ \"t\" : \"库存\",\n  \"a\": 2.0, \"b\": 1 }"))
	if err != nil {
		t.Fatalf("算声明哈希失败：%v", err)
	}
	if a != b {
		t.Fatalf("同一份声明的不同写法必须得到同一个哈希：%s vs %s", a, b)
	}
	// 内容真的变了（多一个字段）→ 哈希必须变。上面那条「不同写法同一个哈希」若没有
	// 这一条配着，就分不清「规范化生效」和「哈希根本不看内容」。
	c, err := declarationHash([]byte(`{"a":2,"b":1,"t":"库存","extra":true}`))
	if err != nil {
		t.Fatalf("算声明哈希失败：%v", err)
	}
	if a == c {
		t.Fatal("内容不同的声明不应得到同一个哈希")
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(a) {
		t.Fatalf("声明哈希的线上形状必须是 sha256:<64 位小写十六进制>，得到 %s", a)
	}
	if _, err := declarationHash([]byte(`不是 JSON`)); err == nil {
		t.Fatal("manifest 不是 JSON 时应报错，而不是算出一个哈希")
	}
}

// TestBaseURLForAddr 覆盖「监听地址 → 自报地址」的折叠：通配地址必须折成 127.0.0.1，
// 否则 host 会拿一个自己拨不通的地址去派回合。
func TestBaseURLForAddr(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{":8081", "http://127.0.0.1:8081"},
		{"0.0.0.0:8081", "http://127.0.0.1:8081"},
		{"127.0.0.1:8082", "http://127.0.0.1:8082"},
		{"localhost:8083", "http://localhost:8083"},
		{"[::1]:8084", "http://[::1]:8084"},
		{"[::]:8085", "http://127.0.0.1:8085"},
		{"http://example.com:9000", "http://example.com:9000"},
		{"", ""},
	}
	for _, c := range cases {
		if got := BaseURLForAddr(c.addr); got != c.want {
			t.Fatalf("BaseURLForAddr(%q) = %q，期望 %q", c.addr, got, c.want)
		}
	}
}

// TestStartRegistrySkipsWithoutHost 断言「未设置 TENON_HOST 就跳过，但不是静默跳过」：
// 返回值是 (nil, nil)，警告由 StartRegistry 打进日志。
func TestStartRegistrySkipsWithoutHost(t *testing.T) {
	t.Setenv(EnvHost, "")
	f := newFixture()
	client, err := f.module.StartRegistry(context.Background(), "http://127.0.0.1:8081")
	if err != nil {
		t.Fatalf("未设置 %s 是合法形态，不该返回错误：%v", EnvHost, err)
	}
	if client != nil {
		t.Fatalf("未设置 %s 时不应启动推送器", EnvHost)
	}
}

// TestStartRegistryFromEnv 覆盖 main 的接线路径：TENON_HOST + 既有密钥读取逻辑 +
// 模块自己产出的 manifest，最终发出的报文必须能被 host 侧同一套算法接受。
func TestStartRegistryFromEnv(t *testing.T) {
	fake := newFakeHost(t)
	secret := prepareEnv(t) // 写入 HMAC 密钥文件并指向它（复用模块侧既有读取逻辑）
	f := newFixture()
	t.Setenv(EnvHost, fake.srv.URL)
	t.Setenv(EnvHeartbeatInterval, "40ms")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, err := f.module.StartRegistry(ctx, "http://127.0.0.1:8081")
	if err != nil {
		t.Fatalf("装配注册客户端失败：%v", err)
	}
	if client == nil {
		t.Fatalf("设置了 %s 时应启动推送器", EnvHost)
	}
	// 覆盖值生效：40ms 而不是默认 5s，否则这条用例要跑几秒。
	if client.interval != 40*time.Millisecond {
		t.Fatalf("心跳间隔应取 %s 的覆盖值 40ms，得到 %s", EnvHeartbeatInterval, client.interval)
	}

	waitFor(t, "应发一次注册", func() bool {
		return countOf(fake.snapshot(), registryRegisterPath) >= 1
	})
	first := fake.snapshot()[0]
	id, baseURL, manifest, hash := decodeRegister(t, first.body)
	if id != fixtureModuleID {
		t.Fatalf("注册报文应上报模块自身的 id %q，得到 %q", fixtureModuleID, id)
	}
	if baseURL != "http://127.0.0.1:8081" {
		t.Fatalf("注册报文应原样上报 baseURL，得到 %q", baseURL)
	}
	// manifest 必须是模块自己产出的那一份（与 /.well-known/tenon 同源），哈希也必须由
	// 同一份字节算出——这正是 host 侧 declarationHashOf 的校验口径。
	want, err := f.module.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON 失败：%v", err)
	}
	var shipped, produced any
	if err := json.Unmarshal(manifest, &shipped); err != nil {
		t.Fatalf("注册报文里的 manifest 不合法：%v", err)
	}
	if err := json.Unmarshal(want, &produced); err != nil {
		t.Fatalf("ManifestJSON 的结果不合法：%v", err)
	}
	shippedCanon, _ := protocol.CanonicalJSON(shipped)
	producedCanon, _ := protocol.CanonicalJSON(produced)
	if string(shippedCanon) != string(producedCanon) {
		t.Fatalf("注册报文的 manifest 应与 ManifestJSON 同源：\n申报 %s\n端点 %s", shippedCanon, producedCanon)
	}
	if hash != hashOf(t, manifest) {
		t.Fatalf("申报的哈希与随行的 manifest 对不上：%s", hash)
	}
	// 签名用的是密钥文件里的字节（去首尾空白），与 host 侧读的是同一份材料。
	if err := protocol.Verify(secret, first.sig, first.body, time.Now(), protocol.SignatureTolerance); err != nil {
		t.Fatalf("应以 %s 里的密钥签名，验签失败：%v", EnvHMACSecretFile, err)
	}
}
