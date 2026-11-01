package module

// 模块侧的注册/心跳推送（设计 spec §3、§8）：启动即注册，之后每 5s 一次心跳，
// 被 host 要求重申报（409）就立刻补一份完整声明。
//
// 为什么模块主动推而不是 host 探活（D1）：模块是唯一知道自己「现在能用」的一方，
// 而 host 探活要出网、要有失败重试策略、要区分「慢」与「死」。推送把失效判定单点
// 收到 host 的 TTL 清扫里：模块只要还在推，host 就认它活着；不推了，三个心跳周期
// 后被判 lost（registry.DefaultTTL）。
//
// 三条硬约束，都在本文件里落地：
//
//   - **注册失败不阻塞业务**（spec §8.4）：Start 立即返回，推送循环是独立 goroutine，
//     任何失败只记日志。fail-closed 在 host 侧——host 看不见这个模块，就不会把回合
//     派给它；模块自己照常提供端点，只是暂时不在能力清单里。
//   - **声明哈希与 host 侧同算**（D3、§3.1）：sha256: + hex(sha256(JCS(manifest)))，
//     走 internal/protocol.CanonicalJSON 这一份实现。两侧各写一份规范化必然分叉，
//     而哈希不容分叉——分叉的表现是模块永远注册不上（409 declaration-mismatch）。
//   - **心跳只证「还是那一份声明」**（D3）：报文只带 moduleId + declarationHash，
//     完整声明只在注册时给。哈希对不上是契约冲突（409），不是正常回执，所以模块侧
//     收到 409 的第 0 优先级动作是重申报，而不是把心跳再重试一次。
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"tenon.dev/internal/controlplane/registry"
	"tenon.dev/internal/protocol"
)

// 注册与心跳端点（设计 spec §3.1、§3.2）。与 host 侧的路由表一一对应。
const (
	registryRegisterPath  = "/tenon/registry/register"
	registryHeartbeatPath = "/tenon/registry/heartbeat"

	// defaultRegistryTimeout 是单次推送的 HTTP 超时。它必须存在：主循环是串行的，
	// 一个挂住的请求会拖住后面的心跳，模块因此在 host 眼里失联。
	defaultRegistryTimeout = 5 * time.Second

	// maxRegistryResponseBytes 是回执读取上限。回执都是几百字节的小对象，限制一下
	// 免得一个坏掉的 host 把模块的内存吃掉。
	maxRegistryResponseBytes = 64 << 10
)

// 模块侧注册推送读的两个环境变量。
const (
	// EnvHost 是宿主基地址，例如 http://127.0.0.1:8080。
	//
	// 未设置表示「本模块独立调试、不向任何 host 注册」——这是合法的本地形态，但
	// StartRegistry 会打一行警告：静默不注册会让运维以为注册在工作，而 host 永远
	// 看不到这个模块（与 host 侧 D7「缺密钥拒绝启动」同一个立场）。
	EnvHost = "TENON_HOST"
	// EnvHeartbeatInterval 覆盖心跳间隔（time.ParseDuration 形式，如 100ms）。
	// 只给测试与快速演示用：正常部署不该改它，默认值 registry.HeartbeatInterval 与
	// host 的 TTL（3 个周期）是一对，改短改长都会改变「几个心跳算过期」的语义。
	EnvHeartbeatInterval = "TENON_HEARTBEAT_INTERVAL"
)

// ClientConfig 是 RegistryClient 的装配参数。
//
// Manifest 是完整的能力摘要（module-manifest.schema.json，与 /.well-known/tenon
// 同形）。它不在这里生成，由 (*Module).ManifestJSON() 给出——注册申报与能力发现
// 必须是同一份字节，否则两侧的声明哈希对不上。
type ClientConfig struct {
	// Host 是宿主基地址（可带尾斜杠，会被去掉）。
	Host string
	// ModuleID 是本模块 id，心跳用它寻址。
	ModuleID string
	// BaseURL 是本模块对外自报的地址，host 用它把回合派过来。
	BaseURL string
	// Manifest 是完整声明原文（JSON）。
	Manifest []byte
	// Secret 是共享 HMAC 密钥材料，与 host 的 TENON_HMAC_SECRET_FILE 同一份字节。
	Secret []byte
	// Interval 是心跳间隔；<=0 时取 registry.HeartbeatInterval（5s）。
	Interval time.Duration
	// HTTP 可为 nil（默认带 5s 超时）。测试可注入 httptest 的客户端。
	HTTP *http.Client
}

