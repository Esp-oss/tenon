package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenon.dev/internal/spec"
)

// ---------- 测试辅助 ----------

// testSpec 加载仓库内真实的 schema：Plan 的判据必须与线上同一份文件。
func testSpec(t *testing.T) *spec.Set {
	t.Helper()
	dir := filepath.Join("..", "..", spec.DefaultDir)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("找不到 schema 目录 %s：%v", dir, err)
	}
	set, err := spec.Load(dir)
	if err != nil {
		t.Fatalf("加载 schema 失败：%v", err)
	}
	return set
}

// quoteCompleted 构造一个 D1 A 轮形状的事件（STD-1002 要 12，在库 3）。
func quoteCompleted(qty int) DomainEvent {
	return DomainEvent{
		EventID:       "evt_test0001",
		Type:          "quote.completed",
		Version:       "0.1.0",
		Subject:       json.RawMessage(`"quote:Quote/Q-1"`),
		OccurredAt:    "2026-10-27T10:00:00Z",
		CorrelationID: "cor:quote/Q-1",
		Payload:       json.RawMessage(fmt.Sprintf(`{"quoteNo":"Q-1","lines":[{"sku":"STD-1002","qty":%d}]}`, qty)),
	}
}

// testStep 是构造测试计划里一步的简写。
type testStep struct {
	id        string
	action    string
	kind      string
	args      string
	dependsOn string // JSON 数组字面量；空表示 []
}

// planJSON 组装一份能过 plan.schema.json 的计划。参数由测试给定（这正是不许在生产
// 代码里出现 action 字面量的反面：用例需要可控的固定动作才能脚本化假 LLM 与假模块）。
func planJSON(planID string, steps ...testStep) string {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		kind := s.kind
		if kind == "" {
			kind = "write"
		}
		args := s.args
		if strings.TrimSpace(args) == "" {
			args = `{"sku":"STD-1002","qty":12,"referenceNo":"Q-1"}`
		}
		deps := s.dependsOn
		if strings.TrimSpace(deps) == "" {
			deps = `[]`
		}
		parts = append(parts, fmt.Sprintf(
			`{"stepId":%q,"action":%q,"args":%s,"dependsOn":%s,"kind":%q}`,
			s.id, s.action, args, deps, kind))
	}
	return fmt.Sprintf(`{"planId":%q,"supersedes":null,`+
		`"trigger":{"eventId":"evt_test0001","correlationId":"cor:quote/Q-1"},`+
		`"steps":[%s],"expectedDiff":[],"dryRun":{"status":"pending"},`+
		`"estimatedRisk":"low","rationale":"测试计划"}`, planID, strings.Join(parts, ","))
}

// fakeLLM 是脚本化的假模型：按顺序返回预设的回复，并记录每次的 prompt。
// 全部用例都用它，因此 internal/agent 的测试不发真实请求、不花 token。
type fakeLLM struct {
	replies    []string
	err        error
	Calls      int
	LastSystem string
	LastUser   string
	prompts    []string
}

func (f *fakeLLM) ChatJSON(_ context.Context, system, user string) ([]byte, error) {
	f.Calls++
	f.LastSystem = system
	f.LastUser = user
	f.prompts = append(f.prompts, user)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.replies) == 0 {
		return nil, errors.New("fakeLLM：没有脚本化回复")
	}
	idx := f.Calls - 1
	if idx >= len(f.replies) {
		idx = len(f.replies) - 1
	}
	return []byte(f.replies[idx]), nil
}

// ---------- 规划器 ----------

func TestPlanValidatedAgainstSchema(t *testing.T) {
	bad := `{"planId":"pln_1","steps":"oops"}`
	f := &fakeLLM{replies: []string{bad, bad}}
	p := &Planner{LLM: f, Spec: testSpec(t)}

	if _, err := p.Plan(context.Background(), PlanInput{Event: quoteCompleted(12)}); err == nil {
		t.Fatal("不合 schema 的 Plan 必须报错")
	}
	if f.Calls != maxPlanAttempts {
		t.Fatalf("坏输出应当「带上校验错误重试 1 次」（共 %d 次），实际调用 %d 次", maxPlanAttempts, f.Calls)
	}
}

