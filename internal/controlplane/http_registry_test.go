package controlplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/protocol"
)

// 注册/心跳端点的用例：签名是真的（protocol.Sign），哈希是真的（sha256(JCS(manifest))）。
//
// 这里刻意**不**在用例里手写 declarationHash：手写的值只能证明「被测实现复现了我的手算」，
// 证明不了「两侧用的是同一份 JCS 规范化」。所以统一走下面的 declarationHash helper。

const testHMACSecret = "s3cr3t"

// newRegistryHost 起一个装配了共享密钥的控制平面。
//
// 只配 AuditPath 与 HMACSecret：注册/心跳不碰策略、授权与入站队列，多配它们只会让
// 失败时的排查面变大。
func newRegistryHost(t *testing.T, secret []byte) *httptest.Server {
	t.Helper()
	srv := newTestServer(t, Config{
		AuditPath:  filepath.Join(t.TempDir(), "audit.jsonl"),
		HMACSecret: secret,
	})
	return serveHost(t, srv)
}

// testManifest 是一份「键序与空白都不规范」的 manifest。
//
// 这不只是随手写的样子货：直接对**原始字节**求 sha256 与对**JCS 规范化后的字节**求
// sha256 会得到两个不同的值，所以这份报文同时守住了「必须先规范化再算哈希」。
func testManifest() []byte {
	return []byte(`{
  "tenon": "0.1",
  "transports": ["http"],
  "module": { "version": "1.0.0", "id": "inventory", "title": "库存" },
  "actions": [], "events": [], "capabilities": []
}`)
}

// declarationHash 按规范算声明哈希：sha256: + hex(sha256(JCS(manifest)))。
func declarationHash(t *testing.T, manifest []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(manifest, &v); err != nil {
		t.Fatalf("manifest 解析失败：%v", err)
	}
	canon, err := protocol.CanonicalJSON(v)
	if err != nil {
		t.Fatalf("manifest 规范化失败：%v", err)
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// registerBody 用 encoding/json 拼报文（不要手拼字符串：manifest 里的引号会破坏它）。
func registerBody(t *testing.T, manifest []byte, hash string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"module": map[string]any{
			"id":       "inventory",
			"baseURL":  "http://127.0.0.1:8081",
			"manifest": json.RawMessage(manifest),
		},
		"declarationHash": hash,
	})
	if err != nil {
		t.Fatalf("构造注册报文失败：%v", err)
	}
	return body
}

// heartbeatBody 拼心跳报文。
func heartbeatBody(t *testing.T, id, hash string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"moduleId": id, "declarationHash": hash})
	if err != nil {
		t.Fatalf("构造心跳报文失败：%v", err)
	}
	return body
}

// postWithHeader 发一个 JSON POST，签名头由调用方给（空串表示不带）。
func postWithHeader(t *testing.T, url string, body []byte, signature string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(protocol.HeaderSignature, signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败：%v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败：%v", err)
	}
	return resp.StatusCode, raw
}

// postSigned 发一个正确签名的 POST。
func postSigned(t *testing.T, url string, body, secret []byte, ts int64) (int, []byte) {
	t.Helper()
	return postWithHeader(t, url, body, protocol.Sign(secret, ts, body))
}

// registerModule 走一次完整的注册请求并断言 200，供后续心跳用例铺场。
func registerModule(t *testing.T, host *httptest.Server, manifest []byte, hash string) {
	t.Helper()
	status, raw := postSigned(t, host.URL+"/tenon/registry/register",
		registerBody(t, manifest, hash), []byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("注册应 200，得到 %d：%s", status, raw)
	}
}

