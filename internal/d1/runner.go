// Package d1 实现 Tenon 的 D1 差异测试（计划 Task 14、设计增量 §3）。
//
// D1 要证明的不是「能跑通」，而是：**摘掉一个模块后，agent 的规划自适应改变**——
// 既不能崩溃（outcome=failed），也不能照旧执行（A/B 动作序列相同）。靠硬编码
// 「事件类型 → 动作」映射的实现过不了这一关：它的第二份计划里仍会有被摘掉的模块。
//
// 两轮之间只改一件事——被摘除的模块是否在场：
//
//	A 轮：注册 quote + inventory + procurement → reset 全部模块 → 触发报价 → 等 agent 的结果行
//	B 轮：POST /tenon/admin/modules 把 procurement 置 enabled=false → reset 全部模块
//	     → 清空结果文件 → 换一个新的报价单号（SKU 与数量不变）再触发 → 等结果行
//
// 判据是四条合取（见 decide）：序列不同、B 轮不碰被摘除模块、B 轮转人工、两轮都没崩。
//
// 关于「动作序列」的来源：读的是 cmd/agent 写出的 TENON_RESULT_FILE（每回合追加
// 一行 JSON）。**不从 GET /tenon/audit 读**——A 轮常在预演闸门就 fail-closed 转人工
// （procurement 故意不实现 :dry-run），一个动作都没派发，审计里没有可用的 action
// 行；B 轮的结局也可能是「不派发即可」。所以 D1 判的是**规划层**的动作序列，
// 而不是「执行了几个」，也因此 A 轮允许结局是 needs_human：D1 不要求 A 轮跑完。
//
// 前置条件（缺一样就会立刻失败并把原因写进报告）：
//   - 宿主以 TENON_TEST_MODE=1 启动（POST /tenon/admin/modules 否则不存在）；
//   - 每个模块也以测试态启动（POST /tenon/admin/reset 否则 404，两轮输入就无法相同）；
//   - agent 以 TENON_RESULT_FILE=<path> 启动，且与本 runner 的 --agent-result 一致。
//
// agent 的生命周期有个不显然的约束：**每轮都应当是新的 agent 进程**
// （TENON_ONCE=1 + 外部循环拉起），不要用同一个长驻 agent 跑完 A/B 两轮。
// 原因是模块 reset 会把发件箱序号清回 0，B 轮的新事件又从序号 1 编号，而长驻 agent
// 手里的游标正好也是 1（A 轮读过的那条）——`GET /tenon/events?since=1` 会把序号 1
// 的新事件当成「已读过」过滤掉，B 轮于是被静默饿死，报告只能给出「等不到结果行」。
// 新进程从空游标开始读，没有这个冲突；代价是每轮只能是一个 TENON_ONCE 回合，
// 因此两轮之间必须清空结果文件（本 runner 每轮触发前都会清）。
//
// 运行期失败（HTTP 不通、注册表没生效、等不到结果行）一律落成 verdict=fail 加具体
// reason，而不是返回 error：报告要能直接被归档与比对，error 只留给「参数本身不合法」。
package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"tenon.dev/internal/protocol"
)

