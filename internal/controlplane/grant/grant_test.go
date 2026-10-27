package grant

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// 用例里反复出现的动作与授权标识（grantId 形如 schema 要求的 gr-<...>）。
	actionDeduct    = "inventory.DeductStock"
	actionGet       = "inventory.GetStock"
	actionRefund    = "finance.Refund"
	grantFulfilment = "gr-01JFULFILMENT"
	grantNarrow     = "gr-01JNARROW"
	grantWide       = "gr-01JWIDE01"
	agentFulfilment = "ag_fulfillment"
)

// fixedNow 是可控时钟的起点；用例通过改 *time.Time 推进时间，不依赖真实时钟。
var fixedNow = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

// subjects 是交集规则的两名委托人：alice 能扣减与查询，bob 只能查询。
var subjects = map[string][]string{
	"user:alice": {actionDeduct, actionGet},
	"user:bob":   {actionGet},
}

// clock 返回一个读 *now 的时钟；推进时间就是改 now 指向的值。
func clock(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}

// engine 造一个带 alice/bob 权限表的引擎，时间源固定在 now。
func engine(grants []Grant, now *time.Time) *GrantEngine {
	return NewGrantEngine(grants, WithSubjects(subjects), WithClock(clock(now)))
}

// grantAt 造一条对 agentFulfilment 生效、到 at 为止的授权。
func grantAt(at time.Time, entries ...GrantEntry) []Grant {
	return []Grant{{
		GrantID:   grantFulfilment,
		Agent:     agentFulfilment,
		Allow:     entries,
		ExpiresAt: at,
	}}
}

// deductEntry 造一条允许 actionDeduct 的 entry，constraints 可为 nil。
func deductEntry(constraints *GrantConstraints) GrantEntry {
	return GrantEntry{Module: "inventory", Actions: []string{actionDeduct}, Constraints: constraints}
}

// TestEvaluateIsIntersection 验证决策 D5：生效权限 = agent 被授予 ∩ 委托人权限。
// alice 有扣减权 → 放行；bob 只有查询权 → SUBJECT_NOT_PERMITTED；未登记的人同样拒绝。
func TestEvaluateIsIntersection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		subject    string
		action     string
		wantAllow  bool
		wantReason string
	}{
		{name: "alice 有扣减权", subject: "user:alice", action: actionDeduct, wantAllow: true},
		{name: "alice 有查询权", subject: "user:alice", action: actionGet, wantAllow: true},
		{name: "bob 只有查询权不能扣减", subject: "user:bob", action: actionDeduct, wantReason: ReasonSubjectNotPermitted},
		{name: "bob 能查询", subject: "user:bob", action: actionGet, wantAllow: true},
		{name: "未登记的委托人", subject: "user:carol", action: actionDeduct, wantReason: ReasonSubjectNotPermitted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
			e := engine(grantAt(fixedNow.Add(time.Hour),
				GrantEntry{Module: "inventory", Actions: []string{actionDeduct, actionGet}}), &now)
			got, err := e.Evaluate(agentFulfilment, tc.subject, tc.action, 1)
			if err != nil {
				t.Fatalf("Evaluate 不应报错：%v", err)
			}
			if got.Allowed != tc.wantAllow {
				t.Fatalf("Allowed = %v，期望 %v（%+v）", got.Allowed, tc.wantAllow, got)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q，期望 %q", got.Reason, tc.wantReason)
			}
			if got.Allowed && got.MatchedGrantID != grantFulfilment {
				t.Fatalf("放行未给出命中的 grant：%+v", got)
			}
		})
	}
}