// reRegister 解析 409 的报文：{"reRegister": true, "reason": "..."}。
func reRegister(t *testing.T, raw []byte) (bool, string) {
	t.Helper()
	var resp struct {
		ReRegister bool   `json:"reRegister"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("409 报文应是 {reRegister, reason}：%v（原文 %s）", err, raw)
	}
	return resp.ReRegister, resp.Reason
}

// signatures 解析 401 的 tenonError 信封，返回 code 与 details.reason。
func signatureRejection(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	var env protocol.ErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("401 报文应是 tenonError 信封：%v（原文 %s）", err, raw)
	}
	reason, _ := env.TenonError.Details["reason"].(string)
	return env.TenonError.Code, reason
}

// 用例 1：正确签名 + 正确哈希 → 200，且注册后立刻在 GET /tenon/modules 里看得见。
func TestRegistryRegisterThenVisible(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	hash := declarationHash(t, manifest)

	status, raw := postSigned(t, host.URL+"/tenon/registry/register",
		registerBody(t, manifest, hash), []byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("注册应 200，得到 %d：%s", status, raw)
	}
	var resp struct {
		Accepted bool `json:"accepted"`
		TTLSec   int  `json:"ttlSec"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("注册响应解析失败：%v（原文 %s）", err, raw)
	}
	if !resp.Accepted {
		t.Fatalf("注册响应应 accepted=true：%s", raw)
	}
	// ttlSec 必须与 TTL 常量同源：模块侧据此定心跳间隔，两处各自写死会悄悄错配。
	if want := int(registry.DefaultTTL / time.Second); resp.TTLSec != want {
		t.Fatalf("ttlSec 应为 %d，得到 %d", want, resp.TTLSec)
	}

	got, err := http.Get(host.URL + "/tenon/modules")
	if err != nil {
		t.Fatalf("GET /tenon/modules 失败：%v", err)
	}
	defer func() { _ = got.Body.Close() }()
	var listed modulesResponse
	if err := json.NewDecoder(got.Body).Decode(&listed); err != nil {
		t.Fatalf("模块列表解析失败：%v", err)
	}
	for _, entry := range listed.Modules {
		if entry.ID != "inventory" {
			continue
		}
		if entry.State != registry.StateAlive || !entry.Enabled {
			t.Fatalf("注册后应是 enabled && alive，得到 %+v", entry)
		}
		if entry.DeclarationHash != hash {
			t.Fatalf("登记的声明哈希应是 %s，得到 %s", hash, entry.DeclarationHash)
		}
		if entry.BaseURL != "http://127.0.0.1:8081" {
			t.Fatalf("登记的地址应是模块自报的 baseURL，得到 %s", entry.BaseURL)
		}
		return
	}
	t.Fatalf("注册后 GET /tenon/modules 应看得见 inventory，实际只有 %+v", listed.Modules)
}

// 用例 2：签名缺失/格式错/密钥不一致 → 401，且理由区分 format 与 invalid。
func TestRegistryRejectsBadSignature(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	body := registerBody(t, manifest, declarationHash(t, manifest))
	now := time.Now().Unix()

	cases := []struct {
		name      string
		signature string
		wantCode  string
		wantWhy   string
	}{
		{"缺签名头", "", "SIGNATURE_FORMAT", "format"},
		{"签名头不是 t=,v1=", "deadbeef", "SIGNATURE_FORMAT", "format"},
		{"密钥不一致", protocol.Sign([]byte("not-the-secret"), now, body), "SIGNATURE_INVALID", "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := postWithHeader(t, host.URL+"/tenon/registry/register", body, c.signature)
			if status != http.StatusUnauthorized {
				t.Fatalf("验签失败应 401，得到 %d：%s", status, raw)
			}
			code, why := signatureRejection(t, raw)
			if code != c.wantCode || why != c.wantWhy {
				t.Fatalf("理由应是 %s/%s，得到 %s/%s：%s", c.wantCode, c.wantWhy, code, why, raw)
			}
		})
	}

	// 心跳同样要验签：它是「轻量的存活刷新」，但同样是写入口，不能因为轻就免签。
	status, raw := postWithHeader(t, host.URL+"/tenon/registry/heartbeat",
		heartbeatBody(t, "inventory", "sha256:"+strings.Repeat("0", 64)), "")
	if status != http.StatusUnauthorized {
		t.Fatalf("心跳缺签名应 401，得到 %d：%s", status, raw)
	}
	if _, why := signatureRejection(t, raw); why != "format" {
		t.Fatalf("心跳缺签名的理由应是 format，得到 %q：%s", why, raw)
	}
}

// 用例 3：时间戳超出 ±300s → 401，理由必须是 expired（不是笼统的 invalid）。
func TestRegistryRejectsExpiredSignature(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	body := registerBody(t, manifest, declarationHash(t, manifest))
	now := time.Now().Unix()

	cases := []struct {
		name string
		ts   int64
	}{
		{"过旧（301s 前）", now - 301},
		{"超前（301s 后）", now + 301},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := postSigned(t, host.URL+"/tenon/registry/register",
				body, []byte(testHMACSecret), c.ts)
			if status != http.StatusUnauthorized {
				t.Fatalf("超差时间戳应 401，得到 %d：%s", status, raw)
			}
			code, why := signatureRejection(t, raw)
			if code != "SIGNATURE_EXPIRED" || why != "expired" {
				t.Fatalf("理由应是 SIGNATURE_EXPIRED/expired，得到 %s/%s：%s", code, why, raw)
			}
		})
	}
}

