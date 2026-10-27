// Package conformance 是 Tenon 的可执行规范：把 spec/0.1 的断言做成能对任意实现跑的黑盒测试。
//
// 模块侧用例：C1（manifest）、C2（幂等）、C3（未知字段）、C8（可选能力容忍）、
// C9（错误分类）、C10（反模式扫描）。
//
// 宿主侧用例：C4（审计两段写 + 哈希链）与 C5（fail-closed）只有在给了宿主地址
// （Options.HostTarget）时才能判定；没有宿主地址就**显式记为 SKIP 并写明原因**，
// 不允许用「没跑就是通过」蒙过去——见 Report.ExitCode 与 --strict 的语义。
// D1 差异测试需要会推理的规划器（LLM），本包永远记 SKIP。
package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tenon.dev/internal/spec"
	"tenon.dev/tools/lint"
)

// 以下取值都是 spec 词表，不是某个实现的内部常量：conformance 是黑盒套件（README 的
// 「职责边界」），断言只能建立在规范文本上。若改成 import 宿主包里的同名常量，
// 「规范」就跟着实现漂——实现改常量等于改判据，第三方自证合规也就没有意义了。
const (
	// stageDispatched / stageResult 是审计两段写的两段（interfaces.md §4.3）。
	stageDispatched = "dispatched"
	stageResult     = "result"
	// outcomeDeny 是策略处置的拒绝态（policy.schema.json#/$defs/PolicyOutcome）。
	outcomeDeny = "deny"
	// reasonNoMatch 是「无任何策略命中 → 默认拒绝」的理由码
	// （policy.schema.json#/$defs/PolicyEvaluationResponse 的示例值）。
	reasonNoMatch = "NO_POLICY_MATCH_DEFAULT_DENY"
	// hashScheme 是审计链摘要的前缀（common.schema.json#/$defs/Sha256Hash）。
	hashScheme = "sha256:"
)

// Status 是一条用例的结论。SKIP 不等于 PASS：它表示「这条没资格下结论」。
type Status string

const (
	// Pass 表示断言成立。
	Pass Status = "pass"
	// Fail 表示断言不成立。
	Fail Status = "fail"
	// Skip 表示本轮无法判定（缺依赖、缺场景数据、或需要未实现的能力）。
	Skip Status = "skip"
)

// Result 是单条用例的结论与证据。
type Result struct {
	Case     string   `json:"case"`
	Title    string   `json:"title"`
	Status   Status   `json:"status"`
	Detail   string   `json:"detail,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// Report 是一次运行的完整结果。
type Report struct {
	TenonVersion string    `json:"tenonVersion"`
	Target       string    `json:"target"`
	Module       string    `json:"module,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	Results      []Result  `json:"results"`
}

// Counts 返回 pass/fail/skip 计数。
func (r Report) Counts() (pass, fail, skip int) {
	for _, x := range r.Results {
		switch x.Status {
		case Pass:
			pass++
		case Fail:
			fail++
		case Skip:
			skip++
		}
	}
	return
}

// ExitCode 是 CLI 的退出码：有 FAIL 即 1；SKIP 默认不算失败（由 --strict 决定）。
func (r Report) ExitCode(strict bool) int {
	_, fail, skip := r.Counts()
	if fail > 0 {
		return 1
	}
	if strict && skip > 0 {
		return 2
	}
	return 0
}

// Options 控制一轮运行。
type Options struct {
	// Target 是被测模块的基地址，例如 http://localhost:8081。
	Target string
	// Spec 是已加载的 schema 集合，用于 C1 的结构校验。
	Spec *spec.Set
	// LintRoot 是 C10 扫描的根目录；为空则 C10 记为 SKIP。
	LintRoot string
	// Fixtures 提供需要真实业务参数才能跑的场景（C2/C3/C9）；为 nil 时这些用例 SKIP。
	Fixtures *Fixtures
	// Client 可注入（测试用），默认 10s 超时。
	Client *http.Client
	// ModuleID 用于挑选 Fixtures 里的模块；空则用 Target 对应唯一模块。
	ModuleID string
	// HostTarget 是被测宿主的基地址（控制平面端点：policies:evaluate / grants:evaluate /
	// audit），例如 http://localhost:8080。
	//
	// 为空时 C4/C5 记 SKIP：模块模式下这两条在本轮**没有判定**，报告必须能区分
	// 「判过且成立」与「没判」。非空但不可达则判 FAIL——你要求测宿主，宿主没应答就是失败。
	HostTarget string
}