// registryRegisterBody 是 POST /tenon/registry/register 的请求体（spec §3.1）。
type registryRegisterBody struct {
	Module          registryModuleDecl `json:"module"`
	DeclarationHash string             `json:"declarationHash"`
}

// registryModuleDecl 是注册报文里的 module 段：id + 自报地址 + 完整声明。
type registryModuleDecl struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	// Manifest 按 RawMessage 原样嵌入：manifest 已是 JSON，再经一次 struct 投影
	// 会把「申报的声明」与「端点给的声明」变成两份字节，哈希就对不上了。
	Manifest json.RawMessage `json:"manifest"`
}

// registryHeartbeatBody 是 POST /tenon/registry/heartbeat 的请求体（spec §3.2）。
type registryHeartbeatBody struct {
	ModuleID        string `json:"moduleId"`
	DeclarationHash string `json:"declarationHash"`
}

// registryReRegister 是 409 的报文：告诉模块「别重试心跳，重申报」（与 host 侧同形）。
type registryReRegister struct {
	ReRegister bool   `json:"reRegister"`
	Reason     string `json:"reason"`
}

// RegistryClient 是模块侧的注册/心跳推送器。零值不可用，请用 NewRegistryClient。
//
// 它是「尽力而为」的：任何失败都不向业务层回报（也没有回报的渠道）——注册推送的
// 成败只影响 host 能不能看见本模块，不影响模块端点的可用性（spec §8.4）。
type RegistryClient struct {
	host     string
	moduleID string
	baseURL  string
	manifest json.RawMessage
	secret   []byte
	interval time.Duration
	http     *http.Client

	// hash 是声明哈希；hashErr 是构造期的计算失败（manifest 不合法）。声明在进程
	// 生命周期里不会变，所以只算一次，不每个周期重算。
	hash    string
	hashErr error

	// mu 只保护 started：Start 被调用两次会让同一个模块推两份心跳，
	// 在 host 侧表现为「心跳频率翻倍」，是那种不报错但很难查的问题。
	mu      sync.Mutex
	started bool
}

// NewRegistryClient 装配一个推送器，**不发任何请求**；请求由 Start 发出。
//
// 构造期就把声明哈希算出来并缓存：哈希只取决于 manifest，而 manifest 是启动配置，
// 运行期不会再变。算不出来（manifest 不是合法 JSON）不在这里 panic，而是留给 Start
// 记日志——模块的业务端点不依赖声明哈希，不该因为一个编不出来的摘要起不来。
func NewRegistryClient(cfg ClientConfig) *RegistryClient {
	interval := cfg.Interval
	if interval <= 0 {
		// 与 host 的 TTL 同源：两边各写一个 5s 字面量，将来只改一处就会让
		// 「几个心跳算过期」失准（registry.HeartbeatInterval 的注释也是这么说的）。
		interval = registry.HeartbeatInterval
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRegistryTimeout}
	}
	c := &RegistryClient{
		host:     strings.TrimRight(strings.TrimSpace(cfg.Host), "/"),
		moduleID: strings.TrimSpace(cfg.ModuleID),
		baseURL:  strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		manifest: json.RawMessage(bytes.TrimSpace(cfg.Manifest)),
		secret:   append([]byte(nil), cfg.Secret...),
		interval: interval,
		http:     httpClient,
	}
	c.hash, c.hashErr = declarationHash(c.manifest)
	return c
}

