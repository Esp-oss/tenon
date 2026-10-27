# P1 实施计划：host 入站面（队列 / 身份 / 来源判定 / 业务规则）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `cmd/host` 具备「人 → agent」的入站面：可落盘的入站帧队列、dev 登录换 token、`origin`（对话/事件）进入策略求值、业务级规则从 `skills/*/approval.json` 加载并做加载期路径校验、待批公告板。全部可用 curl 独立验证，不依赖 agent 与前端。

**Architecture:** 入站与出站是两条独立队列（出站的 `ack` 是累积游标，不能与入站共用一个）；入站队列走 `Queue` 端口，默认落盘 JSONL。`origin` 由宿主签发并登记合成事件 id 后判定，agent 不得自报。业务规则与平台规则同 schema、同加载器、同池合并，取更严。

**Tech Stack:** Go 1.26（标准库为主：`net/http`、`crypto/hmac`、`encoding/json`、`os`）；测试用 `net/http/httptest` + `t.TempDir()`；schema 校验用仓内 `internal/spec`（`pnpm validate` 走 `scripts/validate-spec.mjs`）。

**对应 spec:** [`docs/superpowers/specs/2026-10-27-tenon-codrive-chat-entry-design.md`](../specs/2026-10-27-tenon-codrive-chat-entry-design.md)（§3.2、§3.5、§3.7、§3.8、§3.10、§3.11、§8、§9）

---

## 0. 前置说明（执行前必读）

**工作目录**：直接在 `D:\Progect\dsh_for_sys` 工作。若要隔离，先执行：

```bash
git worktree add ../dsh_for_sys-p1 -b feat/p1-host-inbound
```

**已落地（Task 1 完成，commit `de45903`）**：`Queue` 端口与 `memoryQueue` 已在 `internal/controlplane/queue.go`，出站四个调用点已改为导出方法。后续任务在此基础上继续，**不要重复创建 `queue.go`**。

**已知限制（P1 不解决，记录在案）**：`Queue` 是导出接口，但签名里带未导出类型 `queuedFrame`，两个构造函数也未导出——包外无法实现或构造它，只能经 `Config.InboundDir` 间接装配。spec §8 把 Queue 定义为「内部接口」，所以本轮不修；将来要支持从 `cmd/host` 注入自定义队列实现时，需要同时导出行类型与构造函数。

**运维约定（必须写进 Task 9 的启动说明）**：重置入站队列要**同时删** `requests.jsonl`、`requests.jsonl.meta` 与 `requests-index.jsonl`。前两者的理由是序号水位（只删主文件时水位从 meta 续上，序号不回 1）；**第三个是治理状态而非缓存**——判定规则是「未登记即视为 `event`」，单删索引会把在途 request 静默降级成事件驱动，绕开「对话发起必须审批」（spec §3.11）。所以清理索引必须与清空 pending 一起做。

**计划勘误（执行期发现，后续任务按此为准）**

1. **`writeTenonError` / `writeTenonErrorStatus` 的最后一个参数是 `protocol.Error` 值，不是指针**。`newError(...)` 返回 `*protocol.Error`，所以调用处必须写 `*newError(...)`。Task 3 与 Task 4 的代码片段里写成了传指针，照抄会编译不过。
2. **`newTestServer` 补 `InboundDir` 缺省属于 Task 4 而不是 Task 3**：`Config.InboundDir` 是 Task 4 才新增的字段，Task 3 阶段写进去编译不过。Task 4 必须补 `if strings.TrimSpace(cfg.InboundDir) == "" { cfg.InboundDir = t.TempDir() + "/inbound" }`，否则入站队列会落到仓库工作目录的 `out/inbound`，几十个用例互相污染。
3. **`bearerClaims` 失败时已经把 401 响应写好了**，调用方只 `return`，**绝对不要再写一次响应**（header 重复写会让响应失效）。
4. **`New()` 的校验顺序已是契约**：审计路径非空 → 打开审计 → **会话密钥非空** → 策略自检。任何断言"自检错误"的新用例都必须同时给 `SessionSecret`，否则测到的是密钥那条路径。
5. **`Config.SessionSecret` 零值 = 装配失败**（不是"禁用登录"）；`Config.Users` 零值 = 登录永远 401（不是"跳过鉴权"）。所有 `New()` 调用点都必须给非空密钥。
6. **`Queue` 接口含 `Broken() error`**（Task 4 复核后追加）：`Push` 只回序号，落盘失败必须另有出口，否则「已接受但没落盘」会被伪装成「已持久化」。`memoryQueue.Broken()` 恒为 nil；`jsonlQueue` 返回第一次失败。`POST /tenon/requests` 据此回 `200 {ok,seq,durable}`，`durable:false` 时打告警——**不回 500**，理由见 spec §3.2（重试会重发用户那句话）。
7. **入站 ack 必须拒负游标**（`400 INVALID_CURSOR`），与出站 `serveIntentAck` 同校验；否则会回一个负的 `acked`。
8. **`TENON_REQUIRE_TRIGGER` 默认必须为 false，且只认 `== "1"`**。一旦默认 true，现有 `cmd/agent`（还不发 `triggerEventId`）会拿到 `200 + deny + MISSING_TRIGGER`——传输层与业务拒绝无法区分，agent 会把每份 plan 都报成"策略拒绝"，P1 上线即停摆。开启时必须在启动日志打一条显式告警。
9. **不要把 HTTP 请求体直接 `Unmarshal` 进 `policy.PolicyRequest`**：它现在带 `json:"origin,omitempty"`，透传就会让"来源只能由宿主判定"的保证作废。HTTP 层必须继续用自己的 `policyEvaluateRequest`。
10. **`origin` 不是 inputSchema 路径**：Task 7 的加载期校验只能校验 `arg.*`；`origin` 与 `risk.*` 不是动作入参，别拿它们去 `inputSchema.properties` 里找，否则合法规则会被整份丢弃。

**本计划刻意包含一处受控重构**：把现有 `intentQueue` 提取成 `Queue` 端口的 `memoryQueue` 实现。出站行为**不变**，靠既有的 `TestIntentsRESTRoundTrip` / `TestIntentsRejectsBadFrame` 当安全网。不做这个提取，就要复制约 70 行队列代码，两份迟早漂移。

**一处与 spec 的时序偏差（已获准，记录在案）**：spec §3.11 要求 `policies:evaluate` 的 `triggerEventId` 必填、缺失即 `deny`。但现有 `cmd/agent` 还不发这个字段，P1 若立刻强制会导致整个 agent 停摆（P2 才改 agent）。因此 P1 引入过渡开关：

- 默认：缺失 `triggerEventId` → 视为 `origin="event"`（保持现有行为）
- `TENON_REQUIRE_TRIGGER=1`：缺失 → `deny`（P4 收口时把默认值改为强制）

这条会同步写回 spec §3.11 的「分阶段启用」小节，避免文档与实现脱节。

**每个任务结束都要 `git commit`**，commit message 用本计划里给出的原文。

---

## 文件结构（先锁边界）

| 文件 | 职责 | 动作 |
|---|---|---|
| `internal/controlplane/queue.go` | `Queue` 端口 + `memoryQueue`（从 `http.go` 提取） | 新建 |
| `internal/controlplane/jsonlqueue.go` | 落盘实现：append-only JSONL + 游标 + 归档 | 新建 |
| `internal/controlplane/session.go` | dev 用户表、无状态 token 签发/校验 | 新建 |
| `internal/controlplane/inbound.go` | 3 个入站端点 + 帧校验 + `origin` 登记表（独立落盘） | 新建 |
| `internal/controlplane/pending.go` | 待批公告板（幂等 upsert + 按角色过滤） | 新建 |
| `internal/controlplane/rules.go` | `skills/*/approval.json` 加载 + 加载期路径校验 | 新建 |
| `internal/controlplane/static.go` | `GET /` 静态页（P1 只做路由与目录，页面内容在 P3） | 新建 |
| `internal/controlplane/http.go` | `Config`/`Server` 增字段、`ServeHTTP` 增分派、`intentQueue` → `memoryQueue` | 修改 |
| `internal/controlplane/policy/policy.go` | `PolicyRequest.Origin` + `parsePath` 支持单段 `origin` | 修改 |
| `spec/0.1/schemas/policy.schema.json` | `ConditionLeaf` 的 propertyName 允许多段或单段 | 修改 |
| `cmd/host/main.go` | 新增环境变量装配与启动告警 | 修改 |
| `config/users.dev.json` | dev 账号（alice=clerk / bob=manager） | 新建 |
| `skills/quote-to-fulfillment/approval.json` | 业务级规则示例 | 新建 |

---

## Task 1: 提取 `Queue` 端口（受控重构）

**Files:**
- Create: `internal/controlplane/queue.go`
- Modify: `internal/controlplane/http.go`（删除 `intentQueue` 定义，改用 `memoryQueue`）

- [ ] **Step 1: 新建 `queue.go`，定义端口与内存实现**