// Invocation 是一条带真实参数的调用场景。
type Invocation struct {
	Name   string          `json:"name"`
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args"`
	Expect Expectation     `json:"expect"`
}

// Expectation 是这条调用应当满足的结果。
type Expectation struct {
	Status     int    `json:"status,omitempty"`
	ErrorClass string `json:"errorClass,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty"`
	Idempotent bool   `json:"idempotent,omitempty"`
	UnknownKey bool   `json:"unknownFieldTolerated,omitempty"`
}

// ModuleFixture 是一个模块的场景包。
type ModuleFixture struct {
	Module      string       `json:"module"`
	Invocations []Invocation `json:"invocations"`
}

// Fixtures 是场景包集合。
type Fixtures struct {
	Modules []ModuleFixture `json:"modules"`
}

// Pick 按模块 id 取场景包。
func (f *Fixtures) Pick(module string) *ModuleFixture {
	if f == nil {
		return nil
	}
	for i := range f.Modules {
		if f.Modules[i].Module == module {
			return &f.Modules[i]
		}
	}
	return nil
}

// Run 执行一轮一致性测试。
func Run(ctx context.Context, opts Options) Report {
	started := time.Now()
	rep := Report{TenonVersion: "0.1", Target: opts.Target, Module: opts.ModuleID, StartedAt: started}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	manifest, raws, c1 := checkManifest(ctx, client, opts)
	rep.Results = append(rep.Results, c1)

	fix := opts.Fixtures.Pick(opts.ModuleID)
	rep.Results = append(rep.Results, checkIdempotency(ctx, client, opts.Target, fix)...)
	rep.Results = append(rep.Results, checkUnknownField(ctx, client, opts.Target, fix)...)
	rep.Results = append(rep.Results, checkErrorClass(ctx, client, opts.Target, fix)...)
	rep.Results = append(rep.Results, checkDryRunOptional(ctx, client, opts.Target, manifest, raws)...)
	rep.Results = append(rep.Results, checkLint(opts.LintRoot))
	rep.Results = append(rep.Results, hostSideChecks(ctx, client, opts.HostTarget)...)

	rep.FinishedAt = time.Now()
	return rep
}

// --- C1 manifest ---

func checkManifest(ctx context.Context, client *http.Client, opts Options) (manifest *Manifest, raws map[string]json.RawMessage, res Result) {
	res = Result{Case: "C1", Title: "manifest 合法且摘要不含 inputSchema"}

	body, status, err := doJSON(ctx, client, http.MethodGet, opts.Target+"/.well-known/tenon", nil, nil)
	if err != nil {
		res.Status = Fail
		res.Detail = fmt.Sprintf("请求 manifest 失败：%v", err)
		return nil, nil, res
	}
	if status != http.StatusOK {
		res.Status = Fail
		res.Detail = fmt.Sprintf("manifest 状态码 %d，期望 200", status)
		return nil, nil, res
	}
	res.Evidence = append(res.Evidence, fmt.Sprintf("GET /.well-known/tenon → %d, %d bytes", status, len(body)))

	if opts.Spec != nil {
		if err := opts.Spec.Validate("module-manifest.schema.json", body); err != nil {
			res.Status = Fail
			res.Detail = "manifest 不符合 schema: " + err.Error()
			return nil, nil, res
		}
		res.Evidence = append(res.Evidence, "schema 校验：module-manifest.schema.json OK")
	} else {
		res.Status = Fail
		res.Detail = "调用方未提供 schema 集合（Options.Spec 为空），无法判定 C1"
		return nil, nil, res
	}

	if bytes.Contains(body, []byte(`"inputSchema"`)) {
		res.Status = Fail
		res.Detail = "manifest 摘要里出现了 inputSchema——必须按 id 按需拉取，否则上下文成本不可控"
		return nil, nil, res
	}
	res.Evidence = append(res.Evidence, "摘要未含 inputSchema")

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		res.Status = Fail
		res.Detail = "manifest 反序列化失败：" + err.Error()
		return nil, nil, res
	}

	// 每个 write action 的完整定义必须有 idempotency；read 必须没有。
	raws = map[string]json.RawMessage{}
	for _, a := range m.Actions {
		full, st, err := doJSON(ctx, client, http.MethodGet, opts.Target+"/tenon/actions/"+a.ID, nil, nil)
		if err != nil || st != http.StatusOK {
			res.Status = Fail
			res.Detail = fmt.Sprintf("拉取 action %s 失败（status=%d, err=%v）", a.ID, st, err)
			return nil, nil, res
		}
		raws[a.ID] = full
		if opts.Spec != nil {
			if err := opts.Spec.Validate("action.schema.json", full); err != nil {
				res.Status = Fail
				res.Detail = fmt.Sprintf("action %s 不符合 action.schema.json: %v", a.ID, err)
				return nil, nil, res
			}
		}
		var probe struct {
			Kind        string          `json:"kind"`
			Idempotency json.RawMessage `json:"idempotency"`
		}
		_ = json.Unmarshal(full, &probe)
		hasIdem := len(probe.Idempotency) > 0 && string(probe.Idempotency) != "null"
		if probe.Kind == "write" && !hasIdem {
			res.Status = Fail
			res.Detail = fmt.Sprintf("action %s 是 write 但未声明 idempotency", a.ID)
			return nil, nil, res
		}
		if probe.Kind == "read" && hasIdem {
			res.Status = Fail
			res.Detail = fmt.Sprintf("action %s 是 read 却声明了 idempotency", a.ID)
			return nil, nil, res
		}
	}
	res.Evidence = append(res.Evidence, fmt.Sprintf("逐个校验 %d 个 action 的读写分类与幂等声明", len(m.Actions)))
	res.Status = Pass
	return &m, raws, res
}