const (
	// VerdictPass / VerdictFail / VerdictInconclusive 是报告 verdict 的三个取值。
	//
	// inconclusive 表示「四条判据都过了，但命题没被检验」：A 轮计划里根本没有被摘模块
	// 的动作，于是「B 轮不碰它」是白送的，两轮差异也不是摘除造成的。这种情形**不能**
	// 记 pass——一次空洞的通过会被后来的人当成合格证（2026-10-27 的真模型跑就撞上了）。
	VerdictPass         = "pass"
	VerdictFail         = "fail"
	VerdictInconclusive = "inconclusive"

	// outcomeNeedsHuman / outcomeFailed 与 internal/agent 的三态结局同名：
	// 结果行里的 outcome 由 agent 原样写出，本包只做比较，不新增词汇。
	outcomeNeedsHuman = "needs_human"
	outcomeFailed     = "failed"

	// moduleQuote / moduleInventory / procurementModuleID 是三个模块的注册表 id
	// （module-manifest 的 module.id），不是地址：地址由调用方给。
	moduleQuote       = "quote"
	moduleInventory   = "inventory"
	procurementModule = "procurement"

	// quoteActionID 是触发器模块的业务动作（设计增量 §3 第 1 步）。
	quoteActionID = "quote.CreateStandardPartQuote"

	// defaultSKU / defaultQty 是设计增量 §3 的场景参数：在库 3、需求 12，
	// 于是「扣得出的部分 + 无力补足」是唯一诚实的结论。
	defaultSKU = "STD-1002"
	defaultQty = 12

	// DefaultRoundTimeout 是一轮等结果行的缺省上限。它必须有限：等不到就要判 fail，
	// runner 自己不许挂死。
	DefaultRoundTimeout = 120 * time.Second
	// defaultPollInterval 是轮询结果文件的间隔（文件是本机小文件，读一次很便宜）。
	defaultPollInterval = 100 * time.Millisecond

	adminModulesPath = "/tenon/admin/modules"
	adminResetPath   = "/tenon/admin/reset"
	modulesPath      = "/tenon/modules"

	// maxBodyBytes 是响应体上限：注册表与动作出参都是 KB 量级。
	maxBodyBytes = 1 << 20
)

// Module 是一个待登记的模块。
type Module struct {
	ID      string
	BaseURL string
}

// Config 是一次 D1 运行的全部输入。
//
// SKU / Qty 刻意不是命令行参数（CLI 只暴露地址与超时）：D1 比的是「同一个输入下
// 两轮的行为差异」，参数可调会让两次报告不可比。留给测试与将来 e2e 用。
type Config struct {
	Host        string
	Quote       string
	Inventory   string
	Procurement string

	// AgentResult 是 agent 的 TENON_RESULT_FILE：每回合一行 JSON，A/B 两轮的
	// 动作序列都从这里取。
	AgentResult string

	// ExtraModules 是只在 A 轮出现的额外模块（B 轮与 procurement 一起摘除）。
	ExtraModules []Module

	SKU string
	Qty int

	RoundTimeout time.Duration
	PollInterval time.Duration

	// HTTPClient 可替换（测试用）；nil 时用带超时的缺省客户端。
	HTTPClient *http.Client
}

// Report 是 `tenon d1 run` 的产物：一份可归档、可比对的结论。
// 字段集合是固定的（含两轮的动作序列），下游脚本按它取 planA/planB。
type Report struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Rounds  Rounds `json:"rounds"`
	// RoundTimeout 是每轮等结果行的上限（秒），写进报告是为了让「超时」这个
	// 结论可复核：同样的报告配上不同的超时不是同一个实验。
	RoundTimeout float64 `json:"roundTimeout"`
}

// Rounds 是 A/B 两轮的观测。
type Rounds struct {
	A Round `json:"a"`
	B Round `json:"b"`
}

// Round 是一轮的观测：结局、计划标识、动作序列。
//
// Actions 是**规划出来的**动作序列（按计划声明顺序），不是「执行过的」——
// 理由见包注释。CorrelationID 与 Detail 不进制定报告，只在失败理由里引用。
type Round struct {
	Outcome string   `json:"outcome"`
	PlanID  string   `json:"planId"`
	Actions []string `json:"actions"`

	CorrelationID string `json:"-"`
	Detail        string `json:"-"`
}

// agentResult 是 cmd/agent 结果行（TENON_RESULT_FILE）的一行。
type agentResult struct {
	At            string   `json:"at"`
	CorrelationID string   `json:"correlationId"`
	Outcome       string   `json:"outcome"`
	PlanID        string   `json:"planId"`
	Actions       []string `json:"actions"`
	Detail        string   `json:"detail"`
}

// quoteRequest / quoteLine 是触发器的入参（quote.CreateStandardPartQuote 的
// inputSchema：required 为 quoteNo 与 lines）。
type quoteRequest struct {
	QuoteNo string      `json:"quoteNo"`
	Lines   []quoteLine `json:"lines"`
}

