# P2 实施计划：agent 挂起与恢复

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `cmd/agent` 消费 host 的入站队列（`tenon.request` / `tenon.confirm`），把命中 `approve` 的计划落盘挂起，并在合法批准后按原计划恢复执行；崩溃与重放都不产生重复副作用。

**Architecture:** agent 首次拥有持久状态，统一放在 `TENON_AGENT_STATE_DIR`（`pending/`、`pending/archived/`、`processed.jsonl`），与 host 的 `TENON_INBOUND_DIR` **分开**。挂起 = 三次有序落盘（pending → dispatched 审计 → 回写标记，I7）；恢复 = 校验审批人 → 过期 → 重跑 G4 → 重跑 policy/grant → 执行全部步骤。一个 tick 依次做：入站 drain（逐帧 ack）→ 过期清理 → 公告板重推 → 事件轮询。

**Tech Stack:** Go 1.26；`net/http`（假 host 用 `httptest`）、`os`（tmp+rename 原子写）、`sync`、`testing` + `t.TempDir()`。不引入新依赖、不引入数据库。

**对应 spec:** [`docs/superpowers/specs/2026-10-27-tenon-agent-resume-design.md`](../specs/2026-10-27-tenon-agent-resume-design.md)（§3 数据契约、§4 I7、§5 恢复流程、§6 tick、§7 粗筛、§8 身份、§10 不变量）

---

## 0. 前置说明（执行前必读）

### 0.1 Task 1 复核后追加的执行期约束（比正文更权威）

1. **测试夹具一律注入固定时间，不许用 `time.Now()`**。`samplePending().ExpiresAt = 2026-10-27T09:30:03Z` 是个会到期的常量：任何断言"保留/补标记"的用例若传 `time.Now()`，越过该时刻后就会自己变红。Task 3 的 `TestSelfCheck*` 必须传 `time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")`。
2. **`Archive` 不幂等**：pending 不在盘时返回 `agent: 归档时找不到 <id>`。Task 8 的恢复路径要允许"已被别人归档"，把该错误当已归档处理并记日志。
3. **`Archive` 两步（写归档件 → 删 pending 件）之间失败会留双份**：`List()` 仍会返回那份 `awaiting_approval` 的 pending。恢复路径必须容忍"pending 与 archived 同名共存"——进 resume 前先查 archived，若已 `done` 直接按幂等返回。
4. **`StateStore.List()` 静默跳过坏 JSON**：损坏的 pending 文件对 `List()`/`selfCheck` 都不可见。Task 3 必须补一个 `CountCorrupt()` 并在自检里报数（只告警、不中断）。
5. **`Save` 是整文件替换、无锁**：串行化只能落在单线程 tick（Task 10），不要指望 store。
6. **`NewStateStore("")` 报错**：`Deps.State == nil` 才表示"没有挂起通道"；装配时不要把空串传进去。
7. **夹具必须填 `ExpiresAt`**：`Expired()` 对解析失败 fail-closed 返回 true，漏填的夹具会被立刻判过期。


**已完成的前置**：P1 全量落地（host 入站面 + `origin` + 业务规则 + `role:agent`），commit `74182cd`。本次改动**只碰 `internal/agent/`、`cmd/agent/`、`internal/controlplane/`（仅 Task 11）与文档**。

**已存在的类型（别重复定义，先读再写）**：

| 名字 | 位置 | 说明 |
|---|---|---|
| `RunInput{Event, CorrelationID}` / `RunResult{Outcome, PlanID, ActionsTaken, Detail}` | `internal/agent/run.go:92,99` | 回合入口与结局；本计划要给 `RunInput` 加 `Request`、给 `RunResult` 加 `PendingPlanID` |
| `Outcome` 三态 | `run.go:75-85` | `completed` / `needs_human` / `failed`；**不得新增第四态**（`run.go:74` 注释写死了三态封闭） |
| `Deps{Host, AgentID, OnBehalfOf, Planner, HTTP, Skills, Trace}` | `run.go:110` | 本计划不扩它，新依赖走新的子包 |
| `Plan` / `PlanStep` / `PlanTrigger` | `internal/agent/planner.go:100-126` | `PlanTrigger{EventID, CorrelationID}` |
| `PlanInput{Event, SkillText, Modules, Feedback}` | `planner.go:90` | 本计划加 `Request *UserRequest` |
| `round()` 的闸门顺序 | `run.go:215-314` | 策略 → 授权 → 预演 → 执行；**Task 7 要把它重构为「全部闸门跑完再决定挂起还是执行」** |
| `auditRecord` / `auditBase` / `resultAudit` | `run.go:644-750` | 计划级审计用 `stepId` 为空（`nilIfEmpty` 会把空串写成 `null`） |
| `httptest` 假 host 的写法 | `internal/agent/run_test.go` | 已有假 host 与假 LLM 的桩，沿用其风格 |

**一处与既有行为的冲突（Task 7 必须处理）**：`run.go:239-243` 现在是"策略不是 `auto` 就转人工"，本计划要求"`approve` 走挂起、`deny` 仍转人工"，且**授权与预演要在挂起之前跑完**。现有测试 `TestPolicyApproveGoesNeedsHumanWithoutApprovalChannel` 会红，Task 7 要求把它改成"无入站通道时转人工"（spec §12）。

**每完成一个 Task 就提交一次**，commit message 用任务里给的原话。

---

## 文件结构（先锁边界）

| 文件 | 职责 | 动作 |
|---|---|---|
| `internal/agent/state.go` | `Pending` 类型、`StateStore`（读/写/归档/列表/清理）、I7 的原子写 | 新建 |
| `internal/agent/ledger.go` | `Ledger`：`processed.jsonl` 的写前标记与查询 | 新建 |
| `internal/agent/suspend.go` | 挂起的三次落盘（I7）与自检分流 | 新建 |
| `internal/agent/resume.go` | 恢复流程（匹配/审批人/过期/G4/policy/执行/归档） | 新建 |
| `internal/agent/hostapi.go` | 登录/重登、入站 poll/ack、公告板推送 | 新建 |
| `internal/agent/skillselect.go` | skill 确定性粗筛 | 新建 |
| `internal/agent/run.go` | `RunInput` 加 `Request`、`RunResult` 加 `PendingPlanID`、`round()` 闸门重构、`userPrompt` 分流 | 修改 |
| `internal/agent/planner.go` | `PlanInput` 加 `Request`、`userPrompt` 增用户请求块 | 修改 |
| `cmd/agent/main.go` | tick 四段顺序、状态目录与环境变量装配、登录 | 修改 |
| `internal/controlplane/http.go` | Task 11：`TENON_TEST_MODE` 下不挂入站端点 | 修改 |

---

## Task 1: 状态目录与 `Pending` 读写

**状态：✅ 已完成（commit `3421ff0`）**

**Files:**
- Create: `internal/agent/state.go`
- Test: `internal/agent/state_test.go`

- [x] **Step 1: 写失败测试**

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func samplePending() Pending {
	rid := "req_1"
	return Pending{
		PlanID:            "pln_1",
		State:             StateAwaitingApproval,
		RequestID:         &rid,
		TriggerEventID:    "evt_1",
		Origin:            "request",
		By:                "user:alice",
		Plan:              Plan{PlanID: "pln_1", Steps: []PlanStep{{StepID: "s1", Action: "inventory.DeductStock", Kind: "write"}}},
		Labels:            []string{"从标准件仓扣掉 12 件"},
		Summary:           "扣 12 件",
		RequiredRoles:     []string{"role:warehouse_manager"},
		DispatchedAuditID: "aud_1",
		CreatedAt:         "2026-10-27T09:15:03Z",
		ExpiresAt:         "2026-10-27T09:30:03Z",
	}
}

func TestStateStoreSaveLoad(t *testing.T) {
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	want := samplePending()
	if err := store.Save(want); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	got, ok, err := store.Load("pln_1")
	if err != nil || !ok {
		t.Fatalf("读取失败：%v ok=%v", err, ok)
	}
	if got.State != StateAwaitingApproval || got.Plan.Steps[0].Action != "inventory.DeductStock" {
		t.Fatalf("内容不符：%+v", got)
	}
	// 没有临时文件残留（原子写必须清干净）。
	entries, _ := os.ReadDir(filepath.Join(store.Dir(), "pending"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件：%s", e.Name())
		}
	}
}

func TestStateStoreArchiveMovesFile(t *testing.T) {
	store, _ := NewStateStore(t.TempDir())
	p := samplePending()
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := store.Archive(p.PlanID, StateDone); err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	if _, ok, _ := store.Load(p.PlanID); ok {
		t.Fatalf("归档后 pending/ 里不该还有它")
	}
	archived, err := store.LoadArchived(p.PlanID)
	if err != nil || archived.State != StateDone {
		t.Fatalf("归档件状态不对：%+v err=%v", archived, err)
	}
}

func TestStateStoreListSkipsArchived(t *testing.T) {
	store, _ := NewStateStore(t.TempDir())
	a, b := samplePending(), samplePending()
	b.PlanID = "pln_2"
	if err := store.Save(a); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := store.Save(b); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := store.Archive("pln_2", StateExpired); err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if len(list) != 1 || list[0].PlanID != "pln_1" {
		t.Fatalf("List 只该返回 pending/：%+v", list)
	}
}

