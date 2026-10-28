# skill 的写法（`skills/<name>/SKILL.md`）

一份 skill 是一段**可审阅的业务知识**，不是可执行流程。它只影响两件事：要不要为某个事件开会话、以及请求回合里是否被选为「业务语义参考」。

## frontmatter

```yaml
---
name: quote-to-fulfillment
version: 0.1.0
tenon:
  subscribes: ["quote.completed"]        # 事件订阅：决定「要不要开回合」
  expectedModules: ["inventory", "procurement"]  # 请求回合的粗筛词表
  outcome: "标准件报价完成后核对库存并履约"
---
```

| 字段 | 作用 | 不写会怎样 |
|---|---|---|
| `subscribes` | 事件回合：事件类型命中即开回合（**只决定开不开，不决定做什么**） | 该事件永远不会唤醒这份 skill |
| `expectedModules` | 请求回合：粗筛词表。请求文本里出现这些模块名才会选中本 skill | 退化为「订阅项里点号前的模块名」；都没有则**永远不会被请求回合选中** |
| `outcome` | 给人看的业务目标 | 仅影响可读性 |

## 为什么有 `expectedModules`

请求回合的候选集是**确定性粗筛**（不调 LLM）：把请求文本与每份 skill 自己声明的模块名求交集，命中数降序、同数按名字典序、最多取 2 份。

词表**由 skill 自己声明**，agent 里没有任何硬编码模块清单——所以：

> **新增业务 = 加一个模块 + 加一份 skill**，不需要改 agent 代码。

2026-10-27 之前这里有一份硬编码的 `knownModuleNames = ["inventory","procurement","quote"]`，新模块的 skill 永远选不中；已删除（见 `internal/agent/skillselect.go`）。

## 正文

- 正文是**业务判断依据**，会被整段放进 prompt；
- 不写具体动作序列（动作由规划器从能力清单里选）；
- 正文为空也可以（`subscribes` 命中仍会开回合），但请求回合里空正文的 skill 只是「命中占位」，信息量很低。
