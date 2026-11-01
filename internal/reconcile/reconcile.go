// Package reconcile 把宿主审计与模块侧审计对起来（P0-2）。
//
// 它不做裁决，只做一件事：指出两侧不一致的地方，并区分「预期内的差」与
// 「有人在绕路」。区分比发现更重要——把正常重试报成篡改，运维三天就不看报告了。
package reconcile

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"tenon.dev/internal/controlplane/audit"
	"tenon.dev/internal/protocol"
)

// 差异类别。前四类是"账对不上"，后两类是"有人动过手"。
const (
	ClassHostOnly        = "host_only"
	ClassHostOnlyUnknown = "host_only_unknown"
	ClassModuleOnly      = "module_only"
	ClassFieldMismatch   = "field_mismatch"
	ClassChainBroken     = "chain_broken"
	ClassLegacyGap       = "legacy_gap"
)

// ModuleSource 是一个待对账的模块。
type ModuleSource struct {
	ID      string
	BaseURL string
}

// Options 是对账的输入。
type Options struct {
	HostAuditPath string
	Modules       []ModuleSource
	HTTP          *http.Client
	Now           func() time.Time
}

// Finding 是一条差异。
type Finding struct {
	Class   string `json:"class"`
	Module  string `json:"module"`
	AuditID string `json:"auditId,omitempty"`
	Action  string `json:"action,omitempty"`
	Detail  string `json:"detail"`
}

// ModuleHead 是被对账模块自报的链位置。
type ModuleHead struct {
	Module   string `json:"module"`
	HeadSeq  int64  `json:"headSeq"`
	HeadHash string `json:"headHash"`
	Legacy   int    `json:"legacyRecords"`
	Error    string `json:"error,omitempty"`
}

// Report 是对账结果。
type Report struct {
	GeneratedAt time.Time      `json:"generatedAt"`
	Heads       []ModuleHead   `json:"heads"`
	Findings    []Finding      `json:"findings"`
	Counts      map[string]int `json:"counts"`
}

// ExitCode 把报告映射成退出码：0 干净，1 只差账，2 有绕过或改账，3 链断了。
func (r Report) ExitCode() int {
	c := r.Counts
	switch {
	case c[ClassChainBroken] > 0:
		return 3
	case c[ClassModuleOnly] > 0 || c[ClassFieldMismatch] > 0:
		return 2
	case c[ClassHostOnly] > 0 || c[ClassHostOnlyUnknown] > 0 || c[ClassLegacyGap] > 0:
		return 1
	}
	return 0
}

type moduleChain struct {
	records []auditRecord
	head    ModuleHead
	err     error
}

// auditRecord 只需要对账用到的那几个字段，不引模块包（避免把模块的运行时拖进来）。
type auditRecord struct {
	Kind           string `json:"kind"`
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotencyKey"`
	ArgsHash       string `json:"argsHash"`
	Result         string `json:"result"`
	AuditID        string `json:"auditId"`
	PlanID         string `json:"planId"`
	StepID         string `json:"stepId"`
	Legacy         bool   `json:"legacy"`
	Hash           string `json:"hash"`
}

