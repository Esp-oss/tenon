#!/usr/bin/env node
/**
 * module-host 最小版的端到端验收（计划 Task 3）。
 *
 * 它自己起**真进程**：Go 的 `cmd/host`（控制平面）+ `cmd/inventory`（真模块），然后分两层跑：
 *
 *   ① 编排层：直接 import `dist/orchestrate.js` 的 `runAction`，跑三条验收
 *       a) `DeductStock qty=5` → 库存 120→115、审计两段齐全；
 *       b) `qty=12`          → `needs_approval`（approve 只报告）、库存**不变**；
 *       c) 未登记动作        → `TENON_POLICY_DENIED`（fail-closed 的默认拒绝）。
 *   ② 工具层：import `dist/index.js`，用假 ctx 装插件 → 调被注册出来的 `tenon_action.execute`，
 *       断言 qty=5 得 `executed`、qty=12 得 `needs_approval`。这一层证明「模型能摸到的那一面」
 *       和编排层是同一套闸门，不是两条路。
 *
 * 为什么不用假宿主：编排的单测（`src/orchestrate.test.ts`）已经用真 HTTP 打脚本化回包钉住了
 * 请求序列；这里要钉的是**真 host 的策略引擎 + 真模块的幂等/预演/库存状态**——两件事都无法
 * 用打桩代替。假的「库存 120→115」只会证明脚本自己会加一减一。
 *
 * 端口：默认 host 8094 / inventory 8095（设计 §7 的约定）。端口被占用时自动改用一个空闲端口
 * 并打印实际值（脚本不该因为本机恰好开着别的服务而红）；也可用 TENON_E2E_HOST_PORT /
 * TENON_E2E_INVENTORY_PORT 显式指定。
 *
 * 运行产物全部落在 `out/verify-module-host/`（已被 .gitignore 的 `out/` 覆盖）：两份进程日志、
 * 临时 HMAC 密钥、本次用的策略文件、两份审计 JSONL。**每次运行先清空该目录**，因此审计链每次
 * 都是全新的一份（host 启动时会校验既有链，残留的旧链会让它起不来）。
 *
 * 用法：`node scripts/verify-module-host.mjs`
 * 退出码：0 全过 / 1 有断言失败 / 2 环境或构建失败（起不来、编译不过）。
 */
import { spawn, spawnSync } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { isDeepStrictEqual } from 'node:util'

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const RUN_DIR = path.join(REPO, 'out', 'verify-module-host')
const PLUGIN_DIR = path.join(REPO, 'packages', 'dsh-plugin-module-host')

/** 端口约定值；被占用时脚本会换一个空闲端口，并把实际值打印出来。 */
const HOST_PORT_WANTED = Number(process.env.TENON_E2E_HOST_PORT ?? 8094)
const INVENTORY_PORT_WANTED = Number(process.env.TENON_E2E_INVENTORY_PORT ?? 8095)

/** 审计 actor 的身份：只进审计，不是鉴权凭据（设计 §3）。 */
const AGENT_ID = 'ag_dsh_e2e'
const ON_BEHALF_OF = 'user:alice'

/** 库存种子（与 inventory 的缺省种子一致，断言 120→115 才有意义）。 */
const SKU = 'STD-1001'
const SEED_ON_HAND = 120

// ---------------------------------------------------------------------------
// 断言簿：每条断言都当场打印，失败也继续跑（要一次看到全部红在哪）
// ---------------------------------------------------------------------------

const tally = { pass: 0, fail: 0 }

function show(value) {
  if (value === undefined) return 'undefined'
  if (typeof value === 'function') return `[function ${value.name || 'anonymous'}]`
  try {
    const text = JSON.stringify(value)
    return text === undefined ? String(value) : text
  } catch {
    return String(value)
  }
}

function pass(label) {
  tally.pass += 1
  console.log(`  ✓ ${label}`)
}

function fail(label, detail) {
  tally.fail += 1
  console.error(`  ✗ ${label}`)
  if (detail) console.error(String(detail).split('\n').map((line) => `      ${line}`).join('\n'))
}

/** 布尔断言。 */
function ok(label, condition, detail) {
  if (condition) pass(label)
  else fail(label, detail ?? '期望为真，实际为假')
  return Boolean(condition)
}

