// Command tenon-mcp 是 Tenon 的 MCP 服务端（stdio 传输），给 dsh 这类对话层用。
//
// 规矩：stdout 只写 JSON-RPC（一行一条），任何诊断都走 stderr。
// 混一行日志进 stdout，客户端就会把整段解析失败——而症状是「工具一个都看不见」，
// 很容易被误判成配置错。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"tenon.dev/internal/mcpserver"
)

func main() {
	var (
		host = flag.String("host", envOr("TENON_HOST", "http://127.0.0.1:8080"),
			"Tenon 宿主根地址")
		agentID = flag.String("agent", envOr("TENON_AGENT_ID", "ag_fulfillment"),
			"预检用的 agent 身份（与 cmd/agent 同一份配置）")
		onBehalfOf = flag.String("on-behalf-of", envOr("TENON_ON_BEHALF_OF", "user:alice"),
			"预检用的 onBehalfOf（与 cmd/agent 同一份配置）")
	)
	flag.Parse()

	srv := &mcpserver.Server{
		Host:       strings.TrimRight(*host, "/"),
		AgentID:    *agentID,
		OnBehalfOf: *onBehalfOf,
		// 人的身份只从环境变量读：放进命令行参数会被 ps/logcmdline 看见，
		// 放进工具参数会进模型上下文。两条都不该走。
		//
		// 优先用 user/password 惰性登录（token 会过期，写死会在某天悄悄失效）；
		// 显式 token 保留给「不想把口令交给 MCP 进程」的场合。
		HumanToken:    os.Getenv("TENON_MCP_HUMAN_TOKEN"),
		HumanUser:     os.Getenv("TENON_MCP_USER"),
		HumanPassword: os.Getenv("TENON_MCP_PASSWORD"),
		Log:           os.Stderr,
	}

	fmt.Fprintf(os.Stderr, "tenon-mcp: host=%s agent=%s onBehalfOf=%s humanToken=%v\n",
		srv.Host, srv.AgentID, srv.OnBehalfOf, srv.HumanToken != "")

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()

	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			resp, ok := srv.HandleLine(line)
			if ok {
				if _, werr := out.Write(append(resp, '\n')); werr != nil {
					fmt.Fprintf(os.Stderr, "tenon-mcp: 写 stdout 失败：%v\n", werr)
					return
				}
				_ = out.Flush()
			}
		}
		if err != nil {
			// EOF 是正常收尾（客户端关掉了管道）。
			return
		}
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