// TestEvaluateReasons 覆盖 NOT_IN_GRANT 与 EXPIRED 的区分，以及拒绝结果的形状。
func TestEvaluateReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants []Grant
		agent  string
		action string
		want   string
	}{
		{
			name:   "动作不在白名单",
			grants: grantAt(fixedNow.Add(time.Hour), deductEntry(nil)),
			agent:  agentFulfilment,
			action: actionRefund,
			want:   ReasonNotInGrant,
		},
		{
			name: "授权属于别的 agent",
			grants: []Grant{{
				GrantID:   grantFulfilment,
				Agent:     "ag_other",
				Allow:     []GrantEntry{deductEntry(nil)},
				ExpiresAt: fixedNow.Add(time.Hour),
			}},
			agent:  agentFulfilment,
			action: actionDeduct,
			want:   ReasonNotInGrant,
		},
		{
			name:   "没有任何授权",
			grants: nil,
			agent:  agentFulfilment,
			action: actionDeduct,
			want:   ReasonNotInGrant,
		},
		{
			name:   "授权已过期",
			grants: grantAt(fixedNow.Add(-time.Second), deductEntry(nil)),
			agent:  agentFulfilment,
			action: actionDeduct,
			want:   ReasonExpired,
		},
		{
			name:   "到期时刻当下即算过期",
			grants: grantAt(fixedNow, deductEntry(nil)),
			agent:  agentFulfilment,
			action: actionDeduct,
			want:   ReasonExpired,
		},
		{
			name: "只有过期的那条提到该动作",
			grants: []Grant{
				{
					GrantID:   grantNarrow,
					Agent:     agentFulfilment,
					Allow:     []GrantEntry{deductEntry(nil)},
					ExpiresAt: fixedNow.Add(-time.Hour),
				},
				{
					GrantID: grantWide,
					Agent:   agentFulfilment,
					Allow: []GrantEntry{{
						Module:  "inventory",
						Actions: []string{actionGet},
					}},
					ExpiresAt: fixedNow.Add(time.Hour),
				},
			},
			agent:  agentFulfilment,
			action: actionDeduct,
			want:   ReasonExpired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
			e := engine(tc.grants, &now)
			got, err := e.Evaluate(tc.agent, "user:alice", tc.action, 1)
			if err != nil {
				t.Fatalf("Evaluate 不应报错：%v", err)
			}
			if got.Allowed || got.Reason != tc.want {
				t.Fatalf("得到 %+v，期望 reason=%s", got, tc.want)
			}
			// 拒绝报文是封闭结构：只有 allowed 与 reason。
			if got.MatchedGrantID != "" || got.Quota != nil {
				t.Fatalf("拒绝结果不该夹带 matchedGrantId/quota：%+v", got)
			}
		})
	}
}

// TestEvaluateQuotaWindow 是计划里的配额用例：6 + 6 > 10，第二次必须被拒。
func TestEvaluateQuotaWindow(t *testing.T) {
	now := fixedNow
	e := engine(grantAt(fixedNow.Add(time.Hour),
		deductEntry(&GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 10})), &now)

	first, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 6)
	if err != nil || !first.Allowed {
		t.Fatalf("6 ≤ 10 应放行：%+v %v", first, err)
	}
	if first.Quota == nil || first.Quota.Remaining != 4 || first.Quota.Unit != UnitQuantity {
		t.Fatalf("余量应为 4 quantity：%+v", first.Quota)
	}

	second, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 6)
	if err != nil {
		t.Fatalf("Evaluate 不应报错：%v", err)
	}
	if second.Allowed || second.Reason != ReasonQuotaExceeded {
		t.Fatalf("累计 12 > 10 应拒：%+v", second)
	}
}

// TestEvaluateQuotaIsSharedPerGrantAndWindow 固定一条判断：配额按 (grantId, window) 记账，
// 同一 grant 内两条 entry 用同一个 window 时共用一个计数器。
func TestEvaluateQuotaIsSharedPerGrantAndWindow(t *testing.T) {
	now := fixedNow
	constraints := &GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 10}
	e := engine([]Grant{{
		GrantID:   grantFulfilment,
		Agent:     agentFulfilment,
		ExpiresAt: fixedNow.Add(time.Hour),
		Allow: []GrantEntry{
			{Module: "inventory", Actions: []string{actionDeduct}, Constraints: constraints},
			{Module: "inventory", Actions: []string{actionGet}, Constraints: constraints},
		},
	}}, &now)

	if got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 6); err != nil || !got.Allowed {
		t.Fatalf("扣减 6 应放行：%+v %v", got, err)
	}
	got, err := e.Evaluate(agentFulfilment, "user:alice", actionGet, 6)
	if err != nil {
		t.Fatalf("Evaluate 不应报错：%v", err)
	}
	if got.Allowed || got.Reason != ReasonQuotaExceeded {
		t.Fatalf("同一 grant 同一窗口应共享计数：%+v", got)
	}
}

