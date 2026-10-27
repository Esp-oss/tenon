package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tenon.dev/internal/spec"
)

// 这个测试用 httptest 造一个「合规的假模块」，验证 runner 自己是对的：
// C1/C2/C3/C8/C9 判 PASS，C4/C5/D1 判 SKIP 且给了原因。
// 它不依赖 cmd/inventory 等真实服务，所以能进 CI 而不需要起进程。

const fakeManifest = `{
  "tenon": "0.1",
  "module": { "id": "fakeinv", "version": "1.0.0", "title": "假库存" },
  "actions": [
    { "id": "fakeinv.GetStock", "version": "1.0.0", "title": "查库存", "kind": "read",
      "risk": { "level": "low", "reversible": true }, "dryRun": { "supported": true } },
    { "id": "fakeinv.DeductStock", "version": "1.0.0", "title": "扣库存", "kind": "write",
      "risk": { "level": "medium", "reversible": true }, "dryRun": { "supported": true } },
    { "id": "fakeinv.ReserveStock", "version": "1.0.0", "title": "预留库存", "kind": "write",
      "risk": { "level": "medium", "reversible": true }, "dryRun": { "supported": false } }
  ],
  "events": [ { "type": "stock.deducted", "version": "1.0.0" } ],
  "transports": ["http"],
  "capabilities": ["dry-run"]
}`

func fakeAction(id, kind string, dryRun bool, withIdem bool) string {
	idem := ""
	if withIdem {
		idem = `,
      "idempotency": { "keyFields": ["sku","referenceNo"], "scope": "module", "duplicateBehavior": "return_first_result" }`
	}
	dr := "false"
	if dryRun {
		dr = "true"
	}
	return fmt.Sprintf(`{
      "id": "%s", "version": "1.0.0", "title": "t", "kind": "%s",
      "inputSchema": { "type": "object", "required": ["sku","qty","referenceNo"] },
      "outputSchema": { "type": "object" },
      "risk": { "level": "medium", "reversible": true },
      "timeoutMs": 5000,
      "dryRun": { "supported": %s }%s
    }`, id, kind, dr, idem)
}

type fake struct {
	mu       sync.Mutex
	calls    int
	idemSeen map[string][]byte
}

func (f *fake) handler() http.Handler {
	mux := http.NewServeMux()
	actions := map[string]string{
		"fakeinv.GetStock":     fakeAction("fakeinv.GetStock", "read", true, false),
		"fakeinv.DeductStock":  fakeAction("fakeinv.DeductStock", "write", true, true),
		"fakeinv.ReserveStock": fakeAction("fakeinv.ReserveStock", "write", false, true),
	}
	dryRunSupported := map[string]bool{
		"fakeinv.GetStock":     true,
		"fakeinv.DeductStock":  true,
		"fakeinv.ReserveStock": false,
	}

	mux.HandleFunc("/.well-known/tenon", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeManifest))
	})
	mux.HandleFunc("/tenon/actions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/tenon/actions/")
		isDryRun := strings.HasSuffix(path, ":dry-run")
		id := strings.TrimSuffix(path, ":dry-run")

		full, ok := actions[id]
		if !ok {
			writeErr(w, http.StatusNotFound, "contract_violation", "UNKNOWN_ACTION")
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(full))
			return
		}
		if isDryRun {
			if !dryRunSupported[id] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"dryRun":{"status":"verified"}}`))
			return
		}

		key := r.Header.Get("Idempotency-Key")
		var args struct {
			Qty int `json:"qty"`
		}
		_ = json.NewDecoder(r.Body).Decode(&args)

		f.mu.Lock()
		defer f.mu.Unlock()
		if key != "" {
			if prev, seen := f.idemSeen[key]; seen {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(prev)
				return
			}
		}
		if args.Qty > 10 {
			body := `{"tenonError":{"class":"business_rejection","code":"STOCK_INSUFFICIENT",
			          "message":"不够","details":{"available":3},"retryable":false}}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(body))
			return
		}
		f.calls++
		body := []byte(fmt.Sprintf(`{"onHand":%d}`, 100-f.calls))
		if key != "" {
			f.idemSeen[key] = body
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	return mux
}

