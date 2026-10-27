package agent

import "testing"

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