func TestPlanRetriesOnceAndFeedsBackValidationError(t *testing.T) {
	good := planJSON("pln_retry1", testStep{id: "s1", action: "inventory.DeductStock"})
	f := &fakeLLM{replies: []string{`{"planId":"pln_bad","steps":[]}`, good}}
	p := &Planner{LLM: f, Spec: testSpec(t)}

	plan, err := p.Plan(context.Background(), PlanInput{Event: quoteCompleted(12)})
	if err != nil {
		t.Fatalf("第二次输出合法时应当成功，得到错误：%v", err)
	}
	if plan.PlanID != "pln_retry1" || len(plan.Steps) != 1 {
		t.Fatalf("解析出的计划不对：%+v", plan)
	}
	if f.Calls != 2 {
		t.Fatalf("应当重试一次（共 2 次调用），实际 %d 次", f.Calls)
	}
	if !strings.Contains(f.prompts[1], "未通过 plan.schema.json 校验") {
		t.Fatalf("第二次 prompt 必须回填校验错误，实际内容：\n%s", f.prompts[1])
	}
	if f.prompts[0] == f.prompts[1] {
		t.Fatal("重试的 prompt 与首次完全相同：模型没有可修正的信息")
	}
}

func TestPromptMentionsOnlyDiscoveredActions(t *testing.T) {
	good := planJSON("pln_prompt1", testStep{id: "s1", action: "inventory.DeductStock"})
	f := &fakeLLM{replies: []string{good}}
	p := &Planner{LLM: f, Spec: testSpec(t)}

	if _, err := p.Plan(context.Background(), PlanInput{
		Event:   quoteCompleted(12),
		Modules: []ModuleDigest{{ID: "inventory", Actions: []string{"inventory.GetStock", "inventory.DeductStock"}}},
	}); err != nil {
		t.Fatalf("规划失败：%v", err)
	}

	if !strings.Contains(f.LastUser, "inventory.DeductStock") {
		t.Fatalf("prompt 必须包含动态发现的 action id，实际 prompt：\n%s", f.LastUser)
	}
	if !strings.Contains(f.LastUser, "inventory.GetStock") {
		t.Fatalf("prompt 必须包含清单里的每个动作，实际 prompt：\n%s", f.LastUser)
	}
	// 这是 D1 能成立的前提：未注册模块的 action 不得出现在 prompt 里。
	if strings.Contains(f.LastUser, "procurement") {
		t.Fatalf("prompt 里出现了未注册模块的 action：\n%s", f.LastUser)
	}
	if strings.Contains(f.LastSystem, "inventory.DeductStock") {
		t.Fatal("system prompt 应当只含静态约束，动态清单必须留在 user prompt（否则可测性消失）")
	}
}

func TestPlanRejectsUndiscoveredAction(t *testing.T) {
	// 模型要求了一个摘除后不再可达的模块的动作：schema 完全合法，但它不该被执行。
	good := planJSON("pln_gap1", testStep{id: "s1", action: "procurement.CreatePurchaseRequest"})
	f := &fakeLLM{replies: []string{good, good}}
	p := &Planner{LLM: f, Spec: testSpec(t)}

	_, err := p.Plan(context.Background(), PlanInput{
		Event:   quoteCompleted(12),
		Modules: []ModuleDigest{{ID: "inventory", Actions: []string{"inventory.DeductStock"}}},
	})
	if err == nil {
		t.Fatal("引用清单外动作的计划必须被拒绝")
	}
	var gap *capabilityGapError
	if !errors.As(err, &gap) {
		t.Fatalf("应当是 capabilityGapError（能力不足 ≠ 模型输出坏），实际：%v", err)
	}
	if len(gap.Actions) != 1 || gap.Actions[0] != "procurement.CreatePurchaseRequest" {
		t.Fatalf("缺口清单不对：%+v", gap.Actions)
	}
	if gap.PlanID != "pln_gap1" {
		t.Fatalf("缺口错误必须带上 planId（Intent 帧要用它），实际 %q", gap.PlanID)
	}
	if f.Calls != 1 {
		t.Fatalf("能力不足不是格式问题，不重试：期望 1 次调用，实际 %d 次", f.Calls)
	}
}

