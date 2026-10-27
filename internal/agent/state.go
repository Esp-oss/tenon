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
			continue // 坏文件不让整个列表失败；CountCorrupt 会单独把它数出来供自检告警
		}
		out = append(out, p)
	}
	return out, nil
}