// --- 场景驱动用例（需要 fixtures）---

func checkIdempotency(ctx context.Context, client *http.Client, target string, fix *ModuleFixture) []Result {
	res := Result{Case: "C2", Title: "幂等：同 Idempotency-Key 提交两次只产生一次副作用"}
	if fix == nil {
		res.Status = Skip
		res.Detail = "没有该模块的 fixtures，无法构造合法业务参数"
		return []Result{res}
	}
	var inv *Invocation
	for i := range fix.Invocations {
		if fix.Invocations[i].Expect.Idempotent {
			inv = &fix.Invocations[i]
			break
		}
	}
	if inv == nil {
		res.Status = Skip
		res.Detail = "fixtures 里没有标记 idempotent 的调用"
		return []Result{res}
	}
	key := "tenon:conformance/" + inv.Name
	h := map[string]string{"Idempotency-Key": key}
	b1, s1, e1 := doJSON(ctx, client, http.MethodPost, target+"/tenon/actions/"+inv.Action, inv.Args, h)
	b2, s2, e2 := doJSON(ctx, client, http.MethodPost, target+"/tenon/actions/"+inv.Action, inv.Args, h)
	if e1 != nil || e2 != nil {
		res.Status = Fail
		res.Detail = fmt.Sprintf("请求失败：%v / %v", e1, e2)
		return []Result{res}
	}
	if s1 != s2 || !bytes.Equal(bytes.TrimSpace(b1), bytes.TrimSpace(b2)) {
		res.Status = Fail
		res.Detail = fmt.Sprintf("两次响应不一致：status %d/%d，body %q/%q", s1, s2, clip(b1), clip(b2))
		return []Result{res}
	}
	res.Evidence = append(res.Evidence, fmt.Sprintf("%s 两次均 %d，响应体一致", inv.Action, s1))
	res.Status = Pass
	return []Result{res}
}

func checkUnknownField(ctx context.Context, client *http.Client, target string, fix *ModuleFixture) []Result {
	res := Result{Case: "C3", Title: "未知字段必须被忽略（前向兼容）"}
	if fix == nil {
		res.Status = Skip
		res.Detail = "没有该模块的 fixtures"
		return []Result{res}
	}
	var inv *Invocation
	for i := range fix.Invocations {
		if fix.Invocations[i].Expect.UnknownKey {
			inv = &fix.Invocations[i]
			break
		}
	}
	if inv == nil {
		res.Status = Skip
		res.Detail = "fixtures 里没有标记 unknownFieldTolerated 的调用"
		return []Result{res}
	}
	var obj map[string]any
	_ = json.Unmarshal(inv.Args, &obj)
	obj["totallyUnknownFieldFromConformance"] = map[string]any{"nested": true}
	patched, _ := json.Marshal(obj)
	h := map[string]string{"Idempotency-Key": "tenon:conformance/unknown-field/" + inv.Name}
	body, status, err := doJSON(ctx, client, http.MethodPost, target+"/tenon/actions/"+inv.Action, patched, h)
	if err != nil {
		res.Status = Fail
		res.Detail = "请求失败：" + err.Error()
		return []Result{res}
	}
	if status == http.StatusBadRequest {
		res.Status = Fail
		res.Detail = fmt.Sprintf("注入未知字段后返回 400（%s）——实现用了严格字段校验，违反「只增不删、未知字段忽略」", clip(body))
		return []Result{res}
	}
	res.Evidence = append(res.Evidence, fmt.Sprintf("注入未知字段后 status=%d（非 400）", status))
	res.Status = Pass
	return []Result{res}
}

