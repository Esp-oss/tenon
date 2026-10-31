package conformance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tenon.dev/internal/protocol"
)

// 这条用例只做"负向"：证明没有票就进不去。
//
// 正向（签一张合法票并执行）需要宿主私钥，一致性套件不该、也不能持有它——
// 所以正向留在宿主自己的集成测试里，这里只回答"门到底有没有装"。
func checkTicketEnforcement(ctx context.Context, client *http.Client, target string,
	manifest *Manifest, fix *ModuleFixture) []Result {

	res := Result{Case: "C11", Title: "执行票据：无票与伪造票都必须被拒"}
	if manifest == nil {
		res.Status = Skip
		res.Detail = "C1 未取到 manifest，无法判定动作"
		return []Result{res}
	}
	if !hasCapability(manifest, "ticket") {
		// warn 模式的模块会真的执行无票请求：套件不能拿人家的数据去"验"这个。
		res.Status = Skip
		res.Detail = "模块未声明 ticket 能力（TENON_TICKET_MODE 不是 enforce），跳过以免产生副作用"
		return []Result{res}
	}

	action := firstWriteAction(manifest)
	if action == "" {
		res.Status = Skip
		res.Detail = "manifest 里没有写动作可测"
		return []Result{res}
	}
	args := argsFor(fix, action)
	if args == nil {
		args = json.RawMessage(`{}`)
	}

	// 1) 无票
	status, body, err := postAction(ctx, client, target, action, args, nil)
	if err != nil {
		res.Status = Fail
		res.Detail = "无票请求发送失败：" + err.Error()
		return []Result{res}
	}
	if status != http.StatusForbidden || !strings.Contains(body, protocol.CodeTicketMissing) {
		res.Status = Fail
		res.Detail = fmt.Sprintf("无票应 403 %s，实际 %d：%s", protocol.CodeTicketMissing, status, clip([]byte(body)))
		return []Result{res}
	}

	// 2) 结构非法的票
	status, body, err = postAction(ctx, client, target, action, args,
		map[string]string{protocol.HeaderTicket: "v1.bm90LWEtdGlja2V0.bm90LWEtc2ln"})
	if err != nil {
		res.Status = Fail
		res.Detail = "伪造票请求发送失败：" + err.Error()
		return []Result{res}
	}
	if status != http.StatusForbidden ||
		(!strings.Contains(body, protocol.CodeTicketInvalid) && !strings.Contains(body, protocol.CodeTicketUnknownKey)) {
		res.Status = Fail
		res.Detail = fmt.Sprintf("伪造票应 403 TICKET_INVALID/UNKNOWN_KEY，实际 %d：%s", status, clip([]byte(body)))
		return []Result{res}
	}

	// 3) 用一把宿主不认识的密钥签的票：签名本身是合法的，只是 kid 不在信任集里。
	if forged := signWithRandomKey(action, args); forged != "" {
		status, body, err = postAction(ctx, client, target, action, args,
			map[string]string{protocol.HeaderTicket: forged})
		if err != nil {
			res.Status = Fail
			res.Detail = "陌生密钥票发送失败：" + err.Error()
			return []Result{res}
		}
		if status != http.StatusForbidden ||
			(!strings.Contains(body, protocol.CodeTicketUnknownKey) && !strings.Contains(body, protocol.CodeTicketInvalid)) {
			res.Status = Fail
			res.Detail = fmt.Sprintf("陌生密钥签的票必须被拒，实际 %d：%s", status, clip([]byte(body)))
			return []Result{res}
		}
	}

	res.Status = Pass
	res.Detail = fmt.Sprintf("动作 %s：无票与两种伪造票均 403", action)
	return []Result{res}
}