// Run 执行一次对账。
func Run(ctx context.Context, opts Options) (Report, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	rep := Report{GeneratedAt: now().UTC(), Counts: map[string]int{}}

	hostRecords, err := readHostAudit(opts.HostAuditPath)
	if err != nil {
		return rep, err
	}
	dispatched, results := indexHost(hostRecords)

	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	for _, src := range opts.Modules {
		ch := fetchChain(ctx, client, src)
		rep.Heads = append(rep.Heads, ch.head)
		if ch.err != nil {
			rep.find(ClassChainBroken, src.ID, "", "", ch.err.Error())
			continue
		}
		byAudit := map[string]auditRecord{}
		legacyByKey := map[string]auditRecord{}
		for _, rec := range ch.records {
			if rec.Kind != "action" {
				continue
			}
			if rec.AuditID != "" {
				byAudit[rec.AuditID] = rec
			} else {
				legacyByKey[rec.IdempotencyKey] = rec
			}
		}

		seen := map[string]bool{}
		for id, rec := range byAudit {
			seen[id] = true
			host, ok := dispatched[id]
			if !ok {
				rep.find(ClassModuleOnly, src.ID, id, rec.Action,
					"模块有记录、宿主没有：这笔写没有经过宿主裁决")
				continue
			}
			if host.InputHash != "" && host.InputHash != rec.ArgsHash {
				rep.find(ClassFieldMismatch, src.ID, id, rec.Action,
					fmt.Sprintf("入参摘要不一致：宿主 %s，模块 %s", host.InputHash, rec.ArgsHash))
			}
			if res, ok := results[id]; ok && res.Result != nil {
				if got := resultStatus(res.Result); got != "" && got != rec.Result {
					rep.find(ClassFieldMismatch, src.ID, id, rec.Action,
						fmt.Sprintf("结果不一致：宿主 %s，模块 %s", got, rec.Result))
				}
			}
		}

		for id, host := range dispatched {
			if seen[id] {
				continue
			}
			if _, legacy := legacyByKey[host.IdempotencyKey]; legacy {
				// 升级前的账：模块侧没有 auditId，按幂等键能对上就不追究。
				rep.find(ClassLegacyGap, src.ID, id, host.Action,
					"模块侧是升级前的老记录（无 auditId），按幂等键匹配")
				continue
			}
			if res, ok := results[id]; !ok || len(res.Result) == 0 {
				rep.find(ClassHostOnlyUnknown, src.ID, id, host.Action,
					"宿主已签发但查无结果：重试尝试未落地，属预期内")
				continue
			}
			rep.find(ClassHostOnly, src.ID, id, host.Action,
				"宿主有结果、模块无记录：调用未达或模块丢账")
		}
	}

	sort.Slice(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].Module != rep.Findings[j].Module {
			return rep.Findings[i].Module < rep.Findings[j].Module
		}
		return rep.Findings[i].AuditID < rep.Findings[j].AuditID
	})
	return rep, nil
}

func (r *Report) find(class, module, auditID, action, detail string) {
	r.Findings = append(r.Findings, Finding{Class: class, Module: module,
		AuditID: auditID, Action: action, Detail: detail})
	r.Counts[class]++
}

func readHostAudit(path string) ([]audit.AuditRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []audit.AuditRecord
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var rec audit.AuditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("宿主审计第 %d 行不是合法 JSON：%w", len(out)+1, err)
		}
		out = append(out, rec)
	}
	return out, s.Err()
}

func indexHost(recs []audit.AuditRecord) (map[string]audit.AuditRecord, map[string]audit.AuditRecord) {
	dispatched := map[string]audit.AuditRecord{}
	results := map[string]audit.AuditRecord{}
	for _, rec := range recs {
		// 只对账步骤级写：计划级 dispatched 是挂起链路的，不对应副作用。
		if rec.StepID == "" {
			continue
		}
		switch rec.Stage {
		case audit.StageDispatched:
			dispatched[rec.AuditID] = rec
		case audit.StageResult:
			results[rec.AuditID] = rec
		}
	}
	return dispatched, results
}

// resultStatus 从宿主审计的 result 对象里取 ok/error，取不到就返回空（不比较）。
func resultStatus(raw json.RawMessage) string {
	var v struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if v.OK != nil {
		if *v.OK {
			return "ok"
		}
		return "error"
	}
	if v.Error != "" {
		return "error"
	}
	return ""
}

func fetchChain(ctx context.Context, client *http.Client, src ModuleSource) moduleChain {
	out := moduleChain{head: ModuleHead{Module: src.ID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(src.BaseURL, "/")+"/tenon/audit/local?limit=500", nil)
	if err != nil {
		out.err = err
		return out
	}
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	resp, err := client.Do(req)
	if err != nil {
		out.err = fmt.Errorf("取模块审计失败：%w", err)
		return out
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		HeadSeq  int64         `json:"headSeq"`
		HeadHash string        `json:"headHash"`
		Legacy   int           `json:"legacyRecords"`
		Records  []auditRecord `json:"records"`
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			TenonError struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"tenonError"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		out.err = fmt.Errorf("%s：%s", e.TenonError.Code, e.TenonError.Message)
		return out
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		out.err = fmt.Errorf("模块审计响应不是合法 JSON：%w", err)
		return out
	}
	out.head.HeadSeq = body.HeadSeq
	out.head.HeadHash = body.HeadHash
	out.head.Legacy = body.Legacy
	out.records = body.Records
	return out
}