// Start 启动推送循环，**立即返回**（非阻塞）：注册、心跳、重申报全在后台 goroutine 里。
//
// 这就是「注册失败不阻塞模块启动」的落地方式——Start 没有返回值、也没有错误通道，
// 调用方（cmd/*/main.go）在读它的那一刻起就已经在提供业务端点了。配置不完整时只记
// 一行日志并返回：一个装错的推送器不该让模块起不来。
//
// ctx 取消后循环退出，在途请求也随之取消；重复调用只有第一次生效。
func (c *RegistryClient) Start(ctx context.Context) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		log.Printf("tenon 模块 %s 的注册客户端已经启动过，忽略重复的 Start", c.moduleID)
		return
	}
	c.started = true
	c.mu.Unlock()

	if err := c.validate(); err != nil {
		log.Printf("tenon 模块 %s 的注册客户端未启动：%v", c.moduleID, err)
		return
	}
	go c.run(ctx)
}

// validate 检查装配是否完整。宁可在这里说清楚缺什么，也不要等请求发出后由 host
// 用 401 告诉我们签名不对。
func (c *RegistryClient) validate() error {
	switch {
	case c.host == "":
		return errors.New("ClientConfig.Host 为空：不知道把注册/心跳推给谁")
	case c.moduleID == "":
		return errors.New("ClientConfig.ModuleID 为空：host 无法寻址一个没有 id 的模块")
	case len(c.secret) == 0:
		return errors.New("ClientConfig.Secret 为空：注册与心跳都必须带 HMAC 签名")
	case c.hashErr != nil:
		return c.hashErr
	}
	return nil
}

// run 是推送循环：启动即注册，之后每 interval 一次心跳；注册没成功就每个周期重试注册。
//
// 注册没成功时不发心跳：host 不认识这个 id，只会回 409 unknown-module，白跑一趟。
// 注册/心跳失败都不退出循环——host 起得比模块晚是常态，网络抖动也是常态，而模块
// 的可见性本来就允许有一个心跳周期量级的延迟（spec §6 的重启窗口）。
func (c *RegistryClient) run(ctx context.Context) {
	registered := c.register(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !registered {
			registered = c.register(ctx)
			continue
		}
		switch c.heartbeat(ctx) {
		case heartbeatAlive:
		case heartbeatReRegister:
			// 409：host 不认识这个 id（host 重启过），或登记的声明与本地这份不是同一份
			// （模块升级、地址变了）。心跳再发多少次都不会变好，立刻整份重申报（D3）。
			registered = c.register(ctx)
		case heartbeatFailed:
			// 5xx / 网络错误：模块在 host 眼里仍然活着（TTL 内），下个周期再试。
			// 真的连续丢够三个周期，由 host 的 TTL 判 lost——模块侧不替它猜。
		}
	}
}

// heartbeatOutcome 是心跳的三种结局。分三态而不是 bool，是因为「被要求重申报」与
// 「这次没成」的处置完全不同：前者必须立刻注册，后者只等下一个周期。
type heartbeatOutcome int

const (
	// heartbeatAlive：200，存活已刷新。
	heartbeatAlive heartbeatOutcome = iota
	// heartbeatReRegister：409，必须先重申报。
	heartbeatReRegister
	// heartbeatFailed：其它失败（5xx、超时、报文错误），下个周期重试。
	heartbeatFailed
)

