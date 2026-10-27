package agent

import (
	"sort"
	"strings"
)

// knownModuleNames 是粗筛用的模块名词表。它不是"平台为每个系统适配"——
// 这些词来自本次请求与 skill 正文的**交集**，词表只用来判断"这段话提到了哪些模块"。
// 加新模块时在这里补一行；它不参与任何判定，只影响 skill 候选集。
var knownModuleNames = []string{"inventory", "procurement", "quote"}

// SelectSkills 按确定性规则挑出候选 skill：
//  1. 统计每份 skill 的命中数（正文/subscribes 里出现的模块名 ∩ 请求文本与 view 模块名）；
//  2. 命中数为 0 的丢掉；
//  3. 命中数降序，同数按 skill 名字典序；
//  4. 最多返回 limit 份。
//
// 不调 LLM：同一 (text, view) 必得同一结果，可表驱动测试。
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
		body := strings.ToLower(s.Body + " " + strings.Join(s.Subscribes, " "))
		for _, mod := range knownModuleNames {
			if strings.Contains(haystack, mod) && strings.Contains(body, mod) {
				hits++
			}
		}
		if hits > 0 {
			picks = append(picks, scored{skill: s, hits: hits})
		}
	}
	sort.Slice(picks, func(i, j int) bool {
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