/** 深比较断言（字面量与数组都按结构比，不看引用）。 */
function eq(label, actual, expected) {
  if (isDeepStrictEqual(actual, expected)) {
    pass(label)
    return true
  }
  fail(label, `期望：${show(expected)}\n实际：${show(actual)}`)
  return false
}

/** 正则断言（把「形状」写成机器可判定的样子）。 */
function match(label, value, pattern) {
  if (typeof value === 'string' && pattern.test(value)) {
    pass(label)
    return true
  }
  fail(label, `期望匹配：${pattern}\n实际：${show(value)}`)
  return false
}

/** 跑一个用例段落：段内抛出的意外异常（HTTP 挂了、字段缺失）也算一条失败，不中断后面。 */
async function section(title, body) {
  console.log(`\n${title}`)
  try {
    await body()
  } catch (error) {
    fail(`${title} 未能跑完`, error instanceof Error ? (error.stack ?? error.message) : String(error))
  }
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/** 起子进程前的环境：**抹掉继承来的 TENON_\***，避免开发机上的残留变量改变本次判定。 */
function cleanEnv() {
  const env = { ...process.env }
  for (const key of Object.keys(env)) {
    if (key.startsWith('TENON_')) delete env[key]
  }
  return env
}

/** 端口是否可用（按「全接口绑定」判定，与 Go 的 `:port` 同语义）。 */
function portFree(port) {
  return new Promise((resolve) => {
    const server = net.createServer()
    server.once('error', () => resolve(false))
    server.listen(port, () => server.close(() => resolve(true)))
  })
}

/** 向内核要一个空闲端口。 */
function ephemeralPort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer()
    server.once('error', reject)
    server.listen(0, () => {
      const address = server.address()
      const port = typeof address === 'object' && address !== null ? address.port : 0
      server.close(() => (port > 0 ? resolve(port) : reject(new Error('内核没有给出空闲端口'))))
    })
  })
}

/** 优先用约定端口；被占用就换一个，并明说换成了哪个。 */
async function usablePort(wanted, label) {
  if (await portFree(wanted)) return wanted
  const fallback = await ephemeralPort()
  console.log(`  ⚠ ${label} 端口 ${wanted} 已被占用，改用 ${fallback}`)
  return fallback
}

function sha256Hex(text) {
  return createHash('sha256').update(text, 'utf8').digest('hex')
}

function logTail(file, lines = 25) {
  try {
    const text = readFileSync(file, 'utf8').trimEnd()
    if (text.length === 0) return '（日志为空）'
    return text.split('\n').slice(-lines).join('\n')
  } catch {
    return '（读不到日志）'
  }
}

// ---------------------------------------------------------------------------
// 构建：Go 两个进程 + 插件的 dist
// ---------------------------------------------------------------------------

function goBuild(outName, pkg) {
  const out = path.join(RUN_DIR, outName)
  const result = spawnSync('go', ['build', '-o', out, pkg], { cwd: REPO, stdio: 'inherit' })
  if (result.error) throw new Error(`go build ${pkg} 起不来：${result.error.message}`)
  if (result.status !== 0) throw new Error(`go build ${pkg} 失败（exit=${result.status}）`)
  return out
}

/** dist 是否落后于 src（落后就重编，避免拿旧产物验收新源码）。 */
function pluginBuildReason() {
  const entry = path.join(PLUGIN_DIR, 'dist', 'index.js')
  if (!existsSync(entry)) return 'dist/index.js 不存在'
  const builtAt = statSync(entry).mtimeMs
  const srcDir = path.join(PLUGIN_DIR, 'src')
  const stale = readdirSync(srcDir)
    .filter((name) => name.endsWith('.ts'))
    .filter((name) => statSync(path.join(srcDir, name)).mtimeMs > builtAt)
  return stale.length > 0 ? `源码比 dist 新：${stale.join(', ')}` : null
}