type quoteLine struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// Run 跑完 A/B 两轮并给出结论。
//
// 返回 error 只表示**参数或环境前置条件不成立到无法开始**（地址不合法、缺结果文件
// 路径）。一旦开始跑，任何失败都落成 Report{Verdict: fail, Reason: 具体原因}，
// 因为 D1 的报告本身就是交付物：它要说清「为什么这一轮不算通过」。
func Run(ctx context.Context, cfg Config) (Report, error) {
	norm, err := cfg.normalized()
	if err != nil {
		return Report{}, err
	}
	return newRunner(norm).run(ctx), nil
}

// ---------- 配置校验 ----------

// normalized 校验并补默认值。地址一律规范化成不带尾斜杠的绝对 http(s) URL：
// 后面要往上拼 /tenon/...，地址带尾巴拼出来必然是错的。
func (c Config) normalized() (Config, error) {
	var err error
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"--host", &c.Host},
		{"--quote", &c.Quote},
		{"--inventory", &c.Inventory},
		{"--procurement", &c.Procurement},
	} {
		if *field.value, err = normalizeBase(field.name, *field.value); err != nil {
			return Config{}, err
		}
	}

	seen := map[string]bool{moduleQuote: true, moduleInventory: true, procurementModule: true}
	for i := range c.ExtraModules {
		m := c.ExtraModules[i]
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" {
			return Config{}, fmt.Errorf("--module-a 第 %d 项缺少模块 id（形如 id=url）", i+1)
		}
		if seen[m.ID] {
			return Config{}, fmt.Errorf("--module-a 第 %d 项的 id %q 与已有模块重复", i+1, m.ID)
		}
		seen[m.ID] = true
		if m.BaseURL, err = normalizeBase("--module-a "+m.ID, m.BaseURL); err != nil {
			return Config{}, err
		}
		c.ExtraModules[i] = m
	}

	if strings.TrimSpace(c.AgentResult) == "" {
		return Config{}, errors.New("缺少 agent 结果文件路径：D1 从它取 A/B 两轮的动作序列" +
			"（--agent-result，或让 tenon 继承 agent 的 TENON_RESULT_FILE）")
	}
	if strings.TrimSpace(c.SKU) == "" {
		c.SKU = defaultSKU
	}
	if c.Qty <= 0 {
		c.Qty = defaultQty
	}
	if c.RoundTimeout <= 0 {
		c.RoundTimeout = DefaultRoundTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.HTTPClient == nil {
		// 单次请求（注册/reset/触发）都不该慢：等结果行另有 RoundTimeout 兜着。
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return c, nil
}

// normalizeBase 校验一个基地址。
func normalizeBase(name, raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", fmt.Errorf("%s 必填", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q 不是合法 URL：%w", name, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s=%q 的 scheme 必须是 http 或 https", name, raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s=%q 缺少主机名", name, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s=%q 不应带查询串或锚点", name, raw)
	}
	return raw, nil
}

// ---------- 运行 ----------

// runner 持有一次 D1 运行的状态。它只在 Run 的调用栈里使用，不并发。
type runner struct {
	cfg     Config
	client  *http.Client
	modules []Module // A 轮登记的全部模块（按登记顺序）
	removed []string // B 轮摘除的模块 id（procurement + 只在 A 轮出现的额外模块）
}

func newRunner(cfg Config) *runner {
	modules := []Module{
		{ID: moduleQuote, BaseURL: cfg.Quote},
		{ID: moduleInventory, BaseURL: cfg.Inventory},
		{ID: procurementModule, BaseURL: cfg.Procurement},
	}
	modules = append(modules, cfg.ExtraModules...)
	removed := []string{procurementModule}
	for _, m := range cfg.ExtraModules {
		removed = append(removed, m.ID)
	}
	return &runner{cfg: cfg, client: cfg.HTTPClient, modules: modules, removed: removed}
}

