# D1 判定与收口设计（真模型 A/B + 收尾）

- 状态：**待评审**
- 日期：2026-10-27
- 关系：本文件合两份工作——**A：用真模型跑 D1 判掉既有 SKIP**；**C：把分支状态与文档对齐后推送**。两者共用一次全量回归，故合并成一份 spec（都无架构决策：A 是判定作业 + 一个编排脚本，C 是清单）。
- 前置：P1 `74182cd`、P2（Task 1–11）、UI `586de2f`、容器化 `43474d1` 均已落地；`feat/m05-host-agent` 领先 `origin` 26 个提交。

---

## 0. 目标与非目标

**目标**

1. **D1 从「未判定」变成「已判定」**：用真模型跑 A/B 两轮，产出 `verdict` 与可归档的证据；判 pass 就写 pass，判 fail 就写 fail 并给根因。
2. **判定可复现**：把「起 4 个进程 + 每轮换 agent 进程」写成一个脚本，前置条件失败时立即报错而不是给出假 verdict。
3. **分支收口**：三处文档漂移、P2 复选框、`static.go` 缺测试，一并修掉，然后推 `feat/m05-host-agent`。

**非目标（明确不做）**

| 不做 | 理由 |
|---|---|
| 为了 pass 去改 prompt / skill / 规划器 | 会把 D1 做成假命题；本轮只判不调 |
| 改 D1 判据（四条合取） | 判据是 M0 立的，改判据等于自己给自己发合格证 |
| 把容器冒烟用 stub 模型的结论当 D1 判定 | 冒烟短路了规划环节（`out/docker-smoke` 的 rationale 明写"桩模型"） |
| 合并进 `main` | 已定：只推特性分支 |
| 修 CHANGELOG 里的 `summaryTemplate` 声明 | 那是 B 切片（白话）负责兑现的事，这里不动 |
| M1 其它剩余项（WS、grant 真校验、amend、撤销补偿、对账） | 各有归属，不在本轮 |

---

## 1. 事实基线

| 现状 | 位置 | 含义 |
|---|---|---|
| D1 runner 已实现 | `internal/d1/runner.go`（757 行）+ `runner_test.go`（20KB） | 判据与前置条件都已写死，缺的只是执行 |
| 判据是四条合取 | `runner.go:13` `decide` | 序列不同、B 轮不碰被摘模块、B 轮转人工、两轮都没崩 |
| 前置条件写在文件头 | `runner.go:21-32` | host 与三模块都要 `TENON_TEST_MODE=1`；agent 要 `TENON_RESULT_FILE`；**agent 每轮必须是新进程**（reset 把发件箱序号清回 0，长驻 agent 的游标会把 B 轮新事件吞掉） |
| D1 结论是 SKIP | `README.md:197`、`README.md:210-217` | 本轮要把它变成判定的 pass/fail |
| 现有端到端证据都用了桩模型 | `out/docker-smoke/agent-state/pending/archived/pln_*.json` 的 `rationale` | 只能证明链路，不能证明规划自适应 |
| 凭据可用 | 本轮实测 `GET https://api.deepseek.com/models` | 可用模型：`deepseek-flash`、`deepseek-v4-pro`；客户端缺省名真实存在，无需改代码 |
| 26 个提交未推 | `git rev-list --count origin/feat/m05-host-agent..HEAD` | pre-push 钩子已配（`.githooks/pre-push`，`core.hooksPath=.githooks`） |
| 三处文档漂移 | 见 §3 表格 | 读文档的人会得到与 HEAD 不符的状态 |
| `static.go` 无测试 | `internal/controlplane/static.go` 存在、无 `static_test.go` | 页面路由（503 / 越界 / 200 / 405）零覆盖 |

---

## 2. A：D1 判定作业

### 2.1 编排脚本 `scripts/d1-e2e.ps1`

三种做法与取舍：

| 做法 | 取舍 |
|---|---|
| 照 README 手工起 5 个进程 | 不可复现；「每轮换 agent 进程」靠人手重启，漏一次就得到「等不到结果行」的假 fail |
| **脚本编排（选定）** | 前置条件变成启动断言；一次命令可重跑；跑完自动收尸 |
| 塞进 compose `smoke` profile | agent 的「每轮换进程」在 compose 里表达不了，且 d1 要与宿主共享结果文件；成本大于收益 |

