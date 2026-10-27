# D1 判定与收口 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用真模型跑一次 D1 A/B 把「未判定」变成 pass/fail，并把分支的文档漂移、测试缺口收干净后推特性分支。

**Architecture:** 不碰判定逻辑（`internal/d1` 已完成），只补三样：一个把「4 个进程 + agent 每轮换进程」写成断言的编排脚本、一份可归档的判定报告与 README 记录、一处静态页路由测试。然后修文档、勾计划、推送。

**Tech Stack:** PowerShell 7（本机 Windows）、Go 1.26、`tenon d1 run`、DeepSeek OpenAI 兼容接口（`TENON_LLM_*`）。

**对应 spec:** [`docs/superpowers/specs/2026-10-27-tenon-d1-verdict-closeout-design.md`](../specs/2026-10-27-tenon-d1-verdict-closeout-design.md)

---

## 文件结构

| 文件 | 职责 | 动作 |
|---|---|---|
| `internal/controlplane/static_test.go` | 静态页路由：503 / 越界 / 200 / 405 | 新建 |
| `scripts/d1-e2e.ps1` | D1 编排：构建 → 起模块 → 起 host → agent 重启循环 → 跑 d1 → 收尸 | 新建 |
| `README.md` | D1 判定结果 + 两处漂移 | 修改 |
| `docker/README.md` | 「已验证」表格与事实对齐 | 修改 |
| `docs/superpowers/plans/2026-10-27-tenon-agent-resume-p2.md` | 勾 Task 11/12 复选框 | 修改 |

---

## Task 1: 静态页路由测试（补覆盖，不改实现）

**Files:**
- Test: `internal/controlplane/static_test.go`

这一步是给**已有行为**补覆盖，所以用例应当一次跑绿；若某条失败，说明 `static.go` 与注释里的承诺不符，先按注释修实现再继续。

- [ ] **Step 1: 写用例**

新建 `internal/controlplane/static_test.go`：