func TestPendingExpired(t *testing.T) {
	p := samplePending()
	now, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	if p.Expired(now) {
		t.Fatalf("还没到期")
	}
	late, _ := time.Parse(time.RFC3339, "2026-10-27T09:31:00Z")
	if !p.Expired(late) {
		t.Fatalf("已过期")
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestStateStore|TestPendingExpired' -count=1`
Expected: `undefined: Pending`

- [x] **Step 3: 实现 `state.go`**

```go
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PendingState 是 pending 文件的状态。它写在文件里，不用改名做迁移：
// 改名会让「同一 confirm 重放」分不清是「已执行」还是「不存在」。
type PendingState string

const (
	StateAwaitingApproval PendingState = "awaiting_approval"
	StateResuming         PendingState = "resuming"
	StateDone             PendingState = "done"
	StateRejected         PendingState = "rejected"
	StateExpired          PendingState = "expired"
	StateStale            PendingState = "stale"
)

// Pending 是一份挂起的计划。Plan 存全文：审计只有 planId/stepId/action/inputHash，
// 它是「当时规划了什么」的唯一完整副本，所以归档不删（30 天后清理）。
type Pending struct {
	PlanID            string       `json:"planId"`
	State             PendingState `json:"state"`
	RequestID         *string      `json:"requestId"`
	TriggerEventID    string       `json:"triggerEventId"`
	Origin            string       `json:"origin"`
	By                string       `json:"by"`
	Plan              Plan         `json:"plan"`
	Labels            []string     `json:"labels"`
	Summary           string       `json:"summary"`
	RequiredRoles     []string     `json:"requiredRoles"`
	DispatchedAuditID string       `json:"dispatchedAuditId"`
	AuditSettled      bool         `json:"auditSettled"`
	CreatedAt         string       `json:"createdAt"`
	ExpiresAt         string       `json:"expiresAt"`
	ArchivedStatus    *string      `json:"archivedStatus"`
}

// Expired 判断是否过了审批窗口。解析失败按「已过期」处理（fail-closed）。
func (p Pending) Expired(now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return true
	}
	return now.After(exp)
}

// StateStore 是 pending/ 与 pending/archived/ 的读写口。所有写都是 tmp + rename。
type StateStore struct {
	dir string
}

// NewStateStore 准备目录结构（pending、pending/archived）。
func NewStateStore(dir string) (*StateStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("agent: 状态目录为空")
	}
	for _, sub := range []string{"pending", filepath.Join("pending", "archived")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("agent: 建状态目录失败：%w", err)
		}
	}
	return &StateStore{dir: dir}, nil
}

// Dir 返回状态根目录（测试与运维用）。
func (s *StateStore) Dir() string { return s.dir }

func (s *StateStore) pendingPath(planID string) string {
	return filepath.Join(s.dir, "pending", planID+".json")
}

func (s *StateStore) archivedPath(planID string) string {
	return filepath.Join(s.dir, "pending", "archived", planID+".json")
}

// Save 原子写入 pending 文件。
func (s *StateStore) Save(p Pending) error {
	if p.PlanID == "" {
		return fmt.Errorf("agent: pending 缺少 planId")
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.pendingPath(p.PlanID), raw)
}

// Load 读 pending/ 下的一份；不存在返回 ok=false（不是错误）。
func (s *StateStore) Load(planID string) (Pending, bool, error) {
	return readPending(s.pendingPath(planID))
}

// LoadArchived 读 archived/ 下的一份。
func (s *StateStore) LoadArchived(planID string) (Pending, error) {
	p, ok, err := readPending(s.archivedPath(planID))
	if err != nil {
		return Pending{}, err
	}
	if !ok {
		return Pending{}, fmt.Errorf("agent: 归档里没有 %s", planID)
	}
	return p, nil
}

// List 列出所有待恢复的（不含 archived/）。
func (s *StateStore) List() ([]Pending, error) {
	return listPendingDir(filepath.Join(s.dir, "pending"))
}

// ListArchived 列出所有归档件。
func (s *StateStore) ListArchived() ([]Pending, error) {
	return listPendingDir(filepath.Join(s.dir, "pending", "archived"))
}

// Archive 把一份 pending 标成终态并移入 archived/：单次 rename，原子且可审计。
func (s *StateStore) Archive(planID string, status PendingState) error {
	p, ok, err := s.Load(planID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("agent: 归档时找不到 %s", planID)
	}
	p.State = status
	st := string(status)
	p.ArchivedStatus = &st
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.archivedPath(planID), raw); err != nil {
		return err
	}
	return os.Remove(s.pendingPath(planID))
}

// PruneArchived 删除超过保留期的归档件，返回删除数量。
func (s *StateStore) PruneArchived(now time.Time, retain time.Duration) (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "pending", "archived"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < retain {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, "pending", "archived", e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// writeFileAtomic 先写 .tmp 再 rename：半个文件比没有文件更难查。
func writeFileAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readPending(path string) (Pending, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Pending{}, false, nil
	}
	if err != nil {
		return Pending{}, false, err
	}
	var p Pending
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pending{}, false, fmt.Errorf("agent: pending %s 不是合法 JSON：%w", path, err)
	}
	return p, true, nil
}

func listPendingDir(dir string) ([]Pending, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 顺序稳定：同一目录必得同一顺序
	out := make([]Pending, 0, len(names))
	for _, name := range names {
		p, ok, err := readPending(filepath.Join(dir, name))
		if err != nil || !ok {
			continue // 坏文件不让整个列表失败；自检会另外报它
		}
		out = append(out, p)
	}
	return out, nil
}
```

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestStateStore|TestPendingExpired' -count=1 -v`
Expected: 4 个用例全 PASS

- [x] **Step 5: Commit**

```bash
git add internal/agent/state.go internal/agent/state_test.go
git commit -m "feat(agent): 状态目录与 Pending 读写（tmp+rename 原子写）"
```

---

## Task 2: 去重账本（write-ahead）

**状态：✅ 已完成（commit `92815e3`）**

**Files:**
- Create: `internal/agent/ledger.go`
- Test: `internal/agent/ledger_test.go`

- [x] **Step 1: 写失败测试**

```go
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
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestLedger' -count=1`
Expected: `undefined: NewLedger`

- [x] **Step 3: 实现 `ledger.go`**

```go
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledgerEntry 是 processed.jsonl 的一行。
type ledgerEntry struct {
	RequestID string `json:"requestId"`
	Phase     string `json:"phase"` // claimed | done
	PlanID    string `json:"planId,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	At        string `json:"at"`
}

// Ledger 是 requestId 去重账本，write-ahead 语义：
// 处理前写 claimed（fsync），处理完写 done。重放时见 claimed 无 done 即跳过——
// 因为重新规划会生成新 planId → 新幂等键 → 模块幂等表挡不住，后果是重复扣库存。
type Ledger struct {
	mu   sync.Mutex
	path string
	seen map[string]string // requestId -> phase
}

// NewLedger 打开（或新建）账本并把已有条目读进内存。
func NewLedger(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("agent: 建账本目录失败：%w", err)
	}
	l := &Ledger{path: path, seen: map[string]string{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var e ledgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // 半行跳过，不阻塞
		}
		l.seen[e.RequestID] = e.Phase
	}
	return l, sc.Err()
}

// Claim 尝试占用一个 requestId：返回 false 表示它已被处理过（或正在处理中）。
func (l *Ledger) Claim(requestID string) (bool, error) {
	if requestID == "" {
		return true, nil // 事件回合没有 requestId，不走账本
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[requestID]; ok {
		return false, nil
	}
	if err := l.append(ledgerEntry{RequestID: requestID, Phase: "claimed", At: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return false, err
	}
	l.seen[requestID] = "claimed"
	return true, nil
}

// Done 记录处理完毕。
func (l *Ledger) Done(requestID, planID, outcome string) error {
	if requestID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.append(ledgerEntry{
		RequestID: requestID, Phase: "done", PlanID: planID, Outcome: outcome,
		At: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	l.seen[requestID] = "done"
	return nil
}

func (l *Ledger) append(e ledgerEntry) error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	// claimed 必须落盘才算数：它挡的是重复副作用，不能只活在页缓存里。
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
```

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestLedger' -count=1 -v`
Expected: 3 个用例全 PASS

- [x] **Step 5: Commit**

```bash
git add internal/agent/ledger.go internal/agent/ledger_test.go
git commit -m "feat(agent): requestId 去重账本（write-ahead，claimed 落盘）"
```

---

## Task 3: 挂起的三次落盘（I7）与崩溃分流

**状态：✅ 已完成（commit `3302ac5` + 审计补丁 `c97277e`）**

**Files:**
- Create: `internal/agent/suspend.go`
- Modify: `internal/agent/state.go`（只加 `CountCorrupt` 与改准一句注释）
- Test: `internal/agent/suspend_test.go`

- [x] **Step 1: 写失败测试**

```go
package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// fakeAuditHost 只实现 POST /tenon/audit，可注入失败。
func fakeAuditHost(t *testing.T, failAt int) *httptest.Server {
	t.Helper()
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tenon/audit" {
			calls++
			if failAt > 0 && calls == failAt {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
}

func newSuspendFixture(t *testing.T, hostURL string) (*StateStore, Pending, Deps) {
	t.Helper()
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	deps := Deps{Host: hostURL, AgentID: "ag_test", OnBehalfOf: "user:alice", HTTP: http.DefaultClient}
	return store, samplePending(), deps
}

// I7 的三步全成功：pending 在盘、审计已写、标记已回写。
func TestSuspendWritesPendingThenAuditThenFlag(t *testing.T) {
	srv := fakeAuditHost(t, 0)
	defer srv.Close()
	store, p, deps := newSuspendFixture(t, srv.URL)

	if err := suspend(context.Background(), store, deps, p); err != nil {
		t.Fatalf("挂起失败：%v", err)
	}
	got, ok, _ := store.Load(p.PlanID)
	if !ok {
		t.Fatalf("pending 未落盘")
	}
	if !got.AuditSettled || got.DispatchedAuditID == "" {
		t.Fatalf("标记或审计 id 缺失：%+v", got)
	}
}

// 审计写失败 → 整个挂起失败，且**不留半截 pending**。
func TestSuspendFailsClosedWhenAuditFails(t *testing.T) {
	srv := fakeAuditHost(t, 1)
	defer srv.Close()
	store, p, deps := newSuspendFixture(t, srv.URL)

	if err := suspend(context.Background(), store, deps, p); err == nil {
		t.Fatalf("审计失败时挂起必须报错")
	}
	if _, ok, _ := store.Load(p.PlanID); ok {
		t.Fatalf("审计失败后不许留 pending")
	}
}

// 崩溃分流：pending 在、审计链查不到 → 归档 stale，绝不执行。
func TestSelfCheckArchivesStaleWhenAuditMissing(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	p.AuditSettled = false
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	// 固定时间：夹具的 ExpiresAt 是会到期的常量，用 time.Now() 会让用例过几小时自己变红。
	fixedNow, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	rep, err := selfCheck(context.Background(), store, auditProbeNotFound{})(fixedNow)
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	if rep.Stale != 1 {
		t.Fatalf("应归档 1 份 stale，得到 %+v", rep)
	}
	if _, ok, _ := store.Load(p.PlanID); ok {
		t.Fatalf("stale 之后不该还在 pending/")
	}
}

// 崩溃分流：审计查得到 → 补标记，继续等审批。
func TestSelfCheckSettlesAuditWhenPresent(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	p.AuditSettled = false
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	fixedNow, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	rep, err := selfCheck(context.Background(), store, auditProbeFound{})(fixedNow)
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	if rep.Settled != 1 {
		t.Fatalf("应补标记 1 份，得到 %+v", rep)
	}
	got, ok, _ := store.Load(p.PlanID)
	if !ok || !got.AuditSettled {
		t.Fatalf("标记未补上：%+v ok=%v", got, ok)
	}
}

// 坏文件必须被看见：List() 会跳过它，自检要单独把它数出来（只告警）。
func TestSelfCheckCountsCorruptFiles(t *testing.T) {
	store, p, _ := newSuspendFixture(t, "http://127.0.0.1:1")
	if err := store.Save(p); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "pending", "pln_broken.json"), []byte("{半截"), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}
	fixedNow, _ := time.Parse(time.RFC3339, "2026-10-27T09:20:00Z")
	rep, err := selfCheck(context.Background(), store, auditProbeFound{})(fixedNow)
	if err != nil {
		t.Fatalf("自检失败：%v", err)
	}
	if rep.Corrupt != 1 {
		t.Fatalf("应报 1 个坏文件，得到 %+v", rep)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestSuspend|TestSelfCheck' -count=1`
Expected: `undefined: suspend`

- [x] **Step 3: 实现 `suspend.go`**

```go
package agent

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// pendingTTL 是审批窗口。超时即归档 expired，绝不执行（I3）。
const pendingTTL = 15 * time.Minute

// auditProbe 用于自检：审计链上有没有这条 dispatched 记录。
type auditProbe interface {
	Has(ctx context.Context, host, auditID string) (bool, error)
}

type auditProbeFound struct{}

func (auditProbeFound) Has(context.Context, string, string) (bool, error) { return true, nil }

type auditProbeNotFound struct{}

func (auditProbeNotFound) Has(context.Context, string, string) (bool, error) { return false, nil }

// suspend 执行 I7 的三次落盘：
//
//	① 写 pending（auditSettled=false，dispatchedAuditId 已生成）
//	② 写 dispatched 审计（计划级：stepId 为空，approvedBy=null）
//	③ 回写 auditSettled=true
//
// 任一步失败都删掉 pending 并返回错误：宁可这轮转人工，也不留半截 pending。
func suspend(ctx context.Context, store *StateStore, deps Deps, p Pending) error {
	if p.PlanID == "" {
		return fmt.Errorf("agent: 挂起缺少 planId")
	}
	if p.DispatchedAuditID == "" {
		p.DispatchedAuditID = newID("aud_")
	}
	p.State = StateAwaitingApproval
	p.AuditSettled = false
	if err := store.Save(p); err != nil {
		return err
	}
	cleanup := func(err error) error {
		_ = store.Archive(p.PlanID, StateStale)
		return err
	}
	rec := auditRecord{
		AuditID: p.DispatchedAuditID,
		Stage:   auditStageDispatched,
		At:      time.Now().UTC().Format(time.RFC3339),
		Actor:   auditActor{Type: "agent", ID: deps.AgentID, OnBehalfOf: nilIfEmpty(p.By)},
		Trigger: auditTrigger{EventID: nilIfEmpty(p.TriggerEventID), CorrelationID: nilIfEmpty(correlationOf(p))},
		PlanID:  nilIfEmpty(p.PlanID),
		Action:  "",
		Decision: auditDecision{
			Outcome:    "approve",
			PolicyID:   nil,
			GrantID:    nil,
			ApprovedBy: nil, // 计划级 dispatched：此刻确实还没人批（I2）
		},
		Effects: auditEffects{Entities: nil},
	}
	if err := postAuditRecord(ctx, deps, rec); err != nil {
		return cleanup(fmt.Errorf("写 dispatched 审计失败（拒绝挂起）：%w", err))
	}
	p.AuditSettled = true
	if err := store.Save(p); err != nil {
		return cleanup(err)
	}
	return nil
}

// correlationOf 取 pending 里的业务链锚点（存在 Plan.Trigger 里）。
func correlationOf(p Pending) string { return p.Plan.Trigger.CorrelationID }

// postAuditRecord 把一条审计记录发给宿主；非 200 一律算失败（宿主承诺 200 即已落盘）。
func postAuditRecord(ctx context.Context, deps Deps, rec auditRecord) error {
	client := deps.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	host := deps.Host
	status, raw, err := postJSONWith(ctx, client, host+"/tenon/audit", rec, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("POST /tenon/audit 返回 %d：%s", status, snippet(raw))
	}
	return nil
}

// selfCheckReport 是一次自检的结果，供日志与测试断言。
type selfCheckReport struct {
	Kept    int // 原样保留
	Expired int // 归档 expired
	Stale   int // 归档 stale（审计缺失）
	Settled int // 补上 auditSettled 标记
	Corrupt int // 无法解析的 pending 文件数（List 会跳过它们）
}

// selfCheck 按 I7 的表给每份 pending 分流。返回一个以「now」为参数的函数，便于测试注入时间。
func selfCheck(ctx context.Context, store *StateStore, probe auditProbe) func(time.Time) (selfCheckReport, error) {
	return func(now time.Time) (selfCheckReport, error) {
		var rep selfCheckReport
		corrupt, cerr := store.CountCorrupt()
		if cerr != nil {
			return rep, cerr
		}
		rep.Corrupt = corrupt
		items, err := store.List()
		if err != nil {
			return rep, err
		}
		for _, p := range items {
			if !p.AuditSettled {
				found, perr := probe.Has(ctx, "", p.DispatchedAuditID)
				if perr != nil {
					return rep, perr
				}
				if !found {
					// 崩在 ① 之后 ② 之前：不执行。审计缺失的 pending 一律 stale。
					if err := store.Archive(p.PlanID, StateStale); err != nil {
						return rep, err
					}
					rep.Stale++
					continue
				}
				p.AuditSettled = true
				if err := store.Save(p); err != nil {
					return rep, err
				}
				rep.Settled++
			}
			if p.Expired(now) {
				if err := store.Archive(p.PlanID, StateExpired); err != nil {
					return rep, err
				}
				rep.Expired++
				continue
			}
			rep.Kept++
		}
		return rep, nil
	}
}
```

`CountCorrupt` 补进 `state.go`（Task 3 允许改这个文件，只加这一个方法与一句注释）：

```go
// CountCorrupt 数一遍 pending/ 下解析不了的文件。
//
// 必须有这个出口：List() 刻意跳过坏文件（不让一个坏件拖垮整个列表），
// 但「被跳过」不等于「不存在」——不数出来它就会永远躺在盘上、永不清理、也永不告警。
func (s *StateStore) CountCorrupt() (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "pending"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	bad := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if _, ok, err := readPending(filepath.Join(s.dir, "pending", e.Name())); err != nil || !ok {
			bad++
		}
	}
	return bad, nil
}
```

同时把 `state.go` 里 `listPendingDir` 的那句注释改准——它现在写着「坏文件不让整个列表失败；自检会另外报它」，而自检的报数正是本任务补的 `CountCorrupt`。

`postJSONWith` 是抽出来的最小化 POST 助手（`run.go` 里现有的 `postJSON` 是 `runner` 的方法，不能复用）：

```go
// postJSONWith 发一个 JSON POST，返回状态码与响应体。
func postJSONWith(ctx context.Context, client *http.Client, url string, body any, headers map[string]string) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, out, err
}
```

（`import` 需补 `bytes`、`encoding/json`、`io`。）

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestSuspend|TestSelfCheck' -count=1 -v`
Expected: 4 个用例全 PASS

- [x] **Step 5: Commit**

```bash
git add internal/agent/suspend.go internal/agent/suspend_test.go
git commit -m "feat(agent): 挂起的 I7 三次落盘与自检分流"
```

---

## Task 4: host 客户端（登录 / 重登 / 入站 poll+ack / 公告板）

**状态：✅ 已完成（commit `f192b94`）**

**Files:**
- Create: `internal/agent/hostapi.go`
- Test: `internal/agent/hostapi_test.go`

- [x] **Step 1: 写失败测试**

```go
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeInboundHost 提供 session / requests poll / ack / pending。
func fakeInboundHost(t *testing.T, frames string) (*httptest.Server, *int32) {
	t.Helper()
	var logins int32
	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/session", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&logins, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "tok-" + "1", "user": "agent:test", "roles": []string{"role:agent"},
		})
	})
	mux.HandleFunc("/tenon/requests/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"tenonError":{"code":"AUTH"}}`))
			return
		}
		_, _ = w.Write([]byte(frames))
	})
	mux.HandleFunc("/tenon/requests/ack", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"acked":1}`))
	})
	mux.HandleFunc("/tenon/pending", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return httptest.NewServer(mux), &logins
}

func TestHostClientLoginThenPoll(t *testing.T) {
	srv, logins := fakeInboundHost(t, `{"frames":[{"seq":1,"frame":{"type":"tenon.request","requestId":"req_1"}}],"nextCursor":1,"dropped":0}`)
	defer srv.Close()
	c := NewHostClient(srv.URL, "agent", "agent-dev", http.DefaultClient)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("登录失败：%v", err)
	}
	frames, next, err := c.PollInbound(context.Background(), 0)
	if err != nil {
		t.Fatalf("poll 失败：%v", err)
	}
	if len(frames) != 1 || next != 1 || frames[0].Seq != 1 {
		t.Fatalf("poll 结果不符：%+v next=%d", frames, next)
	}
	if atomic.LoadInt32(logins) != 1 {
		t.Fatalf("应只登录 1 次，得到 %d", atomic.LoadInt32(logins))
	}
}

// 401 → 重登一次并重试；并发 401 只允许一次登录在飞。
func TestHostClientReLoginsOnUnauthorized(t *testing.T) {
	var logins int32
	mux := http.NewServeMux()
	mux.HandleFunc("/tenon/session", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&logins, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + itoa(int(n))})
	})
	mux.HandleFunc("/tenon/requests/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"frames":[],"nextCursor":0,"dropped":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHostClient(srv.URL, "agent", "agent-dev", http.DefaultClient)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("首次登录失败：%v", err)
	}
	if _, _, err := c.PollInbound(context.Background(), 0); err != nil {
		t.Fatalf("重登后 poll 应成功：%v", err)
	}
	if got := atomic.LoadInt32(&logins); got != 2 {
		t.Fatalf("应登录 2 次（首次 + 一次重登），得到 %d", got)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestHostClient' -count=1`