// run 是 A/B 两轮的完整流程。每一步失败都带着「失败在哪一步」回到报告里：
// D1 的价值有一半在于失败时能立刻指出是注册表没摘掉、还是 agent 没自适应。
func (r *runner) run(ctx context.Context) Report {
	rep := newReport(r.cfg.RoundTimeout)

	// ---- A 轮 ----
	if err := r.registerModules(ctx); err != nil {
		return failed(rep, "A 轮登记模块失败："+err.Error())
	}
	if err := r.resetModules(ctx); err != nil {
		return failed(rep, "A 轮 reset 模块失败（两轮输入相同的前提不成立）："+err.Error())
	}
	if err := clearResult(r.cfg.AgentResult); err != nil {
		return failed(rep, "A 轮清空结果文件失败："+err.Error())
	}
	a, err := r.playRound(ctx, "A")
	if err != nil {
		return failed(rep, "A 轮失败："+err.Error())
	}
	rep.Rounds.A = a

	// ---- B 轮：真的把模块摘掉，而不是让 agent 关掉自己的过滤器 ----
	if err := r.disableModules(ctx); err != nil {
		return failed(rep, "摘除模块失败："+err.Error())
	}
	if err := r.verifyDisabled(ctx); err != nil {
		return failed(rep, "摘除模块后注册表没有生效："+err.Error())
	}
	if err := r.resetModules(ctx); err != nil {
		return failed(rep, "B 轮 reset 模块失败（两轮输入相同的前提不成立）："+err.Error())
	}
	if err := clearResult(r.cfg.AgentResult); err != nil {
		return failed(rep, "B 轮清空结果文件失败："+err.Error())
	}
	b, err := r.playRound(ctx, "B")
	if err != nil {
		return failed(rep, "B 轮失败："+err.Error())
	}
	rep.Rounds.B = b

	rep.Verdict, rep.Reason = decide(a, b, r.removed)
	return rep
}

// playRound 触发一次报价并等 agent 写出这一轮的结果行。
func (r *runner) playRound(ctx context.Context, label string) (Round, error) {
	quoteNo := newQuoteNo(label)
	if err := r.triggerQuote(ctx, quoteNo); err != nil {
		return Round{}, fmt.Errorf("触发报价（quoteNo=%s）失败：%w", quoteNo, err)
	}
	res, err := r.waitResult(ctx, label)
	if err != nil {
		return Round{}, err
	}
	actions := res.Actions
	if actions == nil {
		actions = []string{}
	}
	return Round{
		Outcome:       strings.TrimSpace(res.Outcome),
		PlanID:        res.PlanID,
		Actions:       actions,
		CorrelationID: res.CorrelationID,
		Detail:        res.Detail,
	}, nil
}

// ---------- 注册表与模块状态 ----------

// registerModules 登记全部模块，并显式写 enabled=true。
//
// 显式打开是因为 D1 可能被反复跑：上一轮的 B 轮会把 procurement 关掉，不关回来
// 的话这一轮的 A 轮就已经缺模块了，「两轮只有一处不同」的前提直接不成立。
func (r *runner) registerModules(ctx context.Context) error {
	for _, m := range r.modules {
		body := map[string]any{"id": m.ID, "baseURL": m.BaseURL, "enabled": true}
		if err := r.postAdmin(ctx, r.cfg.Host+adminModulesPath, body, "登记模块 "+m.ID); err != nil {
			return err
		}
	}
	return nil
}

// disableModules 把被摘除的模块在注册表里关掉（设计增量 §2.1 的测试面）。
//
// 这是 B 轮唯一的作用点：agent 自己按 enabled 过滤注册表，所以这一刀下去之后
// 它连 procurement 的 manifest 都不会去读，规划里也就不可能再有 procurement.*。
func (r *runner) disableModules(ctx context.Context) error {
	for _, id := range r.removed {
		body := map[string]any{"id": id, "enabled": false}
		if err := r.postAdmin(ctx, r.cfg.Host+adminModulesPath, body, "摘除模块 "+id); err != nil {
			return err
		}
	}
	return nil
}