// TestEvaluateMaxQtyPerCall 验证单次上限：超限直接拒，不做部分放行，也不记账。
func TestEvaluateMaxQtyPerCall(t *testing.T) {
	now := fixedNow
	e := engine(grantAt(fixedNow.Add(time.Hour),
		deductEntry(&GrantConstraints{MaxQtyPerCall: 5, Window: "P1D", MaxUnitsPerWindow: 10})), &now)

	over, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 6)
	if err != nil {
		t.Fatalf("Evaluate 不应报错：%v", err)
	}
	if over.Allowed || over.Reason != ReasonQuotaExceeded {
		t.Fatalf("单次 6 > 5 应拒：%+v", over)
	}

	ok, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 5)
	if err != nil || !ok.Allowed {
		t.Fatalf("单次 5 ≤ 5 应放行：%+v %v", ok, err)
	}
	if ok.Quota == nil || ok.Quota.Remaining != 5 {
		t.Fatalf("被拒的那次不该记账：%+v", ok.Quota)
	}
}

// TestEvaluateWindowRolls 验证滚动窗口：窗口外的旧记录滚出去，额度重新可用。
func TestEvaluateWindowRolls(t *testing.T) {
	now := fixedNow
	e := engine(grantAt(fixedNow.Add(72*time.Hour),
		deductEntry(&GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 10})), &now)

	if got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 10); err != nil || !got.Allowed {
		t.Fatalf("首次 10 应放行：%+v %v", got, err)
	}

	now = fixedNow.Add(23 * time.Hour) // 还在窗口内
	got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
	if err != nil {
		t.Fatalf("Evaluate 不应报错：%v", err)
	}
	if got.Allowed || got.Reason != ReasonQuotaExceeded {
		t.Fatalf("23 小时后仍在窗口内应拒：%+v", got)
	}

	now = fixedNow.Add(25 * time.Hour) // 首条记录已滚出窗口
	if rolled, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 10); err != nil || !rolled.Allowed {
		t.Fatalf("记录滚出窗口后应重新放行：%+v %v", rolled, err)
	}
}

