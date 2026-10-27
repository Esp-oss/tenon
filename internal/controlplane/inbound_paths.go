package controlplane

import "strings"

// isInboundPath 判断一条路径是否属于「入站通道」（人 → agent）。
//
// 入站通道的四条路由在测试模式下整条不挂：入站承载外部输入（人说过的话），
// 它的有无会改变 agent 的结局，因此与 D1 的「同一输入 → 同一行为」冲突。
// 新增入站端点时必须同步这里，并由 testmode_test.go 的表驱动用例发现遗漏。
//
// 不在此列：/tenon/session（只发 token，不含业务输入；关掉它 agent 起不来）、
// /tenon/intents*（agent → 前端的出站队列）。
func isInboundPath(path string) bool {
	const prefix = "/tenon/requests"
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return true
	}
	return path == "/tenon/pending"
}