// 只验"读端点存在且形状对"：链的内容校验是模块自己的事（C4 已覆盖两段写与链序）。
func checkAuditChain(ctx context.Context, client *http.Client, target string, manifest *Manifest) Result {
	res := Result{Case: "C12", Title: "模块审计链可读且只读"}
	if manifest == nil {
		res.Status = Skip
		res.Detail = "C1 未取到 manifest"
		return res
	}
	raw, status, err := doJSON(ctx, client, http.MethodGet, target+"/tenon/audit/local?limit=5", nil, nil)
	if err != nil {
		res.Status = Fail
		res.Detail = "请求失败：" + err.Error()
		return res
	}
	if status == http.StatusNotFound {
		// 0.1 的模块本来就没有这个端点：这不是缺陷，是版本差异。
		res.Status = Skip
		res.Detail = "模块未提供 0.2.0 的 /tenon/audit/local"
		return res
	}
	if status != http.StatusOK {
		res.Status = Fail
		res.Detail = fmt.Sprintf("GET /tenon/audit/local 返回 %d（期望 200）：%s", status, clip(raw))
		return res
	}
	var got struct {
		HeadSeq  int64 `json:"headSeq"`
		HeadHash string
		Legacy   int `json:"legacyRecords"`
		Records  []struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		res.Status = Fail
		res.Detail = fmt.Sprintf("响应不是 {headSeq,headHash,records}：%v（%s）", err, clip(raw))
		return res
	}
	prev := int64(-1)
	for _, rec := range got.Records {
		if rec.Seq <= prev {
			res.Status = Fail
			res.Detail = fmt.Sprintf("记录 seq 未递增：%d 出现在 %d 之后", rec.Seq, prev)
			return res
		}
		prev = rec.Seq
	}
	if got.HeadSeq > 0 && got.HeadHash == "" {
		res.Status = Fail
		res.Detail = "有记录却没有 headHash：对账方无法锚定"
		return res
	}

	// 只读：写方法必须 405，否则"对账凭证"可以被对账方自己改。
	_, status, _ = doJSON(ctx, client, http.MethodPost, target+"/tenon/audit/local", []byte(`{}`), nil)
	if status != http.StatusMethodNotAllowed {
		res.Status = Fail
		res.Detail = fmt.Sprintf("POST /tenon/audit/local 应 405，实际 %d", status)
		return res
	}

	res.Status = Pass
	res.Detail = fmt.Sprintf("headSeq=%d legacy=%d 记录 %d 条，只读成立", got.HeadSeq, got.Legacy, len(got.Records))
	return res
}

func hasCapability(m *Manifest, cap string) bool {
	for _, c := range m.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func firstWriteAction(m *Manifest) string {
	for _, a := range m.Actions {
		if a.Kind == "write" {
			return a.ID
		}
	}
	return ""
}

func argsFor(fix *ModuleFixture, action string) json.RawMessage {
	if fix == nil {
		return nil
	}
	for _, inv := range fix.Invocations {
		if inv.Action == action {
			return inv.Args
		}
	}
	return nil
}

func postAction(ctx context.Context, client *http.Client, target, action string,
	args json.RawMessage, headers map[string]string) (int, string, error) {

	h := map[string]string{protocol.HeaderIdempotencyKey: "tenon:conf-c11/s1"}
	for k, v := range headers {
		h[k] = v
	}
	raw, status, err := doJSON(ctx, client, http.MethodPost, target+"/tenon/actions/"+action, args, h)
	return status, string(raw), err
}

// signWithRandomKey 造一张"签名结构正确、kid 不在信任集"的票。
func signWithRandomKey(action string, args json.RawMessage) string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return ""
	}
	now := time.Now().UTC()
	tk := protocol.Ticket{
		TicketID: "tkt_conf_forged", Module: "unknown", Action: action,
		ArgsHash: protocol.ArgsHash(args), IdempotencyKey: "tenon:conf-c11/s1",
		PlanID: "pln_conf", StepID: "s1", AuditID: "aud_conf",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(30e9).Unix(),
	}
	token, err := protocol.SignTicket(priv, tk)
	if err != nil {
		return ""
	}
	return token
}