// TestEvaluateQuotaUnits 验证计量单位：calls 每次记 1，quantity 按数值累加，未知单位报错。
func TestEvaluateQuotaUnits(t *testing.T) {
	constraints := &GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 3}

	t.Run("calls 每次记 1", func(t *testing.T) {
		now := fixedNow
		e := NewGrantEngine(grantAt(fixedNow.Add(time.Hour), deductEntry(constraints)),
			WithSubjects(subjects), WithClock(clock(&now)),
			WithQuotaUnits(map[string]string{actionDeduct: UnitCalls}))

		for i := 1; i <= 3; i++ {
			// 业务量再大，calls 也只记 1。
			got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 99)
			if err != nil || !got.Allowed {
				t.Fatalf("第 %d 次应放行：%+v %v", i, got, err)
			}
			if want := float64(3 - i); got.Quota == nil || got.Quota.Remaining != want || got.Quota.Unit != UnitCalls {
				t.Fatalf("第 %d 次余量 = %+v，期望 %v calls", i, got.Quota, want)
			}
		}
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
		if err != nil {
			t.Fatalf("Evaluate 不应报错：%v", err)
		}
		if got.Allowed || got.Reason != ReasonQuotaExceeded {
			t.Fatalf("第 4 次应超配额：%+v", got)
		}
	})

	t.Run("quantity 按数值累加", func(t *testing.T) {
		now := fixedNow
		e := engine(grantAt(fixedNow.Add(time.Hour), deductEntry(constraints)), &now)

		if got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 2); err != nil || !got.Allowed {
			t.Fatalf("2 ≤ 3 应放行：%+v %v", got, err)
		} else if got.Quota == nil || got.Quota.Remaining != 1 || got.Quota.Unit != UnitQuantity {
			t.Fatalf("余量应为 1 quantity：%+v", got.Quota)
		}
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 2)
		if err != nil {
			t.Fatalf("Evaluate 不应报错：%v", err)
		}
		if got.Allowed || got.Reason != ReasonQuotaExceeded {
			t.Fatalf("累计 4 > 3 应拒：%+v", got)
		}
	})

	t.Run("未知单位是配置错误", func(t *testing.T) {
		now := fixedNow
		e := NewGrantEngine(grantAt(fixedNow.Add(time.Hour), deductEntry(constraints)),
			WithSubjects(subjects), WithClock(clock(&now)),
			WithQuotaUnits(map[string]string{actionDeduct: "money"}))

		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
		if !errors.Is(err, ErrUnsupportedQuotaUnit) {
			t.Fatalf("期望 ErrUnsupportedQuotaUnit，得到 %v", err)
		}
		if got.Allowed {
			t.Fatal("配置读不懂时不得放行")
		}
	})
}

// TestEvaluateUnsupportedWindowErrors 验证不受支持的窗口写法返回 error 而不是放行。
func TestEvaluateUnsupportedWindowErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		window    string
		maxUnits  float64
		quotaNote string
	}{
		{name: "月份", window: "P1M", maxUnits: 10, quotaNote: "PnM 不在支持列表"},
		{name: "分钟", window: "PT30M", maxUnits: 10, quotaNote: "PTnM 不在支持列表"},
		{name: "年", window: "P1Y", maxUnits: 10, quotaNote: "PnY 不在支持列表"},
		{name: "混合写法", window: "P1DT12H", maxUnits: 10, quotaNote: "混合不属于 PnD/PnW/PTnH"},
		{name: "零长度", window: "P0D", maxUnits: 10, quotaNote: "零长度窗口语义矛盾"},
		{name: "前导零", window: "P01D", maxUnits: 10, quotaNote: "前导零读不准"},
		{name: "小写", window: "p1d", maxUnits: 10, quotaNote: "ISO-8601 设计符是大写"},
		{name: "没设上限也要读懂", window: "P1M", maxUnits: 0, quotaNote: "配置错误不该被忽略"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
			e := engine(grantAt(fixedNow.Add(time.Hour),
				deductEntry(&GrantConstraints{Window: tc.window, MaxUnitsPerWindow: tc.maxUnits})), &now)

			got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
			if !errors.Is(err, ErrUnsupportedWindow) {
				t.Fatalf("%s：期望 ErrUnsupportedWindow，得到 %v", tc.quotaNote, err)
			}
			if got.Allowed {
				t.Fatal("窗口读不懂时不得放行")
			}
			if !strings.Contains(err.Error(), "UNSUPPORTED_WINDOW") {
				t.Fatalf("错误文本应带错误码 UNSUPPORTED_WINDOW：%v", err)
			}
		})
	}
}