```go
package controlplane

import (
	"encoding/json"
	"sync"
)

// queuedFrame 是队列里的一帧：序号由队列分配，帧体原样保留。
type queuedFrame struct {
	Seq   int64           `json:"seq"`
	Frame json.RawMessage `json:"frame"`
}

// Queue 是可回放的帧队列：序号单调递增，Ack 掉已确认的前缀。
//
// 入站与出站各持一条：出站的 Ack 是累积游标，两个消费者共用一条队列会互相吞帧。
type Queue interface {
	Push(frame json.RawMessage) int64
	Since(after int64) (frames []queuedFrame, next int64, dropped int64)
	Ack(upto int64) int64
	Highest() int64
	Close() error
}

// memoryQueue 是有界内存队列：满则丢最旧并累加 dropped。重启即丢（出站用）。
type memoryQueue struct {
	mu       sync.Mutex
	capacity int
	seq      int64
	frames   []queuedFrame
	dropped  int64
}

var _ Queue = (*memoryQueue)(nil)

func newMemoryQueue(capacity int) *memoryQueue {
	if capacity <= 0 {
		capacity = maxIntentFrames
	}
	return &memoryQueue{capacity: capacity, frames: make([]queuedFrame, 0, capacity)}
}

func (q *memoryQueue) Push(frame json.RawMessage) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	q.frames = append(q.frames, queuedFrame{Seq: q.seq, Frame: frame})
	for len(q.frames) > q.capacity {
		q.frames = q.frames[1:]
		q.dropped++
	}
	return q.seq
}

func (q *memoryQueue) Since(after int64) ([]queuedFrame, int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queuedFrame, 0, len(q.frames))
	cursor := after
	for _, f := range q.frames {
		if f.Seq <= after {
			continue
		}
		out = append(out, f)
		cursor = f.Seq
	}
	return out, cursor, q.dropped
}

func (q *memoryQueue) Ack(upto int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 裁剪到已分配的最大序号：upto 超过 highest 时不能回一个不存在的序号，
	// 否则对外契约（acked 永远是真实确认到的位置）会被打破。
	if upto > q.seq {
		upto = q.seq
	}
	kept := make([]queuedFrame, 0, len(q.frames))
	for _, f := range q.frames {
		if f.Seq > upto {
			kept = append(kept, f)
		}
	}
	q.frames = kept
	return upto
}

func (q *memoryQueue) Highest() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.seq
}

func (q *memoryQueue) Close() error { return nil }
```

- [ ] **Step 2: 从 `http.go` 删掉旧的 `intentQueue` 段并把字段类型换成 `Queue`**

删除 `http.go` 里 `// intentQueue 是有界内存帧队列…` 到该类型全部方法结束（原 `http.go:962-1030` 一带），并把：

```go
	intents      *intentQueue
```

改为：

```go
	intents      Queue
```

`New()` 里的 `intents: newIntentQueue(maxIntentFrames)` 改为 `intents: newMemoryQueue(maxIntentFrames)`。

- [ ] **Step 3: 跑既有出站用例，确认行为未变**

Run: `go test ./internal/controlplane/ -run 'TestIntents' -count=1 -v`
Expected: `TestIntentsRESTRoundTrip`、`TestIntentsRejectsBadFrame` 全 PASS

- [ ] **Step 4: 全量编译与单测**

Run: `go build ./... && go test ./internal/controlplane/... -count=1`
Expected: 编译通过，`ok  tenon.dev/internal/controlplane`（含 policy/grant/audit/registry 子包）

- [ ] **Step 5: Commit**

```bash
git add internal/controlplane/queue.go internal/controlplane/http.go
git commit -m "refactor(controlplane): 提取 Queue 端口，intentQueue -> memoryQueue"
```

---

## Task 2: `jsonlQueue`（入站落盘队列）

**Files:**
- Create: `internal/controlplane/jsonlqueue.go`
- Test: `internal/controlplane/jsonlqueue_test.go`

- [ ] **Step 1: 先写失败测试**

```go
package controlplane

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestJSONLQueueRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("打开队列失败：%v", err)
	}
	defer q.Close()

	if seq := q.Push(json.RawMessage(`{"type":"tenon.request","n":1}`)); seq != 1 {
		t.Fatalf("首个序号应为 1，得到 %d", seq)
	}
	q.Push(json.RawMessage(`{"type":"tenon.request","n":2}`))

	frames, next, dropped := q.Since(0)
	if len(frames) != 2 || next != 2 || dropped != 0 {
		t.Fatalf("回放不符：len=%d next=%d dropped=%d", len(frames), next, dropped)
	}
	if q.Ack(1) != 1 {
		t.Fatalf("ack 返回值不对")
	}
	frames, _, _ = q.Since(0)
	if len(frames) != 1 || frames[0].Seq != 2 {
		t.Fatalf("ack 后应只剩 seq=2，得到 %+v", frames)
	}
}

// 重启是入站队列存在的理由：用户说过的话不能因为进程重启就消失。
func TestJSONLQueueSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	q.Push(json.RawMessage(`{"n":1}`))
	q.Push(json.RawMessage(`{"n":2}`))
	q.Ack(1)
	if err := q.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	again, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	defer again.Close()
	frames, next, _ := again.Since(1)
	if len(frames) != 1 || frames[0].Seq != 2 || next != 2 {
		t.Fatalf("重开后续号/游标不对：%+v next=%d", frames, next)
	}
	if seq := again.Push(json.RawMessage(`{"n":3}`)); seq != 3 {
		t.Fatalf("重开后序号应从 3 继续，得到 %d", seq)
	}
}

func TestJSONLQueueDropsOldestWhenFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 2)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	defer q.Close()
	q.Push(json.RawMessage(`{"n":1}`))
	q.Push(json.RawMessage(`{"n":2}`))
	q.Push(json.RawMessage(`{"n":3}`))
	frames, _, dropped := q.Since(0)
	if len(frames) != 2 || dropped != 1 || frames[0].Seq != 2 {
		t.Fatalf("容量 2 时应丢最旧：len=%d dropped=%d first=%d", len(frames), dropped, frames[0].Seq)
	}
}

// 断电留下的半行不该让整个队列读不出来。
func TestJSONLQueueIgnoresTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbound.jsonl")
	q, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	q.Push(json.RawMessage(`{"n":1}`))
	q.Close()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if _, err := f.WriteString(`{"seq":2,"fra`); err != nil {
		t.Fatalf("写半行失败：%v", err)
	}
	f.Close()

	again, err := newJSONLQueue(path, 1024)
	if err != nil {
		t.Fatalf("半行应被忽略而不是报错：%v", err)
	}
	defer again.Close()
	if again.Highest() != 1 {
		t.Fatalf("半行不该被计入序号，最高应为 1，得到 %d", again.Highest())
	}
}
```

在文件头补 `"os"` import。

- [ ] **Step 2: 跑测试确认编译/断言失败**

Run: `go test ./internal/controlplane/ -run 'TestJSONLQueue' -count=1`
Expected: `undefined: newJSONLQueue`

- [ ] **Step 3: 实现 `jsonlqueue.go`**

```go
package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// jsonlQueue 是落盘的入站队列：一行一帧，ack 后重写存活部分。
//
// 为什么落盘：入站承载的是「人说过的话」。丢了不会有人报错，业务员只会以为提过。
// 出站 Intent 是界面提示，丢了可由公告板与 pending 重建，因此仍留内存实现。
type jsonlQueue struct {
	mu       sync.Mutex
	path     string
	capacity int
	seq      int64
	frames   []queuedFrame
	dropped  int64
}

var _ Queue = (*jsonlQueue)(nil)

func newJSONLQueue(path string, capacity int) (*jsonlQueue, error) {
	if capacity <= 0 {
		capacity = maxIntentFrames
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("controlplane: 创建入站队列目录失败：%w", err)
	}
	q := &jsonlQueue{path: path, capacity: capacity, frames: make([]queuedFrame, 0, capacity)}
	if err := q.load(); err != nil {
		return nil, err
	}
	return q, nil
}

// load 只读已 ack 之后仍存活的帧；末尾半行（崩溃留下）跳过而不是报错。
func (q *jsonlQueue) load() error {
	f, err := os.Open(q.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("controlplane: 打开入站队列失败：%w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec queuedFrame
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // 半行/坏行：跳过，不阻塞后续帧
		}
		if rec.Seq > q.seq {
			q.seq = rec.Seq
		}
		q.frames = append(q.frames, rec)
	}
	return sc.Err()
}

// rewrite 把存活帧重写回磁盘（ack 之后调用）。先写临时文件再改名，避免半个文件。
func (q *jsonlQueue) rewrite() error {
	tmp := q.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("controlplane: 写入站队列失败：%w", err)
	}
	w := bufio.NewWriter(f)
	for _, rec := range q.frames {
		raw, err := json.Marshal(rec)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}

func (q *jsonlQueue) Push(frame json.RawMessage) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	q.frames = append(q.frames, queuedFrame{Seq: q.seq, Frame: frame})
	for len(q.frames) > q.capacity {
		q.frames = q.frames[1:]
		q.dropped++
		// 入站丢的是「用户说过的话」，与出站丢界面提示不是一个量级的事：必须留痕（spec §7）。
		log.Printf("controlplane: 入站队列已满（容量 %d），丢弃最旧一帧，累计 %d", q.capacity, q.dropped)
	}
	// 落盘失败不吞：调用方（POST 处理器）应当据此回 500，而不是假装入队成功。
	if err := q.rewrite(); err != nil {
		q.broken = err
	}
	return q.seq
}

func (q *jsonlQueue) Since(after int64) ([]queuedFrame, int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queuedFrame, 0, len(q.frames))
	cursor := after
	for _, rec := range q.frames {
		if rec.Seq <= after {
			continue
		}
		out = append(out, rec)
		cursor = rec.Seq
	}
	return out, cursor, q.dropped
}

func (q *jsonlQueue) Ack(upto int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 与 memoryQueue 保持同一契约：裁剪到已分配的最大序号，永远返回真实确认位置。
	// 两个实现的行为一旦分叉，同一段客户端代码在「落盘/内存」两种配置下就会拿到不同结果。
	if upto > q.seq {
		upto = q.seq
	}
	kept := make([]queuedFrame, 0, len(q.frames))
	for _, rec := range q.frames {
		if rec.Seq > upto {
			kept = append(kept, rec)
		}
	}
	q.frames = kept
	if err := q.rewrite(); err != nil {
		q.broken = err
	}
	return upto
}

func (q *jsonlQueue) Highest() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.seq
}

func (q *jsonlQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.broken
}
```

同时给结构体补一个字段（上面的 `Push`/`Ack` 用到）：

```go
	// broken 记住第一次落盘失败：队列已不可信，让 Close() 把它暴露出来。
	broken error
