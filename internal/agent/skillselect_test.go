package agent

import (
	"strings"
	"testing"
)

func TestSelectSkillsByText(t *testing.T) {
	skills := []Skill{
		{Name: "quote-to-fulfillment", Body: "报价完成后核对 inventory 库存，缺口走 procurement。"},
		{Name: "procurement-only", Body: "只管 procurement 的采购单。"},
		{Name: "unrelated", Body: "和库存无关。"},
	}
	got := SelectSkills(skills, "Q-2026-0417 这单货不够，先扣现有的，缺的发起采购", "inventory", 2)
	if len(got) != 1 || got[0].Name != "quote-to-fulfillment" {
		t.Fatalf("应命中最相关的 1 份，得到 %+v", got)
	}
}

func TestSelectSkillsDeterministicOrder(t *testing.T) {
	skills := []Skill{
		{Name: "b-skill", Body: "inventory procurement"},
		{Name: "a-skill", Body: "inventory procurement"},
	}
	first := SelectSkills(skills, "inventory 缺货", "inventory", 2)
	second := SelectSkills(skills, "inventory 缺货", "inventory", 2)
	if len(first) != 2 || first[0].Name != second[0].Name || first[1].Name != second[1].Name {
		t.Fatalf("同一输入必须同一顺序：%+v vs %+v", first, second)
	}
	if first[0].Name != "a-skill" {
		t.Fatalf("同命中数按名字典序，a-skill 应在前：%+v", first)
	}
}

func TestSelectSkillsEmptyWhenNoMatch(t *testing.T) {
	skills := []Skill{{Name: "unrelated", Body: "nothing here"}}
	if got := SelectSkills(skills, "完全无关的话", "", 2); len(got) != 0 {
		t.Fatalf("无命中应返回空，得到 %+v", got)
	}
}

// 候选集的渲染：格式与 TextFor 逐字一致，但空正文渲染成显式占位——绝不渲染空串。
func TestTextForSelected(t *testing.T) {
	store := &SkillStore{}

	// 两份选中：有正文的原样渲染，空正文（只靠 subscribes 命中）渲染成占位。
	// 这份断言写的是**完整字符串**：格式是契约，不能靠印象。
	got := store.TextForSelected([]Skill{
		{Name: "a-with-body", Body: "  正文 A  "},
		{Name: "b-no-body", Body: "   "},
	})
	want := "## skill a-with-body\n正文 A\n\n## skill b-no-body\n（无正文，仅按订阅命中）"
	if got != want {
		t.Fatalf("候选集渲染格式不对：\n 实际 %q\n 期望 %q", got, want)
	}

	// 单独一份空正文：返回的必须是一句显式的话，不是空串。
	single := store.TextForSelected([]Skill{{Name: "only-subscription", Subscribes: []string{"inventory"}}})
	if single == "" {
		t.Fatal("空正文的 skill 被选中时必须渲染占位，绝不渲染空串")
	}
	if !strings.Contains(single, "（无正文，仅按订阅命中）") {
		t.Fatalf("空正文的占位文本不对：%q", single)
	}

	// 与 TextFor 同格式：同一份**非空正文**的 skill，两条路径的输出必须逐字相同。
	sub := &SkillStore{skills: []Skill{{
		Name: "quote-skill", Subscribes: []string{"quote.completed"}, Body: "正文 A",
	}}}
	if a, b := sub.TextFor("quote.completed"), sub.TextForSelected(sub.Matching("quote.completed")); a != b {
		t.Fatalf("TextForSelected 必须与 TextFor 同格式：\n TextFor         %q\n TextForSelected %q", a, b)
	}

	// 一份都没选中：空串，让 userPrompt 走它自己的「本次没有匹配」分支。
	if got := store.TextForSelected(nil); got != "" {
		t.Fatalf("没选中任何 skill 时应返回空串，得到 %q", got)
	}
}