// TestParseWindow 是窗口解析的表驱动用例：只认 PnD / PnW / PTnH。
func TestParseWindow(t *testing.T) {
	for _, tc := range []struct {
		window  string
		want    time.Duration
		wantErr bool
	}{
		{window: "P1D", want: 24 * time.Hour},
		{window: "P7D", want: 7 * 24 * time.Hour},
		{window: "P1W", want: 7 * 24 * time.Hour},
		{window: "P2W", want: 14 * 24 * time.Hour},
		{window: "PT1H", want: time.Hour},
		{window: "PT24H", want: 24 * time.Hour},
		{window: "", wantErr: true},
		{window: "P", wantErr: true},
		{window: "PT", wantErr: true},
		{window: "1D", wantErr: true},
		{window: "P1", wantErr: true},
		{window: "p1d", wantErr: true},
		{window: "P1M", wantErr: true},
		{window: "P1Y", wantErr: true},
		{window: "PT30M", wantErr: true},
		{window: "PT1S", wantErr: true},
		{window: "P1DT12H", wantErr: true},
		{window: "P1.5D", wantErr: true},
		{window: "P0D", wantErr: true},
		{window: "P01D", wantErr: true},
		{window: "P-1D", wantErr: true},
		{window: "P9223372036854775807D", wantErr: true},  // 数字合法但会溢出 time.Duration
		{window: "P99999999999999999999D", wantErr: true}, // 超出 int64，连解析都过不去
	} {
		t.Run(tc.window, func(t *testing.T) {
			got, err := parseWindow(tc.window)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupportedWindow) {
					t.Fatalf("parseWindow(%q) 期望 ErrUnsupportedWindow，得到 %v（%v）", tc.window, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWindow(%q) 不应报错：%v", tc.window, err)
			}
			if got != tc.want {
				t.Fatalf("parseWindow(%q) = %v，期望 %v", tc.window, got, tc.want)
			}
		})
	}
}

// TestEvaluateNarrowestGrantFirst 验证「最小满足者匹配」：命中多条时取白名单最窄的那条，
// 且与输入顺序无关。
func TestEvaluateNarrowestGrantFirst(t *testing.T) {
	narrow := Grant{
		GrantID:   grantNarrow,
		Agent:     agentFulfilment,
		Allow:     []GrantEntry{{Module: "inventory", Actions: []string{actionDeduct}}},
		ExpiresAt: fixedNow.Add(time.Hour),
	}
	wide := Grant{
		GrantID: grantWide,
		Agent:   agentFulfilment,
		Allow: []GrantEntry{{
			Module:  "inventory",
			Actions: []string{actionDeduct, actionGet, actionRefund},
		}},
		ExpiresAt: fixedNow.Add(time.Hour),
	}

	for _, tc := range []struct {
		name   string
		grants []Grant
	}{
		{name: "窄的在前", grants: []Grant{narrow, wide}},
		{name: "窄的在后", grants: []Grant{wide, narrow}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
			e := engine(tc.grants, &now)
			got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
			if err != nil || !got.Allowed {
				t.Fatalf("应放行：%+v %v", got, err)
			}
			if got.MatchedGrantID != narrow.GrantID {
				t.Fatalf("命中 %q，期望最窄的 %q", got.MatchedGrantID, narrow.GrantID)
			}
		})
	}
}

// TestEvaluateTieBreaksByGrantID 验证同样宽的两条授权按 grantId 字典序取，
// 结果仍然与输入顺序无关（确定性比「随便挑一条」重要：记账要能复现）。
func TestEvaluateTieBreaksByGrantID(t *testing.T) {
	first := Grant{
		GrantID:   grantNarrow, // gr-01JNARROW
		Agent:     agentFulfilment,
		Allow:     []GrantEntry{{Module: "inventory", Actions: []string{actionDeduct}}},
		ExpiresAt: fixedNow.Add(time.Hour),
	}
	second := Grant{
		GrantID:   grantWide, // gr-01JWIDE01：字典序更大
		Agent:     agentFulfilment,
		Allow:     []GrantEntry{{Module: "inventory", Actions: []string{actionDeduct}}},
		ExpiresAt: fixedNow.Add(time.Hour),
	}

	for _, tc := range []struct {
		name   string
		grants []Grant
	}{
		{name: "字典序小的在前", grants: []Grant{first, second}},
		{name: "字典序小的在后", grants: []Grant{second, first}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
			e := engine(tc.grants, &now)
			got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
			if err != nil || !got.Allowed {
				t.Fatalf("应放行：%+v %v", got, err)
			}
			if got.MatchedGrantID != first.GrantID {
				t.Fatalf("同宽时应按 grantId 取 %q，实际 %q", first.GrantID, got.MatchedGrantID)
			}
		})
	}
}

