package agent

import (
	"sort"
	"strings"
)

// skillModules 抽出「这份 skill 关心哪些模块」：tenon.expectedModules 优先，
// 再从 subscribes 的事件名里取模块前缀（"quote.completed" → "quote"）。
//
// 为什么不再有 knownModuleNames 硬编码词表：那等于「新增业务必须改 agent 代码」。
// 现在词表由 skill 自己声明，agent 只做确定性的计数。
func skillModules(s Skill) []string {
	seen := make(map[string]struct{}, len(s.ExpectedModules)+len(s.Subscribes))
	out := make([]string, 0, len(s.ExpectedModules)+len(s.Subscribes))
	add := func(raw string) {
		m := strings.ToLower(strings.TrimSpace(raw))
		if m == "" {
			return
		}
		if _, dup := seen[m]; dup {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	for _, m := range s.ExpectedModules {
		add(m)
	}
	for _, sub := range s.Subscribes {
		sub = strings.TrimSpace(sub)
		if sub == "" {
			continue
		}
		// 订阅项可能是事件类型（"quote.completed" → quote），也可能就是模块名（"inventory"）。
		if i := strings.Index(sub, "."); i > 0 {
			add(sub[:i])
		} else {
			add(sub)
		}
	}
	sort.Strings(out)
	return out
}

// SelectSkills 按确定性规则挑出候选 skill：
//  1. 统计每份 skill 的命中数（它自己声明的模块名 ∩ 请求文本与 view 模块名）；
//  2. 命中数为 0 的丢掉；
//  3. 命中数降序，同数按 skill 名字典序；
//  4. 最多返回 limit 份。
//
// 不调 LLM：同一 (text, view) 必得同一结果，可表驱动测试。
//
// 词表来自 skill 自己（见 skillModules），所以**新模块不需要改这里**——只要它的 skill
// 在 frontmatter 里写了 tenon.expectedModules 或 subscribes。
func SelectSkills(skills []Skill, text, viewModule string, limit int) []Skill {
	if limit <= 0 {
		limit = 2
	}
	haystack := strings.ToLower(text + " " + viewModule)
	type scored struct {
		skill Skill
		hits  int
	}
	var picks []scored
	for _, s := range skills {
		hits := 0
		for _, mod := range skillModules(s) {
			if strings.Contains(haystack, mod) {
				hits++
			}
		}
		if hits > 0 {
			picks = append(picks, scored{skill: s, hits: hits})
		}
	}
	// SliceStable：名字与命中数全同的两份 skill 也要顺序稳定，否则截断会切掉任意一份。
	// 这个函数的全部价值就是确定性，用非稳定排序等于把一半价值让掉。
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].hits != picks[j].hits {
			return picks[i].hits > picks[j].hits
		}
		return picks[i].skill.Name < picks[j].skill.Name
	})
	if len(picks) > limit {
		picks = picks[:limit]
	}
	out := make([]Skill, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.skill)
	}
	return out
}
