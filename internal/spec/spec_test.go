package spec

import (
	"strings"
	"testing"
)

func mustLoad(t *testing.T) *Set {
	t.Helper()
	s, err := Load("../../spec/0.1/schemas")
	if err != nil {
		t.Fatalf("加载 schema 失败：%v", err)
	}
	return s
}

// TestLoadAllSchemas 锁定「九类契约 + common」共 10 份，缺一份就说明真源被动了。
func TestLoadAllSchemas(t *testing.T) {
	s := mustLoad(t)
	names := s.Names()
	if len(names) != 10 {
		t.Fatalf("期望 10 份 schema，实际 %d：%v", len(names), names)
	}
	for _, want := range []string{
		"common.schema.json", "module-manifest.schema.json", "action.schema.json",
		"domain-event.schema.json", "plan.schema.json", "frames.schema.json",
		"grant.schema.json", "policy.schema.json", "audit-event.schema.json", "error.schema.json",
	} {
		if _, err := s.Schema(want); err != nil {
			t.Errorf("缺少 %s: %v", want, err)
		}
	}
}

// validWriteAction 是一个合法的写动作实例（字段按 action.schema.json 的 required）。
const validWriteAction = `{
  "id": "inventory.DeductStock",
  "version": "1.0.0",
  "title": "扣减库存",
  "kind": "write",
  "inputSchema": { "type": "object", "required": ["sku","qty","referenceNo"] },
  "outputSchema": { "type": "object" },
  "risk": { "level": "medium", "reversible": true },
  "idempotency": { "keyFields": ["sku","referenceNo"], "scope": "module", "duplicateBehavior": "return_first_result" },
  "timeoutMs": 5000,
  "dryRun": { "supported": true }
}`

// TestActionSchemaPositive 是正反对拍里的「正」：合法实例必须过。
func TestActionSchemaPositive(t *testing.T) {
	s := mustLoad(t)
	if err := s.Validate("action.schema.json", []byte(validWriteAction)); err != nil {
		t.Fatalf("合法写动作被拒绝：%v", err)
	}
}

// TestActionSchemaNegative 是正反对拍里的「反」：每条已知违规都必须挂。
func TestActionSchemaNegative(t *testing.T) {
	s := mustLoad(t)
	cases := []struct {
		name string
		json string
		want string // 期望错误里出现的关键字（宽松匹配）
	}{
		{
			name: "write 缺 idempotency",
			json: `{"id":"inventory.DeductStock","version":"1.0.0","title":"t","kind":"write",
			        "inputSchema":{"type":"object"},"outputSchema":{"type":"object"},
			        "risk":{"level":"medium","reversible":true},"timeoutMs":5000,"dryRun":{"supported":true}}`,
			want: "idempotency",
		},
		{
			name: "read 声明了 idempotency",
			json: `{"id":"inventory.GetStock","version":"1.0.0","title":"t","kind":"read",
			        "inputSchema":{"type":"object"},"outputSchema":{"type":"object"},
			        "risk":{"level":"low","reversible":true},"timeoutMs":1000,"dryRun":{"supported":false},
			        "idempotency":{"keyFields":["sku"],"scope":"module","duplicateBehavior":"return_first_result"}}`,
			// schema 用 if/then + not 表达「read 禁止声明 idempotency」，
			// 报错是 'not' failed，不会出现 field 名——只断言失败即可。
			want: "",
		},
		{
			name: "kind 非法",
			json: `{"id":"x.Y","version":"1.0.0","title":"t","kind":"execute",
			        "inputSchema":{"type":"object"},"outputSchema":{"type":"object"},
			        "risk":{"level":"low","reversible":true},"timeoutMs":1000,"dryRun":{"supported":true}}`,
			want: "kind",
		},
		{
			name: "keyFields 为空数组",
			json: `{"id":"x.Y","version":"1.0.0","title":"t","kind":"write",
			        "inputSchema":{"type":"object"},"outputSchema":{"type":"object"},
			        "risk":{"level":"low","reversible":true},"timeoutMs":1000,"dryRun":{"supported":true},
			        "idempotency":{"keyFields":[],"scope":"module","duplicateBehavior":"return_first_result"}}`,
			want: "keyFields",
		},
		{
			name: "未知顶层字段",
			json: `{"id":"x.Y","version":"1.0.0","title":"t","kind":"read",
			        "inputSchema":{"type":"object"},"outputSchema":{"type":"object"},
			        "risk":{"level":"low","reversible":true},"timeoutMs":1000,"dryRun":{"supported":true},
			        "mysteryField":1}`,
			want: "mysteryField",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Validate("action.schema.json", []byte(c.json))
			if err == nil {
				t.Fatalf("应当失败但通过了")
			}
			if c.want != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
				t.Fatalf("错误信息没提到 %q：%v", c.want, err)
			}
			t.Logf("预期失败：%v", err)
		})
	}
}