// TestClampRemaining 固定「余量不得为负」这条 schema 约束的守卫行为：
// 浮点结合律不同会让 windowMax - used - charge 出现极小的负数。
func TestClampRemaining(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   float64
		want float64
	}{
		{name: "正常值", in: 4, want: 4},
		{name: "零", in: 0, want: 0},
		{name: "浮点负残差", in: -1e-16, want: 0},
		{name: "NaN", in: math.NaN(), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampRemaining(tc.in); got != tc.want {
				t.Fatalf("clampRemaining(%v) = %v，期望 %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestEvaluateQuotaFallsThroughToWiderGrant 验证窄授权满额后落到宽授权上：
// 两条授权的窗口互相独立，总可用量是各自上限之和。
func TestEvaluateQuotaFallsThroughToWiderGrant(t *testing.T) {
	now := fixedNow
	narrow := Grant{
		GrantID:   grantNarrow,
		Agent:     agentFulfilment,
		ExpiresAt: fixedNow.Add(time.Hour),
		Allow:     []GrantEntry{deductEntry(&GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 5})},
	}
	wide := Grant{
		GrantID: grantWide,
		Agent:   agentFulfilment,
		Allow: []GrantEntry{{
			Module:      "inventory",
			Actions:     []string{actionDeduct, actionGet},
			Constraints: &GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 10},
		}},
		ExpiresAt: fixedNow.Add(time.Hour),
	}
	e := engine([]Grant{wide, narrow}, &now)

	first, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 5)
	if err != nil || !first.Allowed || first.MatchedGrantID != grantNarrow {
		t.Fatalf("首批 5 应记在窄授权上：%+v %v", first, err)
	}
	second, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 5)
	if err != nil || !second.Allowed || second.MatchedGrantID != grantWide {
		t.Fatalf("窄授权满额后应落到宽授权上：%+v %v", second, err)
	}
	if second.Quota == nil || second.Quota.Remaining != 5 {
		t.Fatalf("宽授权余量应为 5：%+v", second.Quota)
	}
	// 窄的已满 5/5、宽的已用 5/10：再要 6 会把宽的顶穿（5+6 > 10）。
	third, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 6)
	if err != nil {
		t.Fatalf("Evaluate 不应报错：%v", err)
	}
	if third.Allowed || third.Reason != ReasonQuotaExceeded {
		t.Fatalf("两条额度都不够时应拒：%+v", third)
	}
}