```

**两处易漏（Task 1 的 subagent 已提醒）**：① 结构体里必须真的加上 `broken error` 字段，否则 `Push`/`Ack`/`Close` 三处编译不过；② import 必须是上面列的那 7 个——**没有 `io`**（计划早期版本写过，已删），多了 `log`。

- [ ] **Step 4: 跑测试至全绿**

Run: `go test ./internal/controlplane/ -run 'TestJSONLQueue' -count=1 -v`
Expected: 4 个用例全 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/controlplane/jsonlqueue.go internal/controlplane/jsonlqueue_test.go
git commit -m "feat(controlplane): 入站队列落盘实现（jsonlQueue，含崩溃半行容忍）"
```

---

## Task 2b: 序号高位水位（Task 2 复核后追加）

**为什么追加**：Task 2 的实现用「文件里最大的 `seq`」恢复游标。若整份文件损坏（只剩半行、或全被截断），恢复出的 `seq` 退回 0，之后 `Push` 从 1 重新分配——**持有旧游标的客户端会永远看不到新帧**（`Since(after=5)` 把所有 `Seq<=5` 滤掉），而日志里没有任何异常。这正是「丢了没人知道，业务员以为提过」的失败模式。

**Files:**
- Modify: `internal/controlplane/jsonlqueue.go`
- Modify: `internal/controlplane/jsonlqueue_test.go`

- [ ] **Step 1: 写失败测试**

```go
// 整份文件损坏时，序号不能退回 0：否则持旧游标的客户端会漏掉以后所有帧。
func TestJSONLQueueCorruptFileKeepsSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 16)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	q.Push(json.RawMessage(`{"n":1}`))
	q.Push(json.RawMessage(`{"n":2}`))
	if err := q.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	// 把主文件整体写坏：模拟磁盘损坏/被截断。
	if err := os.WriteFile(path, []byte(`{"seq":9,"fra`), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}

	again, err := newJSONLQueue(path, 16)
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	defer again.Close()
	if got := again.Highest(); got != 2 {
		t.Fatalf("水位应由 meta 恢复为 2，得到 %d", got)
	}
	if seq := again.Push(json.RawMessage(`{"n":3}`)); seq != 3 {
		t.Fatalf("坏文件后新帧序号应为 3，得到 %d", seq)
	}
}

// meta 文件是水位的水位：它必须真的被写下来。
func TestJSONLQueueWritesMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbound.jsonl")
	q, err := newJSONLQueue(path, 16)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	defer q.Close()
	q.Push(json.RawMessage(`{"n":1}`))
	raw, err := os.ReadFile(path + ".meta")
	if err != nil {
		t.Fatalf("meta 未写出：%v", err)
	}
	if !strings.Contains(string(raw), `"seq":1`) {
		t.Fatalf("meta 内容不对：%s", raw)
	}
}
```

测试文件 import 补 `"strings"`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controlplane/ -run 'TestJSONLQueueCorruptFileKeepsSequence|TestJSONLQueueWritesMeta' -count=1`
Expected: 第一个用例 `水位应由 meta 恢复为 2，得到 0`；第二个 `meta 未写出`

- [ ] **Step 3: 改 `jsonlqueue.go`**

结构体补一个字段：

```go
	// metaPath 存序号高位水位。它不存帧内容，只在主文件损坏时兜底序号，
	// 避免游标退回 0 导致持旧游标的客户端永久漏帧。
	metaPath string
```

`newJSONLQueue` 里构造完 `q` 之后、`q.load()` 之前补 `q.metaPath = path + ".meta"`；`load()` 末尾（`return sc.Err()` 之前）改为：

```go
	if err := sc.Err(); err != nil {
		return err
	}
	q.loadMeta()
	return nil
```

新增两个方法：

```go
// loadMeta 读回水位。失败只告警不回退：主文件里仍可能有一份更大的 seq。
func (q *jsonlQueue) loadMeta() {
	raw, err := os.ReadFile(q.metaPath)
	if err != nil {
		return // 没有 meta 是正常情况（首次运行）
	}
	var meta struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		log.Printf("controlplane: 入站队列水位文件不可解析，忽略：%v", err)
		return
	}
	if meta.Seq > q.seq {
		q.seq = meta.Seq
	}
}

// saveMeta 落水位。先写临时文件再改名：半个 meta 比没有 meta 更坏。
func (q *jsonlQueue) saveMeta() {
	tmp := q.metaPath + ".tmp"
	raw, err := json.Marshal(map[string]int64{"seq": q.seq})
	if err != nil {
		return
	}
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		log.Printf("controlplane: 写入站队列水位失败：%v", err)
		return
	}
	if err := os.Rename(tmp, q.metaPath); err != nil {
		log.Printf("controlplane: 提交入站队列水位失败：%v", err)
	}
}
```

`Push()` 里在 `q.rewrite()` 成功之后加一行 `q.saveMeta()`：

```go
	if err := q.rewrite(); err != nil {
		q.broken = err
	} else {
		q.saveMeta()
	}
```

`rewrite()` 的错误路径补上临时文件清理（三处 `return err` 前加 `_ = os.Remove(tmp)`，`f.Close()` 之后同样处理）：

```go
		tmp := q.path + ".tmp" // 已在函数开头
		// 任何失败路径都清掉半成品，避免下次启动把 .tmp 误当数据
		defer func() {
			if _, statErr := os.Stat(tmp); statErr == nil {
				_ = os.Remove(tmp)
			}
		}()
```

> 注意：成功路径在 `os.Rename` 之后 `.tmp` 已不存在，`defer` 里的 `Stat` 会失败、自然跳过，不会误删。

- [ ] **Step 4: 跑测试至全绿**

Run: `go test ./internal/controlplane/ -run 'TestJSONLQueue' -count=1 -v`
Expected: 6 个用例全 PASS（原有 4 个 + 新增 2 个）

- [ ] **Step 5: 全包回归**

Run: `go test ./internal/controlplane/... -count=1 && gofmt -l internal/controlplane/`
Expected: 五个包全 ok；gofmt 无输出

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/jsonlqueue.go internal/controlplane/jsonlqueue_test.go
git commit -m "fix(controlplane): 入站队列序号水位独立落盘（主文件损坏时不回绕）"
```

**明确不改的**：`Push` 仍做全量 rewrite。改成 append 看似更快，但文件末尾若留半行（崩溃），append 会把新帧拼在坏行后面产出**一条更坏的行**，新帧静默丢失；而全量 rewrite 顺带把坏行治好。这条理由要写进代码注释，别让后人"优化"回去。

**复核追加（已随实现落地）**：Task 2b 的执行者发现 `load()` 在 `os.IsNotExist` 处**早退**，绕过了 `loadMeta()`——主文件被删、`.meta` 尚在时序号仍会回绕。已补一行 `q.loadMeta()` 与用例 `TestJSONLQueueKeepsSequenceWhenMainFileRemoved`（断言主文件缺席时 `Highest()` 仍为 2、下一帧为 3）。至此「坏文件」与「文件被删」两条路径都由 meta 兜底。

---

## Task 3: dev 身份（`config/users.dev.json` + token 签发/校验 + `POST /tenon/session`）

**Files:**
- Create: `internal/controlplane/session.go`, `internal/controlplane/session_test.go`, `config/users.dev.json`

- [ ] **Step 1: 新建 dev 账号文件**

```json
[
  {"user": "alice", "password": "alice-dev", "roles": ["role:warehouse_clerk"]},
  {"user": "bob", "password": "bob-dev", "roles": ["role:warehouse_manager", "role:procurement_manager"]}
]
```

- [ ] **Step 2: 写失败测试**

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionTokenRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	tok, err := SignToken(secret, "user:alice", []string{"role:warehouse_clerk"}, now, time.Hour)
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}
	claims, err := VerifyToken(secret, tok, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if claims.Subject != "user:alice" || len(claims.Roles) != 1 {
		t.Fatalf("claims 不符：%+v", claims)
	}
}

func TestVerifyTokenRejectsTamperedAndExpired(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	tok, _ := SignToken(secret, "user:alice", nil, now, time.Hour)

	if _, err := VerifyToken(secret, tok+"x", now); err == nil {
		t.Fatalf("篡改后必须失败")
	}
	if _, err := VerifyToken([]byte("other"), tok, now); err == nil {
		t.Fatalf("换密钥必须失败")
	}
	if _, err := VerifyToken(secret, tok, now.Add(2*time.Hour)); err == nil {
		t.Fatalf("过期必须失败")
	}
}

func TestSessionEndpointIssuesTokenForKnownUser(t *testing.T) {
	s := newTestServer(t, Config{
		AuditPath: t.TempDir() + "/audit.jsonl",
		Users: []User{
			{User: "alice", Password: "pw-a", Roles: []string{"role:warehouse_clerk"}},
		},
		SessionSecret: []byte("secret"),
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	status, body := postJSON(t, ts.URL+"/tenon/session", `{"user":"alice","password":"pw-a"}`)
	if status != http.StatusOK {
		t.Fatalf("登录应成功，得到 %d：%s", status, body)
	}
	var got struct {
		Token string   `json:"token"`
		User  string   `json:"user"`
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if got.User != "user:alice" || len(got.Roles) != 1 || got.Token == "" {
		t.Fatalf("响应不符：%+v", got)
	}

	if status, _ := postJSON(t, ts.URL+"/tenon/session", `{"user":"alice","password":"bad"}`); status != http.StatusUnauthorized {
		t.Fatalf("错口令应 401，得到 %d", status)
	}
}
```


- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/controlplane/ -run 'TestSession|TestVerifyToken' -count=1`
Expected: `undefined: SignToken`、`Config.Users` 不存在

- [ ] **Step 4: 实现 `session.go`**

```go
package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// User 是 dev 账号一行。文件名与字段都带 dev 语义：这不是真认证。
type User struct {
	User     string   `json:"user"`
	Password string   `json:"password"`
	Roles    []string `json:"roles"`
}

// Claims 是 token 的有效载荷。
type Claims struct {
	Subject string   `json:"sub"`
	Roles   []string `json:"roles"`
	Exp     int64    `json:"exp"`
}