func checkErrorClass(ctx context.Context, client *http.Client, target string, fix *ModuleFixture) []Result {
	res := Result{Case: "C9", Title: "错误分类：class/code/retryable 与状态码一致"}
	if fix == nil {
		res.Status = Skip
		res.Detail = "没有该模块的 fixtures"
		return []Result{res}
	}
	var out []Result
	checked := 0
	for _, inv := range fix.Invocations {
		if inv.Expect.ErrorClass == "" {
			continue
		}
		checked++
		h := map[string]string{"Idempotency-Key": "tenon:conformance/err/" + inv.Name}
		body, status, err := doJSON(ctx, client, http.MethodPost, target+"/tenon/actions/"+inv.Action, inv.Args, h)
		r := Result{Case: "C9." + inv.Name, Title: "错误分类 " + inv.Action}
		if err != nil {
			r.Status = Fail
			r.Detail = "请求失败：" + err.Error()
			out = append(out, r)
			continue
		}
		var env struct {
			TenonError struct {
				Class     string `json:"class"`
				Code      string `json:"code"`
				Retryable *bool  `json:"retryable"`
			} `json:"tenonError"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			r.Status = Fail
			r.Detail = fmt.Sprintf("响应不是 tenonError 信封（status=%d）：%s", status, clip(body))
			out = append(out, r)
			continue
		}
		if env.TenonError.Class != inv.Expect.ErrorClass {
			r.Status = Fail
			r.Detail = fmt.Sprintf("class=%q，期望 %q", env.TenonError.Class, inv.Expect.ErrorClass)
			out = append(out, r)
			continue
		}
		if inv.Expect.ErrorCode != "" && env.TenonError.Code != inv.Expect.ErrorCode {
			r.Status = Fail
			r.Detail = fmt.Sprintf("code=%q，期望 %q", env.TenonError.Code, inv.Expect.ErrorCode)
			out = append(out, r)
			continue
		}
		wantRetryable := inv.Expect.ErrorClass == "retryable"
		if env.TenonError.Retryable == nil || *env.TenonError.Retryable != wantRetryable {
			r.Status = Fail
			r.Detail = fmt.Sprintf("retryable 与 class 不匹配（class=%s）", inv.Expect.ErrorClass)
			out = append(out, r)
			continue
		}
		if inv.Expect.Status != 0 && status != inv.Expect.Status {
			r.Status = Fail
			r.Detail = fmt.Sprintf("HTTP %d，期望 %d", status, inv.Expect.Status)
			out = append(out, r)
			continue
		}
		r.Status = Pass
		r.Evidence = append(r.Evidence, fmt.Sprintf("%s → %d %s/%s", inv.Action, status, env.TenonError.Class, env.TenonError.Code))
		out = append(out, r)
	}
	if checked == 0 {
		res.Status = Skip
		res.Detail = "fixtures 里没有声明期望错误的调用"
		return []Result{res}
	}
	return out
}

func checkDryRunOptional(ctx context.Context, client *http.Client, target string, m *Manifest, raws map[string]json.RawMessage) []Result {
	res := Result{Case: "C8", Title: "可选能力容忍：不支持 dry-run 必须优雅降级"}
	if m == nil {
		res.Status = Skip
		res.Detail = "C1 未通过，无法判断 dry-run 能力"
		return []Result{res}
	}
	checked := 0
	for _, a := range m.Actions {
		full, ok := raws[a.ID]
		if !ok {
			continue
		}
		var probe struct {
			DryRun struct {
				Supported bool `json:"supported"`
			} `json:"dryRun"`
		}
		_ = json.Unmarshal(full, &probe)
		if probe.DryRun.Supported {
			continue
		}
		checked++
		body, status, err := doJSON(ctx, client, http.MethodPost, target+"/tenon/actions/"+a.ID+":dry-run", json.RawMessage(`{}`), nil)
		if err != nil {
			res.Status = Fail
			res.Detail = fmt.Sprintf("%s 的 dry-run 请求失败：%v", a.ID, err)
			return []Result{res}
		}
		// 契约：不支持的 dry-run 必须以 404/501 表示「能力不存在」，不能用 5xx 或 200 假装。
		if status != http.StatusNotFound && status != http.StatusNotImplemented {
			res.Status = Fail
			res.Detail = fmt.Sprintf("%s 声明 dryRun.supported=false，但 :dry-run 返回 %d（应 404/501）：%s", a.ID, status, clip(body))
			return []Result{res}
		}
	}
	if checked == 0 {
		res.Status = Skip
		res.Detail = "所有 action 都声明支持 dry-run，没有可验证的降级路径"
		return []Result{res}
	}
	res.Status = Pass
	res.Evidence = append(res.Evidence, fmt.Sprintf("%d 个不支持 dry-run 的 action 均以 404/501 回应", checked))
	return []Result{res}
}

func checkLint(root string) Result {
	res := Result{Case: "C10", Title: "反模式扫描：无事件/错误码 → action 的硬编码映射"}
	if root == "" {
		res.Status = Skip
		res.Detail = "未指定 --source-dir"
		return res
	}
	findings, err := lint.ScanTree(root)
	if err != nil {
		res.Status = Fail
		res.Detail = "扫描失败：" + err.Error()
		return res
	}
	var live []lint.Finding
	for _, f := range findings {
		if !f.Exempt {
			live = append(live, f)
		}
	}
	if len(live) > 0 {
		res.Status = Fail
		var b strings.Builder
		for i, f := range live {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "%s:%d %s %s; ", f.File, f.Line, f.Rule, f.Message)
		}
		res.Detail = fmt.Sprintf("%d 处非豁免命中：%s", len(live), b.String())
		return res
	}
	res.Status = Pass
	res.Evidence = append(res.Evidence, fmt.Sprintf("扫描 %s：0 处非豁免命中", root))
	return res
}

// --- 宿主侧用例（需要 Options.HostTarget）---

// 三条宿主侧用例的固定标题：报告按 case 号取值，标题只作人读。
const (
	auditCaseTitle      = "审计可查（两段写 + 哈希链）"
	failClosedCaseTitle = "fail-closed：证据不足时必须 deny"
	diffCaseTitle       = "差异测试：摘掉 procurement 后 Plan 必须改变且不抛错"
)

// hostSideChecks 返回宿主侧三条用例：C4/C5 取决于有没有宿主地址，D1 永远 SKIP。
//
// 没有宿主地址时 C4/C5 记 SKIP 而不是「通过」：模块模式没有宿主侧端点，这两条本轮
// 没有下过任何结论。
func hostSideChecks(ctx context.Context, client *http.Client, host string) []Result {
	host = strings.TrimSpace(host)
	out := make([]Result, 0, 3)
	if host == "" {
		out = append(out,
			Result{Case: "C4", Title: auditCaseTitle, Status: Skip,
				Detail: "未给宿主地址（Options.HostTarget 为空）：模块模式没有宿主侧 GET /tenon/audit 可查，两段写与哈希链本轮未判定。"},
			Result{Case: "C5", Title: failClosedCaseTitle, Status: Skip,
				Detail: "未给宿主地址（Options.HostTarget 为空）：模块模式没有宿主侧 POST /tenon/policies:evaluate 可查，fail-closed 本轮未判定。"},
		)
	} else {
		host = strings.TrimSuffix(host, "/")
		out = append(out, checkAuditTrail(ctx, client, host), checkPolicyFailClosed(ctx, client, host))
	}
	out = append(out, Result{Case: "D1", Title: diffCaseTitle, Status: Skip,
		Detail: "需要会推理的规划器（LLM）：未配置 LLM 时不能声称通过——规则式规划器会让 D1 变成假命题（摘掉模块后 Plan 必然不变）。"})
	return out
}

// checkAuditTrail 是宿主模式下的 C4：两段写各写一条，再从 GET /tenon/audit 查回来验链。
//
// 判据（interfaces.md §4.3）：同一 auditId 的 stage=dispatched 与 stage=result 都在、
// prevHash 串成链且链首为 null、hash 以 sha256: 开头。
//
// 前提：本轮运行期间没有别的写入者。两段之间若有别人插队，result 的 prevHash 会指向插队
// 那条（链本身没断），本用例会把它当失败——并发写入下的链连续性校验是 M1 的事。
func checkAuditTrail(ctx context.Context, client *http.Client, host string) Result {
	res := Result{Case: "C4", Title: auditCaseTitle}
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	auditID := "aud_conformance_" + tag
	planID := "pln_conformance_" + tag

	// inputHash 用真实摘要而不是常量：审计记录若拿假值也能通过，用例就只证明了
	// 「字段存在」，证明不了宿主原样保存了调用方的摘要。
	args := []byte(`{"sku":"STD-1001","qty":2,"referenceNo":"CONF-1"}`)
	common := fmt.Sprintf(`"auditId":%q,"planId":%q,"stepId":"s1","action":"inventory.DeductStock",`+
		`"actor":{"kind":"agent","id":"ag_conformance"},"decision":{"outcome":"auto"},`+
		`"inputHash":%q,"idempotencyKey":"tenon:%s/s1"`,
		auditID, planID, hashScheme+sha256Hex(args), planID)
	segments := []struct {
		stage string
		label string
		extra string
	}{
		{stageDispatched, "第一段（stage=dispatched）", ""},
		{stageResult, "第二段（stage=result）", `,"result":{"status":"succeeded"}`},
	}
	for _, seg := range segments {
		body := []byte(fmt.Sprintf(`{%s,"stage":%q%s}`, common, seg.stage, seg.extra))
		raw, status, err := doJSON(ctx, client, http.MethodPost, host+"/tenon/audit", body, nil)
		if err != nil {
			res.Status = Fail
			res.Detail = fmt.Sprintf("%s审计写入请求失败：%v —— 审计落不了盘时两段写不成立，写操作必须中止", seg.label, err)
			return res
		}
		if status != http.StatusOK {
			res.Status = Fail
			res.Detail = fmt.Sprintf("%s审计写入返回 %d（期望 200）：%s", seg.label, status, clip(raw))
			return res
		}
		res.Evidence = append(res.Evidence, fmt.Sprintf("%s 写入 %d（auditId=%s）", seg.label, status, auditID))
	}

	// 查回来：按 planId 过滤，是 interfaces.md §4.3 给出的查询形态。
	raw, status, err := doJSON(ctx, client, http.MethodGet,
		host+"/tenon/audit?"+url.Values{"planId": {planID}}.Encode(), nil, nil)
	if err != nil {
		res.Status = Fail
		res.Detail = "审计查询请求失败：" + err.Error()
		return res
	}
	if status != http.StatusOK {
		res.Status = Fail
		res.Detail = fmt.Sprintf("GET /tenon/audit?planId=%s 返回 %d（期望 200）：%s", planID, status, clip(raw))
		return res
	}
	var got AuditQueryResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		res.Status = Fail
		res.Detail = fmt.Sprintf("审计查询响应不是 {records,nextCursor}：%v（原文 %s）", err, clip(raw))
		return res
	}

	idxDispatched, idxResult := -1, -1
	for i, rec := range got.Records {
		if rec.AuditID != auditID {
			continue
		}
		switch rec.Stage {
		case stageDispatched:
			if idxDispatched < 0 {
				idxDispatched = i
			}
		case stageResult:
			if idxResult < 0 {
				idxResult = i
			}
		}
	}
	if idxDispatched < 0 {
		res.Status = Fail
		res.Detail = fmt.Sprintf("审计查询缺第一段：GET /tenon/audit?planId=%s 的 %d 条记录里没有 auditId=%s 且 stage=dispatched 的那条",
			planID, len(got.Records), auditID)
		return res
	}
	if idxResult < 0 {
		res.Status = Fail
		res.Detail = fmt.Sprintf("审计查询缺第二段：GET /tenon/audit?planId=%s 里只有 stage=dispatched，没有 auditId=%s 且 stage=result 的那条 —— 两段写只做了一段",
			planID, auditID)
		return res
	}
	if idxResult < idxDispatched {
		res.Status = Fail
		res.Detail = fmt.Sprintf("两段顺序颠倒：stage=result 在第 %d 条、stage=dispatched 在第 %d 条 —— dispatched 必须是先落盘的那条",
			idxResult+1, idxDispatched+1)
		return res
	}
	dispatched, result := got.Records[idxDispatched], got.Records[idxResult]
	if !strings.HasPrefix(dispatched.Hash, hashScheme) || !strings.HasPrefix(result.Hash, hashScheme) {
		res.Status = Fail
		res.Detail = fmt.Sprintf("hash 未按 %s 前缀编码：dispatched=%q result=%q（common.schema.json#/$defs/Sha256Hash）",
			hashScheme, dispatched.Hash, result.Hash)
		return res
	}
	if result.PrevHash == nil || *result.PrevHash != dispatched.Hash {
		res.Status = Fail
		res.Detail = fmt.Sprintf("prevHash 未接上第一段：result.prevHash=%s，dispatched.hash=%s —— 链断在两段之间，审计无法自证未被改动",
			describeHash(result.PrevHash), dispatched.Hash)
		return res
	}

	// 链首：interfaces.md §4.3 要求链首 prevHash 为 null。取全链第一条（limit=1）来断言，
	// 这样宿主上已有历史记录时也能判定「链的起点是 null」，而不是要求本次写入恰好在最前。
	headRaw, headStatus, headErr := doJSON(ctx, client, http.MethodGet, host+"/tenon/audit?limit=1", nil, nil)
	if headErr != nil {
		res.Status = Fail
		res.Detail = "读取审计链首失败：" + headErr.Error()
		return res
	}
	var head AuditQueryResponse
	if headStatus != http.StatusOK || json.Unmarshal(headRaw, &head) != nil || len(head.Records) == 0 {
		res.Status = Fail
		res.Detail = fmt.Sprintf("读取审计链首失败：GET /tenon/audit?limit=1 返回 %d、记录数 %d（原文 %s）",
			headStatus, len(head.Records), clip(headRaw))
		return res
	}
	headRec := head.Records[0]
	if headRec.PrevHash != nil {
		res.Status = Fail
		res.Detail = fmt.Sprintf("链首 prevHash 不是 null（auditId=%s 的 prevHash=%s）：审计链的起点必须为空，否则第一条记录在引用一条不存在的记录",
			headRec.AuditID, describeHash(headRec.PrevHash))
		return res
	}
	// 中段的 prevHash 为 null 等价于「链在这里断成两条」：只有链首可以是 null。
	if dispatched.PrevHash == nil && headRec.AuditID != dispatched.AuditID {
		res.Status = Fail
		res.Detail = fmt.Sprintf("中段的 prevHash 为 null（auditId=%s 不是链首，链首是 %s）：审计链断成两条互不相干的链",
			dispatched.AuditID, headRec.AuditID)
		return res
	}

	res.Evidence = append(res.Evidence,
		fmt.Sprintf("GET /tenon/audit?planId=%s → %d，命中 2 条（该查询共 %d 条）", planID, status, len(got.Records)),
		fmt.Sprintf("两段齐备：dispatched 在第 %d 条、result 在第 %d 条（写入顺序）", idxDispatched+1, idxResult+1),
		fmt.Sprintf("hash 前缀 %s：dispatched=%s result=%s", hashScheme, shortenHash(dispatched.Hash), shortenHash(result.Hash)),
		fmt.Sprintf("prevHash：result.prevHash == dispatched.hash（%s）", shortenHash(dispatched.Hash)),
		fmt.Sprintf("链首（写入顺序第 1 条）prevHash=null：auditId=%s", headRec.AuditID),
	)
	if dispatched.PrevHash == nil {
		res.Evidence = append(res.Evidence, "本次两段是全链最先两条（dispatched.prevHash=null）")
	} else {
		res.Evidence = append(res.Evidence, fmt.Sprintf("本次两段接在历史之后：dispatched.prevHash=%s（宿主审计日志非空）",
			shortenHash(*dispatched.PrevHash)))
	}
	res.Status = Pass
	return res
}

// checkPolicyFailClosed 是宿主模式下的 C5：证据不足时必须 deny，不能因为「没有规则/不认识
// 这个动作」就放行（interfaces.md §4.2：无命中 = deny；semantics.md §12：不可用默认 deny）。
//
// 四个探针的共同点是宿主**不可能**有正当理由放行：
//
//	① 空 plan（零步计划）：没有可执行的步骤，放行等于给「什么都不做」发通行证；
//	② 合法 ActionId 但没有任何规则命中：配置缺口不等于可以执行；
//	③ 未知动作（连 ActionId 词法都不成立）：请求本身不成立时也必须落在拒绝上；
//	④ 语法成立但没有模块声明它的动作：宿主不认识的动作不得被当成可执行。
//
// 断言锁 outcome=deny；①② 额外要求 reasons 里出现 NO_POLICY_MATCH_DEFAULT_DENY（「无命中
// 默认拒绝」的显式证据）。③④ 只锁 outcome：一个会先查注册表的宿主完全可以用别的理由码拒绝，
// 那是更强的实现，不该被判失败。
func checkPolicyFailClosed(ctx context.Context, client *http.Client, host string) Result {
	res := Result{Case: "C5", Title: failClosedCaseTitle}
	probes := []struct {
		label      string
		plan       string
		mustReason string
	}{
		{"① 空 plan（零步）", `{"plan":{}}`, reasonNoMatch},
		{"② 合法 action 但无规则命中",
			`{"plan":{"planId":"pln_conformance_nomatch","steps":[{"stepId":"s1","action":"tenonconformance.ProbeNoRuleMatch","args":{}}]}}`,
			reasonNoMatch},
		{"③ 未知动作（不满足 ActionId 词法）",
			`{"plan":{"planId":"pln_conformance_unknown","steps":[{"stepId":"s1","action":"UnknownAction","args":{}}]}}`,
			""},
		{"④ 未登记模块的动作",
			`{"plan":{"planId":"pln_conformance_ghost","steps":[{"stepId":"s1","action":"ghostmod.GhostAction","args":{}}]}}`,
			""},
	}
	withReason := make([]string, 0, len(probes))
	for _, p := range probes {
		raw, status, err := doJSON(ctx, client, http.MethodPost, host+"/tenon/policies:evaluate", []byte(p.plan), nil)
		if err != nil {
			res.Status = Fail
			res.Detail = fmt.Sprintf("探针 %s 的 policies:evaluate 请求失败：%v —— 策略评估不可达时必须由调用方 fail-closed，本用例却无法确认宿主的判定", p.label, err)
			return res
		}
		if status != http.StatusOK {
			res.Status = Fail
			res.Detail = fmt.Sprintf("探针 %s 返回 %d（期望 200 + outcome=deny）：%s", p.label, status, clip(raw))
			return res
		}
		var ev PolicyEvaluationView
		if err := json.Unmarshal(raw, &ev); err != nil {
			res.Status = Fail
			res.Detail = fmt.Sprintf("探针 %s 的响应不是 PolicyEvaluationResponse：%v（原文 %s）", p.label, err, clip(raw))
			return res
		}
		if ev.Outcome != outcomeDeny {
			res.Status = Fail
			res.Detail = fmt.Sprintf("探针 %s 得到 outcome=%q（decidedBy=%v），期望 deny —— 宿主在证据不足时放行了写操作，这是 fail-open",
				p.label, ev.Outcome, ev.DecidedBy)
			return res
		}
		res.Evidence = append(res.Evidence, fmt.Sprintf("%s → 200 outcome=deny decidedBy=%v reasons=%v",
			p.label, ev.DecidedBy, ev.Reasons))
		if p.mustReason != "" {
			if !hasString(ev.Reasons, p.mustReason) {
				res.Status = Fail
				res.Detail = fmt.Sprintf("探针 %s 判 deny 但 reasons 里没有 %s（实际 %v）：拒绝理由分不清「策略本意拒绝」与「无命中默认拒绝」，运维看不出这是配置缺口",
					p.label, p.mustReason, ev.Reasons)
				return res
			}
			withReason = append(withReason, p.label)
		}
	}
	res.Evidence = append(res.Evidence, fmt.Sprintf("%d/%d 个探针 outcome=deny；%s 出现在 %d 个探针的 reasons 里（%s）",
		len(probes), len(probes), reasonNoMatch, len(withReason), strings.Join(withReason, "、")))
	res.Status = Pass
	return res
}

// describeHash 把可空的 hash 打成日志里能读的形式（null 与空串要能区分）。
func describeHash(h *string) string {
	if h == nil {
		return "null"
	}
	return *h
}

// shortenHash 只截掉长摘要的中段，保留前缀与头部字符，便于证据里对齐比较。
func shortenHash(h string) string {
	if len(h) <= len(hashScheme)+8 {
		return h
	}
	return h[:len(hashScheme)+8] + "…"
}

// sha256Hex 返回 b 的 SHA-256 十六进制摘要（不带前缀）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hasString 判断列表里有没有某个值（探针的理由码检查用，元素量级只有个位数）。
func hasString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// --- 传输辅助 ---

func doJSON(ctx context.Context, client *http.Client, method, url string, body []byte, headers map[string]string) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Tenon-Version", "0.1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return data, resp.StatusCode, err
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