// TestEvaluateConcurrentQuota 让 100 个 goroutine 同时抢同一份额度，断言三件事：
// 放行次数恰好等于「上限 ÷ 每次量」、其余全部 QUOTA_EXCEEDED、结束后余量精确无泄漏。
//
// 为什么这几个不变量能当并发证明用：配额判定是「读已用 → 比上限 → 记账」三步，
// 若临界区被拆开，两个 goroutine 会同时读到旧的已用量、双双放行——每次量 > 1 时更敏感
// （3 单位放行 4 次 = 12 > 10，一眼看出超额记账）。收尾再要「正好剩余的量」必须放行、
// 再要 1 单位必须拒，这条能排掉「放行数对但余量记错」的撕裂。
//
// 本机无法跑 -race（CGO_ENABLED=0 且无 C 编译器），所以并发正确性靠这里的端到端不变量
// 加 Evaluate 的锁范围（整个函数体一个临界区，usage 的读写全部在锁内）来论证。
func TestEvaluateConcurrentQuota(t *testing.T) {
	const workers = 100

	for _, tc := range []struct {
		name        string
		amount      float64 // 每次调用要的业务量
		quota       float64 // 窗口上限
		wantAllowed int32
	}{
		{name: "每次 1 单位", amount: 1, quota: 10, wantAllowed: 10},
		{name: "每次 3 单位", amount: 3, quota: 10, wantAllowed: 3},
		{name: "每次 7 单位", amount: 7, quota: 10, wantAllowed: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
			e := engine(grantAt(fixedNow.Add(time.Hour),
				deductEntry(&GrantConstraints{Window: "P1D", MaxUnitsPerWindow: tc.quota})), &now)

			var (
				wg      sync.WaitGroup
				allowed atomic.Int32
				denied  atomic.Int32
				broken  atomic.Int32
			)
			start := make(chan struct{})
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // 一起冲进 Evaluate，让「读已用」尽量撞在一起
					got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, tc.amount)
					switch {
					case err != nil:
						broken.Add(1)
					case got.Allowed && got.MatchedGrantID == grantFulfilment:
						allowed.Add(1)
					case !got.Allowed && got.Reason == ReasonQuotaExceeded:
						denied.Add(1)
					default:
						broken.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()

			if got := allowed.Load(); got != tc.wantAllowed {
				t.Fatalf("放行 %d 次，期望恰好 %d 次（配额被击穿或临界区被拆开）", got, tc.wantAllowed)
			}
			if got := denied.Load(); got != workers-tc.wantAllowed {
				t.Fatalf("拒绝 %d 次，期望 %d 次", got, workers-tc.wantAllowed)
			}
			if got := broken.Load(); got != 0 {
				t.Fatalf("出现 %d 次异常结果", got)
			}

			// 收尾：先要光剩下的量（必须放行），再要 1 单位（必须拒）——记账没有泄漏或重复。
			if rest := tc.quota - float64(allowed.Load())*tc.amount; rest > 0 {
				fill, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, rest)
				if err != nil || !fill.Allowed {
					t.Fatalf("剩余 %v 应放行：%+v %v", rest, fill, err)
				}
			}
			last, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
			if err != nil {
				t.Fatalf("Evaluate 不应报错：%v", err)
			}
			if last.Allowed || last.Reason != ReasonQuotaExceeded {
				t.Fatalf("额度用尽后应拒：%+v", last)
			}
		})
	}
}

// TestEvaluateFailClosedDefaults 验证几处「默认可疑就拒」的判断：
// 未装配委托人权限表、nil 引擎、负数量与 NaN 不能刷高余额。
func TestEvaluateFailClosedDefaults(t *testing.T) {
	t.Run("未装配委托人权限表", func(t *testing.T) {
		now := fixedNow
		e := NewGrantEngine(grantAt(fixedNow.Add(time.Hour), deductEntry(nil)), WithClock(clock(&now)))
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
		if err != nil {
			t.Fatalf("Evaluate 不应报错：%v", err)
		}
		if got.Allowed || got.Reason != ReasonSubjectNotPermitted {
			t.Fatalf("没有权限来源时不得默认放行：%+v", got)
		}
	})

	t.Run("nil 引擎不 panic", func(t *testing.T) {
		var e *GrantEngine
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
		if err != nil || got.Allowed {
			t.Fatalf("nil 引擎应按拒绝处理：%+v %v", got, err)
		}
	})

	t.Run("WithClock(nil) 退回真实时钟", func(t *testing.T) {
		// 用真实时间造一条一小时后到期的授权：时钟回退失败会 panic 或误判过期。
		grants := grantAt(time.Now().Add(time.Hour), deductEntry(nil))
		e := NewGrantEngine(grants, WithSubjects(subjects), WithClock(nil))
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
		if err != nil || !got.Allowed {
			t.Fatalf("WithClock(nil) 应退回 time.Now：%+v %v", got, err)
		}
	})

	t.Run("负数量与 NaN 不能刷高余额", func(t *testing.T) {
		now := fixedNow
		e := engine(grantAt(fixedNow.Add(time.Hour),
			deductEntry(&GrantConstraints{Window: "P1D", MaxUnitsPerWindow: 10})), &now)

		// 负调整按绝对值占配额：|-1| = 1。
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, -1)
		if err != nil || !got.Allowed {
			t.Fatalf("负数量应放行但按 1 记账：%+v %v", got, err)
		}
		if got.Quota == nil || got.Quota.Remaining != 9 {
			t.Fatalf("余量应为 9：%+v", got.Quota)
		}

		// NaN 记 0：既不放大量，也不让余量变 NaN。
		nan, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, math.NaN())
		if err != nil || !nan.Allowed {
			t.Fatalf("NaN 业务量应视为 0 放行：%+v %v", nan, err)
		}
		if nan.Quota == nil || nan.Quota.Remaining != 9 {
			t.Fatalf("NaN 不该改变余量：%+v", nan.Quota)
		}

		// 负数不能当「充电宝」：|-100| 超上限，必须拒。
		huge, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, -100)
		if err != nil {
			t.Fatalf("Evaluate 不应报错：%v", err)
		}
		if huge.Allowed || huge.Reason != ReasonQuotaExceeded {
			t.Fatalf("负数量的绝对值超限应拒：%+v", huge)
		}
	})

	t.Run("无配额约束时省略 quota", func(t *testing.T) {
		now := fixedNow
		e := engine(grantAt(fixedNow.Add(time.Hour), deductEntry(nil)), &now)
		got, err := e.Evaluate(agentFulfilment, "user:alice", actionDeduct, 1)
		if err != nil || !got.Allowed {
			t.Fatalf("应放行：%+v %v", got, err)
		}
		if got.Quota != nil {
			t.Fatalf("不限量时没有可报的余量：%+v", got.Quota)
		}
	})
}