func writeErr(w http.ResponseWriter, status int, class, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(
		`{"tenonError":{"class":%q,"code":%q,"message":"m","retryable":false}}`, class, code)))
}

func TestRunAgainstCompliantFakeModule(t *testing.T) {
	f := &fake{idemSeen: map[string][]byte{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	set, err := spec.Load(filepath.Join("..", "spec", "0.1", "schemas"))
	if err != nil {
		t.Fatalf("加载 schema 失败：%v", err)
	}

	fixtures := &Fixtures{Modules: []ModuleFixture{{
		Module: "fakeinv",
		Invocations: []Invocation{
			{
				Name: "deduct-ok", Action: "fakeinv.DeductStock",
				Args:   json.RawMessage(`{"sku":"S1","qty":1,"referenceNo":"R1"}`),
				Expect: Expectation{Status: 200, Idempotent: true, UnknownKey: true},
			},
			{
				Name: "deduct-too-much", Action: "fakeinv.DeductStock",
				Args: json.RawMessage(`{"sku":"S1","qty":999,"referenceNo":"R2"}`),
				Expect: Expectation{
					Status: 422, ErrorClass: "business_rejection", ErrorCode: "STOCK_INSUFFICIENT",
				},
			},
		},
	}}}

	rep := Run(context.Background(), Options{
		Target: srv.URL, Spec: set, Fixtures: fixtures, ModuleID: "fakeinv",
	})

	byCase := map[string]Status{}
	for _, r := range rep.Results {
		byCase[r.Case] = r.Status
		if r.Status == Fail {
			t.Errorf("%s 判 FAIL：%s", r.Case, r.Detail)
		}
	}

	// C9 只为「声明了期望错误」的调用生成子用例，所以这里只期望一条。
	for _, want := range []string{"C1", "C2", "C3", "C8", "C9.deduct-too-much"} {
		if got, ok := byCase[want]; !ok || got != Pass {
			t.Errorf("%s 期望 PASS，实际 %q（存在=%v）", want, got, ok)
		}
	}
	for _, wantSkip := range []string{"C10", "C4", "C5", "D1"} {
		if got := byCase[wantSkip]; got != Skip {
			t.Errorf("%s 期望 SKIP，实际 %q", wantSkip, got)
		}
	}

	if code := rep.ExitCode(false); code != 0 {
		t.Errorf("有 SKIP 但无 FAIL 时退出码应为 0，实际 %d", code)
	}
	if code := rep.ExitCode(true); code != 2 {
		t.Errorf("--strict 下有 SKIP 退出码应为 2，实际 %d", code)
	}
	pass, fail, skip := rep.Counts()
	t.Logf("pass=%d fail=%d skip=%d", pass, fail, skip)
}

// 反向用例：manifest 摘要里塞 inputSchema 必须判 C1 FAIL——
// 这正是「按需拉取、控上下文」被破坏时的可观测信号。
func TestC1FailsWhenManifestLeaksInputSchema(t *testing.T) {
	bad := strings.Replace(fakeManifest,
		`"kind": "write",
      "risk"`,
		`"kind": "write",
      "inputSchema": {"type":"object"},
      "risk"`, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/tenon" {
			_, _ = w.Write([]byte(bad))
			return
		}
		writeErr(w, 404, "contract_violation", "UNKNOWN_ACTION")
	}))
	defer srv.Close()

	set, err := spec.Load(filepath.Join("..", "spec", "0.1", "schemas"))
	if err != nil {
		t.Fatalf("加载 schema 失败：%v", err)
	}
	rep := Run(context.Background(), Options{Target: srv.URL, Spec: set})
	if len(rep.Results) == 0 || rep.Results[0].Case != "C1" {
		t.Fatalf("结果顺序异常：%+v", rep.Results)
	}
	if rep.Results[0].Status != Fail {
		t.Fatalf("摘要泄漏 inputSchema 应判 FAIL，实际 %q", rep.Results[0].Status)
	}
	if !strings.Contains(rep.Results[0].Detail, "inputSchema") {
		t.Errorf("失败原因应点名 inputSchema：%s", rep.Results[0].Detail)
	}
}
