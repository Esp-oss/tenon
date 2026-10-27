package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pendingTTL 是审批窗口。超时即归档 expired，绝不执行（I3）。
const pendingTTL = 15 * time.Minute

// auditProbe 用于自检：审计链上有没有这条 dispatched 记录。
type auditProbe interface {
	// Has 回答「审计链上有没有这条 dispatched 记录」。
	//
	// 签名里刻意**没有 host**：probe 由谁持有 host 是它的实现细节（真实现是
	// hostAuditProbe，绑 HostClient；测试桩不需要 host）。早先的签名带一个 host 参数、
	// 调用处传空串，等于给「拼出相对 URL」留了个口子——空串在类型上就不该出现。
	Has(ctx context.Context, auditID string) (bool, error)
}

type auditProbeFound struct{}

func (auditProbeFound) Has(context.Context, string) (bool, error) { return true, nil }

type auditProbeNotFound struct{}

func (auditProbeNotFound) Has(context.Context, string) (bool, error) { return false, nil }

// auditProbePage 是翻审计链时的每页条数。取宿主的分页上限（internal/controlplane 的
// maxAuditPage = 500）：超过它的 limit 会被宿主静默截断，不如一开始就按满页取。
const auditProbePage = 500

// hostAuditProbe 是 auditProbe 的真实现：用 HostClient 查宿主审计链。
//
// host 绑在 client 里、不进 Has 的签名（理由见 auditProbe 的注释：早先带一个 host 参数、
// 调用处传空串，等于给「拼出相对 URL」留了个口子）。
type hostAuditProbe struct{ client *HostClient }

// HostAuditProbe 返回「用 HostClient 查审计链」的 probe，供 SelfCheck 的第三个参数使用。
//
// 导出它只是为了装配：auditProbe 是未导出接口、真实现也只能由本包构造——没有这个
// 构造函数，包外即使拿到导出的 SelfCheck 也传不进 probe。
func HostAuditProbe(client *HostClient) auditProbe { return hostAuditProbe{client: client} }