// verifyDisabled 回读注册表，确认被摘除的模块**真的**是 enabled=false。
//
// 只信 admin 端点的 200 是不够的：摘除没生效时 B 轮会因为别的原因失败（例如序列
// 相同），理由就会指向 agent，而真正的问题在注册表。这里先把它钉死。
func (r *runner) verifyDisabled(ctx context.Context) error {
	status, raw, err := r.get(ctx, r.cfg.Host+modulesPath)
	if err != nil {
		return fmt.Errorf("GET %s 失败：%w", modulesPath, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s 返回 %d：%s", modulesPath, status, snippet(raw))
	}
	var resp struct {
		Modules []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("GET %s 的响应不是合法 JSON：%w", modulesPath, err)
	}
	enabled := make(map[string]bool, len(resp.Modules))
	for _, m := range resp.Modules {
		enabled[m.ID] = m.Enabled
	}
	for _, id := range r.removed {
		on, found := enabled[id]
		if !found {
			return fmt.Errorf("注册表里没有模块 %q：它既不在场也谈不上被摘除", id)
		}
		if on {
			return fmt.Errorf("模块 %q 仍是 enabled=true：摘除没有落到注册表上", id)
		}
	}
	return nil
}

// resetModules 重置每个模块的内存状态（设计增量 §2.3）。
//
// 两轮必须都 reset：否则 B 轮是在 A 轮留下的库存与幂等表上跑的，
// 「同一输入」的前提不成立，序列差异也就说明不了任何事。
func (r *runner) resetModules(ctx context.Context) error {
	for _, m := range r.modules {
		status, raw, err := r.post(ctx, m.BaseURL+adminResetPath, map[string]any{}, "")
		if err != nil {
			return fmt.Errorf("POST %s%s 失败：%w", m.ID, adminResetPath, err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("POST %s%s 返回 %d：%s（模块是否以测试态启动？）",
				m.ID, adminResetPath, status, snippet(raw))
		}
	}
	return nil
}

// triggerQuote 用真实业务动作触发一次报价完成的业务事实。
//
// 两轮的 quoteNo 必须不同：quote 模块按 quoteNo 做业务级幂等，同一个单号第二轮
// 会命中「首次结果」而**不再发事件**，agent 根本收不到触发源。SKU 与数量则完全
// 相同——那是「同一输入」的部分。
func (r *runner) triggerQuote(ctx context.Context, quoteNo string) error {
	body := quoteRequest{QuoteNo: quoteNo, Lines: []quoteLine{{SKU: r.cfg.SKU, Qty: r.cfg.Qty}}}
	// 幂等键由 quoteNo 派生：同一次触发重放会拿到首次结果，不会多出一条事件。
	status, raw, err := r.post(ctx, r.cfg.Quote+"/tenon/actions/"+quoteActionID, body, "tenon:d1/"+quoteNo)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("POST %s 返回 %d：%s", quoteActionID, status, snippet(raw))
	}
	return nil
}

// postAdmin 发一条管理面请求并处理失败。
func (r *runner) postAdmin(ctx context.Context, target string, body any, what string) error {
	status, raw, err := r.post(ctx, target, body, "")
	if err != nil {
		return fmt.Errorf("%s：POST %s 失败：%w", what, target, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s：POST %s 返回 %d：%s（宿主是否以 TENON_TEST_MODE=1 启动？）",
			what, target, status, snippet(raw))
	}
	return nil
}

// ---------- 结果行 ----------

// waitResult 轮询结果文件，等这一轮的结果行。超时即失败并说明「等的是什么」。
//
// 这里不重试 agent：D1 观测的是 agent 的真实行为，重发同一个事件只会得到一个
// 新的 planId（新幂等键），把「没自适应」掩盖成「第二次恰好自适应」。
func (r *runner) waitResult(ctx context.Context, label string) (agentResult, error) {
	deadline := time.Now().Add(r.cfg.RoundTimeout)
	var lastErr error
	for {
		res, err := readRoundResult(r.cfg.AgentResult)
		if err == nil {
			return res, nil
		}
		lastErr = err

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := r.cfg.PollInterval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return agentResult{}, fmt.Errorf("%s 轮等待结果行时被取消：%w", label, ctx.Err())
		case <-time.After(wait):
		}
	}
	return agentResult{}, fmt.Errorf(
		"%s 轮在 %s 内没等到 agent 的结果行（最后观测：%v）。"+
			"检查：agent 是否以 TENON_RESULT_FILE=%s 启动；这一轮是否真的开了回合（events?direction=out）；"+
			"agent 是否是本轮新起的进程（长驻 agent 的游标会与 reset 后的新事件序号撞上，B 轮可能被饿死）",
		label, r.cfg.RoundTimeout, lastErr, r.cfg.AgentResult)
}