**拓扑与端口**：`inventory=8081`、`procurement=8082`、`quote=8083`、`host=8080`、agent 无端口。

**环境变量（脚本里钉死）**

| 进程 | 关键变量 |
|---|---|
| 三个模块 | `TENON_TEST_MODE=1`（挂 `POST /tenon/admin/reset`，两轮输入才可比）、`PORT`、`TENON_MODULE_ID`、**`TENON_HMAC_SECRET_FILE`**（必需：模块 `/readyz` 会检查 `hmac_secret` 与 `audit_file`，缺一项永远不就绪——见 `internal/module` 的 `serveReady`） |
| host | `TENON_TEST_MODE=1`（挂 `POST /tenon/admin/modules`，D1 用它摘模块）、**`TENON_STRICT_RULES=1`**（业务规则没加载即启动失败，见下）、`TENON_MODULES=quote=…,inventory=…,procurement=…`、`TENON_USERS_FILE=config/users.dev.json`、`TENON_SESSION_SECRET`、`TENON_SKILLS_DIR=skills`、`TENON_POLICY_FILE=config/policies.dev.json`、`TENON_GRANT_FILE=config/grants.dev.json`、`TENON_INBOUND_DIR=out/d1/inbound` |
| agent | `TENON_HOST_URL=http://127.0.0.1:8080`、`TENON_AGENT_ID=ag_fulfillment`、`TENON_ON_BEHALF_OF=user:alice`、`TENON_ONCE=1`（**由外层循环反复拉起**）、`TENON_RESULT_FILE=out/d1/agent-result.jsonl`、`TENON_AGENT_STATE_DIR=out/d1/agent-state`、`TENON_AGENT_USER=agent`、`TENON_AGENT_PASSWORD=agent-dev`、`TENON_LLM_API_KEY_FILE=docker/secrets/llm.key`、`TENON_LLM_MODEL=deepseek-v4-pro` |
| d1 | `tenon d1 run --host … --quote … --inventory … --procurement … --agent-result out/d1/agent-result.jsonl --report out/d1/report.json` |

**模型选择**：`deepseek-v4-pro`（规划质量优先，一次判定总共两次调用，成本可忽略）。脚本里写成可覆盖的缺省值，`deepseek-flash` 作为回退。

**运行环境的一处硬约束（实测）**：本机只有 Windows PowerShell 5.1，脚本必须存成**带 UTF-8 BOM 的 .ps1**，否则中文串会被按系统代码页解码并报 `string is missing the terminator`；调用方式是 `powershell -NoProfile -File scripts/d1-e2e.ps1`。

**凭据注入**：只走 `TENON_LLM_API_KEY_FILE=docker/secrets/llm.key`（`docker/secrets/` 已在 `.gitignore`）。key 不进命令行、不进日志、不进仓库。

**生命周期（脚本顺序即断言顺序）**

1. `go build ./...` 失败 → 退出；
2. 探测端口占用（8080–8083），被占则报出占者信息并退出；
3. 起三模块 → 轮询各自 `/readyz`（超时即退出，打印该进程 stderr）；
4. 起 host → 轮询 `/readyz`。业务规则是否真的加载**不靠抓日志**：host 用 `TENON_STRICT_RULES=1` 启动，任一份业务规则未生效即启动失败（`cmd/host/main.go:24` 的语义），所以「host 起来了」就等于「规则加载了」，比 grep 一行中文日志可靠；
5. 起 agent 重启循环（`TENON_ONCE=1`，退出即以空游标重启）；
6. 执行 `tenon d1 run`；
7. 收尸（结束所有子进程），保留 `out/d1/` 下全部产物。

任一步失败都**不产出 verdict**，只产出失败原因：宁可不判定，也不给假结论。

### 2.2 判定与报告

- verdict 来源：`out/d1/report.json`（runner 产物，含两轮动作序列与四条合取的逐条结论）；
- 证据来源：`out/d1/agent-result.jsonl`（每回合一行：结局、planId、动作序列）；
- 落文档：`README.md` 新增一节「D1 判定（2026-10-27）」，写 verdict + 四条逐条 + 复现命令。**SKIP 与 FAIL 都不写成 PASS**。

### 2.3 判 fail 时的交付（已定边界）

不做调参，只交三样：

