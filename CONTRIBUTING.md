# 参与贡献

## 改规范的第一原则

**schema 是唯一真源。** 字段定义只写在 `spec/0.1/schemas/*.json`，Markdown 只写语义、理由和例子。手改 Markdown 里的字段表会导致真源分叉。

## 提规范变更的流程

1. **先开 issue 说清要解决什么**。协议变更的代价不在写代码，在下游所有实现的兼容成本。
2. **改 schema**，并在 PR 里粘出 `node scripts/validate-spec.mjs` 的输出。
3. **每条新规则必须配一致性用例**（`conformance/cases/`）。写不出用例的规则不许进协议——这条是硬规定，见设计文档 §2.1。
4. **兼容性自检**：
   - 只增不删字段
   - 未知字段必须能被忽略（前向兼容）
   - 破坏性变更只能进下一个 major，且先在 minor 里标 deprecated
5. 更新 `spec/0.1/CHANGELOG.md`。

## 反模式检查

任何 PR 里如果出现「事件类型/错误码 → 具体 action」的映射代码，会被 `tools/lint` 拦下，也会被 review 拒绝。原因见设计文档 §2.2：

> 接入方为了跑通业务流程，必须写「某事件/某错误 → 调某 action」的映射代码 —— 此时系统已退化为程序驱动，Tenon 在该接入点失效。

## 本地检查

```bash
node scripts/validate-spec.mjs   # 规范静态校验（零依赖）
go vet ./...                     # Go 工具链骨架
gofmt -l .                       # 应为空
```

CI 在 Node 22 与 24 上跑规范校验，Go 侧跑 `go vet` 与 `gofmt`。

## 工具链

| 层 | 需要装 |
|---|---|
| 规范（`spec/`） | 只需 Node 22+（校验脚本零依赖） |
| dsh 插件与类型（`packages/`） | Node 22+、pnpm 11+ |
| 一致性套件、lint、Go 模块 | Go 1.26+ |

改动 `packages/` 才需要 pnpm；只改 `spec/` 或 Go 代码时不要引入 Node 依赖。

## 提交信息

Conventional Commits：

```
spec: 给 Action 增加 quotaField
conformance: 补 C7 补偿用例
fix(core): EntityRef 解析漏了 module 里的点号
docs: 补 M1 里程碑的出口判据
```

`spec:` 前缀的提交会被视为协议变更，需要额外一位 reviewer 通过。

## 实现类 PR

- 先看对应包 README 的「阶段状态」，只做该里程碑范围内的东西。
- 加新依赖前先在 issue 里讨论；协议层包（`tenon-core`）**不允许依赖任何 dsh 包**。
- 带测试。没有测试的实现不合并。

## 许可

贡献即同意以 [Apache-2.0](LICENSE) 授权。
