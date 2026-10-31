// Package mcpserver 把 Tenon 的能力暴露成一个 MCP（Model Context Protocol）服务端，
// 供 dsh 这类对话层调用。
//
// 设计边界（见 docs/superpowers/specs/2026-11-01-dsh-as-conversation-layer-design.md）：
//
//   - 这里**没有**执行写动作的工具。对话层拿不到「调一下就落库」的能力，
//     写动作只能变成待批卡，由宿主签票后执行。
//   - 也不写公告板：可恢复的卡依赖 agent 侧的 argsHash 快照与本地 store，
//     由对话层造卡等于把「谁的计划被审计记录」搅浑。
//   - 本进程不持有宿主签名私钥，不调 tickets:issue。
//
// 传输用 stdio：stdout 只写 JSON-RPC（一行一条），日志一律走 stderr。
// 混进一行日志就会让客户端解析失败，所以规则是硬的。
package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// protocolVersion 是 MCP 的协议版本。客户端会带着自己的版本进来，服务端选一个双方都支持的；
// 这里只支持这一版，因为本服务端只用 tools 子集（没有 resources/prompts/sampling）。
const protocolVersion = "2025-06-18"

// Server 是一次 MCP 会话的服务端。
type Server struct {
	// Host 是 Tenon 宿主根地址（如 http://127.0.0.1:8080）。
	Host string
	// AgentID / OnBehalfOf 与 cmd/agent 用同一份配置：预检与签票必须是同一个主体，
	// 否则会出现「预检说 allowed、签票却 GRANT_DENIED」。
	AgentID    string
	OnBehalfOf string
	// HumanToken 是人（业务员）的 bearer token，只用于 pending_* 两个工具。
	// 它不从工具参数进：参数会进模型上下文，token 不该去那里。
	HumanToken string
	// HumanUser/HumanPassword 是本地调试用的惰性登录凭据：配了它们时，
	// 第一次调用 pending_* 会用 /tenon/session 换 token 并缓存。
	//
	// 为什么需要它：token 有有效期，把它抄进 dsh 配置就会在某天悄悄失效，
	// 而症状是「审批按钮不灵」——比登录失败更难查。凭据换来的是「过期自动重登」。
	HumanUser     string
	HumanPassword string

	mu    sync.Mutex
	token string
	HTTP       *http.Client
	// Log 是诊断输出（stderr）。默认丢弃，由 cmd 接上 os.Stderr。
	Log io.Writer
}

// rpcRequest 是一条 JSON-RPC 2.0 请求。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC 标准错误码。
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// HandleLine 处理一行输入，返回要写回的一行输出。
//
// ok=false 表示这条消息不需要响应（通知，例如 notifications/initialized）。
func (s *Server) HandleLine(line []byte) (out []byte, ok bool) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil, false
	}

	var req rpcRequest
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		return mustMarshal(rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: codeParseError, Message: "不是合法 JSON：" + err.Error()},
		}), true
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return mustMarshal(rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: codeInvalidRequest, Message: "缺少 jsonrpc=2.0 或 method"},
		}), true
	}

	// 通知没有 id：客户端不期待响应，写回去反而会打乱它的请求队列。
	isNotification := len(req.ID) == 0

	result, rerr := s.dispatch(req.Method, req.Params)
	if isNotification {
		return nil, false
	}
	if rerr != nil {
		return mustMarshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}), true
	}
	return mustMarshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}), true
}

func (s *Server) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tenon-mcp", "version": "0.1.0"},
			// instructions 会进模型的系统上下文：把「你只能提议、不能执行」写在这里，
			// 比在每个工具的 description 里重复一遍更可靠。
			"instructions": "这是 Tenon 的能力边界：你可以查注册中心、调用只读动作、预检一份计划，" +
				"也可以代人工批准/驳回待批卡。你**没有**执行写动作的能力——写动作只会变成待批卡。",
		}, nil

	case "notifications/initialized", "notifications/cancelled":
		return map[string]any{}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": toolSpecs()}, nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call 参数坏：" + err.Error()}
		}
		return s.callTool(p.Name, p.Arguments)

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "未实现的方法：" + method}
	}
}

// callTool 执行工具并把结果包成 MCP 的 content 结构。
//
// 工具内部的「业务失败」（策略拒绝、无权限、动作不是只读）不是协议错误：
// 一律 isError=false 返回正文，让模型读到原因。只有工具不存在才是协议错误。
func (s *Server) callTool(name string, args json.RawMessage) (any, *rpcError) {
	handler, ok := toolHandlers()[name]
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "未知工具：" + name}
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	text, isErr := handler(s, args)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}, nil
}

func (s *Server) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (s *Server) logf(format string, a ...any) {
	if s.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(s.Log, "tenon-mcp: "+format+"\n", a...)
}

func mustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// 走到这里说明我们的响应结构坏了，是 bug；给客户端一个最小可解析的错误而不是空行。
		return []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"响应序列化失败"}}`)
	}
	return raw
}

// humanToken 返回用于 pending_* 的 bearer token：优先用显式配置的，其次惰性登录。
//
// 登录失败返回空串——调用方据此给出「没有可用身份」的明确拒绝，而不是发一个空
// Authorization 头出去（那样宿主回 401，看起来像权限问题，其实是没登录）。
func (s *Server) humanToken() string {
	s.mu.Lock()
	if s.token != "" {
		t := s.token
		s.mu.Unlock()
		return t
	}
	if s.HumanToken != "" {
		s.mu.Unlock()
		return s.HumanToken
	}
	user, pass := s.HumanUser, s.HumanPassword
	s.mu.Unlock()

	if user == "" || pass == "" {
		return ""
	}
	var resp struct {
		Token string `json:"token"`
		User  string `json:"user"`
	}
	// 传 map 而不是先 Marshal 成 []byte：postJSON 自己会序列化，交进去的字节会被
	// 再套一层变成 JSON 字符串，宿主回 400 SCHEMA_MISMATCH（第一次就踩了）。
	creds := map[string]string{"user": user, "password": pass}
	if err := s.postJSON(s.Host+"/tenon/session", creds, &resp); err != nil {
		s.logf("惰性登录失败（%s）：%v", user, err)
		return ""
	}
	if resp.Token == "" {
		s.logf("惰性登录返回空 token（%s）", user)
		return ""
	}
	s.mu.Lock()
	s.token = resp.Token
	s.mu.Unlock()
	return resp.Token
}