function buildPlugin() {
  const reason = pluginBuildReason()
  if (reason === null) {
    console.log('  插件 dist 已是最新，跳过编译')
    return
  }
  console.log(`  编译插件（${reason}）…`)
  // Windows 上 pnpm 是 .cmd，必须经 shell 才能被 spawnSync 找到；参数全是常量，无注入面。
  const result = spawnSync('pnpm', ['--filter', '@tenon/dsh-plugin-module-host', 'run', 'build'], {
    cwd: REPO,
    stdio: 'inherit',
    shell: true,
  })
  if (result.error) throw new Error(`pnpm build 起不来：${result.error.message}`)
  if (result.status !== 0) throw new Error(`pnpm build 失败（exit=${result.status}）`)
  const after = pluginBuildReason()
  if (after !== null) throw new Error(`插件编译后 dist 仍不可用：${after}`)
}

// ---------------------------------------------------------------------------
// 进程管理
// ---------------------------------------------------------------------------

const processes = []

function startProcess(label, bin, env) {
  const logPath = path.join(RUN_DIR, `${label}.log`)
  const fd = openSync(logPath, 'a')
  const child = spawn(bin, [], {
    cwd: REPO,
    env,
    stdio: ['ignore', fd, fd],
    windowsHide: true,
  })
  // 子进程已 dup 这份 fd，父进程这份可以关掉（不关会在长跑时漏句柄）。
  closeSync(fd)
  const record = { label, child, logPath, exited: null }
  child.once('error', (error) => {
    record.exited = { code: null, signal: null, error: error.message }
  })
  child.once('exit', (code, signal) => {
    record.exited = { code, signal }
  })
  processes.push(record)
  return record
}

/** 进程半路死掉时先报出来：否则后面每条断言都会以「connection refused」的形式重复报同一件事。 */
function assertAlive(record) {
  if (record.exited === null) return true
  fail(
    `${record.label} 进程意外退出`,
    `exit=${show(record.exited)}\n日志尾部：\n${logTail(record.logPath)}`,
  )
  return false
}

async function stopProcess(record) {
  if (record.exited !== null) return
  const { child } = record
  if (process.platform === 'win32') {
    // 强杀是刻意的：本轮验收只从 HTTP 侧读审计（Append 时已 fsync），不需要进程优雅收尾；
    // 而留着进程会占住端口，让下一次运行莫名其妙地换端口。
    spawnSync('taskkill', ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' })
  } else {
    child.kill('SIGTERM')
  }
  for (let i = 0; i < 50 && record.exited === null; i += 1) await sleep(100)
}

// ---------------------------------------------------------------------------
// HTTP 小工具（只用真 HTTP：验收的判据就是线上报文）
// ---------------------------------------------------------------------------

const TENON_VERSION = '0.1'

async function httpJSON(url, init = {}) {
  const response = await fetch(url, {
    ...init,
    headers: {
      'Tenon-Version': TENON_VERSION,
      accept: 'application/json',
      ...(init.body === undefined ? {} : { 'content-type': 'application/json' }),
      ...(init.headers ?? {}),
    },
  })
  const text = await response.text()
  return { status: response.status, text }
}

async function getJSON(url) {
  const { status, text } = await httpJSON(url)
  if (status !== 200) throw new Error(`GET ${url} → HTTP ${status}：${text.slice(0, 200)}`)
  return JSON.parse(text)
}

/** 直接问模块要当前在库量：这是业务事实的**唯一**来源（不经过任何一层编排）。 */
async function moduleOnHand(moduleURL, sku) {
  const { status, text } = await httpJSON(`${moduleURL}/tenon/actions/inventory.GetStock`, {
    method: 'POST',
    body: JSON.stringify({ sku }),
  })
  if (status !== 200) throw new Error(`GET GetStock(${sku}) → HTTP ${status}：${text.slice(0, 200)}`)
  const body = JSON.parse(text)
  if (typeof body.onHand !== 'number') throw new Error(`GetStock 的响应里没有 onHand：${text}`)
  return body.onHand
}

/** 按 planId 查审计（host 的 `GET /tenon/audit?planId=`）。 */
async function auditFor(hostURL, planId) {
  const body = await getJSON(`${hostURL}/tenon/audit?planId=${encodeURIComponent(planId)}`)
  if (!Array.isArray(body.records)) throw new Error(`审计查询的响应缺少 records 数组：${show(body)}`)
  return body.records
}

/** 全量审计条数：用来证明「待批」与「策略拒绝」一次都没写审计。 */
async function auditTotal(hostURL) {
  const body = await getJSON(`${hostURL}/tenon/audit`)
  if (!Array.isArray(body.records)) throw new Error(`审计查询的响应缺少 records 数组：${show(body)}`)
  // 记录数远小于一页（本轮最多 6 条），nextCursor 不会翻页；真超了就让断言红出来。
  const next = Number(body.nextCursor ?? 0)
  if (next !== body.records.length) {
    throw new Error(`审计记录超过一页（records=${body.records.length}，nextCursor=${next}），本脚本只读一页`)
  }
  return body.records.length
}

async function waitForHost(hostURL, record, timeoutMs = 25_000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (record.exited !== null) {
      throw new Error(`host 进程在就绪前退出（exit=${show(record.exited)}）：\n${logTail(record.logPath)}`)
    }
    try {
      const { status } = await httpJSON(`${hostURL}/healthz`)
      if (status === 200) return
    } catch {
      // 还没 listen：继续等。
    }
    await sleep(150)
  }
  throw new Error(`host 在 ${timeoutMs}ms 内没有就绪：\n${logTail(record.logPath)}`)
}