// 错误值：调用方（HTTP 处理器）据此决定 401 还是 500。
var (
	ErrTokenInvalid = errors.New("token 无效")
	ErrTokenExpired = errors.New("token 已过期")
)

// LoadUsers 读取 dev 账号表。空文件视为「没有任何账号」（全部登录失败），不是错误。
func LoadUsers(path string) ([]User, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("controlplane: 读取账号文件失败：%w", err)
	}
	var users []User
	if err := json.Unmarshal(raw, &users); err != nil {
		return nil, fmt.Errorf("controlplane: 账号文件不是 JSON 数组：%w", err)
	}
	return users, nil
}

// SignToken 签发无状态 token：base64url(payload).base64url(HMAC-SHA256(payload))。
func SignToken(secret []byte, subject string, roles []string, now time.Time, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("controlplane: 会话密钥为空")
	}
	payload, err := json.Marshal(Claims{Subject: subject, Roles: roles, Exp: now.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// VerifyToken 校验签名与过期时间。任何一步失败都返回错误，不做「降级放行」。
func VerifyToken(secret []byte, token string, now time.Time) (Claims, error) {
	var out Claims
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return out, ErrTokenInvalid
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return out, ErrTokenInvalid
	}
	sig, err := enc.DecodeString(parts[1])
	if err != nil {
		return out, ErrTokenInvalid
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return out, ErrTokenInvalid
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, ErrTokenInvalid
	}
	if now.Unix() >= out.Exp {
		return out, ErrTokenExpired
	}
	return out, nil
}

// bearerClaims 从 Authorization 头取并校验 token。失败时已写好 401 响应。
func (s *Server) bearerClaims(w http.ResponseWriter, r *http.Request) (Claims, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		writeTenonErrorStatus(w, http.StatusUnauthorized, newError(protocol.ClassPolicyDenied,
			"AUTH_REQUIRED", "缺少 Authorization: Bearer <token>"))
		return Claims{}, false
	}
	claims, err := VerifyToken(s.sessionSecret, strings.TrimSpace(raw[len(prefix):]), time.Now().UTC())
	if err != nil {
		writeTenonErrorStatus(w, http.StatusUnauthorized, newError(protocol.ClassPolicyDenied,
			"AUTH_INVALID", err.Error()))
		return Claims{}, false
	}
	return claims, true
}

// serveSession 处理 POST /tenon/session：口令换 token。
func (s *Server) serveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var in struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {\"user\":…,\"password\":…}："+err.Error()))
		return
	}
	for _, u := range s.users {
		if u.User != in.User || u.Password != in.Password {
			continue
		}
		subject := "user:" + u.User
		tok, err := SignToken(s.sessionSecret, subject, u.Roles, time.Now().UTC(), sessionTTL)
		if err != nil {
			writeTenonErrorStatus(w, http.StatusInternalServerError,
				newError(protocol.ClassContractViolation, "TOKEN_SIGN_FAILED", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token": tok,
			"user":  subject,
			"roles": u.Roles,
			"exp":   time.Now().UTC().Add(sessionTTL).Unix(),
		})
		return
	}
	// 不区分「用户不存在」与「口令错」：dev 环境也不送可枚举的信息。
	writeTenonErrorStatus(w, http.StatusUnauthorized,
		newError(protocol.ClassPolicyDenied, "AUTH_INVALID", "账号或口令不正确"))
}
```

- [ ] **Step 5: 在 `http.go` 的 `Config` / `Server` 增字段并注册路由**

`Config` 增：

```go
	// Users 是 dev 账号表；为空表示登录端点永远失败（fail-closed）。
	Users []User
	// SessionSecret 用于签发/校验 token；为空会在 New 里报错。
	SessionSecret []byte
```

`Server` 增：

```go
	users         []User
	sessionSecret []byte
```

`New()` 里，在构造 `s` 之后、自检之前补：

```go
	if len(cfg.SessionSecret) == 0 {
		_ = store.Close()
		return nil, errors.New("controlplane: 会话密钥不能为空（TENON_SESSION_SECRET 或 dev 缺省值）")
	}
	s.users = cfg.Users
	s.sessionSecret = cfg.SessionSecret
```

`ServeHTTP` 的 switch 增：

```go
	case "/tenon/session":
		s.serveSession(w, r)
```

并新增常量（放在 `frameTypeConfirm` 之后）：

```go
	// sessionTTL 是 dev token 有效期。
	sessionTTL = 8 * time.Hour
```

- [ ] **Step 6: 修改测试装配助手 `newTestServer`（否则既有几十个用例会因缺少密钥全部失败）**

`internal/controlplane/http_test.go` 的 `newTestServer` 改为：

```go
// newTestServer 装配一个 Server 并登记清理。
//
// 缺省补齐测试必需的字段：会话密钥与入站目录。少了它们 New 会报错，
// 而既有用例大多不关心这两个字段。
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if len(cfg.SessionSecret) == 0 {
		cfg.SessionSecret = []byte("test-secret")
	}
	if strings.TrimSpace(cfg.InboundDir) == "" {
		cfg.InboundDir = t.TempDir() + "/inbound"
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("装配控制平面失败：%v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("关闭控制平面失败：%v", err)
		}
	})
	return srv
}
```

（文件头补 `"strings"` import。）

- [ ] **Step 7: 跑测试**

Run: `go test ./internal/controlplane/ -run 'TestSession|TestVerifyToken' -count=1 -v`
Expected: 3 个用例全 PASS

- [ ] **Step 8: Commit**

```bash
git add internal/controlplane/session.go internal/controlplane/session_test.go \
        internal/controlplane/http.go internal/controlplane/http_test.go config/users.dev.json
git commit -m "feat(controlplane): dev 身份（口令换无状态 token + /tenon/session）"
```

---

## Task 4: 入站三端点 + `origin` 登记表（独立落盘）

**Files:**
- Create: `internal/controlplane/inbound.go`, `internal/controlplane/inbound_test.go`
- Modify: `internal/controlplane/http.go`（Config/Server 字段、路由）

- [ ] **Step 1: 写失败测试**

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newInboundServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		Users:         []User{{User: "alice", Password: "pw", Roles: []string{"role:warehouse_clerk"}}},
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	return s, httptest.NewServer(s.Handler())
}

func login(t *testing.T, base string) string {
	t.Helper()
	status, body := postJSON(t, base+"/tenon/session", `{"user":"alice","password":"pw"}`)
	if status != http.StatusOK {
		t.Fatalf("登录失败 %d：%s", status, body)
	}
	var got struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("登录响应坏：%v", err)
	}
	return got.Token
}

func postJSONAuth(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

func TestInboundRequiresToken(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	status, _ := postJSON(t, ts.URL+"/tenon/requests", `{"type":"tenon.request","text":"hi"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，得到 %d", status)
	}
}