// 用例 4：心跳哈希与登记的不一致 → 409 + reRegister + declaration-changed。
func TestRegistryHeartbeatDeclarationChanged(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	hash := declarationHash(t, manifest)
	registerModule(t, host, manifest, hash)

	// 格式合法但与登记值不同：模拟「模块升级了 manifest 却没重申报」。
	other := "sha256:" + strings.Repeat("0", 64)
	status, raw := postSigned(t, host.URL+"/tenon/registry/heartbeat",
		heartbeatBody(t, "inventory", other), []byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusConflict {
		t.Fatalf("哈希不一致应 409，得到 %d：%s", status, raw)
	}
	ok, reason := reRegister(t, raw)
	if !ok || reason != "declaration-changed" {
		t.Fatalf("应是 reRegister=true/reason=declaration-changed，得到 %v/%q：%s", ok, reason, raw)
	}
}

// 用例 5：未知模块的心跳 → 409 + reRegister + unknown-module（host 重启后的必经路径）。
func TestRegistryHeartbeatUnknownModule(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))

	status, raw := postSigned(t, host.URL+"/tenon/registry/heartbeat",
		heartbeatBody(t, "ghost", "sha256:"+strings.Repeat("a", 64)),
		[]byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusConflict {
		t.Fatalf("未知模块应 409，得到 %d：%s", status, raw)
	}
	ok, reason := reRegister(t, raw)
	if !ok || reason != "unknown-module" {
		t.Fatalf("应是 reRegister=true/reason=unknown-module，得到 %v/%q：%s", ok, reason, raw)
	}
}

// 用例 6：正确心跳 → 200 alive。
func TestRegistryHeartbeatAccepted(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	hash := declarationHash(t, manifest)
	registerModule(t, host, manifest, hash)

	status, raw := postSigned(t, host.URL+"/tenon/registry/heartbeat",
		heartbeatBody(t, "inventory", hash), []byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("心跳应 200，得到 %d：%s", status, raw)
	}
	var resp struct {
		Alive bool `json:"alive"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("心跳响应解析失败：%v（原文 %s）", err, raw)
	}
	if !resp.Alive {
		t.Fatalf("心跳响应应 alive=true：%s", raw)
	}
}

// 附加用例：申报的哈希与带来的 manifest 不符 → 409 declaration-mismatch，
// 且**不得**在注册表里留下半真半假的条目（那会让后续心跳用一个与 manifest 无关的
// 哈希继续刷新它，从此没人能发现这份声明是假的）。
func TestRegistryRegisterDeclarationMismatch(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	// 拿另一份 manifest 的哈希冒充：格式完全合法，只是对不上。
	wrong := declarationHash(t, []byte(`{"tenon":"0.1","module":{"id":"inventory"}}`))

	status, raw := postSigned(t, host.URL+"/tenon/registry/register",
		registerBody(t, manifest, wrong), []byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusConflict {
		t.Fatalf("哈希与 manifest 不符应 409，得到 %d：%s", status, raw)
	}
	ok, reason := reRegister(t, raw)
	if !ok || reason != "declaration-mismatch" {
		t.Fatalf("应是 reRegister=true/reason=declaration-mismatch，得到 %v/%q：%s", ok, reason, raw)
	}

	got, err := http.Get(host.URL + "/tenon/modules")
	if err != nil {
		t.Fatalf("GET /tenon/modules 失败：%v", err)
	}
	defer func() { _ = got.Body.Close() }()
	var listed modulesResponse
	if err := json.NewDecoder(got.Body).Decode(&listed); err != nil {
		t.Fatalf("模块列表解析失败：%v", err)
	}
	if len(listed.Modules) != 0 {
		t.Fatalf("被拒的申报不该留下条目，实际 %+v", listed.Modules)
	}
}

// 附加用例：十六进制大小写不改变哈希语义。
//
// 守的是「注册宽容、心跳严格」的不对称：若注册收下大写、登记成小写，而心跳原样比较，
// 一个算式完全正确的模块就会陷进「409 → 重申报 → 409」的死循环。
func TestRegistryDeclarationHashCaseInsensitive(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	manifest := testManifest()
	hash := declarationHash(t, manifest)
	upper := "sha256:" + strings.ToUpper(strings.TrimPrefix(hash, "sha256:"))

	status, raw := postSigned(t, host.URL+"/tenon/registry/register",
		registerBody(t, manifest, upper), []byte(testHMACSecret), time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("大写十六进制的哈希也应接受注册，得到 %d：%s", status, raw)
	}
	for _, h := range []string{hash, upper} {
		status, raw = postSigned(t, host.URL+"/tenon/registry/heartbeat",
			heartbeatBody(t, "inventory", h), []byte(testHMACSecret), time.Now().Unix())
		if status != http.StatusOK {
			t.Fatalf("心跳带 %q 应 200（同一份声明的大小写变体），得到 %d：%s", h, status, raw)
		}
	}
}

// 附加用例：注册/心跳只收 POST（其它方法回 405 而不是 404）。
func TestRegistryEndpointsRejectGet(t *testing.T) {
	host := newRegistryHost(t, []byte(testHMACSecret))
	for _, path := range []string{"/tenon/registry/register", "/tenon/registry/heartbeat"} {
		resp, err := http.Get(host.URL + path)
		if err != nil {
			t.Fatalf("GET %s 失败：%v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s 应 405，得到 %d", path, resp.StatusCode)
		}
	}
}