// register 推送一次完整申报，返回 host 是否接受。
//
// 409 declaration-mismatch 落在这里而不是重试：它意味着本模块算出的哈希与 host 从
// 同一份 manifest 算出的不一样。这不是网络问题，重发同一份报文永远不会成功——只记
// 日志（含 host 给的理由），让人去查两侧的规范化是否真的一致。
func (c *RegistryClient) register(ctx context.Context) bool {
	body, err := json.Marshal(registryRegisterBody{
		Module:          registryModuleDecl{ID: c.moduleID, BaseURL: c.baseURL, Manifest: c.manifest},
		DeclarationHash: c.hash,
	})
	if err != nil {
		// 只可能是 manifest 本身编不出来（/.well-known/tenon 端点同样会因此失败）。
		log.Printf("tenon 模块 %s 的注册报文无法编组：%v", c.moduleID, err)
		return false
	}
	status, payload, err := c.post(ctx, registryRegisterPath, body)
	if err != nil {
		// ctx 已取消时不再记日志：那是模块正常退出，不是故障。
		if ctx.Err() == nil {
			log.Printf("tenon 模块 %s 注册失败（%s 后重试）：%v", c.moduleID, c.interval, err)
		}
		return false
	}
	if status/100 == 2 {
		log.Printf("tenon 模块 %s 已注册到 %s（声明 %s）", c.moduleID, c.host, c.hash)
		return true
	}
	reason := reRegisterReason(payload)
	if reason == "" {
		reason = strings.TrimSpace(string(payload))
	}
	log.Printf("tenon 模块 %s 的注册被拒（HTTP %d）：%s", c.moduleID, status, reason)
	return false
}

// heartbeat 发一次心跳。409 的报文由 host 给出 {reRegister, reason}，reason 原样
// 记进日志：unknown-module 与 declaration-changed 的排查方向完全不同。
func (c *RegistryClient) heartbeat(ctx context.Context) heartbeatOutcome {
	body, err := json.Marshal(registryHeartbeatBody{ModuleID: c.moduleID, DeclarationHash: c.hash})
	if err != nil {
		log.Printf("tenon 模块 %s 的心跳报文无法编组：%v", c.moduleID, err)
		return heartbeatFailed
	}
	status, payload, err := c.post(ctx, registryHeartbeatPath, body)
	if err != nil {
		// ctx 已经取消时不再记日志：那是模块正常退出，不是故障。
		if ctx.Err() == nil {
			log.Printf("tenon 模块 %s 心跳失败（%s 后重试）：%v", c.moduleID, c.interval, err)
		}
		return heartbeatFailed
	}
	switch {
	case status/100 == 2:
		return heartbeatAlive
	case status == http.StatusConflict:
		reason := reRegisterReason(payload)
		if reason == "" {
			reason = "未给出理由"
		}
		log.Printf("tenon 模块 %s 的心跳被要求重申报（%s），立即重发完整声明", c.moduleID, reason)
		return heartbeatReRegister
	default:
		log.Printf("tenon 模块 %s 的心跳被拒（HTTP %d）：%s", c.moduleID, status, strings.TrimSpace(string(payload)))
		return heartbeatFailed
	}
}

// post 发一次签名请求，返回状态码与响应体。
//
// 签名覆盖的是**原始请求体**（"<t>." + body），所以必须先编好字节、再对同一份字节
// 签名、再把同一份字节交给请求体——先序列化一次、再让 http 库序列化第二次，签名就
// 对不上了（与 host 侧 readVerifiedBody 的注释同一条理由）。
func (c *RegistryClient) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	ts := time.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// 协议版本头：host 接受缺失，但自报版本能让「模块与 host 版本不一致」在日志里
	// 留下痕迹，而不是等到报文语义分叉才发现。
	req.Header.Set(protocol.HeaderVersion, protocol.Version)
	req.Header.Set(protocol.HeaderSignature, protocol.Sign(c.secret, ts, body))

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}

// reRegisterReason 从 409 报文里取 reason；报文不合形时返回空串（调用方用原文兜底）。
func reRegisterReason(payload []byte) string {
	var rr registryReRegister
	if err := json.Unmarshal(payload, &rr); err != nil {
		return ""
	}
	return strings.TrimSpace(rr.Reason)
}

