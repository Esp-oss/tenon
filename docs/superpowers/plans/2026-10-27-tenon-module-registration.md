# R1 实施计划：模块动态注册（全 push + 声明哈希 + 事件链）

> **For agentic workers:** 本计划由多个子代理逐任务执行。每个任务自带完整上下文；**不要跨任务改别人的文件**。步骤用 `- [ ]` 跟踪。

**Goal:** 模块启动即注册、每 5s 心跳；host 用 TTL 判失联、只把 `alive && enabled` 的模块给 agent 看；`TENON_MODULES` 移除；注册/失联事件写独立哈希链。

**Architecture:** 三个新增件——`internal/controlplane/registry`（状态机 + TTL）、`internal/controlplane/registrylog`（事件链）、`internal/module/registryclient`（模块侧推送）；host 新增两个 HMAC 鉴权端点；`internal/protocol` 抽出可复用的 JCS 规范化（审计与声明哈希共用）。

**Tech Stack:** Go 1.26、`internal/protocol`（HMAC 验签）、`internal/controlplane/audit`（JCS 规范化抽取）、PowerShell 5.1（脚本，必须带 UTF-8 BOM）。

**设计依据：** [`docs/superpowers/specs/2026-10-27-tenon-module-registration-design.md`](../specs/2026-10-27-tenon-module-registration-design.md)（D1–D7）。**执行前先读它**。

**全局约束（每个任务都适用）：**

- **不要 `git push`**（本机到 GitHub 的代理当前不通）。只在本分支提交。
- 每个任务结束必须：`gofmt -l .` 无输出、`go vet ./...` 退出 0、`go test ./... -count=1` 无 FAIL。
- 提交信息用 conventional commits，正文中文。
- PowerShell 脚本保存为 **UTF-8 with BOM**，否则 PS 5.1 会报 `The string is missing the terminator`。
- 不新增第三方依赖。

---

## Task 1: 抽出可复用的 JCS 规范化到 `internal/protocol`

**Files:**
- Modify: `internal/protocol/protocol.go`
- Modify: `internal/controlplane/audit/audit.go`（改为调用新函数，行为不变）
- Test: `internal/protocol/canonical_test.go`（新建）、`internal/controlplane/audit/audit_test.go`（既有用例必须仍绿）

**为什么要这一步**：声明哈希要「同一个 manifest 在模块与 host 两侧算出同一个 sha256」，而 JCS 规范化目前藏在 `audit.hashOfTree` 里。两份实现必然分叉。

- [ ] **Step 1: 读现状**

Run: `Select-String -Path internal\controlplane\audit\audit.go -Pattern 'func hashOfTree' -Context 0,40`
Expected: 看到它调用的内部规范化函数名（如 `canonical`/`encodeCanonical`）。把该函数体连同其依赖一并看清楚。

- [ ] **Step 2: 写失败用例**

`internal/protocol/canonical_test.go`：