func TestInboundOverwritesByAndMintsSyntheticID(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	tok := login(t, ts.URL)

	status, body := postJSONAuth(t, ts.URL+"/tenon/requests", tok,
		`{"type":"tenon.request","requestId":"req_FAKE","text":"扣 12 件","by":"user:mallory","at":"2026-10-27T09:00:00Z"}`)
	if status != http.StatusOK {
		t.Fatalf("入队失败 %d：%s", status, body)
	}

	_, pollBody := getJSON(t, ts.URL+"/tenon/requests/poll?after=0")
	var got struct {
		Frames []struct {
			Seq   int64           `json:"seq"`
			Frame json.RawMessage `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(pollBody), &got); err != nil {
		t.Fatalf("poll 响应坏：%v", err)
	}
	if len(got.Frames) != 1 {
		t.Fatalf("应有 1 帧，得到 %d", len(got.Frames))
	}
	var frame struct {
		Type             string `json:"type"`
		RequestID        string `json:"requestId"`
		By               string `json:"by"`
		SyntheticEventID string `json:"syntheticEventId"`
	}
	if err := json.Unmarshal(got.Frames[0].Frame, &frame); err != nil {
		t.Fatalf("帧不是 JSON：%v", err)
	}
	if frame.By != "user:alice" {
		t.Fatalf("by 必须被 token 覆写，得到 %q", frame.By)
	}
	if frame.RequestID == "req_FAKE" || frame.RequestID == "" {
		t.Fatalf("requestId 必须由宿主签发，得到 %q", frame.RequestID)
	}
	if !strings.HasPrefix(frame.SyntheticEventID, "evt_") {
		t.Fatalf("syntheticEventId 必须以 evt_ 开头，得到 %q", frame.SyntheticEventID)
	}
}

// I8：登记表不能跟着内存队列消失，否则重启后未完成的 request 会被当成事件而自动通过。
func TestInboundOriginRegistrySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AuditPath:     dir + "/audit.jsonl",
		Users:         []User{{User: "alice", Password: "pw"}},
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	}
	s1, err := New(cfg)
	if err != nil {
		t.Fatalf("首次装配失败：%v", err)
	}
	ts1 := httptest.NewServer(s1.Handler())
	tok, _ := SignToken(cfg.SessionSecret, "user:alice", nil, time.Now().UTC(), time.Hour)
	if status, body := postJSONAuth(t, ts1.URL+"/tenon/requests", tok,
		`{"type":"tenon.request","text":"x"}`); status != http.StatusOK {
		t.Fatalf("入队失败 %d：%s", status, body)
	}
	var evtID string
	_, pollBody := getJSON(t, ts1.URL+"/tenon/requests/poll?after=0")
	var poll struct {
		Frames []struct {
			Frame json.RawMessage `json:"frame"`
		} `json:"frames"`
	}
	json.Unmarshal([]byte(pollBody), &poll)
	var frame struct {
		SyntheticEventID string `json:"syntheticEventId"`
	}
	json.Unmarshal(poll.Frames[0].Frame, &frame)
	evtID = frame.SyntheticEventID
	ts1.Close()
	s1.Close()

	s2, err := New(cfg)
	if err != nil {
		t.Fatalf("二次装配失败：%v", err)
	}
	defer s2.Close()
	if !s2.originRegistry().IsRequest(evtID) {
		t.Fatalf("重启后合成事件 id 仍须判定为 request：%q", evtID)
	}
}

func TestInboundRejectsUnknownFrameType(t *testing.T) {
	_, ts := newInboundServer(t)
	defer ts.Close()
	tok := login(t, ts.URL)
	status, _ := postJSONAuth(t, ts.URL+"/tenon/requests", tok, `{"type":"tenon.unknown"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知帧类型应 400，得到 %d", status)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controlplane/ -run 'TestInbound' -count=1`
Expected: `Config.InboundDir` 不存在 / 404 而非 200

- [ ] **Step 3: 实现 `inbound.go`**

```go
package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tenon.dev/internal/protocol"
)

// 入站帧类型：本切片只收这两种（tenon.context 不入站，见 spec §3.2）。
const (
	inboundTypeRequest = "tenon.request"
	inboundTypeConfirm = "tenon.confirm"
)

// originRecord 是宿主签发合成事件 id 时留下的凭证：只用来判定 origin，不参与执行。
type originRecord struct {
	EventID   string `json:"eventId"`
	RequestID string `json:"requestId"`
	By        string `json:"by"`
	At        string `json:"at"`
}

// originStore 是 origin 登记表。**必须独立落盘**（I8）：若跟着内存队列丢掉，
// 重启后未完成的 request 会被判成 event，从而绕过「对话发起必须审批」。
type originStore struct {
	mu      sync.RWMutex
	path    string
	byEvent map[string]originRecord
}

func newOriginStore(path string) (*originStore, error) {
	st := &originStore{path: path, byEvent: map[string]originRecord{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: 打开 origin 登记表失败：%w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var rec originRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		st.byEvent[rec.EventID] = rec
	}
	return st, sc.Err()
}

// Register 追加一条登记并落盘。落盘失败即返回错误：调用方不该在凭证没留下时就入队。
func (s *originStore) Register(rec originRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.byEvent[rec.EventID] = rec
	return nil
}

// IsRequest 回答「这枚事件 id 是不是人发起的」。未登记即视为事件驱动。
func (s *originStore) IsRequest(eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.byEvent[eventID]
	return ok
}

func (s *originStore) Get(eventID string) (originRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byEvent[eventID]
	return rec, ok
}

// originRegistry 只供测试与内部读用。
func (s *Server) originRegistry() *originStore { return s.origin }

// serveInboundEnqueue 处理 POST /tenon/requests。
//
// 三件必须做的事，缺一即失去治理意义：
//  1. 验 token（缺失/坏/过期一律 401，且不入队）；
//  2. 用 token 的 sub 覆写帧里的 by（客户端自称一律不认）；
//  3. 为 request 签发 requestId 与合成 eventId 并登记 origin。
func (s *Server) serveInboundEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	claims, ok := s.bearerClaims(w, r)
	if !ok {
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	frame, perr := normalizeInboundFrame(body, claims.Subject, s.origin)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		writeTenonErrorStatus(w, http.StatusInternalServerError,
			*newError(protocol.ClassContractViolation, "ENCODE_FAILED", err.Error()))
		return
	}
	seq := s.inbound.Push(raw)
	writeJSON(w, http.StatusOK, enqueueResponse{OK: true, Seq: seq})
}

// inboundFrame 不再单独定义类型：规范化后的帧就是 map[string]any（保留原字段 + 宿主签发字段）。

func normalizeInboundFrame(body []byte, subject string, origin *originStore) (map[string]any, *protocol.Error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH", "帧不是 JSON 对象："+err.Error())
	}
	typ, _ := raw["type"].(string)
	switch typ {
	case inboundTypeRequest:
		requestID := newRequestID()
		eventID := newSyntheticEventID()
		raw["requestId"] = requestID
		raw["syntheticEventId"] = eventID
		raw["by"] = subject
		raw["at"] = time.Now().UTC().Format(time.RFC3339)
		if err := origin.Register(originRecord{
			EventID: eventID, RequestID: requestID, By: subject,
			At: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return nil, newError(protocol.ClassContractViolation, "ORIGIN_REGISTER_FAILED",
				"登记合成事件 id 失败，拒绝入队："+err.Error())
		}
		return raw, nil
	case inboundTypeConfirm:
		raw["by"] = subject
		return raw, nil
	default:
		return nil, newError(protocol.ClassContractViolation, "UNKNOWN_FRAME_TYPE",
			"入站只接受 tenon.request 或 tenon.confirm，收到 "+typ)
	}
}

// serveInboundPoll 处理 GET /tenon/requests/poll?after=。
func (s *Server) serveInboundPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	after, perr := parseAfter(r.URL.Query().Get("after"))
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	frames, cursor, dropped := s.inbound.Since(after)
	writeJSON(w, http.StatusOK, pollResponse{Frames: frames, NextCursor: cursor, Dropped: dropped})
}

// serveInboundAck 处理 POST /tenon/requests/ack。
func (s *Server) serveInboundAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if perr := preflight(r); perr != nil {
		writeTenonError(w, *perr)
		return
	}
	body, perr := readBody(r)
	if perr != nil {
		writeTenonError(w, *perr)
		return
	}
	var req ackRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
			"请求体必须是 {\"upto\": <序号>}："+err.Error()))
		return
	}
	upto := s.inbound.Highest()
	if req.Upto != nil {
		upto = *req.Upto
	}
	writeJSON(w, http.StatusOK, ackResponse{OK: true, Acked: s.inbound.Ack(upto)})
}

// newRequestID / newSyntheticEventID 用纳秒+计数器生成，字符集满足 req_/evt_ 的词法约束。
func newRequestID() string      { return "req_" + newID("") }
func newSyntheticEventID() string { return "evt_" + newID("") }
```

`newID` 与 `strings` 未用到时删掉相应 import；若仓内已有 id 生成器（`internal/agent` 的 `newID`），此处需在 controlplane 内自带一份最小实现：

```go
// idSeq 保证同一纳秒内的多次签发不撞号。
var (
	idMu  sync.Mutex
	idSeq uint64
)

// newID 生成 base36 纳秒+序号；不含分隔符，满足 req_/evt_ 后续字符集。
func newID(prefix string) string {
	idMu.Lock()
	idSeq++
	n := idSeq
	idMu.Unlock()
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatUint(n, 36)
}
```

- [ ] **Step 4: 在 `http.go` 接线**

`Config` 增：

```go
	// InboundDir 是入站队列与 origin 登记表的目录。空则用 out/inbound。
	InboundDir string
```

`Server` 增：

```go
	inbound Queue
	origin  *originStore
```

`New()` 里、`s.users`/`s.sessionSecret` 之后补：

```go
	dir := cfg.InboundDir
	if strings.TrimSpace(dir) == "" {
		dir = defaultInboundDir
	}
	inboundQueue, err := newJSONLQueue(filepath.Join(dir, "requests.jsonl"), maxInboundFrames)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	origin, err := newOriginStore(filepath.Join(dir, "requests-index.jsonl"))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	s.inbound, s.origin = inboundQueue, origin
```

常量区补：

```go
	// defaultInboundDir 是入站队列与 origin 登记表的缺省目录。
	defaultInboundDir = "out/inbound"
	// maxInboundFrames 是入站队列容量。**独立于 maxIntentFrames**：后者是出站内存队列
	// 的容量语义，给「人说过的话」复用它，会让两件事的理由互相矛盾。
	maxInboundFrames = 1024
```

`ServeHTTP` 增三个 case：

```go
	case "/tenon/requests":
		s.serveInboundEnqueue(w, r)
	case "/tenon/requests/poll":
		s.serveInboundPoll(w, r)
	case "/tenon/requests/ack":
		s.serveInboundAck(w, r)
```

`Close()` 改为同时关闭入站队列：```go
func (s *Server) Close() error {
	err := s.auditStore.Close()
	if s.inbound != nil {
		if cerr := s.inbound.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
```

**别漏这一步**：给 `newTestServer` 补 `InboundDir` 缺省（Task 3 只补了 `SessionSecret`，见 §0 勘误 2）：

```go
	if strings.TrimSpace(cfg.InboundDir) == "" {
		cfg.InboundDir = t.TempDir() + "/inbound"
	}
```

不补的话，入站队列会落到 `defaultInboundDir = out/inbound`，几十个用例共用仓库工作目录、互相污染，`git status` 里还会冒出 `out/`。

- [ ] **Step 5: 跑测试**

Run: `go test ./internal/controlplane/ -run 'TestInbound' -count=1 -v`
Expected: 4 个用例全 PASS

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/inbound.go internal/controlplane/inbound_test.go internal/controlplane/http.go
git commit -m "feat(controlplane): 入站三端点 + origin 登记表独立落盘（I8）"
```

---

## Task 5: `origin` 进入策略引擎

**Files:**
- Modify: `internal/controlplane/policy/policy.go`, `internal/controlplane/policy/policy_test.go`
- Modify: `spec/0.1/schemas/policy.schema.json`
- Modify: `internal/controlplane/http.go`（`servePoliciesEvaluate` 解析 origin）

- [ ] **Step 1: 写失败测试（追加到 `policy_test.go`）**

```go
func TestConditionOriginRoot(t *testing.T) {
	cases := []struct {
		name   string
		when   string
		origin string
		want   string
	}{
		{"对话发起命中", `{"origin":{"eq":"request"}}`, "request", OutcomeApprove},
		{"事件发起不命中", `{"origin":{"eq":"request"}}`, "event", OutcomeDeny},
		{"缺 origin 不成立", `{"origin":{"eq":"request"}}`, "", OutcomeDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := NewPolicyEngine([]PolicyRule{{
				PolicyID: "p", Priority: 1,
				Match:   PolicyMatch{Module: "procurement", Actions: []string{"procurement.CreatePurchaseRequest"}},
				When:    mustCond(t, tc.when),
				Outcome: OutcomeApprove,
			}})
			got, err := engine.Evaluate(context.Background(), PolicyRequest{
				Action: "procurement.CreatePurchaseRequest",
				Origin: tc.origin,
			})
			if err != nil {
				t.Fatalf("求值失败：%v", err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("期望 %s，得到 %s", tc.want, got.Outcome)
			}
		})
	}
}

func TestParsePathRejectsOriginWithSegments(t *testing.T) {
	if _, _, err := parsePath("origin.sub"); err == nil {
		t.Fatalf("origin 是单段路径，带点多段必须被拒")
	}
	if root, segs, err := parsePath("origin"); err != nil || root != pathRootOrigin || len(segs) != 0 {
		t.Fatalf("origin 单段应被接受，得到 root=%q segs=%v err=%v", root, segs, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controlplane/policy/ -run 'TestConditionOriginRoot|TestParsePathRejectsOrigin' -count=1`
Expected: `PolicyRequest.Origin` 不存在；`origin` 被当非法前缀

- [ ] **Step 3: 改 `policy.go`**

`PolicyRequest` 增字段：

```go
	// Origin 是本次调用的来源：request（人发起）或 event（事件驱动）。
	// **由宿主从登记表判定**，不接受调用方自报。
	Origin string `json:"origin,omitempty"`
```

常量区增：

```go
	// pathRootOrigin 指向调用来源；它是单段路径（不带 `.`）。
	pathRootOrigin = "origin"
```

`evalScope` 增字段并参与取值的 `lookup` 分支——把 `lookup` 开头改为：

```go
func (s *evalScope) lookup(root string, path []string) (any, bool) {
	if root == pathRootOrigin {
		if len(path) != 0 || s.origin == "" {
			return nil, false
		}
		return s.origin, true
	}
	if root == pathRootRisk {
		…（保持原有 risk 分支不动）
```

`evalScope` 结构体补：

```go
	// origin 是本次调用的来源；空串表示未知（条件一律不成立）。
	origin string
```

`parsePath` 改为接受单段 `origin`：

```go
func parsePath(path string) (string, []string, error) {
	if path == pathRootOrigin {
		return pathRootOrigin, nil, nil
	}
	root, rest, found := strings.Cut(path, ".")
	if !found {
		return "", nil, fmt.Errorf("取值路径 %q 必须形如 arg.qty、risk.reversible 或 origin", path)
	}
	if root != pathRootArg && root != pathRootRisk {
		return "", nil, fmt.Errorf("取值路径 %q 的前缀只能是 arg、risk 或 origin", path)
	}
	…（其余段校验保持原样）
```

`Evaluate` 里构造 scope 时传入 origin：

```go
	scope := evalScope{args: req.Args, risk: req.Risk, origin: req.Origin}
```

（具体字段名以现有 `evalScope` 为准；若现有字段叫 `argsRaw`/`riskRaw`，按现状改名，只新增 `origin`。）

- [ ] **Step 4: 跑策略包测试**

Run: `go test ./internal/controlplane/policy/ -count=1`
Expected: 全 PASS（含新增 4 个）

- [ ] **Step 5: 更新 schema 的路径词法**

`spec/0.1/schemas/policy.schema.json` 的 `ConditionLeaf.properties` 里 propertyName 的 pattern：

```json
"pattern": "^([A-Za-z_][A-Za-z0-9_]*)(\\.[A-Za-z_][A-Za-z0-9_]*)*$"
```

并在 `description` 里补一句：`路径前缀为 arg / risk / origin；origin 是单段路径（不带点），语义由引擎校验。`

Run: `pnpm validate`
Expected: 规范自检通过（`scripts/validate-spec.mjs` 无 error）

- [ ] **Step 6: 在 `servePoliciesEvaluate` 里判定 origin**

`http.go` 的策略请求结构体（约 `http.go:300`）增字段：

```go
	// TriggerEventID 是本次计划的触发事件 id；宿主拿它查登记表判定 origin。
	TriggerEventID string `json:"triggerEventId,omitempty"`
```

处理函数里，构造 `policy.PolicyRequest` 之前插入：

```go
	origin := originEvent
	if strings.TrimSpace(in.TriggerEventID) == "" {
		if s.requireTrigger {
			writeJSON(w, http.StatusOK, policy.PolicyEvaluation{
				Outcome:  policy.OutcomeDeny,
				Reasons:  []string{"MISSING_TRIGGER"},
				DecidedBy: []string{},
			})
			return
		}
	} else if s.origin.IsRequest(in.TriggerEventID) {
		origin = originRequest
	}
```

常量区补：

```go
	// originRequest / originEvent 是两条来源轴取值。
	originRequest = "request"
	originEvent   = "event"
```

`Config` 增 `RequireTrigger bool`；`Server` 增 `requireTrigger bool`，`New()` 里赋值 `requireTrigger: cfg.RequireTrigger`。

- [ ] **Step 7: 跑 controlplane 全包测试**

Run: `go test ./internal/controlplane/... -count=1`
Expected: 全 PASS

- [ ] **Step 8: Commit**

```bash
git add internal/controlplane/policy/policy.go internal/controlplane/policy/policy_test.go \
        internal/controlplane/http.go spec/0.1/schemas/policy.schema.json
git commit -m "feat(policy): origin 成为策略输入（区分对话/事件来源）"
```

---

## Task 6: 待批公告板（`POST/GET /tenon/pending`）

**Files:**
- Create: `internal/controlplane/pending.go`, `internal/controlplane/pending_test.go`
- Modify: `internal/controlplane/http.go`（路由）

- [ ] **Step 1: 写失败测试**

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPendingBoardUpsertAndRoleFilter(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		Users: []User{
			{User: "alice", Password: "pw", Roles: []string{"role:warehouse_clerk"}},
			{User: "bob", Password: "pw", Roles: []string{"role:warehouse_manager"}},
		},
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	agentTok, _ := SignToken([]byte("secret"), "agent:ag_fulfillment", nil, time.Now().UTC(), time.Hour)
	body := `{"planId":"pln_1","requestId":"req_1","by":"user:alice",
	          "summary":"扣 12 件、开 1 张采购单","items":[{"label":"从标准件仓扣掉 12 件"}],
	          "requiredRoles":["role:warehouse_manager"],"expiresAt":"2026-10-27T09:15:00Z"}`
	if status, resp := postJSONAuth(t, ts.URL+"/tenon/pending", agentTok, body); status != http.StatusOK {
		t.Fatalf("推摘要失败 %d：%s", status, resp)
	}
	// 幂等 upsert：同 planId 再推一次不应变成两条。
	postJSONAuth(t, ts.URL+"/tenon/pending", agentTok, body)

	bobTok, _ := SignToken([]byte("secret"), "user:bob", []string{"role:warehouse_manager"}, time.Now().UTC(), time.Hour)
	_, raw := getJSONAuth(t, ts.URL+"/tenon/pending", bobTok)
	var got struct {
		Items []struct {
			PlanID string `json:"planId"`
			Mine   bool   `json:"mine"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("响应坏：%v", err)
	}
	if len(got.Items) != 1 || !got.Items[0].Mine {
		t.Fatalf("仓管应看到 1 条且 mine=true，得到 %+v", got.Items)
	}

	aliceTok, _ := SignToken([]byte("secret"), "user:alice", []string{"role:warehouse_clerk"}, time.Now().UTC(), time.Hour)
	_, raw = getJSONAuth(t, ts.URL+"/tenon/pending", aliceTok)
	json.Unmarshal([]byte(raw), &got)
	if len(got.Items) != 1 || got.Items[0].Mine {
		t.Fatalf("业务员应看到同一条但 mine=false，得到 %+v", got.Items)
	}
}
```

需要补测试辅助（放在 `inbound_test.go` 末尾即可）：

```go
func getJSONAuth(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controlplane/ -run 'TestPendingBoard' -count=1`
Expected: 404

- [ ] **Step 3: 实现 `pending.go`**

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"tenon.dev/internal/protocol"
)

// pendingItem 是公告板上一行：只放展示所需信息，不放完整 plan。
type pendingItem struct {
	PlanID        string            `json:"planId"`
	RequestID     string            `json:"requestId,omitempty"`
	By            string            `json:"by,omitempty"`
	Summary       string            `json:"summary"`
	Items         []map[string]any  `json:"items,omitempty"`
	RequiredRoles []string          `json:"requiredRoles"`
	ExpiresAt     string            `json:"expiresAt"`
	Mine          bool              `json:"mine"`
}

// pendingBoard 是待批公告板：**只读 read-model**，权威状态在 agent 盘上的 pending 文件。
// host 不判断、不执行、不授权；GET 的 mine 只用于页面显示。
type pendingBoard struct {
	mu    sync.RWMutex
	items map[string]pendingItem
}

func newPendingBoard() *pendingBoard {
	return &pendingBoard{items: map[string]pendingItem{}}
}

// Upsert 幂等写入（同 planId 覆盖）。agent 每 tick 重推即靠它自愈。
func (b *pendingBoard) Upsert(it pendingItem) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[it.PlanID] = it
}

// List 返回按角色过滤后的列表：过期条目顺手剔除。
func (b *pendingBoard) List(roles []string, now time.Time) []pendingItem {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]pendingItem, 0, len(b.items))
	for id, it := range b.items {
		if exp, err := time.Parse(time.RFC3339, it.ExpiresAt); err == nil && now.After(exp) {
			delete(b.items, id)
			continue
		}
		it.Mine = rolesIntersect(roles, it.RequiredRoles)
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out
}

func rolesIntersect(have, need []string) bool {
	if len(need) == 0 {
		return false
	}
	for _, n := range need {
		for _, h := range have {
			if h == n {
				return true
			}
		}
	}
	return false
}

// servePending 处理 POST（agent 推摘要）与 GET（前端按角色取列表）。
func (s *Server) servePending(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		claims, ok := s.bearerClaims(w, r)
		if !ok {
			return
		}
		if perr := preflight(r); perr != nil {
			writeTenonError(w, *perr)
			return
		}
		body, perr := readBody(r)
		if perr != nil {
			writeTenonError(w, *perr)
			return
		}
		var it pendingItem
		if err := json.Unmarshal(body, &it); err != nil {
			writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH",
				"公告板条目不是合法 JSON："+err.Error()))
			return
		}
		if it.PlanID == "" {
			writeTenonError(w, *newError(protocol.ClassContractViolation, "SCHEMA_MISMATCH", "planId 必填"))
			return
		}
		it.By = claims.Subject
		s.pending.Upsert(it)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodGet:
		claims, ok := s.bearerClaims(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": s.pending.List(claims.Roles, time.Now().UTC()),
		})
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}
```

- [ ] **Step 4: `http.go` 接线**

`Server` 增 `pending *pendingBoard`，`New()` 里 `pending: newPendingBoard(),`；`ServeHTTP` 增：

```go
	case "/tenon/pending":
		s.servePending(w, r)
```

- [ ] **Step 5: 跑测试**

Run: `go test ./internal/controlplane/ -run 'TestPendingBoard' -count=1 -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/pending.go internal/controlplane/pending_test.go \
        internal/controlplane/inbound_test.go internal/controlplane/http.go
git commit -m "feat(controlplane): 待批公告板（幂等 upsert + 按角色过滤）"
```

---

## Task 7: 业务级规则加载 + 加载期路径校验

**Files:**
- Create: `internal/controlplane/rules.go`, `internal/controlplane/rules_test.go`
- Create: `skills/quote-to-fulfillment/approval.json`

- [ ] **Step 1: 新建业务规则文件**

```json
[
  {
    "policyId": "biz-purchase-auto-small",
    "priority": 100,
    "match": {"module": "procurement", "actions": ["procurement.CreatePurchaseRequest"]},
    "when": {"all": [{"arg.qty": {"<=": 5}}]},
    "outcome": "auto"
  },
  {
    "policyId": "biz-purchase-approve",
    "priority": 100,
    "match": {"module": "procurement", "actions": ["procurement.CreatePurchaseRequest"]},
    "when": {"all": []},
    "outcome": "approve",
    "approvers": ["role:procurement_manager"]
  }
]
```

（阈值先用 `arg.qty`：`procurement.CreatePurchaseRequest` 的 inputSchema 只有 `sku/qty/referenceNo`，**没有金额字段**。等业务侧决定加 `amount` 再改这里，属于业务决策不属于平台适配。）

- [ ] **Step 2: 写失败测试**

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeModule 提供一个带 inputSchema 的 action 定义，供规则路径校验使用。
func fakeModule(t *testing.T, moduleID string, schemas map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/actions/", func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Base(r.URL.Path)
		schema, ok := schemas[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "inputSchema": json.RawMessage(schema),
		})
	})
	return httptest.NewServer(mux)
}

func TestLoadApprovalRulesAcceptsValidPaths(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "quote-to-fulfillment")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(`[
	  {"policyId":"biz-1","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[{"arg.qty":{"<=":5}}]},"outcome":"auto"}
	]`), 0o600)

	rules, errs := LoadApprovalRules(filepath.Join(dir, "skills"), func(module string) (string, bool) {
		return mod.URL, true
	})
	if len(errs) != 0 {
		t.Fatalf("不应有错误：%+v", errs)
	}
	if len(rules) != 1 || rules[0].PolicyID != "biz-1" {
		t.Fatalf("规则未加载：%+v", rules)
	}
}

// 字母级笔误在 not 取反下会把「大额要批」翻成「大额自动过」，必须加载期就拒。
func TestLoadApprovalRulesRejectsUnknownPath(t *testing.T) {
	mod := fakeModule(t, "procurement", map[string]string{
		"procurement.CreatePurchaseRequest": `{"type":"object","properties":{"qty":{"type":"integer"}}}`,
	})
	defer mod.Close()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "quote-to-fulfillment")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(`[
	  {"policyId":"biz-typo","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"not":{"arg.amout":{"<=":5000}}},"outcome":"auto"}
	]`), 0o600)

	rules, errs := LoadApprovalRules(filepath.Join(dir, "skills"), func(module string) (string, bool) {
		return mod.URL, true
	})
	if len(rules) != 0 {
		t.Fatalf("路径不存在的规则必须整份丢弃，得到 %+v", rules)
	}
	if len(errs) == 0 {
		t.Fatalf("必须报告错误")
	}
}

// 模块离线时该业务规则不生效，但不能让平台级规则也失效。
func TestLoadApprovalRulesSkipsOfflineModule(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "s1")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "approval.json"), []byte(`[
	  {"policyId":"biz-offline","priority":1,
	   "match":{"module":"procurement","actions":["procurement.CreatePurchaseRequest"]},
	   "when":{"all":[]},"outcome":"approve"}
	]`), 0o600)

	rules, errs := LoadApprovalRules(filepath.Join(dir, "skills"), func(module string) (string, bool) {
		return "", false
	})
	if len(rules) != 0 || len(errs) == 0 {
		t.Fatalf("模块离线应跳过并报错：rules=%d errs=%d", len(rules), len(errs))
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/controlplane/ -run 'TestLoadApprovalRules' -count=1`
Expected: `undefined: LoadApprovalRules`

- [ ] **Step 4: 实现 `rules.go`**

```go
package controlplane

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tenon.dev/internal/controlplane/policy"
)

