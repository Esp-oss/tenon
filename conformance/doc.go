// Package conformance 实现 Tenon 一致性测试套件（C1–C10 + D1 差异测试）。
//
// 本项目当前处于 M0 骨架阶段：本包只有包声明与文档，尚未实现。
// 用例清单与断言见 ./README.md。
//
// 设计约束（见 spec/0.1/semantics.md 与设计文档 §10）：
//   - D1 差异测试是「AI 驱动而非程序驱动」的主判据，C10 的 AST lint 只是辅助。
//   - 套件必须能对任意第三方实现运行，故交付形态是单二进制，不依赖 Node/Python 运行时。
package conformance