1. A/B 两轮的动作序列对照（谁碰了被摘模块）；
2. B 轮结局（是否为 `needs_human`）；
3. 模型 `rationale` 原文摘录（判断是「prompt 里没交代被摘模块」还是「模型无视了事实」）。

在此之上给**最小修复建议**（例如：prompt 的能力摘要是否需要显式标注 `enabled=false` 的模块不可用），但不实现。

---

## 3. C：收口清单

| # | 动作 | 具体改动 | 证据 |
|---|---|---|---|
| 1 | 勾 P2 复选框 | `docs/superpowers/plans/2026-10-27-tenon-agent-resume-p2.md` 的 Task 11（5 个 Step）、Task 12（4 个 Step） | Task 11 = `7cdaa9f`；Task 12 文档 = `ed2e69d`；手工冒烟 = `out/agent-state/pending/archived/pln_smoke1.json` |
| 2 | README 漂移 | (a) 139 行标题「后端就绪，链路未通」→「后端与页面已就绪，链路已通」；(b) 227-229「还没做的」删掉「前端页面」并改为「页面已交付（无构建链静态页），仍缺 amend/undo/WS」；(c) 243「M1（剩余）」删掉已完成的「agent 挂起/恢复 + pendingPlanId」 | 与 `586de2f`、P2 落地事实一致 |
| 3 | docker/README 漂移 | 110-128 表格：完整镜像与端到端 `up` 两行改为「已验证（2026-10-27，compose smoke，四个服务 + stub 模型）」，并**注明冒烟用 stub 而非真模型** | `out/docker-smoke/` 产物 + `tenon/{host,agent,inventory,procurement,llmstub}` 镜像存在 |
| 4 | 补 `internal/controlplane/static_test.go` | 四条：未配 `TENON_UI_DIR` → 503；`/ui/../secret` 越界拒绝；正常文件 → 200 且 `Content-Type` 合理；非 GET/HEAD → 405 | 现无任何覆盖 |
| 5 | 收口验证 + 推送 | `gofmt -l .`（无输出）、`go vet ./...`、`go test ./... -count=1`、`pnpm check`，然后 `git push origin feat/m05-host-agent` | pre-push 钩子会再跑一遍 gofmt/vet/test |

---

## 4. 验证矩阵

| 项 | 命令 | 期望 |
|---|---|---|
| 全仓测试 | `go test ./... -count=1` | 全 ok（含新增 `static_test.go`） |
| 静态检查 | `gofmt -l .` / `go vet ./...` | 无输出 / exit 0 |
| 规格与类型 | `pnpm check` | 11 项校验 0 失败 + tsc 通过 |
| 容器语法 | `docker compose config --quiet` | exit 0 |
| D1 判定 | `pwsh scripts/d1-e2e.ps1` | 产出 `out/d1/report.json`，verdict ∈ {pass, fail} 并写进 README |
| 推送 | `git push` | pre-push 通过，远端更新 `feat/m05-host-agent` |

---

## 5. 风险与已知缺口

| 风险 | 影响 | 处理 |
|---|---|---|
| 真模型在 B 轮仍尝试调用被摘模块 | D1 判 fail | 接受并如实记录；诊断三件套给出原因 |
| 真模型产出不合 schema 的计划 | 回合 `failed`，D1 无结果行 | 报告写「等不到结果行 + agent stderr」；不重试到出结果为止 |
| 本机端口被占 | 脚本起不来 | 脚本先探测并报占者，不静默重试 |
| key 泄漏 | 安全事故 | 只走 gitignored 文件 + 脚本不打印；日志里出现的只有模型名 |
| 判定一次不代表稳定 | 模型有随机性 | 报告写明「单次判定」，不宣称统计意义上的稳定性 |
| 推送后 CI 红 | 分支状态变差 | pre-push 与 CI 同源（gofmt/vet/test/spec/compose），本地先全绿 |

---

## 6. 交付物

- `scripts/d1-e2e.ps1`
- `out/d1/report.json`、`out/d1/agent-result.jsonl`（不入库，`out/` 已忽略）
- `README.md` 新增 D1 判定一节 + 两处漂移修正
- `docker/README.md` 已验证表格修正
- `internal/controlplane/static_test.go`
- P2 计划复选框勾选
- 远端 `feat/m05-host-agent` 更新