// ruleLoadError 描述一条「该 skill 的规则整份不生效」的原因。
type ruleLoadError struct {
	Path string
	Err  error
}

func (e ruleLoadError) Error() string { return e.Path + ": " + e.Err.Error() }

// LoadApprovalRules 遍历 skills/*/approval.json，编译成策略规则并做加载期路径校验。
//
// baseURLFor 返回模块 baseURL；模块离线时该 skill 的规则整份不加载（fail-closed 到平台级规则）。
// 返回的 errs 只用于告警，不中断加载：一个 skill 坏了不该拖垮整个宿主。
func LoadApprovalRules(skillsDir string, baseURLFor func(module string) (string, bool)) ([]policy.PolicyRule, []error) {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("读取 skills 目录失败：%w", err)}
	}
	var (
		rules []policy.PolicyRule
		errs  []error
		names []string
	)
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 顺序稳定：同一份目录必得同一规则顺序

	for _, name := range names {
		path := filepath.Join(skillsDir, name, "approval.json")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, ruleLoadError{Path: path, Err: err})
			continue
		}
		var fileRules []policy.PolicyRule
		if err := json.Unmarshal(raw, &fileRules); err != nil {
			errs = append(errs, ruleLoadError{Path: path, Err: fmt.Errorf("不是合法规则数组：%w", err)})
			continue
		}
		bad, verr := validateRulePaths(fileRules, baseURLFor)
		if verr != nil {
			errs = append(errs, ruleLoadError{Path: path, Err: verr})
			continue
		}
		if bad {
			errs = append(errs, ruleLoadError{Path: path, Err: fmt.Errorf("规则引用的入参路径不存在，整份丢弃")})
			continue
		}
		rules = append(rules, fileRules...)
	}
	return rules, errs
}