// readRoundResult 读结果文件里的第一条有效结果行。
//
// 文件在每轮触发之前被清空，所以「第一条」就是这一轮写的那一条。坏行（写到一半
// 的行）不算数：继续找后面的行，找不到就把错误留给 waitResult 的超时理由。
func readRoundResult(path string) (agentResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agentResult{}, fmt.Errorf("结果文件 %s 还不存在", path)
		}
		return agentResult{}, fmt.Errorf("读结果文件 %s 失败：%w", path, err)
	}
	var lastErr error
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var res agentResult
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			lastErr = fmt.Errorf("结果行不是合法 JSON（%s）：%w", snippet([]byte(line)), err)
			continue
		}
		if !isKnownOutcome(res.Outcome) {
			lastErr = fmt.Errorf("结果行的 outcome=%q 不是 completed/needs_human/failed 之一", res.Outcome)
			continue
		}
		return res, nil
	}
	if lastErr != nil {
		return agentResult{}, lastErr
	}
	return agentResult{}, fmt.Errorf("结果文件 %s 里还没有非空行", path)
}

// clearResult 清空结果文件（保留文件本身）：B 轮只允许读到 B 轮写的那一行，
// 否则 A 轮的残留会被当成 B 轮的观测，D1 就变成了「自己和自己比」。
func clearResult(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("打开结果文件 %q 失败：%w", path, err)
	}
	return f.Close()
}

// isKnownOutcome 判断结果行的结局是不是三态之一。
func isKnownOutcome(outcome string) bool {
	switch strings.TrimSpace(outcome) {
	case "completed", outcomeNeedsHuman, outcomeFailed:
		return true
	default:
		return false
	}
}

// ---------- HTTP ----------

// post 发一个带协议版本头的 POST（管理面与动作端点都走它）。
func (r *runner) post(ctx context.Context, target string, body any, idempotencyKey string) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("请求体编组失败：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, fmt.Errorf("构造请求失败（%s）：%w", target, err)
	}
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set(protocol.HeaderIdempotencyKey, idempotencyKey)
	}
	return r.do(req)
}

// get 发一个带协议版本头的 GET。
func (r *runner) get(ctx context.Context, target string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("构造请求失败（%s）：%w", target, err)
	}
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	return r.do(req)
}

func (r *runner) do(req *http.Request) (int, []byte, error) {
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读取响应体失败：%w", err)
	}
	return resp.StatusCode, body, nil
}

// ---------- 判定 ----------