```go
package controlplane

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 未配 TENON_UI_DIR 必须是 503：这是可诊断的部署问题，
// 伪装成 404 会让运维去查路由，查错方向（static.go:16-18）。
func TestStaticUnavailableWithoutUIDir(t *testing.T) {
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未配 UI 目录应 503，得到 %d", resp.StatusCode)
	}
}

// 配了目录但目录不存在，同样是 503（部署事故要能区分「忘配」与「忘建」）。
func TestStaticUnavailableWhenDirMissing(t *testing.T) {
	s := newTestServer(t, Config{
		AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"),
		UIDir:     filepath.Join(t.TempDir(), "no-such-ui"),
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("目录不存在应 503，得到 %d", resp.StatusCode)
	}
}

// 目录里的真实文件要能取到；`/` 映射到 index.html（uiPath 的约定）。
func TestStaticServesIndexAndAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>t</title>"), 0o644); err != nil {
		t.Fatalf("写 index.html 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatalf("写 app.js 失败：%v", err)
	}
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"), UIDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/ui/app.js"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s 失败：%v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 应 200，得到 %d", path, resp.StatusCode)
		}
	}
}

// 目录穿越必须被拒（filepath.IsLocal 那条规则）：`/ui/../secret` 读到 uiDir 之外
// 等于把仓库里任意文件挂到公网。
func TestStaticRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o644); err != nil {
		t.Fatalf("写上级文件失败：%v", err)
	}
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"), UIDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/ui/../secret.txt", nil)
	if err != nil {
		t.Fatalf("造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET 越界路径失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("越界路径应 404（不是 200、也不是 500），得到 %d", resp.StatusCode)
	}
}

// 只接 GET/HEAD：其它方法 405，不把方法错误降级成 404。
func TestStaticRejectsNonGet(t *testing.T) {
	s := newTestServer(t, Config{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl"), UIDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST / 失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / 应 405，得到 %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: 跑用例**

Run: `go test ./internal/controlplane/ -run 'TestStatic' -count=1 -v`
Expected: 5 个用例 PASS。若 `TestStaticRejectsTraversal` 拿到 200/500，先修 `static.go` 的 `uiPath`（不得放宽 `filepath.IsLocal`），再重跑。

- [ ] **Step 3: Commit**

```bash
git add internal/controlplane/static_test.go
git commit -m "test(controlplane): 静态页路由补覆盖（503/越界/200/405）"
```

---

## Task 2: D1 编排脚本

**Files:**
- Create: `scripts/d1-e2e.ps1`

- [ ] **Step 1: 写脚本**

新建 `scripts/d1-e2e.ps1`：

```powershell
#!/usr/bin/env pwsh
# D1 真模型判定：起三个模块 + host + agent 重启循环，跑 tenon d1 run，收尸。
#
# 为什么需要脚本：D1 的前置条件比「跑个测试」苛刻——host 与三个模块都要测试态
# （否则两轮输入无法相同）、agent 每轮必须是新进程（模块 reset 会把发件箱序号清回 0，
# 长驻 agent 的游标会把 B 轮新事件当成已读吞掉，见 internal/d1/runner.go:26-32）。
# 手工会漏，漏了就得到「等不到结果行」的假 fail。
#
# 用法：
#   pwsh scripts/d1-e2e.ps1                 # 真模型（读 docker/secrets/llm.key）
#   pwsh scripts/d1-e2e.ps1 -Stub           # 只验编排：用 tools/llmstub，不出判定
#   pwsh scripts/d1-e2e.ps1 -Model deepseek-flash
[CmdletBinding()]
param(
	[string]$Model   = 'deepseek-v4-pro',
	[string]$BaseUrl = 'https://api.deepseek.com',
	[string]$KeyFile = 'docker/secrets/llm.key',
	[switch]$Stub,
	[int]$RoundTimeoutSec = 120
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root

$out = Join-Path $root 'out/d1'
$bin = Join-Path $out 'bin'
$logDir = Join-Path $out 'logs'
New-Item -ItemType Directory -Force -Path $bin, $logDir, (Join-Path $out 'inbound') | Out-Null

function Fail([string]$message) {
	Write-Host "✗ $message" -ForegroundColor Red
	exit 1
}

# 1) 端口：被占时报出占者，不静默重试（重试会连到别人的进程上，给出更离谱的结论）。
$ports = [ordered]@{ 8080 = 'host'; 8081 = 'inventory'; 8082 = 'procurement'; 8083 = 'quote' }
if ($Stub) { $ports[8099] = 'llmstub' }
foreach ($port in $ports.Keys) {
	$owner = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
	if ($owner) { Fail "端口 $port（$($ports[$port])）被占用：PID $($owner.OwningProcess -join ',')" }
}

# 2) 凭据：key 只从文件读（docker/secrets/ 已 gitignore），不进命令行、不进日志。
$keyPath = $null
if (-not $Stub) {
	if (-not (Test-Path $KeyFile)) { Fail "缺少 key 文件 $KeyFile" }
	$keyPath = (Resolve-Path $KeyFile).Path
}

# 3) 构建：先构建再起进程，避免「go run 还在编译，readyz 超时」的假失败。
foreach ($svc in 'host', 'agent', 'inventory', 'procurement', 'quote', 'tenon') {
	& go build -o (Join-Path $bin "$svc.exe") "./cmd/$svc"
	if ($LASTEXITCODE -ne 0) { Fail "构建 $svc 失败" }
}
if ($Stub) {
	& go build -o (Join-Path $bin 'llmstub.exe') ./tools/llmstub
	if ($LASTEXITCODE -ne 0) { Fail '构建 llmstub 失败' }
}

# Start-Svc：只给这个子进程设环境变量，起完立刻还原，避免串味。
function Start-Svc([string]$name, [hashtable]$envMap) {
	$exe = Join-Path $bin "$name.exe"
	$saved = @{}
	foreach ($k in $envMap.Keys) {
		$saved[$k] = [Environment]::GetEnvironmentVariable($k)
		[Environment]::SetEnvironmentVariable($k, [string]$envMap[$k])
	}
	$p = Start-Process -FilePath $exe -WorkingDirectory $root -PassThru -NoNewWindow `
		-RedirectStandardOutput (Join-Path $logDir "$name.out") `
		-RedirectStandardError (Join-Path $logDir "$name.err")
	foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
	return $p
}

function Wait-Ready([string]$name, [string]$url, [int]$timeoutSec = 40) {
	$deadline = (Get-Date).AddSeconds($timeoutSec)
	while ((Get-Date) -lt $deadline) {
		try {
			$resp = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2
			if ($resp.StatusCode -eq 200) { return }
		} catch { }
		Start-Sleep -Milliseconds 250
	}
	$tail = ''
	$errFile = Join-Path $logDir "$name.err"
	if (Test-Path $errFile) { $tail = (Get-Content $errFile -Tail 20 | Out-String) }
	Fail "$name 未就绪（$url）。stderr 尾部：`n$tail"
}

$procs = @()
$agentJob = $null
$exitCode = 2
try {
	if ($Stub) {
		$procs += Start-Svc 'llmstub' @{ PORT = '8099' }
		Wait-Ready 'llmstub' 'http://127.0.0.1:8099/healthz'
	}

	# 三个模块：全部测试态，两轮 A/B 才能「同输入」。
	$procs += Start-Svc 'inventory' @{
		PORT = '8081'; TENON_MODULE_ID = 'inventory'; TENON_TEST_MODE = '1'
		TENON_MODULE_AUDIT_PATH = "$out/audit-inventory.jsonl"
	}
	$procs += Start-Svc 'procurement' @{
		PORT = '8082'; TENON_MODULE_ID = 'procurement'; TENON_TEST_MODE = '1'
		TENON_MODULE_AUDIT_PATH = "$out/audit-procurement.jsonl"
	}
	$procs += Start-Svc 'quote' @{
		PORT = '8083'; TENON_MODULE_ID = 'quote'; TENON_TEST_MODE = '1'
		TENON_MODULE_AUDIT_PATH = "$out/audit-quote.jsonl"
	}
	Wait-Ready 'inventory' 'http://127.0.0.1:8081/readyz'
	Wait-Ready 'procurement' 'http://127.0.0.1:8082/readyz'
	Wait-Ready 'quote' 'http://127.0.0.1:8083/readyz'

	# 模块先起、host 后起：host 加载期要拉每个模块的 actions 来校验业务规则，
	# 模块不在时规则会静默不加载。STRICT_RULES=1 把这条变硬：规则没加载就起不来。
	$procs += Start-Svc 'host' @{
		PORT = '8080'; TENON_TEST_MODE = '1'; TENON_STRICT_RULES = '1'
		TENON_MODULES = 'quote=http://127.0.0.1:8083,inventory=http://127.0.0.1:8081,procurement=http://127.0.0.1:8082'
		TENON_USERS_FILE = 'config/users.dev.json'; TENON_SESSION_SECRET = 'dev'
		TENON_SKILLS_DIR = 'skills'; TENON_POLICY_FILE = 'config/policies.dev.json'
		TENON_GRANT_FILE = 'config/grants.dev.json'; TENON_INBOUND_DIR = "$out/inbound"
		TENON_AUDIT_PATH = "$out/audit-host.jsonl"
	}
	Wait-Ready 'host' 'http://127.0.0.1:8080/readyz'

	$agentEnv = @{
		TENON_HOST_URL = 'http://127.0.0.1:8080'; TENON_AGENT_ID = 'ag_fulfillment'
		TENON_ON_BEHALF_OF = 'user:alice'; TENON_ONCE = '1'
		TENON_AGENT_USER = 'agent'; TENON_AGENT_PASSWORD = 'agent-dev'
		TENON_AGENT_STATE_DIR = "$out/agent-state"; TENON_RESULT_FILE = "$out/agent-result.jsonl"
		TENON_SKILLS_DIR = 'skills'; TENON_LLM_TIMEOUT = '120s'
	}
	if ($Stub) {
		$agentEnv.TENON_LLM_BASE_URL = 'http://127.0.0.1:8099/v1'
		$agentEnv.TENON_LLM_API_KEY = 'dummy'
		$agentEnv.TENON_LLM_MODEL = 'stub-plan'
	} else {
		$agentEnv.TENON_LLM_BASE_URL = $BaseUrl
		$agentEnv.TENON_LLM_API_KEY_FILE = $keyPath
		$agentEnv.TENON_LLM_MODEL = $Model
	}

	# agent 重启循环：每轮一个全新进程（空游标），这是 D1 的前提条件。
	$agentExe = Join-Path $bin 'agent.exe'
	$agentJob = Start-Job -Name d1agent -ScriptBlock {
		param($exe, $workDir, $logPath, $envMap)
		Set-Location $workDir
		foreach ($k in $envMap.Keys) { Set-Item -Path "env:$k" -Value ([string]$envMap[$k]) }
		while ($true) {
			& $exe *>> $logPath
			Start-Sleep -Milliseconds 300
		}
	} -ArgumentList $agentExe, $root, (Join-Path $logDir 'agent.err'), $agentEnv

	$reportPath = Join-Path $out 'report.json'
	& (Join-Path $bin 'tenon.exe') d1 run `
		--host 'http://127.0.0.1:8080' --quote 'http://127.0.0.1:8083' `
		--inventory 'http://127.0.0.1:8081' --procurement 'http://127.0.0.1:8082' `
		--agent-result (Join-Path $out 'agent-result.jsonl') `
		--report $reportPath --round-timeout "${RoundTimeoutSec}s"
	$exitCode = $LASTEXITCODE

	Write-Host ''
	Write-Host "d1 退出码：$exitCode" -ForegroundColor Cyan
	if (Test-Path $reportPath) {
		Write-Host "报告：$reportPath" -ForegroundColor Cyan
		Get-Content $reportPath -Raw
	} else {
		Write-Host '没有产出报告：说明前置条件没满足（host/模块/agent 有一环没起来）' -ForegroundColor Yellow
	}
}
finally {
	if ($agentJob) {
		Stop-Job $agentJob -ErrorAction SilentlyContinue
		Remove-Job $agentJob -Force -ErrorAction SilentlyContinue
	}
	foreach ($p in $procs) {
		if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
	}
}

exit $exitCode
```

- [ ] **Step 2: 语法自检**

Run: `pwsh -NoProfile -Command "$null = [scriptblock]::Create((Get-Content scripts/d1-e2e.ps1 -Raw)); 'syntax ok'"`
Expected: 输出 `syntax ok`（能解析即可；真正执行在 Task 3）

- [ ] **Step 3: Commit**

```bash
git add scripts/d1-e2e.ps1
git commit -m "feat(scripts): D1 编排（模块先起、agent 每轮换进程、失败不产假 verdict）"
```

---

## Task 3: 用 stub 先验编排（不消耗真 key，不产判定）

**Files:** 无（只跑）

- [ ] **Step 1: 跑 stub 模式**

Run: `pwsh -NoProfile -File scripts/d1-e2e.ps1 -Stub -RoundTimeoutSec 60`
Expected 之一：

- 编排正确：`d1 退出码：0` 或 `1`，`out/d1/report.json` 存在；
- 若报告里 verdict=fail 且原因是模型没给出预期计划——**这是 stub 的正常表现**，不代表编排错。

- [ ] **Step 2: 只验编排的三条断言**

逐条确认（这是这一步的全部目的）：

1. `out/d1/logs/host.err` 里**没有**「业务级规则未生效」类错误，且 host 确实起来了（`STRICT_RULES=1` 生效）；
2. `out/d1/report.json` 存在，且 JSON 里有 `verdict` 字段；
3. `out/d1/agent-result.jsonl` 有行（说明 agent 真的被循环拉起并处理过事件）。

若 1 失败：看 `logs/modules.err`，多半是模块没在 host 之前就绪。
若 3 为空：看 `logs/agent.err`，多半是登录失败或 LLM 指错。

- [ ] **Step 3: 无代码改动，不提交**

（若为了让 stub 也能跑通而改了脚本，那就回到 Task 2 的 Step 3 一起提交。）

---

## Task 4: 真跑判定并写进 README

**Files:**
- Modify: `README.md`（在「D1 差异测试」一节之后新增判定段）

- [ ] **Step 1: 真跑**

Run: `pwsh -NoProfile -File scripts/d1-e2e.ps1`
Expected: 打印报告；`out/d1/report.json` 存在；退出码 0（pass）或 1（fail）

- [ ] **Step 2: 读报告，抄四条合取**

打开 `out/d1/report.json`，记录：`verdict`、两轮的动作序列、B 轮是否碰了被摘模块、B 轮结局、失败原因（若有）。

- [ ] **Step 3: 写 README 判定段**

先把该节标题从「### D1 差异测试：runner 已实现，结论未判定」改成「### D1 差异测试：runner 已实现，判定见下」（否则标题与本轮结论自相矛盾）。

然后在该节末尾追加：

```markdown
### D1 判定结果（2026-10-27，真模型 A/B）

模型 `deepseek-v4-pro`，复现命令 `pwsh scripts/d1-e2e.ps1`，报告 `out/d1/report.json`。

| 判据 | 结果 | 证据 |
|---|---|---|
| A/B 两轮动作序列不同 | <pass/fail> | <两轮的 actions> |
| B 轮不碰被摘的 procurement | <pass/fail> | <B 轮 actions> |
| B 轮转人工（needs_human） | <pass/fail> | <B 轮 outcome> |
| 两轮都没崩（failed） | <pass/fail> | <outcome> |

**结论：<PASS / FAIL>**。<一句话说明：差在哪一条、下一步最小修复是什么。>

> 口径：这是**单次判定**，不宣称统计稳定性；stub 模型的容器冒烟（`out/docker-smoke`）不算判定。
```

（把 `<...>` 全部替换成报告里的真实值；**不许**在 fail 时写成 pass。）

- [ ] **Step 4: 若 fail，补诊断段**

在判定段下面加三小节，内容全部来自报告与 `out/d1/agent-result.jsonl`：

```markdown
**根因诊断（fail 时）**

1. 被摘模块是否仍出现在 B 轮计划里：<是/否>，出现于 <step>；
2. B 轮结局：<outcome>，与「转人工」的期望差在 <...>；
3. 模型 rationale 摘录：<原文两句>。
```

并在末尾给出**最小修复建议**（只写建议、不改代码）：例如「能力摘要是否需要显式标注 `enabled=false` 的模块不可用」。

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs(README): D1 判定结果与复现命令"
```

---

## Task 5: README 两处漂移

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 改标题（第 139 行）**

把：

```markdown
### 对话入口（M1 切片 1）：后端就绪，链路未通
```

改成：

```markdown
### 对话入口（M1 切片 1 + 切片 2）：后端与页面已就绪，链路已通
```

- [ ] **Step 2: 改「还没做的」（第 227-229 行）**

把列表里的：

```markdown
**前端页面**（`GET /` 只是路由，`TENON_UI_DIR` 不配就回 503；对话入口有后端、没有界面）、
```

替换成：

```markdown
页面已交付（无构建链静态页，`TENON_UI_DIR=ui`），仍缺 `amend`（改步骤再批）与撤销/补偿的界面、
```

- [ ] **Step 3: 改里程碑表「M1（剩余）」（第 243 行）**

把该行开头的内容项：

```markdown
agent 挂起/恢复 + `pendingPlanId` + 补偿 UI 的 amend + 共驾 WS + grant 真校验 + 审计对账
```

改成（删掉已完成项，保留未做项）：

```markdown
补偿 UI 的 amend + 共驾 WS + grant 真校验 + 审计对账 + D1 未过项
```

- [ ] **Step 4: 确认没有别处仍在说「没有界面」**

Run: `Select-String -Path README.md -Pattern '没有界面|未通|前端页面'`
Expected: 无命中（若有残留，按同样口径改掉）

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs(README): 去掉「链路未通」「无前端页面」等过期描述"
```

---

## Task 6: docker/README 的「已验证」表格

**Files:**
- Modify: `docker/README.md`

- [ ] **Step 1: 改三行**

把表格里这三行：

```markdown
| `go-service` **完整镜像**（运行时阶段） | ❌ 未验：`alpine:3.20` 本机拉不到 | — |
| `compose.m0.yaml` 端到端 `up` | ❌ 未验 | — |
| 命名卷对 uid 10001 的可写性 | ❌ 未验 | — |
```

替换成：

```markdown
| 对话入口四服务（`inventory`/`procurement`/`host`/`agent`）端到端 | ✅ 跑通：投一句话 → 挂起 → bob 批准 → 库存 120→108 → 归档 `done`；审计 step 级 `approvedBy=user:bob`、哈希链连续 | 2026-10-27，`compose.yaml` + `-profile smoke` |
| `compose.m0.yaml` 端到端 `up`（M0.5 拓扑） | ❌ 未验 | — |
| 命名卷对 uid 10001 的可写性 | ❌ 未验（对话入口冒烟用宿主目录而非命名卷） | — |
```

并在表格下方的说明里补一句：

```markdown
> 对话入口冒烟用的是 `tools/llmstub`（stub 规划器），它证明的是**编排与落盘链路**，不是模型能力；D1 判定必须用真模型（见根 README 的「D1 判定结果」一节）。
```

- [ ] **Step 2: 确认没有残留的「未验」与事实冲突**

Run: `Select-String -Path docker/README.md -Pattern '端到端 `up` 未验|完整镜像.*未验'`
Expected: 无命中（M0.5 拓扑那行仍可写未验，但它指的是 `compose.m0.yaml`）

- [ ] **Step 3: Commit**

```bash
git add docker/README.md
git commit -m "docs(docker): 对话入口四服务端到端已实测，表格与事实对齐"
```

---

## Task 7: 勾 P2 计划的复选框

**Files:**
- Modify: `docs/superpowers/plans/2026-10-27-tenon-agent-resume-p2.md`（Task 11 与 Task 12 两节）

- [ ] **Step 1: 勾 Task 11（5 个 Step，原 2602-2668 行）**

把该节的 `- [ ] **Step 1: 写失败测试**` … `- [ ] **Step 5: Commit**` 五行的 `[ ]` 改成 `[x]`，`: Commit**` 那行的提交信息按实际落地的 `7cdaa9f` 更正为：

```bash
git commit -m "fix(controlplane): 测试模式下不挂入站通道（D1 可重复性）"
```

- [ ] **Step 2: 勾 Task 12（4 个 Step，原 2684-2721 行）**

同样把四行 `[ ]` 改成 `[x]`，并在 Task 12 的 Step 3（手工冒烟）下补一行证据引用：

```markdown
**证据**：`out/agent-state/pending/archived/pln_smoke1.json`（`state=done`，inventory 侧审计 1 条，宿主审计 4 条哈希链连续）。
```

- [ ] **Step 3: 校验只改了复选框与这一行证据**

Run: `git diff --stat docs/superpowers/plans/2026-10-27-tenon-agent-resume-p2.md`
Expected: `1 file changed`，`git diff` 里只有 9 处 `[ ]`→`[x]` 与 1 行新增证据

若 PowerShell 改写把整份文件的中文弄坏（编码），丢弃重来，改用编辑器逐行改。

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-10-27-tenon-agent-resume-p2.md
git commit -m "docs(plan): 勾 P2 的 Task 11/12，补冒烟证据"
```

---

## Task 8: 全量回归并推送

**Files:** 无

- [ ] **Step 1: 本地全量**

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
pnpm check
docker compose config --quiet
```

Expected: gofmt 无输出；vet/build/test 全绿；`pnpm check` 11 项 0 失败 + tsc 通过；compose 语法 exit 0

- [ ] **Step 2: 确认分支包含本轮全部提交**

Run: `git log --oneline -10`
Expected: 能看到 Task 1-7 的提交与本轮此前的 spec 提交

- [ ] **Step 3: 推送（pre-push 会再跑一遍 gofmt/vet/test）**

Run: `git push origin feat/m05-host-agent`
Expected: pre-push 输出 `[pre-push] ok`，然后 push 成功

- [ ] **Step 4: 核对远端**

Run: `git rev-list --count origin/feat/m05-host-agent..HEAD`
Expected: `0`（本地与远端一致）

---

## 自检对照（spec → 任务）

| spec 要求 | 落在 |
|---|---|
| §2.1 编排脚本与前置断言 | Task 2（代码）+ Task 3（stub 先验） |
| §2.1 端口探测 / 凭据只走文件 | Task 2 |
| §2.2 verdict 与 README 记录 | Task 4 |
| §2.3 fail 时诊断三件套 | Task 4 Step 4 |
| §3 表格第 1 项（P2 复选框） | Task 7 |
| §3 表格第 2 项（README 漂移） | Task 5 |
| §3 表格第 3 项（docker/README） | Task 6 |
| §3 表格第 4 项（static_test） | Task 1 |
| §3 表格第 5 项（回归 + 推送） | Task 8 |