// validateRulePaths 用 action 的 inputSchema 校验每条 arg.* 路径的首段是否存在。
//
// 只校验首段：`not` 取反下的笔误（arg.amout）正落在首段，这一层就够挡住；
// 嵌套段需要完整 schema 递归，收益不足以引入解析器的复杂度。
func validateRulePaths(rules []policy.PolicyRule, baseURLFor func(string) (string, bool)) (bool, error) {
	cache := map[string]map[string]bool{} // actionID -> 允许的 arg 首段
	for _, rule := range rules {
		for _, action := range rule.Match.Actions {
			allowed, ok := cache[action]
			if !ok {
				module := strings.SplitN(action, ".", 2)[0]
				base, online := baseURLFor(module)
				if !online {
					return false, fmt.Errorf("模块 %q 离线，无法校验 %q 的入参路径", module, action)
				}
				schema, err := fetchInputSchema(base, action)
				if err != nil {
					return false, err
				}
				allowed = firstLevelProps(schema)
				cache[action] = allowed
			}
			for _, path := range collectArgPaths(rule.When) {
				first := strings.SplitN(strings.TrimPrefix(path, "arg."), ".", 2)[0]
				if !allowed[first] {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// fetchInputSchema 拉一次 action 定义（manifest 摘要不含 inputSchema，是按需拉取的设计）。
func fetchInputSchema(baseURL, actionID string) (json.RawMessage, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/tenon/actions/" + actionID)
	if err != nil {
		return nil, fmt.Errorf("拉取 %q 的定义失败：%w", actionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取 %q 的定义返回 %d", actionID, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var wrap struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	return wrap.InputSchema, nil
}

// firstLevelProps 取 inputSchema.properties 的键集合。
func firstLevelProps(schema json.RawMessage) map[string]bool {
	out := map[string]bool{}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return out
	}
	for k := range s.Properties {
		out[k] = true
	}
	return out
}

// collectArgPaths 从条件树里收集所有 arg.* 叶子路径（与求值器同一套语法）。
func collectArgPaths(when json.RawMessage) []string {
	var out []string
	if len(when) == 0 {
		return out
	}
	var node map[string]json.RawMessage
	if err := json.Unmarshal(when, &node); err != nil {
		return out
	}
	var walk func(map[string]json.RawMessage)
	walk = func(m map[string]json.RawMessage) {
		for key, val := range m {
			switch key {
			case "all", "any":
				var list []map[string]json.RawMessage
				if err := json.Unmarshal(val, &list); err != nil {
					continue
				}
				for _, child := range list {
					walk(child)
				}
			case "not":
				var child map[string]json.RawMessage
				if err := json.Unmarshal(val, &child); err == nil {
					walk(child)
				}
			default:
				if strings.HasPrefix(key, "arg.") {
					out = append(out, key)
				}
			}
		}
	}
	walk(node)
	sort.Strings(out)
	return out
}
```

- [ ] **Step 5: 跑测试**

Run: `go test ./internal/controlplane/ -run 'TestLoadApprovalRules' -count=1 -v`
Expected: 3 个用例全 PASS

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/rules.go internal/controlplane/rules_test.go \
        skills/quote-to-fulfillment/approval.json
git commit -m "feat(controlplane): 业务级规则加载与加载期路径校验（挡 not 取反笔误）"
```

---

## Task 8: `GET /` 静态页路由（页面内容留给 P3）

**Files:**
- Create: `internal/controlplane/static.go`
- Modify: `internal/controlplane/http.go`（路由 + Config/Server 字段）

- [ ] **Step 1: 实现 `static.go`**

```go
package controlplane

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// serveStatic 只服务 `GET /` 与其下的静态资源；目录不存在时回 503 并说明原因——
// 「页面没配」是可诊断的部署问题，不该伪装成 404。
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if strings.TrimSpace(s.uiDir) == "" {
		http.Error(w, "未配置 UI 目录（TENON_UI_DIR）", http.StatusServiceUnavailable)
		return
	}
	clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "." || clean == string(filepath.Separator) {
		clean = "index.html"
	}
	full := filepath.Join(s.uiDir, clean)
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}
```

- [ ] **Step 2: 接线**

`Config` 增 `UIDir string`；`Server` 增 `uiDir string`；`New()` 里 `uiDir: cfg.UIDir,`。

`ServeHTTP` 在 switch **之前**加一条（静态资源路径不固定）：

```go
	if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/ui/") {
		s.serveStatic(w, r)
		return
	}
```

同时把静态资源实际路径改成 `/ui/` 前缀去目录（`serveStatic` 里 `TrimPrefix` 后已是这样）。

- [ ] **Step 3: 手工验证**

Run: `go test ./internal/controlplane/ -count=1`（确保没打破既有路由）
Expected: 全 PASS

- [ ] **Step 4: Commit**

```bash
git add internal/controlplane/static.go internal/controlplane/http.go
git commit -m "feat(controlplane): GET / 同源静态页路由（无 UI 目录时 503 可诊断）"
```

---

## Task 9: `cmd/host` 装配新环境变量

**Files:**
- Modify: `cmd/host/main.go`

- [ ] **Step 1: 增环境变量常量与装配**

在既有 env 常量区补：

```go
	envUsersFile     = "TENON_USERS_FILE"
	envSessionSecret = "TENON_SESSION_SECRET"
	envInboundDir    = "TENON_INBOUND_DIR"
	envSkillsDir     = "TENON_SKILLS_DIR"
	envUIDir         = "TENON_UI_DIR"
	envRequireTrig   = "TENON_REQUIRE_TRIGGER"
```

`loadConfig()` 里、`cfg.Modules` 之后补：

```go
	if path := strings.TrimSpace(os.Getenv(envUsersFile)); path != "" {
		users, err := controlplane.LoadUsers(path)
		if err != nil {
			return cfg, err
		}
		cfg.Users = users
	} else {
		log.Printf("host: 未设置 %s，登录端点将永远拒绝", envUsersFile)
	}

	secret := strings.TrimSpace(os.Getenv(envSessionSecret))
	if secret == "" {
		secret = devSessionSecret // 见下方：dev 缺省值，启动时打警告
		log.Printf("host: 未设置 %s，使用 dev 缺省密钥 —— 仅限本地演示", envSessionSecret)
	}
	cfg.SessionSecret = []byte(secret)

	cfg.InboundDir = strings.TrimSpace(os.Getenv(envInboundDir))
	cfg.UIDir = strings.TrimSpace(os.Getenv(envUIDir))
	cfg.RequireTrigger = strings.TrimSpace(os.Getenv(envRequireTrig)) == "1"

	if dir := strings.TrimSpace(os.Getenv(envSkillsDir)); dir != "" {
		rules, errs := controlplane.LoadApprovalRules(dir, func(module string) (string, bool) {
			for _, m := range cfg.Modules {
				if m.ID == module && m.Enabled {
					return m.BaseURL, true
				}
			}
			return "", false
		})
		for _, e := range errs {
			log.Printf("host: 业务规则未生效：%v", e)
		}
		cfg.Policies = append(cfg.Policies, rules...)
		if len(rules) > 0 {
			log.Printf("host: 业务级规则已加载 %d 条（来自 %s）", len(rules), dir)
		}
	}
```

常量区补：

```go
	// devSessionSecret 是本地演示用的缺省密钥；生产必须显式设置。
	devSessionSecret = "tenon-dev-session-secret"
```

- [ ] **Step 2: 编译并跑 host 冒烟**

```bash
go build ./... && go vet ./...
```

Expected: 无输出（成功）

- [ ] **Step 3: 端到端 curl 冒烟（手工，记入验收）**

```bash
# 三个终端分别起（第三个之后）
TENON_MODULES='[{"id":"procurement","baseURL":"http://127.0.0.1:8082","enabled":true}]' \
TENON_POLICY_FILE=config/policies.dev.json \
TENON_USERS_FILE=config/users.dev.json \
TENON_SKILLS_DIR=skills \
TENON_UI_DIR=ui \
go run ./cmd/host
```

期望：启动日志出现「使用 dev 缺省密钥」与「业务级规则已加载 N 条」（若 procurement 不在线则出现「业务规则未生效：…离线…」并把动作留给平台规则）。

```bash
# 登录
curl -s -X POST localhost:8080/tenon/session -d '{"user":"alice","password":"alice-dev"}'
# 入队
curl -s -X POST localhost:8080/tenon/requests -H "Authorization: Bearer <token>" \
  -d '{"type":"tenon.request","text":"扣 12 件"}'
# 回放
curl -s "localhost:8080/tenon/requests/poll?after=0"
```

期望：第二条回 `{"ok":true,"seq":1}`；第三条的 `frame.by` 是 `user:alice`、`syntheticEventId` 以 `evt_` 开头。

- [ ] **Step 4: Commit**

```bash
git add cmd/host/main.go config/users.dev.json
git commit -m "feat(host): 装配入站队列/身份/业务规则/静态页环境变量"
```

---

## Task 10: 全量回归 + spec 分阶段说明回填

**Files:**
- Modify: `docs/superpowers/specs/2026-10-27-tenon-codrive-chat-entry-design.md`（§3.11 增「分阶段启用」）

- [ ] **Step 1: 跑全量检查**

```bash
go test ./... -count=1
pnpm check
```

Expected: Go 全 PASS；`pnpm check` = `validate` + `typecheck` 全过

- [ ] **Step 2: 回填 spec 的分阶段说明**

在 spec §3.11 末尾追加：
```markdown
**分阶段启用（实施期记录）**：`triggerEventId` 必填的强制行为由 `TENON_REQUIRE_TRIGGER` 控制。P1 默认关闭（缺失视为 `event`，保持既有 agent 可用）；P2 让 agent 全部带上该字段；P4 收口时把默认改为强制（缺失即 deny）并删除开关。在此之前，spec 记的「必填」是实现目标态，不是当期运行时行为。
```

- [ ] **Step 3: 补 `docs/interfaces.md` 的策略评估请求体**

`docs/interfaces.md §4.2` 的 `POST /tenon/policies:evaluate` 请求体仍只有 `{plan:…}`，与实现已经矛盾。补两处：

1. 请求体加 `triggerEventId`（可选，宿主据此判定 `origin`）；
2. 响应说明补一句：`triggerEventId` 缺失且 `TENON_REQUIRE_TRIGGER=1` 时，回 `200` + `outcome=deny` + `reasons:["MISSING_TRIGGER"]`（不是 4xx）。

- [ ] **Step 4: 确认文档与实现一致后提交**

```bash
git add docs/superpowers/specs/2026-10-27-tenon-codrive-chat-entry-design.md
git commit -m "docs(spec): 记录 triggerEventId 的分阶段启用（P1 过渡开关）"
```

---

## 自检结果（写完后逐条过了一遍 spec）

| spec 要求 | 落在哪个任务 |
|---|---|
| §3.2 入站 3 端点（独立队列，不共用累积游标） | Task 1（端口）+ Task 4 |
| §3.5 公告板 + I5 重建（幂等 upsert） | Task 6 |
| §3.7 dev 登录换 token | Task 3 |
| §3.8 规则分层 + fail-closed 降级 | Task 7 + Task 9（装配告警） |
| §3.10 加载期路径校验（`not` 反例） | Task 7（`TestLoadApprovalRulesRejectsUnknownPath`） |
| §3.11 `origin` 由宿主判定 + 缺字段行为 | Task 4（签发/登记）+ Task 5（进引擎）+ Task 10（分阶段说明） |
| §8 入站落盘、出站不改造 | Task 2（jsonl）+ Task 1（出站保持 memory） |
| §9 测试策略（队列/身份/公告板/规则/`origin`/I8 重启） | Task 2–7 各自的测试；I8 在 `TestInboundOriginRegistrySurvivesRestart` |
| I8 登记表独立持久化 | Task 4 |
| 静态页路由 | Task 8 |

**未覆盖（属于 P2/P3/P4，不在本计划）**：agent 的挂起/恢复（I1–I7）、`pendingPlanId`、skill 粗筛、白话渲染、单页界面、`interfaces.md` / `CHANGELOG` / README 收口、`CODEOWNERS`。

**类型一致性检查**：`Queue` 接口（`Push/Since/Ack/Highest/Close`）在 Task 1 定义，Task 2/4 使用一致；`queuedFrame` 保持原线上形状（`seq`/`frame`），Task 4 的 `pollResponse` 复用未变；`policy.PolicyRule` 字段（`PolicyID/Priority/Match/When/Outcome/Approvers`）与 Task 7 的 approval.json 一致；`Config` 新增字段（`Users/SessionSecret/InboundDir/UIDir/RequireTrigger`）在 Task 3/4/5/8 分别声明、Task 9 装配。
