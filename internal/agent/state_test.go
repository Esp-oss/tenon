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