// Has 回答「审计链上有没有这条记录」。
//
// 端点的真实参数只有 planId / since / limit（internal/controlplane/http.go 的
// serveAuditQuery），**没有 auditId 过滤**——所以只能按写入顺序把链翻完再逐条比对：
// since 是写入顺序的 0 基游标，响应里的 nextCursor 就是下一页要传的 since。
//
// 必须翻到底才能回答 false：「前 limit 条里没有」不等于「链上没有」。把「没翻到」当成
// 「不存在」，自检会把一份审计其实齐全的 pending 归档成 stale——那是替人丢掉一张待批件。
func (h hostAuditProbe) Has(ctx context.Context, auditID string) (bool, error) {
	if h.client == nil {
		return false, fmt.Errorf("agent: 审计探测没有宿主客户端（HostClient 为 nil），无法回答 %q 在不在链上", auditID)
	}
	auditID = strings.TrimSpace(auditID)
	if auditID == "" {
		// 空串只能答出「链上有空 auditId 吗」，而那不是自检要问的问题。
		return false, fmt.Errorf("agent: 审计探测缺少 auditId（空串无法判定）")
	}
	since := 0
	for {
		status, raw, err := h.client.do(ctx, http.MethodGet,
			fmt.Sprintf("%s/tenon/audit?since=%d&limit=%d", h.client.base, since, auditProbePage), nil)
		if err != nil {
			return false, fmt.Errorf("查审计失败：%w", err)
		}
		if status != http.StatusOK {
			return false, fmt.Errorf("查审计返回 %d：%s", status, snippet(raw))
		}
		var page struct {
			Records []struct {
				AuditID string `json:"auditId"`
			} `json:"records"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return false, fmt.Errorf("审计查询的响应不是合法 JSON：%w", err)
		}
		for _, rec := range page.Records {
			if rec.AuditID == auditID {
				return true, nil
			}
		}
		next, err := strconv.Atoi(strings.TrimSpace(page.NextCursor))
		if err != nil {
			// 游标读不懂就答不出「翻完了没有」：不能降级成 false（见上面的 stale 风险）。
			return false, fmt.Errorf("审计查询的 nextCursor 不是整数（%q）：%w", page.NextCursor, err)
		}
		if next <= since {
			// 游标不再前进 = 已经翻到底：链上确实没有这条。
			return false, nil
		}
		since = next
	}
}

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
		// effects.entities 必须是数组（audit-event.schema.json#/$defs/AuditEffects 只允许
		// array）：写 null 会让「声明了 0 个实体」与「字段缺失」在哈希链里变成两种字节，
		// 对账侧也读不出本意（真冒烟暴露的 F3）。挂起时计划一步都没执行，确实没有
		// 实体级影响，所以是空数组而不是编一个实体。
		Effects: auditEffects{Entities: []json.RawMessage{}},
		// audit-event.schema.json：stage=dispatched 时 result.status 必须是 "unknown"
		// （「已派出、结果未定」）。写别的值会让这条记录连自己的 schema 都不符合——
		// 宿主今天只校验 auditId 与 stage，但那是宽松，不是许可。
		Result: auditResult{Status: "unknown"},
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

// renderLabels 逐 step 渲染一句白话。
//
// **这是本计划显式接受的降级，不是占位实现**：真正的人话来自模块 manifest 的
// summaryTemplate（P1 已加 schema 字段，模块尚未填），模板实现属于 P3，届时在这里换掉
// 兜底分支、签名与调用方都不动。在那之前只能用「动作 id + 参数摘要」——它不解释业务
// 含义，但保证审批人看得见「要调哪个动作、带什么参数」。
//
// 数字只从 args 取，模型不参与（spec §5 R1–R3）：重跑恢复时若重新渲染，审批人看到的
// 就必须是当初那份参数，而不是模型的第二次猜测。
func renderLabels(plan Plan) ([]string, error) {
	out := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		args := strings.TrimSpace(string(step.Args))
		if args == "" || args == "{}" {
			out = append(out, step.Action)
			continue
		}
		out = append(out, fmt.Sprintf("%s（%s）", step.Action, truncateRunes(args, 80)))
	}
	return out, nil
}

// labelMaxRunes 是单条白话的上限（R4）：与 Intent 帧 badge 的 120 对齐，**含省略号**。
const labelMaxRunes = 120

// renderSteps 逐 step 出一句白话（labels）与一条技术原文（details）。
//
// 规则见 P3 spec §4：模板是主路径，任何一步失败都退 R2（标题 + 参数），绝不输出
// 半个句子；白话里不出现 action id（那是 details 的职责）；数字只抄 args。
func renderSteps(plan Plan, meta map[string]ActionMeta) (labels, details []string) {
	labels = make([]string, 0, len(plan.Steps))
	details = make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		m := meta[step.Action]
		// truncateRunes 会在截断结果后再补一个省略号，所以传 limit-1，
		// 保证最终长度（含「…」）不超过 120。
		labels = append(labels, truncateRunes(renderStepLabel(step, m), labelMaxRunes-1))
		details = append(details, stepDetail(step))
	}
	return labels, details
}

// renderStepLabel 先试模板，失败退 R2。
func renderStepLabel(step PlanStep, m ActionMeta) string {
	args, ok := parseArgsObject(step.Args)
	if ok && strings.TrimSpace(m.SummaryTemplate) != "" {
		if out, done := fillTemplate(m.SummaryTemplate, args); done {
			return out
		}
	}
	return fallbackLabel(m.Title, args, ok)
}

// fillTemplate 只在**所有**占位符都能取到标量值时才返回 done=true：
// 少一个就整句作废，避免「缺 件 STD-1002」这种半个句子。
func fillTemplate(tpl string, args map[string]any) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(tpl); i++ {
		if tpl[i] != '{' {
			b.WriteByte(tpl[i])
			continue
		}
		rel := strings.IndexByte(tpl[i+1:], '}')
		if rel < 0 {
			return "", false
		}
		name := tpl[i+1 : i+1+rel]
		raw, ok := args[name]
		if !ok {
			return "", false
		}
		s, ok := scalarText(raw)
		if !ok {
			return "", false
		}
		b.WriteString(s)
		i += rel + 1
	}
	return b.String(), true
}

// scalarText 只接受字符串/数字/布尔：数组与对象渲染不出人话（R1），一律判失败。
func scalarText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case json.Number:
		return x.String(), true
	default:
		return "", false
	}
}

// fallbackLabel 是 R2：标题 + 关键参数；标题也缺时给一句可读的占位话，不泄露 action id。
func fallbackLabel(title string, args map[string]any, hasArgs bool) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "未命名动作（参数见详情）"
	}
	if !hasArgs || len(args) == 0 {
		return title
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if s, ok := scalarText(args[k]); ok {
			parts = append(parts, k+"="+s)
		}
	}
	if len(parts) == 0 {
		return title
	}
	return title + "（" + strings.Join(parts, ", ") + "）"
}

// stepDetail 是折叠区用的技术原文：action id + args（截断，避免整卡被一份 args 淹没）。
func stepDetail(step PlanStep) string {
	args := strings.TrimSpace(string(step.Args))
	if args == "" || args == "{}" || args == "null" {
		return step.Action
	}
	return fmt.Sprintf("%s（%s）", step.Action, truncateRunes(args, 120))
}

// parseArgsObject 解出 args 对象；空、非法、非对象一律返回 ok=false（走 R2）。
func parseArgsObject(raw json.RawMessage) (map[string]any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}

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
				found, perr := probe.Has(ctx, p.DispatchedAuditID)
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

// SelfCheck 是 selfCheck 的导出包装：cmd/agent 的 tick 第二段（过期清理与自检）用它，
// 时间取当前时刻（时钟在包装里取，调用方不必也不再自己传）。
//
// 返回的 selfCheckReport 是未导出类型，但**字段全部导出**：包外调用方用
// rep.Kept / rep.Expired / rep.Stale / rep.Settled / rep.Corrupt 读结果即可，不需要
// （也没法）给这个类型命名。Corrupt 必须原样带出去——坏文件对 List() 不可见，它是
// 「盘上有读不了的 pending」唯一的告警出口。
func SelfCheck(ctx context.Context, store *StateStore, probe auditProbe) (selfCheckReport, error) {
	return selfCheck(ctx, store, probe)(time.Now().UTC())
}

// postJSONWith 发一个 JSON POST，返回状态码与响应体。
//
// 包级的对应物：runner.postJSON 挂在回合实例上（带协议版本头与注入的 client），
// 挂起/自检/宿主客户端这些非回合路径需要一个不依赖 runner 的版本。
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
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, out, err
}