func TestPlanRequiresSchemaSet(t *testing.T) {
	p := &Planner{LLM: &fakeLLM{replies: []string{planJSON("pln_nospec1", testStep{id: "s1", action: "inventory.DeductStock"})}}}
	if _, err := p.Plan(context.Background(), PlanInput{Event: quoteCompleted(12)}); err == nil {
		t.Fatal("没有 schema 集时必须报错（不允许跳过校验先跑起来）")
	}
}

func TestPlanRejectsActionWhenModulesDeclareNoActions(t *testing.T) {
	// 模块在场但一个动作都没声明：available 为空不构成「什么都能做」的许可证。
	good := planJSON("pln_gap0002", testStep{id: "s1", action: "inventory.DeductStock"})
	f := &fakeLLM{replies: []string{good}}
	p := &Planner{LLM: f, Spec: testSpec(t)}

	_, err := p.Plan(context.Background(), PlanInput{
		Event:   quoteCompleted(12),
		Modules: []ModuleDigest{{ID: "inventory"}},
	})
	var gap *capabilityGapError
	if !errors.As(err, &gap) {
		t.Fatalf("清单里没有任何动作时，任何 action 都该判成能力缺口，得到：%v", err)
	}
}

// ---------- skill 存储 ----------

// writeSkill 在临时目录里放一份 skill 文档。
func writeSkill(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 skill 失败：%v", err)
	}
}

func TestLoadSkillsParsesSubscribes(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "quote-to-fulfillment/SKILL.md", `---
name: quote-to-fulfillment
tenon:
  subscribes: ["quote.completed", "quote.revised"]
---
报价完成后要保证库存与采购能兑现这张单。
`)
	writeSkill(t, root, "stock-watch/SKILL.md", `---
name: stock-watch
tenon:
  subscribes:
    - stock.low
---
关注库存水位，低于安全库存时提醒人。
`)
	writeSkill(t, root, "plain/SKILL.md", "没有 frontmatter 的文档：不订阅任何事件。\n")

	store, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("加载 skill 失败：%v", err)
	}
	if store.Len() != 3 {
		t.Fatalf("期望 3 份 skill，得到 %d", store.Len())
	}

	text := store.TextFor("quote.completed")
	if !strings.Contains(text, "报价完成后") {
		t.Fatalf("quote.completed 应当命中 quote-to-fulfillment，实际正文：%q", text)
	}
	if strings.Contains(text, "库存水位") {
		t.Fatalf("stock-watch 只订阅 stock.low，不该被 quote.completed 唤醒：%q", text)
	}
	if got := store.TextFor("stock.low"); !strings.Contains(got, "库存水位") {
		t.Fatalf("块列表形式的订阅没有生效：%q", got)
	}
	if got := store.TextFor("quote.cancelled"); got != "" {
		t.Fatalf("没有 skill 订阅的事件必须返回空（调用方据此跳过），实际：%q", got)
	}

	matched := store.Matching("quote.revised")
	if len(matched) != 1 || matched[0].Name != "quote-to-fulfillment" {
		t.Fatalf("订阅解析不对：%+v", matched)
	}
	if subs := matched[0].Subscribes; len(subs) != 2 || subs[1] != "quote.revised" {
		t.Fatalf("subscribes 解析不对：%+v", subs)
	}
}

func TestLoadSkillsRejectsBrokenFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "broken/SKILL.md", "---\nname: broken\n正文没有闭合的 frontmatter\n")
	if _, err := LoadSkills(root); err == nil {
		t.Fatal("frontmatter 不闭合必须报错：静默跳过会让「该开回合却没开」变成看不见的故障")
	}
}

func TestLoadSkillsMissingDirIsEmpty(t *testing.T) {
	store, err := LoadSkills(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("目录不存在应当等于「没有 skill」，得到错误：%v", err)
	}
	if store.Len() != 0 {
		t.Fatalf("期望空集合，得到 %d 份", store.Len())
	}
	if EmptySkills().Len() != 0 {
		t.Fatal("EmptySkills 必须是空集合")
	}
}
