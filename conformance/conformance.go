// Package conformance 是 Tenon 的可执行规范：把 spec/0.1 的断言做成能对任意实现跑的黑盒测试。
//
// M0 覆盖模块侧：C1（manifest）、C2（幂等）、C3（未知字段）、C8（可选能力容忍）、
// C9（错误分类）、C10（反模式扫描）。
// 宿主侧用例（C4 审计、C5 fail-closed）与 D1 差异测试在本包里**显式记为 SKIP 并写明原因**，
// 不允许用「没跑就是通过」蒙过去——见 Report.ExitCode 与 --strict 的语义。
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tenon.dev/internal/spec"
	"tenon.dev/tools/lint"
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
	rep.Results = append(rep.Results, hostSideSkips()...)

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

// hostSideSkips 明确记录本轮**没有判定**的用例，避免「没跑就是通过」。
func hostSideSkips() []Result {
	return []Result{
		{Case: "C4", Title: "审计可查（两段写 + 哈希链）", Status: Skip,
			Detail: "需要宿主侧 GET /tenon/audit；模块模式不覆盖。宿主实现落地后由 --target 指向宿主运行。"},
		{Case: "C5", Title: "fail-closed：策略/授权不可达时拒绝写操作", Status: Skip,
			Detail: "需要宿主侧 policies:evaluate 与 grants:evaluate；模块模式不覆盖。"},
		{Case: "D1", Title: "差异测试：摘掉 procurement 后 Plan 必须改变且不抛错", Status: Skip,
			Detail: "需要会推理的规划器（LLM）。未配置 LLM 时不能声称通过——规则式规划器会让 D1 变成假命题。"},
	}
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