```go
package protocol

import (
	"encoding/json"
	"testing"
)

// JCS 规范化：对象键排序、数字按最短表示、字符串转义与 JSON 一致。
// 这些断言的意义是「同一份语义内容必须得到同一串字节」——声明哈希与审计哈希都靠它。
func TestCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"键排序", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"嵌套排序", `{"z":{"b":1,"a":2}}`, `{"z":{"a":2,"b":1}}`},
		{"整数不带小数点", `{"n":1.0}`, `{"n":1}`},
		{"数组保持顺序", `{"a":[3,1,2]}`, `{"a":[3,1,2]}`},
		{"中文不转义", `{"t":"库存"}`, `{"t":"库存"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(c.in), &v); err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			got, err := CanonicalJSON(v)
			if err != nil {
				t.Fatalf("规范化失败：%v", err)
			}
			if string(got) != c.want {
				t.Fatalf("canonical(%s) = %s，期望 %s", c.in, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 3: 跑用例确认失败**

Run: `go test ./internal/protocol/ -run TestCanonicalJSON -count=1`
Expected: 编译失败 `undefined: CanonicalJSON`

- [ ] **Step 4: 实现（把 audit 的实现搬过来，导出）**

`internal/protocol/protocol.go` 追加：

```go
// CanonicalJSON 输出 JCS 规范化后的字节：对象键按 UTF-16 码元排序、数字用最短表示、
// 不做中文转义。审计哈希与模块声明哈希共用这一份实现——两份必然分叉，而哈希不容分叉。
func CanonicalJSON(v any) ([]byte, error) {
	return canonicalize(v)   // ← 把 audit 里的实现整体搬成私有函数改名为这个
}
```

把 `internal/controlplane/audit/audit.go` 里原来的规范化实现**删除**，改为调用 `protocol.CanonicalJSON`；`hashOfTree` 内部也用 `protocol.CanonicalJSON`。

- [ ] **Step 5: 跑全仓回归（关键：审计哈希不能变）**

Run: `go vet ./... && go test ./... -count=1`
Expected: 全绿。**若有审计链相关用例失败，说明搬家改坏了行为——回到 Step 4 修正，不许改用例期望**。

- [ ] **Step 6: Commit**

```bash
git add internal/protocol internal/controlplane/audit
git commit -m "refactor(protocol): 抽出 JCS 规范化 CanonicalJSON（审计与声明哈希共用）"
```

---

## Task 2: registry 状态机（alive/lost × enabled，TTL 清扫）

**Files:**
- Modify: `internal/controlplane/registry/registry.go`
- Test: `internal/controlplane/registry/registry_test.go`

**要点**：`ModuleEntry` 增加 `State`（`alive`/`lost`）与 `DeclarationHash`、`LastSeen`；`Upsert` 只用于注册（置 `alive`、刷 `LastSeen`、保留 `Enabled` 现值）；新增 `Touch(id, hash) (ok bool, needReregister string)`；新增 `Sweep(now time.Time) []string`（返回变 `lost` 的 id）；`Enabled()` 收紧为「`enabled && alive`」；**移除 `ParseModules` 与 static 概念**（T5 会删调用方，这里先保留函数签名但改成「忽略入参、返回错误」不行——直接删，让编译错误暴露所有调用点）。

- [ ] **Step 1: 写失败用例**

`internal/controlplane/registry/registry_test.go` 追加：

```go
func TestRegistryLifecycle(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://127.0.0.1:8081", DeclarationHash: "sha256:aaa"})

	if got := len(r.Enabled()); got != 1 {
		t.Fatalf("注册后应可见，得到 %d", got)
	}

	// 心跳：哈希一致 → 刷新 LastSeen
	ok, reason := r.Touch("inventory", "sha256:aaa")
	if !ok || reason != "" {
		t.Fatalf("心跳应成功，得到 ok=%v reason=%q", ok, reason)
	}

	// 心跳：哈希不一致 → 要求重申报
	ok, reason = r.Touch("inventory", "sha256:bbb")
	if ok || reason != "declaration-changed" {
		t.Fatalf("哈希变化应要求重申报，得到 ok=%v reason=%q", ok, reason)
	}

	// 未知模块 → 要求重申报
	ok, reason = r.Touch("ghost", "sha256:aaa")
	if ok || reason != "unknown-module" {
		t.Fatalf("未知模块应要求重申报，得到 ok=%v reason=%q", ok, reason)
	}
}

func TestSweepMarksLostAfterTTL(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	// 把 LastSeen 拨回过去
	r.DebugSetLastSeen("inventory", time.Now().Add(-time.Minute))

	lost := r.Sweep(time.Now())
	if len(lost) != 1 || lost[0] != "inventory" {
		t.Fatalf("应标记 inventory 为 lost，得到 %v", lost)
	}
	if got := len(r.Enabled()); got != 0 {
		t.Fatalf("lost 模块不应可见，得到 %d", got)
	}
}

func TestManualDisableIsSticky(t *testing.T) {
	r := NewRegistry()
	r.Upsert(ModuleEntry{ID: "inventory", BaseURL: "http://x", DeclarationHash: "sha256:aaa"})
	if err := r.SetEnabled("inventory", false); err != nil {
		t.Fatalf("disable 失败：%v", err)
	}
	// 心跳正常也不能救回来
	if ok, _ := r.Touch("inventory", "sha256:aaa"); !ok {
		t.Fatal("心跳本身应成功")
	}
	if got := len(r.Enabled()); got != 0 {
		t.Fatalf("人工 disable 必须粘性，得到可见 %d", got)
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/controlplane/registry/ -count=1`
Expected: 编译失败（`Touch`/`Sweep`/`DebugSetLastSeen` 未定义）

- [ ] **Step 3: 实现**

`registry.go` 按下列要点改（结构体与方法的签名要对得上用例）：

```go
const (
	StateAlive = "alive"
	StateLost  = "lost"
	// DefaultTTL 是心跳过期时间：3 个心跳周期（5s）。
	DefaultTTL = 15 * time.Second
)

type ModuleEntry struct {
	ID              string `json:"id"`
	BaseURL         string `json:"baseURL"`
	Enabled         bool   `json:"enabled"`
	State           string `json:"state"`            // alive | lost
	DeclarationHash string `json:"declarationHash"`
	LastSeen        time.Time `json:"lastSeen"`
	Manifest        json.RawMessage `json:"manifest,omitempty"`
}

func (r *Registry) Upsert(e ModuleEntry) // 置 alive、LastSeen=now、保留原 Enabled（不存在则 true）
func (r *Registry) Touch(id, hash string) (bool, string)
func (r *Registry) Sweep(now time.Time) []string
func (r *Registry) Enabled() []ModuleEntry   // 只返回 enabled && alive
func (r *Registry) List() []ModuleEntry      // 管理面：全部
```

`Enabled()` 的注释写明：这不是「注册了就能被看见」，而是「活着且没被人工摘掉」。

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/controlplane/registry/ -count=1 -v`
Expected: 三条新用例 PASS；既有用例若因语义收紧而失败，按下一条处理。

- [ ] **Step 5: 修既有失败（如果 `ParseModules` 被删导致编译错）**

Run: `go build ./... 2>&1 | Select-Object -First 10`
Expected: 会报 `cmd/host/main.go` 里 `ParseModules`/`Modules` 未定义——**本任务不修它**，在提交信息里注明「编译将在 Task 4 修复」，然后执行：

```bash
go build ./internal/... && go test ./internal/controlplane/registry/ -count=1
```

（只验证本包可编译可测；整仓编译由 Task 4 恢复。）

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/registry
git commit -m "feat(registry): 状态机改为 alive/lost × enabled，TTL 清扫与人工粘性"
```

---

## Task 3: 注册/心跳端点（HMAC 验签 + 409 语义）

**Files:**
- Modify: `internal/controlplane/http.go`（挂两个路由）
- Modify: `cmd/host/main.go`（读 `TENON_HMAC_SECRET_FILE`；**缺则启动失败**）
- Test: `internal/controlplane/http_registry_test.go`（新建）

**前置**：`Config` 增加 `HMACSecret []byte`；`New()` 时若非空则编译注册处理器。

- [ ] **Step 1: 写失败用例**

`internal/controlplane/http_registry_test.go`：

```go
package controlplane

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

const testSecret = "s3cr3t"

func signedBody(t *testing.T, body []byte) (*bytes.Reader, string) {
	t.Helper()
	ts := time.Now().Unix()
	return bytes.NewReader(body), protocol.Sign([]byte(testSecret), ts, body)
}

func TestRegistryRegisterAndHeartbeat(t *testing.T) {
	srv, ts := newHostWithRegistry(t, []byte(testSecret))

	manifest := []byte(`{"tenon":"0.1","module":{"id":"inventory","version":"1.0.0","title":"库存"},"actions":[],"events":[],"transports":["http"],"capabilities":[]}`)
	body := []byte(`{"module":{"id":"inventory","baseURL":"http://127.0.0.1:8081","manifest":` + string(manifest) + `},"declarationHash":"sha256:PLACEHOLDER"}`)
	// 正确哈希由被测实现给出：先用 register 探一次，再以返回值修正（见 Step 4 的 helper）
	...
}
```

> 实现者注意：`declarationHash` 的正确值必须由 `protocol.CanonicalJSON(manifest)` 算出，**不要在用例里手写**。写个 helper：

```go
func declarationHash(t *testing.T, manifest []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(manifest, &v); err != nil { t.Fatal(err) }
	canon, err := protocol.CanonicalJSON(v)
	if err != nil { t.Fatal(err) }
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:])
}
```

用例必须覆盖：
1. 正确签名 + 正确哈希 → `200`，且 `GET /tenon/modules` 能看到它；
2. 签名缺失/错误 → `401`；
3. 时间戳超 ±300s → `401`（理由区分 `expired`）；
4. 心跳哈希不一致 → `409` 且 body 含 `"reRegister":true` 与 `"reason":"declaration-changed"`；
5. 心跳未知模块 → `409` 且 `reason":"unknown-module"`；
6. 正确心跳 → `200`。

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/controlplane/ -run TestRegistryRegisterAndHeartbeat -count=1`
Expected: 编译失败（路由/Config 字段不存在）

- [ ] **Step 3: 实现**

`http.go` 路由表加：

```go
case "/tenon/registry/register":
	s.serveRegistryRegister(w, r)
case "/tenon/registry/heartbeat":
	s.serveRegistryHeartbeat(w, r)
```

两个 handler 都：`requireMethod(POST)` → 读 body → `protocol.Verify(secret, header, body, now, 300s)`（失败按 `errors.Is` 分 `format`/`expired`/`invalid` 回 401 + `tenonError`）→ 解析 JSON → 调 registry。

`register`：校验 `declarationHash == declarationHash(manifest)`，不等 → `409`（理由 `declaration-mismatch`）。

`heartbeat`：`registry.Touch(id, hash)`；`ok=false` 时回 `409 {"reRegister":true,"reason":<reason>}`。

`cmd/host/main.go`：新增 `envHMACSecretFile = "TENON_HMAC_SECRET_FILE"`；启动时读；**为空或读不到 → `log.Fatalf`**，信息里点名环境变量与「注册端点会因此不可用」。

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/controlplane/ -run TestRegistry -count=1 -v`
Expected: 全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/controlplane cmd/host
git commit -m "feat(host): 注册/心跳端点（HMAC 验签 + 409 要求重申报，缺密钥拒绝启动）"
```

---

## Task 4: 可见性收紧 + 移除 `TENON_MODULES` + 恢复整仓编译

**Files:**
- Modify: `cmd/host/main.go`（删 `envModules` 与 `cfg.Modules` 装配）
- Modify: `internal/controlplane/http.go`（`GET /tenon/modules` 走 `Enabled()`；`POST /tenon/admin/modules` 保留，仅测试态）
- Test: `internal/controlplane/http_test.go`（既有用例按新语义修）

- [ ] **Step 1: 删 env 与装配，让编译错误暴露全部调用点**

删除 `envModules`、`cfg.Modules` 的解析与赋值；`Config.Modules` 字段一并删除。

- [ ] **Step 2: 修编译错误**

Run: `go build ./... 2>&1 | Select-Object -First 20`
Expected: 逐条修（多为测试里 `Config{Modules: ...}` 的初始化）。**测试里的模块改为先 `registry.Upsert` 再起服务**，不要保留静态注入路径。

- [ ] **Step 3: 跑用例**

Run: `go test ./... -count=1`
Expected: 全绿（含 D1 相关单测）。若有用例依赖「静态注册即可见」，改为「Upsert 后可见」。

- [ ] **Step 4: Commit**

```bash
git add cmd/host internal/controlplane
git commit -m "feat(host): 移除 TENON_MODULES，可见性只看 enabled && alive"
```

---

## Task 5: 注册事件哈希链 `registry-events.jsonl`

**Files:**
- Create: `internal/controlplane/registrylog/registrylog.go`
- Test: `internal/controlplane/registrylog/registrylog_test.go`

**要点**：append-only；每行 `{at,event,moduleId,declarationHash?,prevHash,hash}`；`hash = sha256: + sha256(CanonicalJSON(去掉 hash 的字段))`；`Append` 返回错误时**不改内存态**；提供 `Verify(path) error`。

- [ ] **Step 1: 写失败用例**

```go
func TestAppendAndVerifyChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry-events.jsonl")
	l, err := Open(path)
	if err != nil { t.Fatal(err) }
	for _, ev := range []Event{
		{Event: "registered", ModuleID: "inventory", DeclarationHash: "sha256:aaa"},
		{Event: "lost", ModuleID: "inventory"},
		{Event: "recovered", ModuleID: "inventory"},
	} {
		if err := l.Append(ev, time.Unix(1700000000, 0)); err != nil {
			t.Fatalf("append 失败：%v", err)
		}
	}
	if err := Verify(path); err != nil {
		t.Fatalf("刚写完的链应通过校验：%v", err)
	}
	// 篡改任一行 → 校验失败
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	lines[1] = strings.Replace(lines[1], `"lost"`, `"recovered"`, 1)
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if err := Verify(path); err == nil {
		t.Fatal("篡改后必须校验失败")
	}
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/controlplane/registrylog/ -count=1`
Expected: 包不存在 / 符号未定义

- [ ] **Step 3: 实现**

要求：
- `Open(path)` 读末行 `hash` 作为 `prevHash` 起点（文件不存在则起点为空）；
- `Append(ev, now)` 追加一行并 `Sync`；
- `Verify(path)` 顺序回放，任一 `prevHash` 不接或 `hash` 不匹配即返回错误（错误里给行号）；
- 并发安全：`Open` 返回的对象带 `sync.Mutex`；
- 写失败返回错误，调用方（Task 3 的 handler）**不得**因此改变注册结果——只记日志（与「观测文件不决定业务处置」一致）。

- [ ] **Step 4: 跑用例确认通过**

Run: `go test ./internal/controlplane/registrylog/ -count=1 -v`

- [ ] **Step 5: 接到 registry（跃迁时写）**

`registry` 增加可选 `Log *registrylog.Log` 字段；`Upsert`（新建/哈希变）、`Sweep`（转 lost）、`Touch`（lost→alive）、`SetEnabled` 各写一条。心跳成功但状态未变时**不写**。

补一条用例：连续 3 次成功心跳只产生 1 条 `registered`。

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/registrylog internal/controlplane/registry
git commit -m "feat(registry): 注册事件独立哈希链（只在跃迁时写）+ Verify"
```

---

## Task 6: 模块侧 registry client（注册 + 心跳 + 409 重申报）

**Files:**
- Create: `internal/module/registryclient.go`
- Modify: `internal/module/module.go`（暴露 manifest 摘要与 `BaseURL` 供 client 使用）
- Test: `internal/module/registryclient_test.go`

**要点**：`Start(ctx)` 启动 goroutine；启动即注册；每 `interval` 心跳；`409` 立即重申报；**注册失败不阻塞模块启动**；间隔与 TTL 可用 env 覆盖（测试用 100ms）。

- [ ] **Step 1: 写失败用例**

用 `httptest` 起一个假 host，记录收到的方法/路径/次数：

```go
func TestClientRegistersThenHeartbeats(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock(); calls = append(calls, r.URL.Path); mu.Unlock()
		if r.URL.Path == "/tenon/registry/heartbeat" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"reRegister":true,"reason":"unknown-module"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer host.Close()

	c := NewRegistryClient(ClientConfig{Host: host.URL, ModuleID: "inventory", BaseURL: "http://127.0.0.1:8081",
		Manifest: []byte(`{"tenon":"0.1"}`), Secret: []byte("s"), Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	c.Start(ctx)

	mu.Lock(); defer mu.Unlock()
	if !seen(calls, "/tenon/registry/register") { t.Fatalf("启动应注册一次：%v", calls) }
	if count(calls, "/tenon/registry/register") < 2 { t.Fatalf("收到 409 后应重申报：%v", calls) }
}
```

- [ ] **Step 2: 跑用例确认失败**

Run: `go test ./internal/module/ -run TestClientRegistersThenHeartbeats -count=1`

- [ ] **Step 3: 实现**

```go
type ClientConfig struct {
	Host, ModuleID, BaseURL string
	Manifest                []byte
	Secret                  []byte
	Interval                time.Duration // 默认 5s
	HTTP                    *http.Client
}
func NewRegistryClient(cfg ClientConfig) *RegistryClient
func (c *RegistryClient) Start(ctx context.Context) // 非阻塞
```

每次请求都用 `protocol.Sign(secret, unix(), body)` 生成 `X-Tenon-Signature`；`409` → 立即 register；其他非 2xx 只记日志并按间隔重试。

- [ ] **Step 4: 接到三个模块的 main**

`cmd/inventory|procurement|quote/main.go`：`module.Start()` 之后启动 client（host 地址来自 `TENON_HOST`，未设置则跳过并警告——本地单模块调试用）。**注意：跳过时打警告，不要静默。**

- [ ] **Step 5: 跑用例 + 整仓**

Run: `go test ./internal/module/ -count=1 -v && go test ./... -count=1`

- [ ] **Step 6: Commit**

```bash
git add internal/module cmd/inventory cmd/procurement cmd/quote
git commit -m "feat(module): 启动注册 + 5s 心跳 + 409 重申报（失败不阻塞业务）"
```

---

## Task 7: 脚本与验收（D1 必须真跑一次）

**Files:**
- Modify: `scripts/d1-e2e.ps1`（删 `TENON_MODULES`，改为各模块自注册；给 host 传 `TENON_HMAC_SECRET_FILE`）
- Modify: `scripts/dev-up.ps1`（同上）
- Create: `scripts/verify-registration.ps1`
- Modify: `docs/interfaces.md`（新增两个端点与可见性语义）

- [ ] **Step 1: 改两个编排脚本**

`d1-e2e.ps1` / `dev-up.ps1`：
- host 启动环境加 `TENON_HMAC_SECRET_FILE = $hmacFile`；
- 删掉 `TENON_MODULES`；
- 各模块环境加 `TENON_HOST = "http://127.0.0.1:8080"`；
- **等待就绪的逻辑改成「轮询 `GET /tenon/modules` 直到 3 个模块可见」**（原来可能依赖静态注册）。

- [ ] **Step 2: 写验收脚本**

`scripts/verify-registration.ps1`（UTF-8 BOM），逐条断言 spec §10 的 7 条口径，至少覆盖：
1. 三模块注册后 `/tenon/modules` 可见；
2. 杀掉一个模块进程 → 15s 内从可见列表消失、`registry-events.jsonl` 出现 `lost`；
3. 被杀的模块重启 → 30s 内重新可见（走 409 → 重申报路径）；
4. 人工 disable 后不可见，enable 后可见；
5. host 重启 → 断言「重启后 ≤5s 内不可见、之后恢复」；
6. `node`/脚本校验 `registry-events.jsonl` 链完整。

- [ ] **Step 3: 真跑 D1（关键回归）**

Run: `powershell -NoProfile -File scripts/d1-e2e.ps1`
Expected: 拿到 `verdict`（`pass`/`fail` 都算跑通，`inconclusive` 视为失败）。**若因为注册改动导致 A/B 轮拿不到模块，修脚本，不许放宽判定。**

- [ ] **Step 4: 真跑验收**

Run: `powershell -NoProfile -File scripts/verify-registration.ps1`
Expected: 全部断言通过。

- [ ] **Step 5: 文档**

`docs/interfaces.md` 增补：`POST /tenon/registry/register`、`POST /tenon/registry/heartbeat`（含报文、签名、409 语义）、以及 `GET /tenon/modules` 的可见性定义（`enabled && alive`，无 static 例外）。

- [ ] **Step 6: 全量回归 + Commit**

```bash
gofmt -l . ; go vet ./... ; go test ./... -count=1
pnpm check
git add scripts docs/interfaces.md
git commit -m "feat(scripts): 模块自注册改造 + 注册验收脚本（D1 真跑通过）"
```

---

## 自检对照（spec → 任务）

| spec 要求 | 任务 |
|---|---|
| §2 D5 无 static 豁免、移除 `TENON_MODULES` | Task 2（删 ParseModules）、Task 4 |
| §2 D6 不落盘 | 全计划无持久化任务（有意） |
| §2 D7 缺密钥启动失败 | Task 3 |
| §3 两个端点 + 报文 | Task 3 |
| §3.3 可见性 `enabled && alive` | Task 2、Task 4 |
| §4 状态机 + TTL | Task 2 |
| §5 HMAC ±300s | Task 3 |
| §7 事件链 + Verify | Task 5 |
| §8 模块侧 client | Task 6 |
| §9 D1/脚本兼容 | Task 7 |
| §10 七条验收口径 | Task 7 |