// declarationHash 算声明哈希：sha256: + hex(sha256(JCS(manifest)))（设计 spec §3.1）。
//
// 必须先反序列化成通用 JSON 值再走 protocol.CanonicalJSON：manifest 的键序与空白由
// 各模块自己决定，直接对原始字节求 sha256 会让同一份声明在不同序列化路径下算出不同值，
// 哈希就失去「同一份声明只有一个值」的意义。host 侧（controlplane.declarationHashOf）
// 用的是同一个 protocol.CanonicalJSON，两侧才可能得出同一个哈希。
func declarationHash(manifest []byte) (string, error) {
	var v any
	if err := json.Unmarshal(manifest, &v); err != nil {
		return "", fmt.Errorf("manifest 不是合法 JSON，无法算声明哈希：%w", err)
	}
	canon, err := protocol.CanonicalJSON(v)
	if err != nil {
		return "", fmt.Errorf("manifest 无法按 JCS 规范化，无法算声明哈希：%w", err)
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// StartRegistry 按环境变量装配并启动注册推送，是 cmd/*/main.go 的唯一接线入口。
//
// 返回值语义：
//   - (nil, nil)：TENON_HOST 未设置——本地单模块调试的合法形态，但**打了警告**，
//     否则运维会以为注册在工作而 host 永远看不到这个模块；
//   - (nil, err)：TENON_HOST 设了却装不起来（密钥读不到、manifest 编不出来）。
//     调用方应当只记日志并继续起业务：注册失败不阻塞模块启动（spec §8.4）；
//   - (client, nil)：已在后台推送。
//
// baseURL 是本模块对外自报的地址，由调用方从监听地址折出来（见 BaseURLForAddr）。
func (m *Module) StartRegistry(ctx context.Context, baseURL string) (*RegistryClient, error) {
	host := strings.TrimSpace(os.Getenv(EnvHost))
	if host == "" {
		log.Printf("tenon 模块 %s 未设置 %s：本次启动不向宿主注册/心跳（仅本地单模块调试可用；宿主将看不到本模块）",
			m.id(), EnvHost)
		return nil, nil
	}
	manifest, err := m.ManifestJSON()
	if err != nil {
		return nil, fmt.Errorf("能力摘要编不出来，无法申报：%w", err)
	}
	// 密钥复用模块侧既有读取逻辑：注册鉴权与事件投递验签用的是同一份共享密钥，
	// 造第二个来源只会让两边不同步。
	secret, err := m.secret()
	if err != nil {
		return nil, fmt.Errorf("读不到注册用的 HMAC 密钥（%s）：%w", EnvHMACSecretFile, err)
	}
	interval, err := heartbeatIntervalFromEnv()
	if err != nil {
		// 覆盖值写错不该让模块起不来，但也不能悄悄改用默认值——打出来，让人看见。
		log.Printf("tenon 模块 %s 的 %s 不可用，改用默认心跳间隔 %s：%v",
			m.id(), EnvHeartbeatInterval, registry.HeartbeatInterval, err)
	}
	client := NewRegistryClient(ClientConfig{
		Host:     host,
		ModuleID: m.id(),
		BaseURL:  baseURL,
		Manifest: manifest,
		Secret:   secret,
		Interval: interval,
	})
	client.Start(ctx)
	return client, nil
}

// heartbeatIntervalFromEnv 读心跳间隔覆盖值；未设置返回 0（NewRegistryClient 会取
// 默认值），写了但解析不出来返回错误，由调用方记日志后仍走默认值。
func heartbeatIntervalFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(EnvHeartbeatInterval))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q 不是合法的时长（如 100ms、5s）：%w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("必须为正数，收到 %q", raw)
	}
	return d, nil
}

// BaseURLForAddr 把模块的监听地址折成对外自报的基地址（http://host:port）。
//
// 监听地址与「别人怎么找到我」不是一回事：":8081" 与 "0.0.0.0:8081" 只说明本机绑了
// 所有网卡，host 却不可能拿 0.0.0.0 去发请求，所以通配地址一律折成 127.0.0.1
// （单机部署的默认拓扑）。跨机部署必须显式给 PORT=本机可达地址:端口。
//
// 返回空串表示这个地址折不出来（空串、无法解析），调用方应当把它当成配置错误——
// host 会以 INVALID_MODULE_BASE_URL 拒绝注册，重试也不会好。
func BaseURLForAddr(addr string) string {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		return u.Scheme + "://" + host
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port)
}