/** 等模块完成自注册（注册是 push 的，模块启动后异步申报；host 可见视图只含 alive 的条目）。 */
async function waitForModule(hostURL, moduleId, record, timeoutMs = 25_000) {
  const deadline = Date.now() + timeoutMs
  let last = ''
  while (Date.now() < deadline) {
    if (record.exited !== null) {
      throw new Error(
        `${moduleId} 进程在注册前退出（exit=${show(record.exited)}）：\n${logTail(record.logPath)}`,
      )
    }
    try {
      const body = await getJSON(`${hostURL}/tenon/modules`)
      const entry = (body.modules ?? []).find((item) => item?.id === moduleId)
      if (entry && typeof entry.baseURL === 'string') return entry
      last = show(body.modules)
    } catch (error) {
      last = error instanceof Error ? error.message : String(error)
    }
    await sleep(200)
  }
  throw new Error(`${moduleId} 在 ${timeoutMs}ms 内没有出现在 GET /tenon/modules（最后一次：${last}）`)
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

async function main() {
  console.log('module-host 端到端验收（真 host + 真 inventory + 真编排器）')

  // 只清自己那一层目录：路径是先算出来再核对的，绝不对「计算出来的路径」动手。
  if (!RUN_DIR.endsWith(path.join('out', 'verify-module-host'))) {
    throw new Error(`拒绝清理意外的运行目录：${RUN_DIR}`)
  }
  rmSync(RUN_DIR, { recursive: true, force: true })
  mkdirSync(RUN_DIR, { recursive: true })

  const hostPort = await usablePort(HOST_PORT_WANTED, 'host')
  const inventoryPort = await usablePort(INVENTORY_PORT_WANTED, 'inventory')
  const hostURL = `http://127.0.0.1:${hostPort}`
  const moduleURL = `http://127.0.0.1:${inventoryPort}`
  console.log(`  host=${hostURL}  inventory=${moduleURL}`)
  console.log(`  运行目录：${path.relative(REPO, RUN_DIR)}`)

  // 临时密钥文件：host 缺 TENON_HMAC_SECRET_FILE 会拒绝启动（D7），模块也靠它给注册请求签名。
  const secretPath = path.join(RUN_DIR, 'hmac.key')
  writeFileSync(secretPath, `${randomBytes(24).toString('hex')}\n`, 'utf8')

  // 本次验收用的策略：**只为 inventory.DeductStock 登记两条规则**，刻意不含通配——
  // 未登记的动作（inventory.NoSuchAction）因此无规则命中，由 fail-closed 判 deny，
  // 这正是第 c 条要验的东西。qty>10 转人工、其余自动。
  const policiesPath = path.join(RUN_DIR, 'policies.json')
  writeFileSync(
    policiesPath,
    `${JSON.stringify(
      [
        {
          policyId: 'e2e-inventory-deduct-approve-large',
          priority: 100,
          match: { module: 'inventory', actions: ['inventory.DeductStock'] },
          when: { all: [{ 'arg.qty': { '>': 10 } }] },
          outcome: 'approve',
          approvers: ['role:warehouse_manager'],
        },
        {
          policyId: 'e2e-inventory-deduct-auto-small',
          priority: 10,
          match: { module: 'inventory', actions: ['inventory.DeductStock'] },
          when: { all: [] },
          outcome: 'auto',
        },
      ],
      null,
      2,
    )}\n`,
    'utf8',
  )

  // 授权表：本轮编排**不调** grants:evaluate（M0.5 没有 grant 签发，设计 §2），
  // 这里给一份显式的空表，只是为了让 host 的启动日志不出现「所有调用都会被拒绝」这句误导。
  const grantsPath = path.join(RUN_DIR, 'grants.json')
  writeFileSync(grantsPath, '{"grants":[],"subjects":{}}\n', 'utf8')

  console.log('\n构建：')
  buildPlugin()
  const hostBin = goBuild('tenon-host.exe', './cmd/host')
  const inventoryBin = goBuild('tenon-inventory.exe', './cmd/inventory')

  const baseEnv = cleanEnv()
  const hostEnv = {
    ...baseEnv,
    PORT: String(hostPort),
    TENON_POLICY_FILE: policiesPath,
    TENON_GRANT_FILE: grantsPath,
    TENON_AUDIT_PATH: path.join(RUN_DIR, 'audit-host.jsonl'),
    TENON_HMAC_SECRET_FILE: secretPath,
    TENON_INBOUND_DIR: path.join(RUN_DIR, 'inbound'),
    TENON_REGISTRY_EVENTS: path.join(RUN_DIR, 'registry-events.jsonl'),
    TENON_SKILLS_DIR: path.join(REPO, 'skills'),
    // 显式给出会话密钥：不给的话 host 会用 dev 缺省值并告警，日志里多一句噪音。
    TENON_SESSION_SECRET: 'verify-module-host-e2e',
  }
  const inventoryEnv = {
    ...baseEnv,
    PORT: String(inventoryPort),
    TENON_MODULE_ID: 'inventory',
    TENON_HOST: hostURL,
    TENON_HMAC_SECRET_FILE: secretPath,
    TENON_MODULE_AUDIT_PATH: path.join(RUN_DIR, 'audit-inventory.jsonl'),
    TENON_INVENTORY_SEED: `${SKU}:${SEED_ON_HAND},STD-1002:3`,
    // 心跳按 200ms（只认 time.ParseDuration 形式）：注册是 push 的，缺省 5s 会让本脚本白等。
    TENON_HEARTBEAT_INTERVAL: '200ms',
  }

  // 先起 host、后起模块：注册是 push 的，host 先就绪时模块一启动就能登记，
  // 不必等下一个心跳周期。业务规则（skills/）引用的 procurement 未注册，会被 host 挂起，
  // 不影响本次判定（那批规则只匹配 procurement.*）。
  const hostProc = startProcess('host', hostBin, hostEnv)
  const inventoryProc = startProcess('inventory', inventoryBin, inventoryEnv)

  try {
    await waitForHost(hostURL, hostProc)
    console.log('  host 已就绪（/healthz 200）')
    await waitForModule(hostURL, 'inventory', inventoryProc)
    console.log('  inventory 已自注册（GET /tenon/modules 可见）')

    const config = { hostURL, agentID: AGENT_ID, onBehalfOf: ON_BEHALF_OF, timeoutMs: 10_000 }
    const { runAction } = await import(pathToFileURL(path.join(PLUGIN_DIR, 'dist', 'orchestrate.js')).href)
    const { canonicalJSON } = await import(pathToFileURL(path.join(PLUGIN_DIR, 'dist', 'bridge.js')).href)

    // 每次调用给一个独有的单据号：模块侧业务幂等键是 sku+referenceNo，
    // 撞号会让第二次调用变成「回放首次结果」，库存就不动了——那会把用例变成假通过。
    const stamp = Date.now().toString(36).toUpperCase()
    const ref = (suffix) => `REF-E2E-${stamp}-${suffix}`

    await section('a) 编排层 · DeductStock qty=5（auto 全链路：策略 → 预演 → 审计 → 执行 → 审计）', async () => {
      if (!assertAlive(hostProc) || !assertAlive(inventoryProc)) return

      const before = await moduleOnHand(moduleURL, SKU)
      eq(`STD-1001 起始在库量是 ${SEED_ON_HAND}（inventory 种子）`, before, SEED_ON_HAND)

      const args = { sku: SKU, qty: 5, referenceNo: ref('A') }
      const outcome = await runAction({ config, module: 'inventory', action: 'DeductStock', args })

      eq('结局是 executed（真执行，不是待批）', outcome?.status, 'executed')
      eq('出参里的 onHand 就是扣完的 115', outcome?.result?.onHand, SEED_ON_HAND - 5)
      match('planId 形如 pln_<16 位 base32>', outcome?.planId, /^pln_[A-Za-z0-9_-]{16}$/)

      const after = await moduleOnHand(moduleURL, SKU)
      eq('库存真减了：120 → 115', `${before}→${after}`, '120→115')

      // ---- 审计两段 ----
      const records = await auditFor(hostURL, outcome?.planId)
      eq('审计恰好两段（dispatched + result）', records.map((record) => record.stage), [
        'dispatched',
        'result',
      ])
      const dispatched = records.find((record) => record.stage === 'dispatched')
      const result = records.find((record) => record.stage === 'result')
      if (!dispatched || !result) return

      eq('两段共用同一个 auditId', [dispatched.auditId, result.auditId], [result.auditId, result.auditId])
      eq('两段指向同一份计划与同一步', [dispatched.planId, result.planId, result.stepId], [
        outcome?.planId,
        outcome?.planId,
        's1',
      ])
      eq('两段的 action 都是 inventory.DeductStock', [dispatched.action, result.action], [
        'inventory.DeductStock',
        'inventory.DeductStock',
      ])
      eq('dispatched 段的 result.status = unknown（执行前结果未定）', dispatched.result?.status, 'unknown')
      eq('result 段的 result.status = ok', result.result?.status, 'ok')
      match(
        'result 段的 outputHash 是 sha256 摘要',
        result.result?.outputHash,
        /^sha256:[0-9a-f]{64}$/,
      )
      eq(
        'inputHash 摘要的就是实际发出的那份参数（规范 JSON）',
        result.inputHash,
        `sha256:${sha256Hex(canonicalJSON(args))}`,
      )
      eq('两段的幂等键都是 tenon:<planId>/s1', [dispatched.idempotencyKey, result.idempotencyKey], [
        `tenon:${outcome?.planId}/s1`,
        `tenon:${outcome?.planId}/s1`,
      ])
      eq(
        'actor 身份来自部署配置（不是模型参数）',
        result.actor,
        { type: 'agent', id: AGENT_ID, onBehalfOf: ON_BEHALF_OF },
      )
      eq('auto 路径没有人批准：approvedBy 为空', [dispatched.decision?.approvedBy, result.decision?.approvedBy], [
        null,
        null,
      ])
      eq(
        'decision.policyId 是策略引擎真的裁决过的那条规则',
        result.decision?.policyId,
        'e2e-inventory-deduct-auto-small',
      )
      eq('审计链连续（第二段的 prevHash = 第一段的 hash）', result.prevHash, dispatched.hash)
    })

    await section('b) 编排层 · DeductStock qty=12（approve 只报告待批，绝不执行）', async () => {
      if (!assertAlive(hostProc) || !assertAlive(inventoryProc)) return

      const before = await moduleOnHand(moduleURL, SKU)
      const totalBefore = await auditTotal(hostURL)

      const outcome = await runAction({
        config,
        module: 'inventory',
        action: 'DeductStock',
        args: { sku: SKU, qty: 12, referenceNo: ref('B') },
      })

      eq('结局是 needs_approval', outcome?.status, 'needs_approval')
      eq('需要的角色是仓管', outcome?.requiredRoles, ['role:warehouse_manager'])
      ok('detail 里明写「未执行」（模型不能把它读成完成）', /未执行/.test(outcome?.detail ?? ''), show(outcome?.detail))
      eq('result 为 null（没有出参就是没有执行）', outcome?.result, null)

      const after = await moduleOnHand(moduleURL, SKU)
      eq('库存不变：115 → 115', `${before}→${after}`, '115→115')

      const records = await auditFor(hostURL, outcome?.planId)
      eq('待批不写审计（approve 在审计之前就返回）', records.length, 0)
      eq('全量审计条数不变（一次请求都没发到模块）', await auditTotal(hostURL), totalBefore)
    })

    await section('c) 编排层 · 未登记动作 inventory.NoSuchAction（fail-closed 默认拒绝）', async () => {
      if (!assertAlive(hostProc) || !assertAlive(inventoryProc)) return

      const before = await moduleOnHand(moduleURL, SKU)
      const totalBefore = await auditTotal(hostURL)

      let error = null
      try {
        await runAction({
          config,
          module: 'inventory',
          action: 'NoSuchAction',
          args: { sku: SKU, qty: 1, referenceNo: ref('C') },
        })
      } catch (thrown) {
        error = thrown
      }

      ok('抛错而不是返回一个结局', error !== null, '调用居然成功了')
      eq('错误码 = TENON_POLICY_DENIED', error?.code, 'TENON_POLICY_DENIED')
      eq('错误类 = policy_denied（不是 retryable，重试没有意义）', error?.errorClass, 'policy_denied')
      eq('retryable = false', error?.retryable, false)
      eq('理由码是 NO_POLICY_MATCH_DEFAULT_DENY（无规则命中即拒绝）', error?.details?.reasons, [
        'NO_POLICY_MATCH_DEFAULT_DENY',
      ])
      eq('拒绝不写审计（被拒的调用根本没有发生）', await auditTotal(hostURL), totalBefore)
      eq('库存不变', await moduleOnHand(moduleURL, SKU), before)
    })

    await section('d) 工具层 · 经 dist/index.js 注册出来的 tenon_action.execute', async () => {
      if (!assertAlive(hostProc) || !assertAlive(inventoryProc)) return

      const plugin = await import(pathToFileURL(path.join(PLUGIN_DIR, 'dist', 'index.js')).href)
      const registered = []
      plugin.apply(
        {
          tools: {
            register(definition) {
              registered.push(definition)
              return () => undefined
            },
          },
        },
        config,
      )

      eq('本包只注册一个工具', registered.length, 1)
      const tool = registered[0]
      eq('工具名是 tenon_action', tool?.name, 'tenon_action')
      eq(
        '参数面仍然只有 module/action/args（构建产物与源码一致）',
        Object.keys(tool?.parameters?.properties ?? {}).sort(),
        ['action', 'args', 'module'],
      )

      const exec = { signal: new AbortController().signal }
      const before = await moduleOnHand(moduleURL, SKU)

      const executed = await tool.execute(
        { module: 'inventory', action: 'DeductStock', args: { sku: SKU, qty: 5, referenceNo: ref('D') } },
        exec,
      )
      eq('工具层 qty=5 → status=executed', executed?.status, 'executed')
      eq('工具层 qty=5 真扣了 5 件', await moduleOnHand(moduleURL, SKU), before - 5)

      const pending = await tool.execute(
        { module: 'inventory', action: 'DeductStock', args: { sku: SKU, qty: 12, referenceNo: ref('E') } },
        exec,
      )
      eq('工具层 qty=12 → status=needs_approval', pending?.status, 'needs_approval')
      eq('工具层 qty=12 没有改动库存', await moduleOnHand(moduleURL, SKU), before - 5)
      ok('工具层待批同样明写「未执行」', /未执行/.test(pending?.detail ?? ''), show(pending?.detail))

      // 工具层走的是同一套闸门：执行那次有两段审计，待批那次一条都没有。
      const executedAudit = await auditFor(hostURL, executed?.planId)
      eq('工具层执行的那次也有完整两段审计', executedAudit.map((record) => record.stage), [
        'dispatched',
        'result',
      ])
      eq('工具层待批的那次没有审计', (await auditFor(hostURL, pending?.planId)).length, 0)
    })
  } finally {
    await Promise.all(processes.map((record) => stopProcess(record)))
  }

  console.log('\n────────────────────────────────────────')
  console.log(`小计：pass ${tally.pass} / fail ${tally.fail}`)
  console.log(`运行产物：${path.relative(REPO, RUN_DIR)}（host.log / inventory.log / policies.json / audit-*.jsonl）`)
  if (tally.fail > 0) {
    console.error('有断言失败，逐条看上面的 ✗。')
    process.exitCode = 1
  }
}

main().catch((error) => {
  console.error(`\n验收脚本自身失败：${error instanceof Error ? (error.stack ?? error.message) : String(error)}`)
  process.exitCode = 2
})