// TestJSONAlignsWithSchema 用 grant.schema.json 的字段名反序列化，并检查评估结果的报文形状：
// 字段名写错会在这里露馅，放行/拒绝两种结果都不能多带字段。
func TestJSONAlignsWithSchema(t *testing.T) {
	const sample = `{
	  "grantId": "gr-01JSAMPLE01",
	  "agent": "ag_fulfillment",
	  "allow": [
	    {
	      "module": "inventory",
	      "actions": ["inventory.GetStock", "inventory.DeductStock"],
	      "constraints": { "maxQtyPerCall": 20, "window": "P1D", "maxUnitsPerWindow": 100 }
	    }
	  ],
	  "expiresAt": "2027-01-01T00:00:00Z"
	}`

	var g Grant
	if err := json.Unmarshal([]byte(sample), &g); err != nil {
		t.Fatalf("反序列化失败（字段名与 schema 不对齐？）：%v", err)
	}
	if g.GrantID != "gr-01JSAMPLE01" || g.Agent != agentFulfilment || len(g.Allow) != 1 {
		t.Fatalf("顶层字段没落到结构体：%+v", g)
	}
	if want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); !g.ExpiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v，期望 %v", g.ExpiresAt, want)
	}
	entry := g.Allow[0]
	if entry.Module != "inventory" || len(entry.Actions) != 2 {
		t.Fatalf("entry 字段没落到结构体：%+v", entry)
	}
	if entry.Constraints == nil {
		t.Fatal("constraints 没落到结构体")
	}
	if c := *entry.Constraints; c.MaxQtyPerCall != 20 || c.Window != "P1D" || c.MaxUnitsPerWindow != 100 {
		t.Fatalf("constraints 字段没落到结构体：%+v", c)
	}

	allowed, err := json.Marshal(GrantResult{
		Allowed:        true,
		MatchedGrantID: grantFulfilment,
		Quota:          &QuotaStatus{Remaining: 4, Unit: UnitQuantity},
	})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if want := `{"allowed":true,"matchedGrantId":"gr-01JFULFILMENT","quota":{"remaining":4,"unit":"quantity"}}`; string(allowed) != want {
		t.Fatalf("放行报文 = %s，期望 %s", allowed, want)
	}

	denied, err := json.Marshal(GrantResult{Reason: ReasonQuotaExceeded})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if want := `{"allowed":false,"reason":"QUOTA_EXCEEDED"}`; string(denied) != want {
		t.Fatalf("拒绝报文 = %s，期望 %s", denied, want)
	}
}
