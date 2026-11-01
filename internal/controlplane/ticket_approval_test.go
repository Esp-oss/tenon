package controlplane

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tenon.dev/internal/protocol"
)

// approveArgs 是"人批准的那一版"参数的原文；票里的 ah 必须与它逐字节一致。
const approveArgs = `{"sku":"STD-1001","qty":12}`

func upsertPending(t *testing.T, f ticketFixture, planID, argsHash string) {
	t.Helper()
	body := fmt.Sprintf(`{"planId":%q,"summary":"补货 12 件","requiredRoles":["role:warehouse_manager"],"expiresAt":%q,"steps":[{"stepId":"s1","action":"inventory.DeductStock","argsHash":%q}]}`,
		planID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339), argsHash)
	if status, raw := postAuth(t, f.ts.URL+"/tenon/pending", f.token, body); status != 200 {
		t.Fatalf("推送公告板失败 %d：%s", status, raw)
	}
}

func issueBody(planID, args string) string {
	return fmt.Sprintf(`{"planId":%q,"stepId":"s1","action":"inventory.DeductStock","args":%s,"agent":"ag_fulfillment","onBehalfOf":"user:alice"}`,
		planID, args)
}

// 批准过的参数被换掉：必须拒，且错误码要能与"没批过"区分开。
func TestApproveBindsFrozenArgs(t *testing.T) {
	f := newTicketFixture(t)
	upsertPending(t, f, "pln_appr", protocol.ArgsHash([]byte(approveArgs)))

	if status, raw := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token, issueBody("pln_appr", approveArgs)); status != 200 {
		t.Fatalf("与批准参数一致时应签票，得到 %d：%s", status, raw)
	}

	status, raw := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token,
		issueBody("pln_appr", `{"sku":"STD-1001","qty":800}`))
	if status != 403 {
		t.Fatalf("换参数应 403，得到 %d：%s", status, raw)
	}
	if !strings.Contains(raw, protocol.CodeTicketNotApproved) {
		t.Fatalf("应报 %s（而不是 APPROVAL_REQUIRED），实际：%s", protocol.CodeTicketNotApproved, raw)
	}
}

// 快照只认第一次：批准后再推一版，不能把票绑到新参数上。
func TestSnapshotDoesNotMoveAfterRepublish(t *testing.T) {
	f := newTicketFixture(t)
	upsertPending(t, f, "pln_repub", protocol.ArgsHash([]byte(approveArgs)))

	// agent 重推一版，把摘要换成 800 件。
	upsertPending(t, f, "pln_repub", protocol.ArgsHash([]byte(`{"sku":"STD-1001","qty":800}`)))

	status, raw := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token,
		issueBody("pln_repub", `{"sku":"STD-1001","qty":800}`))
	if status != 403 || !strings.Contains(raw, protocol.CodeTicketNotApproved) {
		t.Fatalf("重推不应改变已冻结的快照，得到 %d：%s", status, raw)
	}
}

// 自动档不需要快照：它本来就不经过人，要求快照会把自动档整个关掉。
func TestAutoPathNeedsNoSnapshot(t *testing.T) {
	f := newTicketFixture(t)
	status, raw := postAuth(t, f.ts.URL+"/tenon/tickets:issue", f.token,
		issueBody("pln_auto", `{"sku":"STD-1001","qty":3}`))
	if status != 200 {
		t.Fatalf("自动档应无需快照即签票，得到 %d：%s", status, raw)
	}
}
