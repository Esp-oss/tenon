package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HostClient 是 agent 对宿主入站面的客户端：登录、轮询入站帧、确认、推公告板。
//
// token 只活在内存里：不落盘、不写日志。落盘会让崩溃转储与备份多出第二份凭证，
// 写日志则等于把它抄进日志轮转——两者都超出「一次进程生命周期的凭证」该有的寿命。
//
// 401 时重登一次，且**并发重登串行化**（单飞）：一批 401 同时到达时若各自去登录，
// 就是登录风暴；而且登录是换新 token，后到的登录会把先到的刚用上的 token 作废。
type HostClient struct {
	base     string
	user     string
	password string
	client   *http.Client

	mu       sync.Mutex
	token    string
	logging  bool
	loggedIn chan struct{} // 单飞：正在登录时其它调用等它关闭
	loginErr error         // 那次登录的结果：等它的人必须拿到真实结果，不能是假 nil
}

// NewHostClient 构造客户端。base 形如 http://127.0.0.1:8080（尾斜杠会被去掉：
// 否则拼出来的 URL 是 //tenon/session，有些反代不认）。
func NewHostClient(base, user, password string, client *http.Client) *HostClient {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return &HostClient{base: base, user: user, password: password, client: client}
}

// Login 换 token。失败即返回错误（agent 启动时据此失败，而不是带着空 token 继续跑）。
func (c *HostClient) Login(ctx context.Context) error {
	return c.relogin(ctx)
}

// relogin 是单飞登录：同一时刻最多一次登录在飞，其它调用等它结束并共享结果。
func (c *HostClient) relogin(ctx context.Context) error {
	c.mu.Lock()
	if c.logging {
		ch := c.loggedIn
		c.mu.Unlock()
		<-ch // 等正在进行的登录
		c.mu.Lock()
		err := c.loginErr
		c.mu.Unlock()
		return err
	}
	c.logging = true
	c.loggedIn = make(chan struct{})
	ch := c.loggedIn
	c.mu.Unlock()

	status, raw, err := postJSONWith(ctx, c.client, c.base+"/tenon/session",
		map[string]string{"user": c.user, "password": c.password}, nil)
	var loginErr error
	if err != nil {
		loginErr = err
	} else if status != http.StatusOK {
		loginErr = fmt.Errorf("登录返回 %d：%s", status, snippet(raw))
	} else {
		var out struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
			loginErr = fmt.Errorf("登录响应没有 token：%s", snippet(raw))
		} else {
			c.mu.Lock()
			c.token = out.Token
			c.mu.Unlock()
		}
	}

	c.mu.Lock()
	c.logging = false
	c.loginErr = loginErr
	// 先落 token 再 close：等在这条 channel 上的人醒来必须能看到新 token。
	close(ch)
	c.mu.Unlock()
	return loginErr
}

func (c *HostClient) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// do 带 token 发请求；遇 401 重登一次再重试该请求。
//
// 只重试一次：重登之后仍 401 说明凭证或角色就是不对，再登下去就是打自己的登录接口。
func (c *HostClient) do(ctx context.Context, method, url string, body any) (int, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok := c.currentToken()
		if tok == "" {
			if err := c.relogin(ctx); err != nil {
				return 0, nil, err
			}
			tok = c.currentToken()
		}
		status, raw, err := c.send(ctx, method, url, body, tok)
		if err != nil {
			return 0, nil, err
		}
		if status != http.StatusUnauthorized || attempt == 1 {
			return status, raw, nil
		}
		if err := c.relogin(ctx); err != nil {
			return 0, nil, err
		}
	}
	return 0, nil, fmt.Errorf("agent: 重登后仍 401")
}

// send 按方法选助手：poll 是 GET（带 token 的自定义头），其余是 JSON POST。
func (c *HostClient) send(ctx context.Context, method, url string, body any, token string) (int, []byte, error) {
	headers := map[string]string{"Authorization": "Bearer " + token}
	if method == http.MethodGet {
		return getJSONWith(ctx, c.client, url, headers)
	}
	return postJSONWith(ctx, c.client, url, body, headers)
}

// PollInbound 拉 after 之后的入站帧，返回帧与下一次应当传入的游标。
func (c *HostClient) PollInbound(ctx context.Context, after int64) ([]QueuedFrame, int64, error) {
	status, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/tenon/requests/poll?after=%d", c.base, after), nil)
	if err != nil {
		return nil, after, err
	}
	if status != http.StatusOK {
		return nil, after, fmt.Errorf("poll 返回 %d：%s", status, snippet(raw))
	}
	var out struct {
		Frames     []QueuedFrame `json:"frames"`
		NextCursor int64         `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, after, err
	}
	return out.Frames, out.NextCursor, nil
}

// AckInbound 确认到 upto（含）为止的帧。宿主对 ack 的承诺是「此后不再回放」，
// 所以调用方必须按帧序逐帧 ack：一次 ack 到末尾等于宣布没处理过的帧也已处理。
func (c *HostClient) AckInbound(ctx context.Context, upto int64) error {
	status, raw, err := c.do(ctx, http.MethodPost, c.base+"/tenon/requests/ack",
		map[string]int64{"upto": upto})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ack 返回 %d：%s", status, snippet(raw))
	}
	return nil
}

// PushPending 幂等 upsert 一条公告板摘要。
//
// payload 由调用方给全（形状与 host 的 pendingItem 一致）：公告板是 read-model，
// host 只做展示，权威状态在 agent 盘上的 pending 文件里。
func (c *HostClient) PushPending(ctx context.Context, payload map[string]any) error {
	status, raw, err := c.do(ctx, http.MethodPost, c.base+"/tenon/pending", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("公告板推送返回 %d：%s", status, snippet(raw))
	}
	return nil
}

// QueuedFrame 是入站队列的一帧（与 host 的线上形状一致：{seq, frame}）。
//
// 序号在帧外面：frames.schema.json 的三帧都是 additionalProperties=false，
// 往帧体里塞一个 seq 就不是合法帧了。
type QueuedFrame struct {
	Seq   int64           `json:"seq"`
	Frame json.RawMessage `json:"frame"`
}

// getJSONWith 发 GET 并带自定义头。
//
// 包级的对应物：runner.getJSON 是回合实例的方法（带协议版本头），入站客户端
// 需要在回合之外发带 Authorization 的 GET，且不能替宿主决定协议版本头。
func getJSONWith(ctx context.Context, client *http.Client, url string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, raw, err
}
