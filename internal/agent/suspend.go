package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
