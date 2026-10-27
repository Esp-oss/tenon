// Package lint 实现 Tenon 的 C10 反模式扫描（事件/错误码 → action 的硬编码映射）。
//
// M0 已实现三条规则（纯词法，不引 cgo），规则与已知限制见 ./README.md。
//
// 交付形态是单二进制命令，可对任意接入方仓库运行，不依赖被扫描方的工具链。
// 已知限制：只覆盖 TS/JS 源码与 skill 文档的正文，非 TS/JS 接入方要靠 D1 差异测试兜底。
package lint