Expected: `undefined: NewHostClient`

- [x] **Step 3: 实现 `hostapi.go`**

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// HostClient 是 agent 对宿主入站面的客户端：登录、轮询入站帧、确认、推公告板。
// token 只存内存；401 时重登一次，且**并发重登串行化**（单飞），否则一批 401 会打登录风暴。
type HostClient struct {
	base     string
	user     string
	password string
	client   *http.Client

	mu       sync.Mutex
	token    string
	logging  bool
	loggedIn chan struct{} // 单飞：正在登录时其它调用等它
}

// NewHostClient 构造客户端。base 形如 http://127.0.0.1:8080（尾斜杠会被去掉）。
func NewHostClient(base, user, password string, client *http.Client) *HostClient {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return &HostClient{base: base, user: user, password: password, client: client}
}

// Login 换 token。失败即返回错误（agent 启动时据此失败）。
func (c *HostClient) Login(ctx context.Context) error {
	return c.relogin(ctx)
}

func (c *HostClient) relogin(ctx context.Context) error {
	c.mu.Lock()
	if c.logging {
		ch := c.loggedIn
		c.mu.Unlock()
		<-ch // 等正在进行的登录
		c.mu.Lock()
		err := error(nil)
		c.mu.Unlock()
		return err
	}
	c.logging = true
	c.loggedIn = make(chan struct{})
	ch := c.loggedIn
	c.mu.Unlock()

	status, raw, err := postJSONWith(ctx, c.client, c.base+"/tenon/session",
		map[string]string{"user": c.user, "password": c.password}, nil)
	var loginErr error
	if err != nil {
		loginErr = err
	} else if status != http.StatusOK {
		loginErr = fmt.Errorf("登录返回 %d：%s", status, snippet(raw))
	} else {
		var out struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
			loginErr = fmt.Errorf("登录响应没有 token：%s", snippet(raw))
		} else {
			c.mu.Lock()
			c.token = out.Token
			c.mu.Unlock()
		}
	}

	c.mu.Lock()
	c.logging = false
	close(ch)
	c.mu.Unlock()
	return loginErr
}

func (c *HostClient) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// do 带 token 发请求；遇 401 重登一次再重试该请求。
func (c *HostClient) do(ctx context.Context, method, url string, body any) (int, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok := c.currentToken()
		if tok == "" {
			if err := c.relogin(ctx); err != nil {
				return 0, nil, err
			}
			tok = c.currentToken()
		}
		status, raw, err := c.send(ctx, method, url, body, tok)
		if err != nil {
			return 0, nil, err
		}
		if status != http.StatusUnauthorized || attempt == 1 {
			return status, raw, nil
		}
		if err := c.relogin(ctx); err != nil {
			return 0, nil, err
		}
	}
	return 0, nil, fmt.Errorf("agent: 重登后仍 401")
}

func (c *HostClient) send(ctx context.Context, method, url string, body any, token string) (int, []byte, error) {
	headers := map[string]string{"Authorization": "Bearer " + token}
	if method == http.MethodGet {
		return getJSONWith(ctx, c.client, url, headers)
	}
	return postJSONWith(ctx, c.client, url, body, headers)
}