// decide 是 D1 的全部判据：四条合取，缺一条就是 fail，且理由必须指向具体那一条。
//
// 判定顺序按「越具体越先说」排，而不是按编号：两轮里有一次崩了，比序列相同更
// 值得先说；B 轮还在调被摘除的模块，比序列相同更能定位问题（是注册表过滤没生效，
// 不是模型不够聪明）。
func decide(a, b Round, removed []string) (string, string) {
	if a.Outcome == outcomeFailed {
		return VerdictFail, fmt.Sprintf(
			"A 轮崩了（outcome=failed，planId=%s，correlationId=%s）：%s。"+
				"D1 的前提是 A 轮能规划并走到闸门，A 轮崩溃时两轮对比没有意义",
			orDash(a.PlanID), orDash(a.CorrelationID), detailOf(a))
	}
	if b.Outcome == outcomeFailed {
		return VerdictFail, fmt.Sprintf(
			"B 轮不是转人工而是 failed（planId=%s，correlationId=%s）：%s。"+
				"摘掉模块后能力不足必须举手（needs_human），崩溃说明「缺模块」被当成了故障处理",
			orDash(b.PlanID), orDash(b.CorrelationID), detailOf(b))
	}
	if offending := filterRemoved(b.Actions, removed); len(offending) > 0 {
		return VerdictFail, fmt.Sprintf(
			"B 轮仍在调用被摘除模块：%s。注册表已把 %s 置 enabled=false，"+
				"agent 却还看得见它——说明 enabled 过滤没有生效（B 轮的动作序列：%s）",
			strings.Join(offending, "、"), strings.Join(removed, "、"), formatSeq(b.Actions))
	}
	if equalSeq(a.Actions, b.Actions) {
		return VerdictFail, fmt.Sprintf(
			"序列相同：摘掉模块后行为没变→说明 agent 没在用能力清单（A 与 B 都是 %s；"+
				"B 轮结局 %s）。这正是 D1 要否掉的情形：硬编码映射的实现会照旧执行",
			formatSeq(a.Actions), orDash(b.Outcome))
	}
	if b.Outcome != outcomeNeedsHuman {
		return VerdictFail, fmt.Sprintf(
			"B 轮不是转人工而是 %s（A=%s，B=%s）：能力不足时必须举手 needs_human，"+
				"不能照旧执行", orDash(b.Outcome), formatSeq(a.Actions), formatSeq(b.Actions))
	}

	// 四条全过，但还要看 A 轮是否**真的用到了**被摘除的模块：如果 A 压根没引用它，
	// 两轮的差异就不是「摘除」造成的，命题没有被真正检验。这种空洞通过记 inconclusive，
	// 不记 pass——否则一次白送的 pass 会被当成合格证。
	if used := filterRemoved(a.Actions, removed); len(used) > 0 {
		return VerdictPass, fmt.Sprintf(
			"自适应：A 轮计划含被摘除模块的动作（%s），B 轮摘除后不再出现；A=%s，B=%s；"+
				"B 轮结局 needs_human（不是 failed，也不是照旧执行）",
			strings.Join(used, "、"), formatSeq(a.Actions), formatSeq(b.Actions))
	}
	return VerdictInconclusive, fmt.Sprintf(
		"命题未被检验（记 inconclusive，不算通过）：A 轮计划没有引用被摘除模块 %s，"+
			"于是「B 轮不碰它」是白送的，两轮差异也不是摘除造成的；"+
			"A=%s，B=%s，B 轮结局 needs_human。"+
			"要让 D1 有意义，A 轮必须真的引用被摘模块（例如先扣库存、再建采购）",
		strings.Join(removed, "、"), formatSeq(a.Actions), formatSeq(b.Actions))
}

// filterRemoved 挑出属于被摘除模块的动作（action id 的前缀是 `<moduleId>.`）。
func filterRemoved(actions, removed []string) []string {
	var out []string
	for _, action := range actions {
		for _, mod := range removed {
			if strings.HasPrefix(action, mod+".") {
				out = append(out, action)
				break
			}
		}
	}
	return out
}

// equalSeq 按顺序比较两个动作序列（D1 比的是序列结构，不比参数与顺序无关的集合）。
func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newQuoteNo 生成一轮的报价单号。
//
// 两轮必须不同（业务级幂等与事件 id 都由 quoteNo 派生），且不得含空白、':' 或 '/'
// （quote 模块把单号编进 subject 的实体引用，词法是 ^[^\s:/]+$）。
func newQuoteNo(label string) string {
	return fmt.Sprintf("D1-%s-%d", label, time.Now().UTC().UnixNano())
}

// ---------- 小工具 ----------

// newReport 是一份「还没有结论」的报告：缺省 verdict=fail（fail-closed），
// 两轮的 actions 初始化成空切片，保证序列化成 [] 而不是 null。
func newReport(roundTimeout time.Duration) Report {
	return Report{
		Verdict: VerdictFail,
		Reason:  "D1 没有得出结论",
		Rounds: Rounds{
			A: Round{Actions: []string{}},
			B: Round{Actions: []string{}},
		},
		RoundTimeout: roundTimeout.Seconds(),
	}
}

// failed 把运行期错误写成 fail 报告。
func failed(rep Report, reason string) Report {
	rep.Verdict = VerdictFail
	rep.Reason = reason
	return rep
}

// formatSeq 把动作序列写成人能读的形状（空序列也要看得出来是空的）。
func formatSeq(actions []string) string {
	if len(actions) == 0 {
		return "（空）"
	}
	return "[" + strings.Join(actions, " → ") + "]"
}

// detailOf 取结局说明，空说明不留半截标点。
func detailOf(r Round) string {
	if strings.TrimSpace(r.Detail) == "" {
		return "没有给出说明"
	}
	return r.Detail
}

// orDash 让空值在理由里可读。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// snippet 截取响应体前面一段放进错误信息。
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	runes := []rune(text)
	if len(runes) <= 300 {
		return text
	}
	return string(runes[:300]) + "…"
}