// TestManifestRejectsInputSchemaInSummary 保证「摘要不含 inputSchema」是被 schema 强制的，
// 而不是靠实现自觉——这是控上下文成本的前提。
func TestManifestRejectsInputSchemaInSummary(t *testing.T) {
	s := mustLoad(t)
	bad := `{
	  "tenon":"0.1",
	  "module":{"id":"inventory","version":"1.4.0","title":"库存"},
	  "actions":[{"id":"inventory.DeductStock","version":"1.0.0","title":"扣减",
	              "kind":"write","risk":{"level":"medium","reversible":true},
	              "dryRun":{"supported":true},
	              "inputSchema":{"type":"object"}}],
	  "events":[{"type":"stock.deducted","version":"1.0.0"}],
	  "transports":["http"],
	  "capabilities":["dry-run"]
	}`
	if err := s.Validate("module-manifest.schema.json", []byte(bad)); err == nil {
		t.Fatal("摘要里带了 inputSchema 却通过了 schema 校验")
	}
	good := `{
	  "tenon":"0.1",
	  "module":{"id":"inventory","version":"1.4.0","title":"库存"},
	  "actions":[{"id":"inventory.DeductStock","version":"1.0.0","title":"扣减",
	              "kind":"write","risk":{"level":"medium","reversible":true},
	              "dryRun":{"supported":true}}],
	  "events":[{"type":"stock.deducted","version":"1.0.0"}],
	  "transports":["http"],
	  "capabilities":["dry-run"]
	}`
	if err := s.Validate("module-manifest.schema.json", []byte(good)); err != nil {
		t.Fatalf("合法 manifest 被拒绝：%v", err)
	}
}

// TestErrorSchemaAllClasses 覆盖四类错误，顺带验证 error.schema.json 的枚举没写歪。
//
// 注意 schema 里有一条一致性约束：class=retryable ⟺ retryable=true。
// 所以这里按类给出对应的 retryable 值，并额外断言「retryable 类却写 false」必须失败。
func TestErrorSchemaAllClasses(t *testing.T) {
	s := mustLoad(t)
	for _, class := range []string{"retryable", "business_rejection", "policy_denied", "contract_violation"} {
		flag := "false"
		if class == "retryable" {
			flag = "true"
		}
		body := `{"tenonError":{"class":"` + class + `","code":"X_CODE","message":"m","retryable":` + flag + `}}`
		if err := s.Validate("error.schema.json", []byte(body)); err != nil {
			t.Errorf("类 %s 被拒绝：%v", class, err)
		}
	}
	negatives := map[string]string{
		"非法 class":             `{"tenonError":{"class":"oops","code":"X","message":"m","retryable":false}}`,
		"retryable 类却写 false":  `{"tenonError":{"class":"retryable","code":"X","message":"m","retryable":false}}`,
		"非 retryable 类却写 true": `{"tenonError":{"class":"policy_denied","code":"X","message":"m","retryable":true}}`,
	}
	for name, body := range negatives {
		if err := s.Validate("error.schema.json", []byte(body)); err == nil {
			t.Errorf("%s 被接受（应拒绝）", name)
		}
	}
}