// PollInbound 拉入站帧。
func (c *HostClient) PollInbound(ctx context.Context, after int64) ([]QueuedFrame, int64, error) {
	status, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/tenon/requests/poll?after=%d", c.base, after), nil)
	if err != nil {
		return nil, after, err
	}
	if status != http.StatusOK {
		return nil, after, fmt.Errorf("poll 返回 %d：%s", status, snippet(raw))
	}
	var out struct {
		Frames     []QueuedFrame `json:"frames"`
		NextCursor int64         `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, after, err
	}
	return out.Frames, out.NextCursor, nil
}

// AckInbound 确认到某个序号。
func (c *HostClient) AckInbound(ctx context.Context, upto int64) error {
	status, raw, err := c.do(ctx, http.MethodPost, c.base+"/tenon/requests/ack",
		map[string]int64{"upto": upto})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ack 返回 %d：%s", status, snippet(raw))
	}
	return nil
}

// PushPending 幂等 upsert 一条公告板摘要。
func (c *HostClient) PushPending(ctx context.Context, payload map[string]any) error {
	status, raw, err := c.do(ctx, http.MethodPost, c.base+"/tenon/pending", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("公告板推送返回 %d：%s", status, snippet(raw))
	}
	return nil
}

// QueuedFrame 是入站队列的一帧（与 host 的线上形状一致：{seq, frame}）。
type QueuedFrame struct {
	Seq   int64           `json:"seq"`
	Frame json.RawMessage `json:"frame"`
}
```

还需补两个小助手（放进 `hostapi.go` 末尾）：

```go
// getJSONWith 发 GET 并带自定义头。
func getJSONWith(ctx context.Context, client *http.Client, url string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}
```

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestHostClient' -count=1 -v`
Expected: 2 个用例全 PASS

- [x] **Step 5: Commit**

```bash
git add internal/agent/hostapi.go internal/agent/hostapi_test.go
git commit -m "feat(agent): host 入站客户端（登录/单飞重登/poll/ack/公告板）"
```

---

## Task 5: skill 确定性粗筛

**状态：✅ 已完成（commit `c439cf5`；确定性补丁在 `sort.SliceStable`）**

**Files:**
- Create: `internal/agent/skillselect.go`
- Test: `internal/agent/skillselect_test.go`

- [x] **Step 1: 写失败测试**

```go
package agent

import "testing"

func TestSelectSkillsByText(t *testing.T) {
	skills := []Skill{
		{Name: "quote-to-fulfillment", Body: "报价完成后核对 inventory 库存，缺口走 procurement。"},
		{Name: "procurement-only", Body: "只管 procurement 的采购单。"},
		{Name: "unrelated", Body: "和库存无关。"},
	}
	got := SelectSkills(skills, "Q-2026-0417 这单货不够，先扣现有的，缺的发起采购", "inventory", 2)
	if len(got) != 1 || got[0].Name != "quote-to-fulfillment" {
		t.Fatalf("应命中最相关的 1 份，得到 %+v", got)
	}
}

func TestSelectSkillsDeterministicOrder(t *testing.T) {
	skills := []Skill{
		{Name: "b-skill", Body: "inventory procurement"},
		{Name: "a-skill", Body: "inventory procurement"},
	}
	first := SelectSkills(skills, "inventory 缺货", "inventory", 2)
	second := SelectSkills(skills, "inventory 缺货", "inventory", 2)
	if len(first) != 2 || first[0].Name != second[0].Name || first[1].Name != second[1].Name {
		t.Fatalf("同一输入必须同一顺序：%+v vs %+v", first, second)
	}
	if first[0].Name != "a-skill" {
		t.Fatalf("同命中数按名字典序，a-skill 应在前：%+v", first)
	}
}

func TestSelectSkillsEmptyWhenNoMatch(t *testing.T) {
	skills := []Skill{{Name: "unrelated", Body: "nothing here"}}
	if got := SelectSkills(skills, "完全无关的话", "", 2); len(got) != 0 {
		t.Fatalf("无命中应返回空，得到 %+v", got)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestSelectSkills' -count=1`
Expected: `undefined: SelectSkills`

- [x] **Step 3: 实现 `skillselect.go`**

```go
package agent

import (
	"sort"
	"strings"
)

// knownModuleNames 是粗筛用的模块名词表。它不是"平台为每个系统适配"——
// 这些词来自本次请求与 skill 正文的**交集**，词表只用来判断"这段话提到了哪些模块"。
// 加新模块时在这里补一行；它不参与任何判定，只影响 skill 候选集。
var knownModuleNames = []string{"inventory", "procurement", "quote"}

// SelectSkills 按确定性规则挑出候选 skill：
//  1. 统计每份 skill 的命中数（正文/subscribes 里出现的模块名 ∩ 请求文本与 view 模块名）；
//  2. 命中数为 0 的丢掉；
//  3. 命中数降序，同数按 skill 名字典序；
//  4. 最多返回 limit 份。
//
// 不调 LLM：同一 (text, view) 必得同一结果，可表驱动测试。
func SelectSkills(skills []Skill, text, viewModule string, limit int) []Skill {
	if limit <= 0 {
		limit = 2
	}
	haystack := strings.ToLower(text + " " + viewModule)
	type scored struct {
		skill Skill
		hits  int
	}
	var picks []scored
	for _, s := range skills {
		hits := 0
		body := strings.ToLower(s.Body + " " + strings.Join(s.Subscribes, " "))
		for _, mod := range knownModuleNames {
			if strings.Contains(haystack, mod) && strings.Contains(body, mod) {
				hits++
			}
		}
		if hits > 0 {
			picks = append(picks, scored{skill: s, hits: hits})
		}
	}
	sort.Slice(picks, func(i, j int) bool {
		if picks[i].hits != picks[j].hits {
			return picks[i].hits > picks[j].hits
		}
		return picks[i].skill.Name < picks[j].skill.Name
	})
	if len(picks) > limit {
		picks = picks[:limit]
	}
	out := make([]Skill, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.skill)
	}
	return out
}
```

> `Skill` 现在只有 `Name` / `Subscribes` / `Body` 三个字段（`planner.go:375`）。若字段名不同，按实际改名——**不要新增字段**。

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestSelectSkills' -count=1 -v`
Expected: 3 个用例全 PASS

- [x] **Step 5: Commit**

```bash
git add internal/agent/skillselect.go internal/agent/skillselect_test.go
git commit -m "feat(agent): skill 确定性粗筛（request 回合无事件类型可用）"
```

---

## Task 6: prompt 分流（用户请求是「指令」，事件是「事实」）

**状态：✅ 已完成（commit `5947b23`）**

**Files:**
- Modify: `internal/agent/planner.go`（`PlanInput` 加字段、`userPrompt` 增块）
- Test: `internal/agent/planner_test.go`

- [x] **Step 1: 写失败测试**

```go
func TestUserPromptSeparatesRequestFromEvent(t *testing.T) {
	ev := DomainEvent{EventID: "evt_syn_1", Type: "tenon.request", CorrelationID: "cor:quote/Q-1"}
	in := PlanInput{
		Event:     ev,
		SkillText: "业务说明",
		Request:   &UserRequest{RequestID: "req_1", Text: "扣 12 件", By: "user:alice"},
	}
	got := userPrompt(in)
	if !strings.Contains(got, "用户请求（这是指令") {
		t.Fatalf("缺少用户请求块：\n%s", got)
	}
	if !strings.Contains(got, "扣 12 件") {
		t.Fatalf("用户原话没进 prompt：\n%s", got)
	}
	// 用户的话绝不能出现在「事件」块里：那块标题写着「不是指令」。
	eventBlock := got[:strings.Index(got, "skill 正文")]
	if strings.Contains(eventBlock, "扣 12 件") {
		t.Fatalf("用户原文被塞进了事件块：\n%s", eventBlock)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestUserPromptSeparatesRequest' -count=1`
Expected: `unknown field Request in struct literal of type PlanInput`

- [x] **Step 3: 改 `planner.go`**

`PlanInput` 增字段：

```go
	// Request 是人工发起时的原话；事件回合为 nil。
	// 它与 Event 语义不同：Event 是领域事实，Request 是**指令**。
	Request *UserRequest
```

新增类型：

```go
// UserRequest 是 tenon.request 帧在 agent 侧的投影。
type UserRequest struct {
	RequestID string
	Text      string
	By        string
}
```

`userPrompt` 在事件块之后插入：

```go
	if in.Request != nil {
		b.WriteString("\n用户请求（这是指令，目标是完成它）：\n")
		b.WriteString(strings.TrimSpace(in.Request.Text))
		b.WriteString("\n")
	}
```

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestUserPrompt' -count=1 -v && go test ./internal/agent/ -count=1`
Expected: 新增用例 PASS，且既有 planner 用例全绿

- [x] **Step 5: Commit**

```bash
git add internal/agent/planner.go internal/agent/planner_test.go
git commit -m "feat(agent): prompt 分流（用户请求指令块与事件事实块分开）"
```

---

## Task 7: `round()` 闸门重构 + `approve` 走挂起

**状态：✅ 已完成（commit `9abd389`）**

**Task 6 复核后追加的接线要求（比正文更权威）**

1. **`run.go:178` 的 `PlanInput` 构造必须补 `Request: r.request`**：`runner` 需要一个 `request *UserRequest` 字段，`Run()` 从 `in.Request` 赋值。漏了这一处，Task 6 的 prompt 分流永远不生效（`PlanInput.Request` 恒 nil）。
2. **事件回合的 `Request` 必须保持 nil**：塞一个非 nil（哪怕 `Text` 为空）的 `UserRequest`，所有事件回合 prompt 都会多出一个空指令块，`TestUserPromptOmitsRequestBlockWhenNil` 立刻红。
3. **`suspend` 不填 `CreatedAt`/`ExpiresAt`**：`buildPending` 必须自己填 `ExpiresAt = now.Add(pendingTTL)`；`pendingTTL` 已在 `suspend.go` 声明，**不要重复声明**。漏填会让 `Expired()` 对空串 fail-closed 返回 true，下一轮自检立刻把这份 pending 归档成 `expired`。
4. **`suspend` 失败会把 pending 归档成 `stale` 再返回 error**：调用方映射成 `needs_human`（不是 `failed`），且**不要用同一份 pending 重试**——重复调用会复用同一个 `auditId`，审计链出现重复记录。
5. **别把整帧 `json.Unmarshal` 进 `UserRequest`**：它没有 json tag，Go 字段匹配大小写不敏感，"碰巧能 work" 靠的是意外行为。帧→`UserRequest` 的显式映射（`text→Text`、`by→By`）属于 Task 10 的 `handleInbound`；本任务只消费构造好的 `*UserRequest`。

**Files:**
- Modify: `internal/agent/run.go`（`RunInput`/`RunResult`、`round()`、`Run()`）
- Modify: `internal/agent/run_test.go`（改既有 approve 用例）
- Test: `internal/agent/run_test.go`

- [x] **Step 1: 改既有用例并新增失败测试**

把 `TestPolicyApproveGoesNeedsHumanWithoutApprovalChannel` 的语义改成「**没有挂起通道时**才转人工」：给 `Deps` 传 `State` 为 nil，断言仍是 `needs_human` 且 `PendingPlanID == nil`。

新增：

```go
// approve 且配置了状态目录 → 挂起：结局仍是 needs_human，但带 pendingPlanId。
func TestPolicyApproveSuspendsWhenChannelConfigured(t *testing.T) {
	env := newRunEnv(t) // 沿用 run_test.go 的既有假 host 工厂
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	env.Deps.State = store
	env.LLM.replies = []string{planJSON("pln_appr01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "approve"

	res := env.RunOnce(t)
	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("挂起后结局仍是 needs_human（三态封闭），得到 %q", res.Outcome)
	}
	if res.PendingPlanID == nil || *res.PendingPlanID != "pln_appr01" {
		t.Fatalf("必须带 pendingPlanId，得到 %+v", res.PendingPlanID)
	}
	if _, ok, _ := store.Load("pln_appr01"); !ok {
		t.Fatalf("pending 未落盘")
	}
}

// deny 仍然直接转人工，不产生 pending。
func TestPolicyDenyDoesNotSuspend(t *testing.T) {
	env := newRunEnv(t)
	store, _ := NewStateStore(t.TempDir())
	env.Deps.State = store
	env.LLM.replies = []string{planJSON("pln_deny01", testStep{id: "s1", action: "inventory.DeductStock"})}
	env.Host.PolicyOutcome = "deny"

	res := env.RunOnce(t)
	if res.Outcome != OutcomeNeedsHuman || res.PendingPlanID != nil {
		t.Fatalf("deny 不该挂起：%+v", res)
	}
	if items, _ := store.List(); len(items) != 0 {
		t.Fatalf("deny 不该写 pending：%+v", items)
	}
}
```

> `newRunEnv` / `env.RunOnce` / `planJSON` / `testStep` 都是 `run_test.go` 里**已有**的测试设施（若名字不同，按实际改名，不要新造一套）。

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestPolicyApprove|TestPolicyDenyDoesNotSuspend' -count=1`
Expected: `Deps.State` 不存在 / 用例失败

- [x] **Step 3: 改 `run.go`**

`Deps` 增：

```go
	// State 是 pending 的持久化口；为 nil 表示「没有挂起通道」，approve 一律转人工。
	State *StateStore
```

`RunInput` 增：

```go
	// Request 是人工发起时的原话；事件回合为 nil。
	Request *UserRequest
```

`RunResult` 增：

```go
	// 结局仍是 needs_human（三态封闭，不新增第四态）；非 nil 表示有一份 pending 可被推进。
	PendingPlanID *string
```

`Run()` 校验放宽：request 回合没有 `event.EventID` 之外的锚点问题——保持现有校验，但允许 `event.Type` 为 `tenon.request`。

`round()` 重构为「先跑完所有闸门，再决定挂起还是执行」：

```go
	// 闸门 2：策略。整份计划取最严（宿主实现）。
	policy, err := r.evaluatePolicies(ctx, plan)
	if err != nil {
		return RunResult{Outcome: OutcomeFailed,
			Detail: "策略评估不可用（fail-closed，不做任何写）：" + err.Error()}, ""
	}
	if policy.Outcome == outcomeDeny {
		return RunResult{Outcome: OutcomeNeedsHuman,
			Detail: fmt.Sprintf("策略闸门拒绝：outcome=%s decidedBy=%v reasons=%v（转人工，不换路绕过）",
				policy.Outcome, policy.DecidedBy, policy.Reasons)}, ""
	}
	needApproval := policy.Outcome == outcomeApprove
	...（授权与预演保持原样，但**不再因 policy=approve 提前返回**）...
	// 闸门 5：需要审批 → 挂起。
	if needApproval {
		if r.deps.State == nil {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: fmt.Sprintf("策略闸门要求审批（outcome=%s decidedBy=%v），但没有挂起通道（Deps.State 为空）：转人工",
					policy.Outcome, policy.DecidedBy)}, ""
		}
		pending, err := r.buildPending(plan, policy)
		if err != nil {
			return RunResult{Outcome: OutcomeNeedsHuman, Detail: err.Error()}, ""
		}
		if err := suspend(ctx, r.deps.State, r.deps, pending); err != nil {
			return RunResult{Outcome: OutcomeNeedsHuman,
				Detail: "挂起失败（不留半截 pending）：" + err.Error()}, ""
		}
		id := pending.PlanID
		return RunResult{Outcome: OutcomeNeedsHuman, PlanID: id, PendingPlanID: &id,
			Detail: fmt.Sprintf("已挂起等待审批：planId=%s 需要 %v", id, pending.RequiredRoles)}, ""
	}
	...（执行循环保持原样）...
```

新增 `buildPending`：

```go
// buildPending 把 plan 与本轮的闸门结论打包成一份 pending。
// labels 在这里渲染并落盘：白话来自身份化模板，模板改了两边文字就会不一致。
func (r *runner) buildPending(plan Plan, policy policyEvaluation) (Pending, error) {
	labels, err := renderLabels(plan)
	if err != nil {
		return Pending{}, fmt.Errorf("渲染白话失败：%w", err)
	}
	exp := time.Now().UTC().Add(pendingTTL).Format(time.RFC3339)
	var reqID *string
	if r.request != nil && r.request.RequestID != "" {
		id := r.request.RequestID
		reqID = &id
	}
	origin := "event"
	if r.request != nil {
		origin = "request"
	}
	return Pending{
		PlanID:         plan.PlanID,
		State:          StateAwaitingApproval,
		RequestID:      reqID,
		TriggerEventID: plan.Trigger.EventID,
		Origin:         origin,
		By:             r.deps.OnBehalfOf,
		Plan:           plan,
		Labels:         labels,
		Summary:        strings.Join(labels, "；"),
		RequiredRoles:  policy.Approvers,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:      exp,
	}, nil
}
```

`renderLabels` 放在 `suspend.go`（Task 3 已建该文件，这里补函数）：

```go
// renderLabels 逐 step 渲染一句白话：优先用 summaryTemplate，否则 title + 关键参数。
// 数字只从 args 取，模型不参与（spec §5 R1–R3）。
func renderLabels(plan Plan) ([]string, error) {
	out := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		out = append(out, fmt.Sprintf("%s（%s）", step.Action, truncateRunes(string(step.Args), 80)))
	}
	return out, nil
}
```

> 说明：真正的人话模板来自模块 manifest 的 `summaryTemplate`，P1 已加 schema 字段但模块尚未填。本任务先用「动作 id + 参数摘要」兜底，**并在代码注释里写明这条兜底**；P3 渲染前再补模板。这不是占位——它是计划明确接受的降级。

- [x] **Step 4: 跑测试至全绿**

Run: `go test ./internal/agent/ -count=1`
Expected: 全绿（含改过的 approve 用例与新增两条）

- [x] **Step 5: Commit**

```bash
git add internal/agent/run.go internal/agent/run_test.go internal/agent/suspend.go
git commit -m "feat(agent): approve 走挂起；闸门顺序改为全部过完再决定"
```

---

## Task 8: 恢复执行（校验审批人 → 过期 → G4 → policy → 执行 → 归档）

**状态：✅ 已完成（commit `eaa64d7`，10 条用例）**

**Task 7 复核后追加的要求（比正文更权威）**

**Step 0（先做，关掉一个 fail-open 口子）**：Task 7 补了「策略处置不是 auto/approve 就转人工」的分支，但**没有用例**。先补一条：

```go
// 未知的策略处置必须 fail-closed：绝不能因为"不认识这个值"就掉进执行。
func TestUnknownPolicyOutcomeDoesNotExecute(t *testing.T) {
	env := newFakeEnv(t)
	env.Host.PolicyOutcome = "maybe" // 未来的新枚举 / 被篡改的响应
	env.LLM.replies = []string{planJSON("pln_unknown", testStep{id: "s1", action: "inventory.DeductStock"})}
	deps := env.Deps()
	res := Run(context.Background(), deps, RunInput{Event: env.Event()})
	if res.Outcome != OutcomeNeedsHuman {
		t.Fatalf("未知处置应转人工，得到 %q", res.Outcome)
	}
	// 只走到策略闸门，不许碰授权/预演/执行。
	if !equalSeq(env.Host.Trace(), []string{"policies:evaluate"}) {
		t.Fatalf("闸门次序不对：%v", env.Host.Trace())
	}
}
```

（`newFakeEnv` / `env.Deps()` / `env.Host` / `equalSeq` 都是 `run_test.go` 里**已有**的设施；字段名以实际为准，不要新造。）

**Step 0b（恢复路径必须自己装模块清单）**：`r.modules` 只在 `run()` 的 `discover()` 里填，`moduleBase()` 靠它。`resume` **不能复用 `round()`**（它含规划与二次挂起），但必须自己 `discover(ctx)` 后再走 `executeStep`，否则每步都报"动作所属模块不在可达清单里"。

**Step 0c（补 `Subject`，别让审计丢实体）**：`Pending` 现在只存 `TriggerEventID` + `Plan.Trigger.CorrelationID`，事件的 `subject` 丢了 → 恢复后 `entities()`/`target()` 退化成空数组（审计 `effects.entities=[]`、Intent 帧 `actions=[]`）。**加一个字段**（改 `state.go`，本任务允许）：

```go
	// Subject 是触发事件的实体寻址（domain-event 的 subject 原样）。审计与 Intent 帧都要它，
	// 而 TriggerEventID 只够锚定事件、锚不住"哪个实体"。
	Subject json.RawMessage `json:"subject,omitempty"`
```

`buildPending`（Task 7 已落地）要补填 `Subject: r.event.Subject`；`resume` 组装 `runner` 时把 `event.Subject` 填回去。补一条用例：恢复执行的审计里 `effects.entities` 非空。

**Step 0d（`approvedBy` 需要通道）**：`gateDecision` 没有 `approvedBy`，`auditBase` 硬写 `decision.ApprovedBy: nil`。恢复执行时**不要复用 `auditBase` 写 result 段**——直接用计划里给的 `writePlanResult`（它自己填 `approvedBy`），并且**不要**把 `decision.outcome` 从 `approve` 改写成 `auto`（`TestAuditRecordsTwoSegments` 锁了"outcome=闸门处置、policyId=策略 id"）。

**Step 0e**：`grantIDs` 不落盘，恢复必须**重跑** `grants:evaluate`（spec §5 也这么要求），别从 pending 里找命中的 grant。

**Files:**
- Create: `internal/agent/resume.go`
- Modify: `internal/agent/state.go`（加 `Subject` 字段）
- Modify: `internal/agent/suspend.go`（`renderLabels` 所在文件；若需补 `Subject` 的填充逻辑在 `run.go`，按实际情况）
- Test: `internal/agent/resume_test.go`

- [x] **Step 1: 写失败测试**

```go
// 合法审批人 → 执行全部步骤 → result 段带 approvedBy → 归档 done。
func TestResumeExecutesAllStepsWithApprovedBy(t *testing.T) { /* 用 Task 3 的假 host + run_test.go 的假模块 */
	env := newResumeEnv(t)
	// 预置一份 pending（用 suspend 造，保证 I7 顺序）
	p := env.SeedPending(t, "pln_r1")
	if err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"})); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if got := env.ExecutedActions(); len(got) != 1 || got[0] != "inventory.DeductStock" {
		t.Fatalf("应执行 1 步，得到 %v", got)
	}
	if by := env.ResultApprovedBy(p.DispatchedAuditID); by != "user:bob" {
		t.Fatalf("result 段 approvedBy 应为 user:bob，得到 %q", by)
	}
	if st := env.ArchivedState(p.PlanID); st != StateDone {
		t.Fatalf("应归档 done，得到 %q", st)
	}
}

// 审批人角色不匹配 → 403 等价：不执行、pending 不动。
func TestResumeRejectsWrongApprover(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r2")
	err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:alice", []string{"role:warehouse_clerk"}))
	if err == nil {
		t.Fatalf("角色不匹配必须报错")
	}
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("不许执行")
	}
	if _, ok, _ := env.Store.Load(p.PlanID); !ok {
		t.Fatalf("pending 必须保持待批")
	}
}

// 过期 → 归档 expired，零执行。
func TestResumeExpiredDoesNotExecute(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPendingExpired(t, "pln_r3")
	err := env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))
	if err == nil {
		t.Fatalf("过期必须报错")
	}
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("不许执行")
	}
	if st := env.ArchivedState(p.PlanID); st != StateExpired {
		t.Fatalf("应归档 expired，得到 %q", st)
	}
}

// G4 失效 → 不硬跑，旧 plan 归档 stale。
func TestResumeG4FailureArchivesStale(t *testing.T) {
	env := newResumeEnv(t)
	env.Module.StockAfterApprove = 7 // 审批期间被别人扣了
	p := env.SeedPending(t, "pln_r4")
	_ = env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("G4 失效不许执行")
	}
	if st := env.ArchivedState(p.PlanID); st != StateStale {
		t.Fatalf("应归档 stale，得到 %q", st)
	}
}

// 恢复时 policy 收紧 → 不执行，但 result 段仍记 approvedBy。
func TestResumePolicyTightenedStillRecordsApprover(t *testing.T) {
	env := newResumeEnv(t)
	env.Host.PolicyAfterResume = "deny"
	p := env.SeedPending(t, "pln_r5")
	_ = env.Resume(t, confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"}))
	if len(env.ExecutedActions()) != 0 {
		t.Fatalf("policy 收紧不许执行")
	}
	if by := env.ResultApprovedBy(p.DispatchedAuditID); by != "user:bob" {
		t.Fatalf("人批过的事实必须留痕，得到 %q", by)
	}
}

// 同一 confirm 重放 → 第二次返回既有结局，不重复执行。
func TestResumeIdempotentOnReplay(t *testing.T) {
	env := newResumeEnv(t)
	p := env.SeedPending(t, "pln_r6")
	cf := confirmFrame(p.PlanID, "approve", "user:bob", []string{"role:warehouse_manager"})
	if err := env.Resume(t, cf); err != nil {
		t.Fatalf("首次恢复失败：%v", err)
	}
	if err := env.Resume(t, cf); err != nil {
		t.Fatalf("重放应返回既有结局而不是报错：%v", err)
	}
	if n := len(env.ExecutedActions()); n != 1 {
		t.Fatalf("副作用必须只发生一次，实际执行 %d 次", n)
	}
}
```

> 这些测试依赖一个测试装配器 `newResumeEnv`（假 host + 假模块 + 状态目录）。**在 `resume_test.go` 里自己写它**，结构参考 `run_test.go` 的既有假 host：`env.Host` 记录审计与策略、`env.Module` 记录执行与库存。不要改 `run_test.go` 的既有设施。

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestResume' -count=1`
Expected: `undefined: resumeConfirm`（或同类）

- [x] **Step 3: 实现 `resume.go`**

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Confirm 是 tenon.confirm 帧在 agent 侧的投影。
type Confirm struct {
	ConfirmID string
	PlanID    string
	Decision  string // approve | reject
	By        string
	Roles     []string
	At        string
}

// resumeError 区分「可重试」与「确定性拒绝」：只有前者值得下一 tick 再来。
type resumeError struct {
	Msg       string
	Retryable bool
}

func (e resumeError) Error() string { return e.Msg }

// resume 处理一条 confirm。返回 nil 表示已处理完毕（含幂等重放）；
// 返回 retryable 错误表示下一 tick 该帧会重放。
func resume(ctx context.Context, deps Deps, store *StateStore, c Confirm) error {
	p, ok, err := store.Load(c.PlanID)
	if err != nil {
		return resumeError{Msg: "读 pending 失败：" + err.Error(), Retryable: true}
	}
	if !ok {
		// 已归档？done 视为幂等成功，其它状态报确定性拒绝。
		archived, aerr := store.LoadArchived(c.PlanID)
		if aerr != nil {
			return resumeError{Msg: fmt.Sprintf("confirm 指向未知 planId：%s", c.PlanID)}
		}
		if archived.State == StateDone {
			return nil // 幂等：已经执行过，不再动
		}
		return resumeError{Msg: fmt.Sprintf("planId %s 已处于 %s，不能再次确认", c.PlanID, archived.State)}
	}
	if c.Decision == "reject" {
		if err := store.Archive(p.PlanID, StateRejected); err != nil {
			return resumeError{Msg: err.Error()}
		}
		return nil // 终态的公告板覆盖由下一个 tick 的自检阶段统一推（见 Task 10 第 2 段）
	}
	// 1) 审批人
	if !rolesIntersect(c.Roles, p.RequiredRoles) {
		return resumeError{Msg: fmt.Sprintf("审批人 %s 的角色不在 %v 中：（403）", c.By, p.RequiredRoles)}
	}
	// 2) 过期
	if p.Expired(time.Now().UTC()) {
		_ = store.Archive(p.PlanID, StateExpired)
		return resumeError{Msg: fmt.Sprintf("planId %s 已过期，请重新发起", p.PlanID)}
	}
	// 3) 重跑 G4 与 policy（复用 runner）
	res := runResumed(ctx, deps, *p, c)
	if res.err != nil {
		_ = store.Archive(p.PlanID, res.archiveAs)
		return resumeError{Msg: res.err.Error(), Retryable: res.retryable}
	}
	if err := store.Archive(p.PlanID, StateDone); err != nil {
		return resumeError{Msg: err.Error(), Retryable: true}
	}
	return nil
}

// r2 已删除：拒绝分支不再单独推公告板，终态覆盖统一由 tick 的自检阶段推（Task 10 第 2 段）。

func rolesIntersect(have, need []string) bool {
	for _, n := range need {
		for _, h := range have {
			if h == n {
				return true
			}
		}
	}
	return false
}

// runResumed 的结果：要么执行完毕，要么给出归档原因。
type resumeOutcome struct {
	err       error
	archiveAs PendingState
	retryable bool
}
```

`runResumed` 与 `finishTerminal` 的完整实现（同一文件）：

```go
// runResumed 重跑 G4 与 policy/grant，放行则执行全部步骤。
func runResumed(ctx context.Context, deps Deps, p Pending, c Confirm) resumeOutcome {
	r := &runner{
		deps:        deps,
		host:        trimSlash(deps.Host),
		event:       DomainEvent{EventID: p.TriggerEventID, Type: "tenon.request", CorrelationID: p.Plan.Trigger.CorrelationID},
		correlationID: p.Plan.Trigger.CorrelationID,
		planID:      p.PlanID,
		client:      deps.HTTP,
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: 10 * time.Second}
	}
	// 闸门：策略（恢复时重跑，收紧即生效）与授权。
	policy, err := r.evaluatePolicies(ctx, p.Plan)
	if err != nil {
		return resumeOutcome{err: fmt.Errorf("恢复时策略评估不可用（不执行）：%w", err), archiveAs: StateStale, retryable: true}
	}
	if policy.Outcome != outcomeApprove && policy.Outcome != outcomeAuto {
		_ = r.writePlanResult(ctx, p, c.By, "policy_denied")
		return resumeOutcome{err: fmt.Errorf("恢复时策略已收紧为 %s，不执行", policy.Outcome), archiveAs: StateStale}
	}
	grantIDs := map[string]string{}
	for _, step := range p.Plan.Steps {
		g, err := r.evaluateGrant(ctx, step)
		if err != nil {
			return resumeOutcome{err: err, archiveAs: StateStale, retryable: true}
		}
		if !g.Allowed {
			_ = r.writePlanResult(ctx, p, c.By, "policy_denied")
			return resumeOutcome{err: fmt.Errorf("恢复时授权拒绝 %s：%s", step.Action, g.Reason), archiveAs: StateStale}
		}
		grantIDs[step.StepID] = g.MatchedGrantID
	}
	// G4：模块前置条件由执行时的 422 暴露（模块自己判），这里先做一次显式探测。
	if err := r.probePreconditions(ctx, p.Plan); err != nil {
		return resumeOutcome{err: err, archiveAs: StateStale}
	}
	// 执行全部步骤（原幂等键）。
	ordered, err := orderSteps(p.Plan.Steps)
	if err != nil {
		return resumeOutcome{err: err, archiveAs: StateStale}
	}
	for _, step := range ordered {
		gate := gateDecision{outcome: outcomeApprove, policyID: firstNonEmpty(policy.DecidedBy...), grantID: grantIDs[step.StepID]}
		verdict := r.executeStep(ctx, p.Plan, step, gate)
		if verdict.kind != verdictOK {
			return resumeOutcome{err: fmt.Errorf("恢复执行失败：%s", verdict.detail), archiveAs: StateStale}
		}
		r.taken = append(r.taken, step.Action)
	}
	if err := r.writePlanResult(ctx, p, c.By, ""); err != nil {
		return resumeOutcome{err: err, archiveAs: StateStale, retryable: true}
	}
	return resumeOutcome{}
}

// probePreconditions 用只读动作重跑一次「够不够」的判断：读失败即 fail-closed（不执行）。
// 具体读哪个动作由 plan 里第一个写步骤的目标实体决定，取不到就跳过（模块侧仍在执行时自校验）。
func (r *runner) probePreconditions(ctx context.Context, plan Plan) error {
	// 现有实现在 executeStep 里按步自校验；这里只要求「能读到当前状态」，
	// 读不到即为并发/可用性问题，按 G4 失效处理。实现方式：对首个写步骤的 action 所属模块
	// 做一次 manifest 可达性探测。
	if len(plan.Steps) == 0 {
		return fmt.Errorf("计划没有步骤")
	}
	if _, ok := r.moduleBase(plan.Steps[0].Action); !ok {
		return fmt.Errorf("恢复时目标模块不可达：%s", plan.Steps[0].Action)
	}
	return nil
}

// writePlanResult 写计划级 result 段（stepId 为空），approvedBy 记审批人。
func (r *runner) writePlanResult(ctx context.Context, p Pending, approvedBy, errClass string) error {
	status := auditStatusOK
	if errClass != "" {
		status = auditStatusError
	}
	by := approvedBy
	rec := auditRecord{
		AuditID: p.DispatchedAuditID,
		Stage:   auditStageResult,
		At:      time.Now().UTC().Format(time.RFC3339),
		Actor:   auditActor{Type: "agent", ID: r.deps.AgentID, OnBehalfOf: nilIfEmpty(p.By)},
		Trigger: auditTrigger{EventID: nilIfEmpty(p.TriggerEventID), CorrelationID: nilIfEmpty(p.Plan.Trigger.CorrelationID)},
		PlanID:  nilIfEmpty(p.PlanID),
		Decision: auditDecision{
			Outcome: "approve", PolicyID: nil, GrantID: nil,
			ApprovedBy: &by, // 人批过的事实必须留痕，即使策略在恢复时拒绝
		},
		Result:  auditResult{Status: status, ErrorClass: nilIfEmpty(errClass)},
		Effects: auditEffects{},
	}
	return postAuditRecord(ctx, r.deps, rec)
}
```

- [x] **Step 4: 跑测试至全绿**

Run: `go run ./cmd/tenon d1 run --help`（确保工具链仍可用）→ 忽略

Run: `go test ./internal/agent/ -run 'TestResume' -count=1 -v`
Expected: 6 个用例全 PASS

- [x] **Step 5: Commit**

```bash
git add internal/agent/resume.go internal/agent/resume_test.go
git commit -m "feat(agent): 恢复执行（审批人/过期/G4/policy/归档）"
```

---

## Task 9: 公告板重推与终态覆盖

**状态：✅ 已完成（commit `15f3438`，5 条用例）；但终态覆盖无生产调用点 + host 侧缺 `state` 字段 → Task 9b 补**

**Task 8 复核后追加的要求（比正文更权威）**

1. **`finishTerminal` 只推公告板，绝不再写 result 段**：`resume.go` 已在归档前写好计划级 result（`auditId = DispatchedAuditID`）。计划正文那句「写计划级 result 段并推终态覆盖」的注释是**错的**——照它再写一次，同一 auditId 会出现两条 result 段（计划级变三条记录）。
2. **`RepushBoard` 要跳过「archived 里已有同名终态」的 pending**：`Archive` 的两步（写归档件 → 删 pending 件）之间失败会留下同名双份，`List()` 仍会返回它，重推会让板上挂一张**早已完成的卡**直到过期。实现：重推前查 `LoadArchived(planId)`，命中终态就跳过（并顺手删掉残留 pending，附日志）。
3. **`state=resuming` 不要按 awaiting 重推**：它是崩溃标记（confirm 已被接受过），重推成"待批"会让审批人再点一次。
4. **reject 分支不写 result 段**（`resume.go` 已如此）。若将来要补：`decision.outcome=deny`、`approvedBy=null`——拒绝就是没有人批准。

**Files:**
- Create: `internal/agent/board.go`
- Modify: `internal/agent/resume.go`（仅在需要收口终态推送时；不得重复写 result 段）
- Modify: `internal/agent/state.go`（若需按 planId 查 archived，用已有 `LoadArchived`）
- Test: `internal/agent/board_test.go`

- [x] **Step 1: 写失败测试**

```go
// 归档终态时推一条 state=终态 的公告板覆盖：否则已执行的单会在板上挂到过期。
func TestFinishTerminalPushesOverride(t *testing.T) {
	pushes := &[]map[string]any{}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tenon/pending" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*pushes = append(*pushes, body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer host.Close()

	p := samplePending()
	if err := finishTerminal(context.Background(),
		NewHostClient(host.URL, "agent", "pw", http.DefaultClient), p, StateDone, ""); err != nil {
		t.Fatalf("推终态失败：%v", err)
	}
	if len(*pushes) != 1 || (*pushes)[0]["state"] != "done" {
		t.Fatalf("应推一条 state=done 的覆盖：%+v", *pushes)
	}
}

// 重推现存 pending：幂等 upsert，payload 带 state=awaiting_approval。
func TestRepushPendingBoard(t *testing.T) {
	pushes := &[]map[string]any{}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*pushes = append(*pushes, body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer host.Close()

	store, _ := NewStateStore(t.TempDir())
	if err := store.Save(samplePending()); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	n, err := RepushBoard(context.Background(), NewHostClient(host.URL, "agent", "pw", http.DefaultClient), store)
	if err != nil || n != 1 {
		t.Fatalf("应推 1 条：n=%d err=%v", n, err)
	}
	if (*pushes)[0]["state"] != "awaiting_approval" {
		t.Fatalf("state 不对：%+v", (*pushes)[0])
	}
}
```

- [x] **Step 2: 实现 `board.go`**

```go
package agent

import "context"

// boardPayload 把 pending 转成公告板条目。state 让页面能区分「待批」与「刚刚处理完」。
func boardPayload(p Pending, state PendingState) map[string]any {
	items := make([]map[string]any, 0, len(p.Labels))
	for _, l := range p.Labels {
		items = append(items, map[string]any{"label": l})
	}
	out := map[string]any{
		"planId":        p.PlanID,
		"summary":       p.Summary,
		"items":         items,
		"requiredRoles": p.RequiredRoles,
		"expiresAt":     p.ExpiresAt,
		"state":         string(state),
	}
	if p.RequestID != nil {
		out["requestId"] = *p.RequestID
	}
	return out
}

// RepushBoard 把现存 pending 幂等 upsert 到公告板。返回推送条数。
func RepushBoard(ctx context.Context, client *HostClient, store *StateStore) (int, error) {
	items, err := store.List()
	if err != nil {
		return 0, err
	}
	for _, p := range items {
		if err := client.PushPending(ctx, boardPayload(p, p.State)); err != nil {
			return 0, err // 公告板失败只告警由调用方决定，不在这里吞
		}
	}
	return len(items), nil
}

// finishTerminal 写计划级 result 段并推一条终态公告板覆盖。
func finishTerminal(ctx context.Context, client *HostClient, p Pending, state PendingState, errClass string) error {
	if client != nil {
		if err := client.PushPending(ctx, boardPayload(p, state)); err != nil {
			return err
		}
	}
	return nil
}
```

- [x] **Step 3: 跑测试至全绿**

Run: `go test ./internal/agent/ -run 'TestFinishTerminal|TestRepushPendingBoard' -count=1 -v`
Expected: 2 个用例全 PASS

- [x] **Step 4: Commit**

```bash
git add internal/agent/board.go internal/agent/board_test.go internal/agent/resume.go
git commit -m "feat(agent): 公告板重推与终态覆盖"
```

---

## Task 9b: host 侧补 `pendingItem.state`（Task 9 复核发现的缺口）

**为什么必须单独一步**：Task 9 让 agent 推 `state`，但 `internal/controlplane/pending.go` 的 `pendingItem` **没有 `State` 字段**——`json.Unmarshal` 会静默丢弃它。今天的实际行为是：agent 推了终态覆盖，host 把同一条目原样重写、页面仍然显示"待批"。也就是说 **Task 9 的成果在线上等于零**。

**Files:**
- Modify: `internal/controlplane/pending.go`（`pendingItem` 加字段 + 缺省值）
- Test: `internal/controlplane/pending_test.go`

- [x] **Step 1: 写失败测试**

```go
// 公告板条目必须带回 state：它是"待批"与"刚刚处理完"的唯一区分。
func TestPendingBoardKeepsState(t *testing.T) {
	// POST 一条 state=done 的覆盖，GET 必须原样带回 "done"。
	// 再 POST 一条**不带 state** 的，GET 必须给缺省 "awaiting_approval"。
}

// 缺省值不是"空串"：旧客户端（不推 state）的条目仍要能显示成待批。
func TestPendingBoardDefaultsStateToAwaiting(t *testing.T) {}
```

（用例骨架实现者补全，沿用 `pending_test.go` 现有的 `newTestServer` / token 夹具；`expiresAt` 用远期常量。）

- [x] **Step 2: 实现**

```go
type pendingItem struct {
	// 原有字段不动…
	// State 区分「待批」与「刚刚处理完」：页面按它决定怎么显示，协议层不替页面过滤。
	// 缺省 awaiting_approval —— 老客户端不推这个字段时不能被当成终态。
	State string `json:"state"`
}
```

在 upsert 里补缺省：`if item.State == "" { item.State = "awaiting_approval" }`；`List` 仍按 `expiresAt` 剔除过期条目（不变）。

- [x] **Step 3: 跑测试**

```bash
go test ./internal/controlplane/ -run 'TestPendingBoard' -count=1 -v
go test ./... -count=1
```

- [x] **Step 4: Commit**

```bash
git add internal/controlplane/pending.go internal/controlplane/pending_test.go
git commit -m "feat(controlplane): 公告板条目带回 state（终态覆盖否则被静默丢弃）"
```

**注意**：本任务**不接线**终态覆盖的调用点——那是 Task 10 的事（`Resume` 缺登录凭证，需要由 poller 推）。

---

## Task 10: `cmd/agent` 的 tick 四段顺序与装配

**Task 9 复核后追加的四条（比正文更权威）**

1. **终态覆盖必须有生产调用点**：`finishTerminal` 未导出、`Resume` 无凭证。**最终选法（比初版的方案 (b) 更省）**：`board.go` 加导出包装 `PushTerminal(ctx, client *HostClient, p Pending, state PendingState) error`；poller 在 `Resume` 成功返回后用 `store.LoadArchived(planId)` 取归档件再推。**`Resume` 签名一个字不用改**——调用方本来就知道 `planId`；重放时归档件已在，再推一次是幂等 upsert。**推失败绝不能回滚归档**（回滚会让同一 confirm 被重放执行第二次），错误只记日志。
2. **挂起前校验 `approvers` 非空**：`rolesIntersect` 对空集 fail-closed，`requiredRoles` 为空的 approve 计划**谁点都是 403**。在 `buildPending`/挂起处判空 → 不挂起、走 `needs_human` 且 Detail 写明"策略要求审批但没给审批人角色"。
3. **`resumeError.Retryable` 要能被调用方判断**：加 `agent.IsRetryable(err) bool`；确定性拒绝（角色不匹配/过期/未知 planId/已归档非 done/未知处置）立即 ack，可重试才留到下一 tick。
4. **合成事件 id 必须确定性**：`evt_req_<requestId>`，**不要用 `newID()`**（时间基会让同一 request 在重启/重放后 triggerEventId 漂移，审计对账对不上）。

- **Task 10b 复核后的更正（比本节正文更权威）**：
  1. **合成事件 id 用宿主签发的 `syntheticEventId`，不要自己拼 `evt_req_<requestId>`**（原第 4 条是错的）：`docs/interfaces.md:382` 明文要求沿用；且宿主 origin 登记表以宿主签发的 id 为键，自造 id 会把 `origin=request` 判成 `event` → `plat-request-needs-human` 失效 → **对话发起的采购单被自动放行**。
  2. **入站游标只跟"已 ack 的 seq"**，不要用 `PollInbound` 的 `nextCursor`：宿主按 `Since(after)` 回放，用 nextCursor 会让未 ack 的帧永远回不来。
  3. **审批人角色通道（Task 10c）**：宿主在 confirm 帧注入 token claims 的 roles（并删除客户端自报），agent 读帧里的 roles。缺这条 approve 永远 403。

**Files:**
- Modify: `cmd/agent/main.go`
- Test: `cmd/agent/main_test.go`

- [x] **Step 1: 写失败测试**

```go
// tick 的四段顺序是唯一的调度契约（入站 → 自检 → 公告板 → 事件），必须有用例锁住。
func TestPollerTickOrder(t *testing.T) {
	env := newPollerEnv(t) // 假 host + 假模块，见下方说明
	if _, err := env.Poller.pollOnce(context.Background()); err != nil {
		t.Fatalf("tick 失败：%v", err)
	}
	want := []string{"poll", "ack", "selfcheck", "board", "events"}
	got := env.Poller.Steps()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tick 顺序不符：want=%v got=%v", want, got)
	}
}
```

`poller` 增一个**只在测试里非空**的记录器（生产传 nil，零成本）：

```go
	// steps 记录本 tick 走过的阶段名；生产为 nil（不收集）。
	steps *[]string
```

`pollOnce` 的四段各写一行：

```go
	p.record("poll")       // 段 1 开始
	...
	p.record("ack")        // 每帧 ack 之后
	...
	p.record("selfcheck")  // 段 2
	...
	p.record("board")      // 段 3
	...
	p.record("events")     // 段 4
```

```go
// record 追加阶段名（生产为 no-op）。
func (p *poller) record(name string) {
	if p.steps != nil {
		*p.steps = append(*p.steps, name)
	}
}

// Steps 返回本 tick 记录的阶段名（测试用）。
func (p *poller) Steps() []string {
	if p.steps == nil {
		return nil
	}
	return append([]string(nil), *p.steps...)
}
```

> `newPollerEnv` 的职责：起 `httptest` 假 host（`/tenon/session`、`/tenon/requests/poll` 返回 1 帧、
> `/tenon/requests/ack`、`/tenon/pending`、`/tenon/audit`、`/tenon/policies:evaluate`）与假模块
> （`/tenon/events`、`/tenon/modules` 契约同 `internal/agent/run_test.go` 的既有假 host），
> 返回装好 `hostc` / `store` / `ledger` / `steps` 的 `poller`。**照 `run_test.go` 的写法抄，不要新造一套风格。**

- [x] **Step 2: 改 `cmd/agent/main.go`**

### Step 2 前置：Task 2 复核后追加的四条硬要求

1. **空 `requestId` 必须在进账本前拒掉**：`Claim("")` 恒返回 `(true, nil)` 且不写盘，等于**静默关闭去重**。`handleInbound` 的 request 分支开头就要校验 `req.RequestID == ""` → 记日志 + 返回错误（不 ack，让 host 侧报文问题暴露出来）。
2. **跳过时要区分 `claimed` 与 `done`**：`Ledger` 现在只有 `Claim`（探测即写盘）。Task 10 **允许给 `ledger.go` 加一个只读方法**：

```go
// Phase 返回某个 requestId 的已知阶段（""=没出现过，claimed=处理中断，done=已处理）。
// 只读，不写盘：排障路径上不要用 Claim 代替它——Claim 每次都会写一行。
func (l *Ledger) Phase(requestID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[requestID]
}
```

`handleInbound` 里 `Claim` 返回 false 时按 phase 分开处理：

```go
		if !ok {
			switch p.ledger.Phase(req.RequestID) {
			case "claimed":
				// 上次处理中断（进程崩了 / 回合失败）：宁可丢这一句，也不重复扣库存。
				log.Printf("agent: request %s 上次处理中断、未完成，按去重策略跳过（这是一句被舍弃的用户请求）", req.RequestID)
				_ = p.hostc.PushPending(ctx, map[string]any{
					"planId": "req-" + req.RequestID, "summary": "该请求处理中断，请重新发起",
					"items": []map[string]any{{"label": "上次处理中断，请重新发起"}},
					"requiredRoles": []string{}, "expiresAt": time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
					"state": "aborted",
				})
			default:
				log.Printf("agent: request %s 已处理过，跳过（去重）", req.RequestID)
			}
			return nil
		}
```

3. **`Done` 必须覆盖所有返回路径**，包括 `res.Outcome == NeedsHuman`（挂起成功，那也是一次完整的处理）。实现时把 `Done` 放在 `Run` 之后立刻执行，不要让它排在可能失败的 `writeRequestResult` 之后。
4. **装配**：`NewLedger(filepath.Join(cfg.stateDir, "processed.jsonl"))`（**文件**路径，不是目录）；`poller.ledger` 必须注入，否则 `drainInbound` 会 nil 解引用。`Deps.State` 为 nil 表示"没有挂起通道"，**不要把空串传给 `NewStateStore`**。

5. **`auditProbe` 的 host 归属已定（`c97277e` 之后）**：签名已改成 `Has(ctx, auditID)`——**没有 host 参数**，空串在类型上不存在了。真实现放在 Task 10：

```go
// hostAuditProbe 用 HostClient 查审计链；host 绑在 client 里，不进签名。
type hostAuditProbe struct{ client *HostClient }

func (h hostAuditProbe) Has(ctx context.Context, auditID string) (bool, error) {
	status, raw, err := h.client.do(ctx, http.MethodGet, h.client.base+"/tenon/audit", nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("查审计返回 %d：%s", status, snippet(raw))
	}
	return bytes.Contains(raw, []byte(`"auditId":"`+auditID+`"`)), nil
}
```

`HostClient.base` 是小写字段，同包可直接读；若确实需要外部访问，加 `BaseURL() string`，**不要**把字段导出。

6. **自检的导出包装与日志**：`selfCheck` 是小写、返回未导出类型 `selfCheckReport`。Task 10 加个导出包装 `SelfCheck(...) (selfCheckReport, error)` 即可（调用方用 `rep.Kept` 这类导出字段，不需要命名类型）。日志行**必须包含 `Corrupt`**——坏文件唯一的告警出口。

7. **`SelectSkills` 接进 `Run` 的三条（Task 5 复核后追加）**
   (i) **`PlanInput.Request`（Task 6）与 `RunInput.Request`（Task 7）必须同时存在**：`Run` 内按 `runInput.Request != nil` 分流——request 回合用 `SelectSkills` 算候选集，事件回合仍走 `Skills.TextFor(event.Type)`。只改一处会得到一个永远不成立的分支。

   (ii) **别用 `SkillStore.TextFor` 渲染候选集**：它只渲染**非空 Body**。只靠 `Subscribes` 命中的 skill（Body 为空）会被选中却渲染成空串 → `userPrompt` 写「（本次没有匹配的 skill 正文）」——**选了 N 份却告诉模型没有**。需要新写 `TextForSelected(selected []Skill) string`，与 `TextFor` 同一格式，但空 Body 渲染成「（无正文，仅按订阅命中）」，绝不渲染空串。

   (iii) **`deps.Skills.Len() == 0` 要单独打日志**：`SelectSkills` 不返回 error，区分不出「压根没加载 skill」与「加载了但没命中」；`LoadSkills` 的注释本就要求把 0 份打出来。另两条写进 `skillselect.go` 注释：`limit <= 0` 会被静默改成 2（传 0 当"不限"只会拿 2 份）；`knownModuleNames` 必须全小写、新模块要记得加行，否则表现为"没有匹配"的静默降级。



`config` 增字段（沿用现有 env 读取风格）：

```go
	stateDir    string // TENON_AGENT_STATE_DIR（缺省 out/agent-state）
	agentUser   string // TENON_AGENT_USER
	agentPass   string // TENON_AGENT_PASSWORD
	retainDays  int    // TENON_PENDING_RETENTION_DAYS（缺省 30）
```

`poller` 增字段：

```go
	hostc   *agent.HostClient
	store   *agent.StateStore
	ledger  *agent.Ledger
	inbound int64 // 入站游标（进程内；重放由 host 侧 ack 保证范围）
```

`pollOnce` 改为四段：

```go
func (p *poller) pollOnce(ctx context.Context) (int, error) {
	rounds := 0
	// 1. 入站 drain
	n, err := p.drainInbound(ctx)
	rounds += n
	if err != nil {
		log.Printf("agent: 入站处理出错（本 tick 继续跑其余段）：%v", err)
	}
	// 2. 过期清理与自检
	if rep, err := agent.SelfCheck(ctx, p.store, p.auditProbe(ctx))(time.Now().UTC()); err != nil {
		log.Printf("agent: 自检失败：%v", err)
	} else if rep.Stale+rep.Expired+rep.Settled > 0 {
		log.Printf("agent: 自检 kept=%d expired=%d stale=%d settled=%d", rep.Kept, rep.Expired, rep.Stale, rep.Settled)
	}
	// 3. 公告板重推
	if n, err := agent.RepushBoard(ctx, p.hostc, p.store); err != nil {
		log.Printf("agent: 公告板重推失败（read-model 可重建）：%v", err)
	} else if n > 0 {
		log.Printf("agent: 公告板已重推 %d 条", n)
	}
	// 4. 事件轮询（原逻辑）
	ev, err := p.pollEvents(ctx)
	return rounds + ev, err
}
```

`drainInbound` 的完整实现（同文件）：

```go
// drainInbound 拉取入站帧并逐帧处理，每帧处理完立即 ack。
func (p *poller) drainInbound(ctx context.Context) (int, error) {
	frames, next, err := p.hostc.PollInbound(ctx, p.inbound)
	if err != nil {
		return 0, err
	}
	starts := 0
	for _, f := range frames {
		var probe struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			PlanID    string `json:"planId"`
		}
		if err := json.Unmarshal(f.Frame, &probe); err != nil {
			log.Printf("agent: 入站帧 %d 不可解析，跳过：%v", f.Seq, err)
		} else if err := p.handleInbound(ctx, probe.Type, f.Frame); err != nil {
			log.Printf("agent: 入站帧 %d（%s）处理失败：%v", f.Seq, probe.Type, err)
			// 不 ack：下一 tick 重放（重试计数见 §6.2，实现时用 map 记 attempts）
			continue
		}
		if err := p.hostc.AckInbound(ctx, f.Seq); err != nil {
			return starts, fmt.Errorf("ack %d 失败：%w", f.Seq, err)
		}
		if probe.Type == "tenon.request" {
			starts++
		}
	}
	p.inbound = next
	return starts, nil
}

// handleInbound 分派 request / confirm。
func (p *poller) handleInbound(ctx context.Context, typ string, raw []byte) error {
	switch typ {
	case "tenon.request":
		var req struct {
			RequestID        string          `json:"requestId"`
			SyntheticEventID string          `json:"syntheticEventId"`
			Text             string          `json:"text"`
			By               string          `json:"by"`
			Context          json.RawMessage `json:"context"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
		ok, err := p.ledger.Claim(req.RequestID)
		if err != nil {
			return err
		}
		if !ok {
			log.Printf("agent: request %s 已处理过，跳过（去重）", req.RequestID)
			return nil
		}
		deps := p.deps
		deps.OnBehalfOf = req.By
		res := agent.Run(ctx, deps, agent.RunInput{
			Event:   agent.DomainEvent{EventID: req.SyntheticEventID, Type: "tenon.request", CorrelationID: req.By},
			Request: &agent.UserRequest{RequestID: req.RequestID, Text: req.Text, By: req.By},
		})
		p.writeRequestResult(req.RequestID, res)
		return p.ledger.Done(req.RequestID, res.PlanID, string(res.Outcome))
	case "tenon.confirm":
		var cf struct {
			PlanID   string   `json:"planId"`
			Decision string   `json:"decision"`
			By       string   `json:"by"`
			At       string   `json:"at"`
		}
		if err := json.Unmarshal(raw, &cf); err != nil {
			return err
		}
		roles, err := p.rolesOf(ctx, cf.By)
		if err != nil {
			return err
		}
		return agent.Resume(ctx, p.deps, p.store, agent.Confirm{
			PlanID: cf.PlanID, Decision: cf.Decision, By: cf.By, Roles: roles, At: cf.At,
		})
	}
	return fmt.Errorf("未知入站帧类型 %q", typ)
}
```

> `rolesOf` 从 token claims 拿：`HostClient` 登录响应里有 `roles`，在 `Login` 时缓存到 `poller`；本任务要求把 `HostClient` 的 claims 暴露为 `Claims() (subject string, roles []string)`。**不要**另造一次登录。

- [x] **Step 3: 跑测试**

Run: `go build ./... && go test ./cmd/agent/ -count=1`
Expected: 编译通过；tick 顺序用例按实现者的骨架断言通过

- [x] **Step 4: Commit**

```bash
git add cmd/agent/main.go cmd/agent/main_test.go
git commit -m "feat(agent): tick 四段顺序（入站/自检/公告板/事件）与入站装配"
```

---

## Task 11: Task 0 —— `TENON_TEST_MODE` 下关闭入站通道

**Files:**
- Modify: `internal/controlplane/http.go`
- Test: `internal/controlplane/http_test.go`

- [ ] **Step 1: 写失败测试**

```go
// D1 的「同一输入同一行为」要求入站通道在测试模式下不存在：
// 挂不挂前端不该改变 agent 的结局。
func TestInboundEndpointsAbsentInTestMode(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		AuditPath:     dir + "/audit.jsonl",
		SessionSecret: []byte("secret"),
		InboundDir:    dir + "/inbound",
		TestMode:      true,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, path := range []string{"/tenon/requests", "/tenon/requests/poll", "/tenon/requests/ack", "/tenon/pending"} {
		status, _ := postJSON(t, ts.URL+path, `{}`)
		if status != http.StatusNotFound {
			t.Fatalf("%s 在测试模式下应 404，得到 %d", path, status)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controlplane/ -run 'TestInboundEndpointsAbsentInTestMode' -count=1`
Expected: 得到 401/405 而不是 404

- [ ] **Step 3: 改 `ServeHTTP`**

```go
	case "/tenon/requests":
		if s.testMode {
			writeTenonErrorStatus(w, http.StatusNotFound, unknownEndpoint(r.URL.Path))
			return
		}
		s.serveInboundEnqueue(w, r)
	case "/tenon/requests/poll":
		if s.testMode {
			writeTenonErrorStatus(w, http.StatusNotFound, unknownEndpoint(r.URL.Path))
			return
		}
		s.serveInboundPoll(w, r)
	case "/tenon/requests/ack":
		if s.testMode {
			writeTenonErrorStatus(w, http.StatusNotFound, unknownEndpoint(r.URL.Path))
			return
		}
		s.serveInboundAck(w, r)
	case "/tenon/pending":
		if s.testMode {
			writeTenonErrorStatus(w, http.StatusNotFound, unknownEndpoint(r.URL.Path))
			return
		}
		s.servePending(w, r)
```

并在包注释的端点清单里注明「测试模式下入站四路由不挂（D1 可重复性）」。

- [ ] **Step 4: 跑测试**

Run: `go test ./internal/controlplane/ -run 'TestInboundEndpointsAbsentInTestMode' -count=1 -v && go test ./... -count=1`
Expected: 新用例 PASS；全仓无 FAIL（既有的入站用例**不设** TestMode，不受影响）

- [ ] **Step 5: Commit**

```bash
git add internal/controlplane/http.go internal/controlplane/http_test.go
git commit -m "fix(controlplane): 测试模式下不挂入站通道（D1 可重复性）"
```

---

## Task 12: 文档与全量回归

**Files:**
- Modify: `docs/superpowers/specs/2026-10-27-tenon-m05-host-agent-design.md`（§1 的「不持久化状态」）
- Modify: `README.md`（「还没做的」删掉已交付项）
- Modify: `docs/interfaces.md`（公告板条目加 `state` 字段）

- [ ] **Step 1: 改三处文档（按 spec §12 的映射逐条改）**

1. M0.5 spec §1 的 agent 行：把「不开端口、不持久化状态」改为「不开端口；持久状态只有 `TENON_AGENT_STATE_DIR` 下的 `pending/`、`archived/` 与 `processed.jsonl`」。
2. README「还没做的」：删掉「agent 侧的挂起/恢复」「前端页面（P3 未做）」中的**前者**（后者保留，P3 未做）。
3. `docs/interfaces.md` §4.7：公告板条目字段补 `state`（`awaiting_approval` / `done` / `rejected` / `expired` / `stale`），并注明「过滤是页面的事，协议层返回全部未过期条目」。
4. **README 的已知缺口必须写明审计形状偏离**：挂起/恢复的计划级审计 `stepId=null`、无 `action`/`inputHash`，**不满足 `audit-event.schema.json` 的完整 required 集**；宿主当前只校验 `auditId` 与 `stage`，加严校验会让挂起路径全线失败。这是评审选定的 A 案（语义最诚实），不是疏漏。

- [ ] **Step 2: 全量回归**

```bash
go test ./... -count=1
pnpm check
gofmt -l .
go vet ./...
go build ./...
```

Expected: 五项全绿。

- [ ] **Step 3: 冒烟（手工，记入验收清单）**

```bash
# 起 host（TEST_MODE 关掉）+ inventory + procurement + agent
TENON_MODULES='inventory=http://127.0.0.1:8081,procurement=http://127.0.0.1:8082' \
TENON_USERS_FILE=config/users.dev.json TENON_SESSION_SECRET=dev \
TENON_SKILLS_DIR=skills TENON_INBOUND_DIR=out/inbound go run ./cmd/host
TENON_AGENT_STATE_DIR=out/agent-state TENON_AGENT_USER=agent TENON_AGENT_PASSWORD=agent-dev \
TENON_RESULT_FILE=out/agent-result.jsonl go run ./cmd/agent
```

期望：`out/agent-state/pending/` 出现一份 json；`GET /tenon/pending` 能查到；投一条 confirm 后 `archived/` 出现 `done`，inventory 数量真的变了，`GET /tenon/audit?planId=…` 的 result 段 `approvedBy=user:bob`。

- [ ] **Step 4: Commit**

```bash
git add docs README.md
git commit -m "docs: P2 收口（agent 状态性质、公告板 state、README 已知限制）"
```

---

## 自检结果

| spec 要求 | 落在哪个任务 |
|---|---|
| §3.1 状态目录（与 InboundDir 分开） | Task 1 + Task 10（装配） |
| §3.2 pending 形状与字段语义 | Task 1 |
| §3.3 去重账本 write-ahead | Task 2 |
| §3.4 公告板 `state` 与终态覆盖 | Task 9 |
| §4 I7 三次落盘 + 崩溃分流 | Task 3（`suspend` / `selfCheck`）+ Task 10（每 tick 调用） |
| §5 恢复流程（匹配/审批人/过期/G4/policy/执行/归档） | Task 8 |
| §6.1 tick 四段 | Task 10 |
| §6.2 逐帧 ack 与重试 | Task 10（`drainInbound`） |
| §6.3 事件驱动的 approve 也挂起 | Task 7（`buildPending` 里 `requestId` 可为 null） |
| §7 skill 粗筛 + prompt 分流 | Task 5 + Task 6 |
| §8 登录/单飞重登 | Task 4 |
| §9 降级矩阵 | Task 3/4/8/10 各自的错误分支 |
| §10 I1–I8 | I1/I4 在 Task 8；I2 在 Task 8 的 `writePlanResult`（不重写 dispatched）；I3 在 Task 3 的 `Expired` 与 Task 8；I5 在 Task 9；I7 在 Task 3；I8 由 P1 保证 |
| §12 三处文档修订 | Task 12 |
| Task 0（TEST_MODE 关入站） | Task 11 |

**类型一致性**：`Pending` / `StateStore`（Task 1）在 Task 3/7/8/9/10 复用；`Ledger`（Task 2）只在 Task 10 用；`HostClient`（Task 4）在 Task 9/10 用；`SelectSkills`（Task 5）在 Task 10 的 request 分支还需接进 `Run`——**Task 10 的 `handleInbound` 里 `agent.Run` 之前要先把 `deps.Skills` 的候选集算好塞进 `RunInput`**（`Run` 内部按 `Request != nil` 走 `SelectSkills`）。实现时若发现 `Run` 里没有这个分支，在 Task 10 一并补上并在汇报里说明。

**未覆盖（属 P3/切片 2/切片 3）**：前端页面、撤销与补偿、`amend`、WS、`summaryTemplate` 的模块实现（Task 7 用「动作 id + 参数摘要」兜底，已在代码注释里写明这是本计划接受的降级）。
