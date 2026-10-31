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

/** 待批计划的落盘目录（在本次运行目录内，脚本每次运行先清空）。 */
const STATE_DIR = path.join(RUN_DIR, 'module-host-state')

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

/**
 * 「语义为空」的归一化。
 *
 * 宿主的审计**读模型**会把空串与 null 归成 null 或干脆省略（实测：计划级记录的
 * `action:""`、`inputHash:""` 读回来是 null，`stepId:null`、`idempotencyKey:null` 读回来没有这个键）。
 * 写侧仍是逐字段照发，这里只按「有没有值」判语义，避免脚本把宿主的读投影当成我们的写入形状。
 */
const norm = (value) => (value === undefined || value === '' ? null : value)

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

/** 用 dev 账号换一个 token（公告板的读与写都要它）。 */
async function loginAs(hostURL, user, password) {
  const { status, text } = await httpJSON(`${hostURL}/tenon/session`, {
    method: 'POST',
    body: JSON.stringify({ user, password }),
  })
  if (status !== 200) throw new Error(`登录 ${user} 失败：HTTP ${status} ${text.slice(0, 200)}`)
  const body = JSON.parse(text)
  if (typeof body?.token !== 'string') throw new Error(`登录 ${user} 的响应里没有 token`)
  return body.token
}

/** 带 Bearer 的 GET（公告板读取侧要 token）。 */
async function getJSONAuth(url, token) {
  const { status, text } = await httpJSON(url, { headers: { authorization: `Bearer ${token}` } })
  if (status !== 200) throw new Error(`GET ${url} → HTTP ${status}：${text.slice(0, 200)}`)
  return JSON.parse(text)
}

/** 带 Bearer 的 POST（提交 confirm 帧要 token）。 */
async function postJSONAuth(url, token, body) {
  const { status, text } = await httpJSON(url, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { authorization: `Bearer ${token}` },
  })
  if (status !== 200) throw new Error(`POST ${url} → HTTP ${status}：${text.slice(0, 200)}`)
  return JSON.parse(text)
}

/**
 * 从公告板读回某一张卡。
 *
 * 这是「卡片真的上去了」的**唯一**证据：不是看 adapter 发了什么，而是看宿主的 read-model 里
 * 有没有这一行——推失败但断言发过请求，等于没验。
 */
async function boardCard(hostURL, token, planId) {
  const body = await getJSONAuth(`${hostURL}/tenon/pending`, token)
  const items = Array.isArray(body?.items) ? body.items : []
  return items.find((item) => item?.planId === planId) ?? null
}

/**
 * 读盘上的待批（或归档）记录。
 *
 * 续跑的证据必须来自**盘上文件**，而不是从返回值反推：否则「到底落盘了没有」这件事
 * 永远没被验证过，而这正是本段要钉的东西。
 *
 * @param planId 计划号（词法不对直接返回 null，不拼路径）。
 * @param sub 子目录（`'archived'` 读归档；缺省读待批）。
 */
function readPendingFile(planId, sub = null) {
  if (typeof planId !== 'string' || !/^pln_[A-Za-z0-9_-]{4,64}$/.test(planId)) return null
  const dir = sub === null ? path.join(STATE_DIR, 'pending') : path.join(STATE_DIR, 'pending', sub)
  const file = path.join(dir, `${planId}.json`)
  if (!existsSync(file)) return null
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    return null
  }
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

  // 授权表：P0-1 起 `tickets:issue` 会真的过授权并扣配额，空表 = 所有写动作 GRANT_DENIED。
  // 这里给验收身份（ag_dsh_e2e / user:alice）一条明确的 grant，额度足够整轮验收用。
  const grantsPath = path.join(RUN_DIR, 'grants.json')
  writeFileSync(
    grantsPath,
    `${JSON.stringify(
      {
        grants: [
          {
            grantId: 'gr-e2e-module-host',
            agent: AGENT_ID,
            // 缺 expiresAt 等于已过期（fail-closed）：dev 的 grant 文件也显式给了远期时间。
            expiresAt: '2030-01-01T00:00:00Z',
            allow: [
              {
                module: 'inventory',
                actions: ['inventory.GetStock', 'inventory.ListStock', 'inventory.DeductStock', 'inventory.ReleaseStock'],
                constraints: { window: 'PT1H', maxUnitsPerWindow: 500 },
              },
            ],
          },
        ],
        subjects: {
          [ON_BEHALF_OF]: [
            'inventory.GetStock',
            'inventory.ListStock',
            'inventory.DeductStock',
            'inventory.ReleaseStock',
          ],
        },
      },
      null,
      2,
    )}\n`,
    'utf8',
  )

  // dev 账号表：公告板的写入侧只允许 role:agent，读取侧要一个真 token，
  // 所以这里造两个账号——agent（推卡片）与 bob（仓管，读公告板）。
  const usersPath = path.join(RUN_DIR, 'users.json')
  writeFileSync(
    usersPath,
    `${JSON.stringify(
      [
        { user: 'agent', password: 'agent-dev', roles: ['role:agent'] },
        { user: 'bob', password: 'bob-dev', roles: ['role:warehouse_manager'] },
      ],
      null,
      2,
    )}\n`,
    'utf8',
  )

  console.log('\n构建：')
  buildPlugin()
  const hostBin = goBuild('tenon-host.exe', './cmd/host')
  const inventoryBin = goBuild('tenon-inventory.exe', './cmd/inventory')
  // P0-1 起宿主缺票据私钥即拒绝启动、模块 enforce 下无票拒写：
  // 验收链必须自己造一对密钥，不能靠「恰好没验票」跑通。
  const tenonBin = goBuild('tenon.exe', './cmd/tenon')
  const ticketKey = path.join(RUN_DIR, 'host-ticket.key')
  const ticketPub = path.join(RUN_DIR, 'host-ticket.pub.json')
  const keygen = spawnSync(tenonBin, ['keygen', '--out', ticketKey, '--pub', ticketPub, '--force'], {
    cwd: REPO,
    stdio: 'inherit',
  })
  if (keygen.status !== 0) throw new Error(`生成票据密钥失败（exit=${keygen.status}）`)

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
    // 签发票据的私钥：缺了宿主会拒绝启动（不降级为无票执行）。
    TENON_HOST_SIGNING_KEY: ticketKey,
    // 显式给出会话密钥：不给的话 host 会用 dev 缺省值并告警，日志里多一句噪音。
    TENON_SESSION_SECRET: 'verify-module-host-e2e',
    // dev 账号表：agent 用来推公告板，bob 用来读公告板（两者都需要真 token）。
    TENON_USERS_FILE: usersPath,
  }
  const inventoryEnv = {
    ...baseEnv,
    PORT: String(inventoryPort),
    TENON_MODULE_ID: 'inventory',
    TENON_HOST: hostURL,
    TENON_HMAC_SECRET_FILE: secretPath,
    TENON_MODULE_AUDIT_PATH: path.join(RUN_DIR, 'audit-inventory.jsonl'),
    TENON_INVENTORY_SEED: `${SKU}:${SEED_ON_HAND},STD-1002:3`,
    // 票据强制：模块只接受宿主签发的票；公钥来自上面刚生成的信任集。
    TENON_TRUSTED_KEYS: ticketPub,
    TENON_TICKET_MODE: 'enforce',
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

    // 公告板读取侧要一个真 token：用仓管 bob 的身份读——他正是能批这张卡的人。
    const bobToken = await loginAs(hostURL, 'bob', 'bob-dev')

    const config = {
      hostURL,
      agentID: AGENT_ID,
      onBehalfOf: ON_BEHALF_OF,
      timeoutMs: 10_000,
      // 待批落盘目录放在本次运行目录里：RUN_DIR 每次运行先清空，待批集合因此每次都是全新的。
      stateDir: STATE_DIR,
      // 挂起时**不给**审批资格：这一段只验「真挂起」，提问与续跑留给 e) 段。
      operatorRoles: [],
      // 公告板写入要 role:agent：这里用 dev 账号换 token（与 Go 的 cmd/agent 同一条路）。
      agentUser: 'agent',
      agentPassword: 'agent-dev',
    }
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
      // 步骤级 dispatched 由宿主在签票时写，没有 result；插件只写 result 段。
      eq('dispatched 段由宿主在签票时写，本身不含 result', dispatched.result, undefined)
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
      eq('auto 路径没有人批准：result.approvedBy 为空', result.decision?.approvedBy, null)
      eq(
        'decision.policyId 是策略引擎真的裁决过的那条规则',
        result.decision?.policyId,
        'e2e-inventory-deduct-auto-small',
      )
      eq('审计链连续（第二段的 prevHash = 第一段的 hash）', result.prevHash, dispatched.hash)
    })

    await section('b) 编排层 · DeductStock qty=12（真挂起：落盘 + 计划级审计，绝不执行）', async () => {
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

      // 挂起写**一条**计划级 dispatched 审计：stepId 为空、没有动作、没人批过。
      const records = await auditFor(hostURL, outcome?.planId)
      eq('挂起写且只写一条审计', records.map((record) => record.stage), ['dispatched'])
      const planDispatched = records[0]
      if (planDispatched !== undefined) {
        eq('计划级记录没有 step', [norm(planDispatched.stepId), norm(planDispatched.action), norm(planDispatched.inputHash)], [
          null,
          null,
          null,
        ])
        eq('计划级记录没有幂等键（批准的是整份 plan）', norm(planDispatched.idempotencyKey), null)
        eq('计划级记录的 decision.outcome = approve', planDispatched.decision?.outcome, 'approve')
        eq('挂起这一刻还没人批：approvedBy 为 null', planDispatched.decision?.approvedBy, null)
        eq('计划级记录的 result.status = unknown', planDispatched.result?.status, 'unknown')
      }
      eq('全量审计条数 +1（只多了这条计划级记录）', await auditTotal(hostURL), totalBefore + 1)

      // 盘上真的有一条待批，且 I7 第三步已回写。
      const stored = readPendingFile(outcome?.planId)
      ok('待批已落盘（没有落盘就无从续跑）', stored !== null, show(outcome?.planId))
      eq('落盘记录与返回的 planId 一致', stored?.planId, outcome?.planId)
      eq('I7 第三步已回写 auditSettled', stored?.auditSettled, true)
      eq('落盘记录保留了审批角色', stored?.requiredRoles, ['role:warehouse_manager'])
      eq('没有审批资格时不会向任何人提问', /不持有上述任一角色/.test(outcome?.detail ?? ''), true)

      // 公告板：从宿主的 read-model 里真的读回这一张卡（不是看 adapter 发了什么请求）。
      const card = await boardCard(hostURL, bobToken, outcome?.planId)
      ok('公告板上能看见这张待批卡', card !== null)
      if (card !== null) {
        eq('卡片 state = awaiting_approval', card.state, 'awaiting_approval')
        eq('卡片带着需要的角色', card.requiredRoles, ['role:warehouse_manager'])
        ok('卡片摘要是给人看的', /inventory\.DeductStock/.test(String(card.summary)), show(card.summary))
        eq('卡片与待批同一有效期', card.expiresAt, stored?.expiresAt)
      }
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

      eq('本包注册三个工具（action / pending / resume）', registered.map((item) => item.name).sort(), [
        'tenon_action',
        'tenon_pending',
        'tenon_resume',
      ])
      const tool = registered.find((item) => item.name === 'tenon_action')
      eq('工具名是 tenon_action', tool?.name, 'tenon_action')
      eq(
        '参数面仍然只有 module/action/args（构建产物与源码一致）',
        Object.keys(tool?.parameters?.properties ?? {}).sort(),
        ['action', 'args', 'module'],
      )
      const resumeTool = registered.find((item) => item.name === 'tenon_resume')
      eq(
        '续跑工具只接受 planId（身份与批准不可由模型传入）',
        Object.keys(resumeTool?.parameters?.properties ?? {}),
        ['planId'],
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

      // 工具层走的是同一套闸门：执行那次两段 step 级审计；待批那次只有一条计划级记录。
      const executedAudit = await auditFor(hostURL, executed?.planId)
      eq('工具层执行的那次也有完整两段审计', executedAudit.map((record) => record.stage), [
        'dispatched',
        'result',
      ])
      const pendingAudit = await auditFor(hostURL, pending?.planId)
      eq('工具层待批的那次是计划级一条（stepId 为空）', pendingAudit.map((record) => record.stage), [
        'dispatched',
      ])
      eq('工具层待批的计划级记录没有 step', norm(pendingAudit[0]?.stepId), null)
      ok('工具层待批的那次也已落盘', readPendingFile(pending?.planId) !== null)
    })

    await section('e) 续跑 · 真 host + 真 inventory + 假 answerer（批准后才执行）', async () => {
      if (!assertAlive(hostProc) || !assertAlive(inventoryProc)) return

      const { resumePlan } = await import(
        pathToFileURL(path.join(PLUGIN_DIR, 'dist', 'orchestrate.js')).href
      )

      // 先挂起一单（用本段自己的 referenceNo，避免与前面几次撞业务幂等键）。
      const before = await moduleOnHand(moduleURL, SKU)
      const suspended = await runAction({
        config,
        module: 'inventory',
        action: 'DeductStock',
        args: { sku: SKU, qty: 12, referenceNo: ref('F') },
      })
      eq('续跑前置：先挂起', suspended?.status, 'needs_approval')
      const planId = suspended?.planId
      ok('待批已落盘', readPendingFile(planId) !== null)

      // ① 本会话没有仓管角色 → 不提问、不执行，待批仍在。
      let asked = 0
      const noQualification = await resumePlan({
        config: { ...config, operatorRoles: [] },
        planId,
        approval: {
          async request() {
            asked += 1
            return 'allowed-once'
          },
        },
        agent: { id: AGENT_ID },
      })
      eq('无资格时结局仍是 needs_approval', noQualification?.status, 'needs_approval')
      eq('无资格时不向任何人提问', asked, 0)
      eq('无资格时库存不变', await moduleOnHand(moduleURL, SKU), before)
      ok('无资格时待批仍然在盘上', readPendingFile(planId) !== null)

      // ② 有资格但人拒了 → 不执行，状态还原成待批。
      const rejected = await resumePlan({
        config: { ...config, operatorRoles: ['role:warehouse_manager'] },
        planId,
        approval: { async request() { return 'rejected' } },
        agent: { id: AGENT_ID },
      })
      eq('拒批结局是 needs_approval', rejected?.status, 'needs_approval')
      ok('拒批明写结局', /rejected/.test(rejected?.detail ?? ''), show(rejected?.detail))
      eq('拒批后库存不变', await moduleOnHand(moduleURL, SKU), before)
      eq('拒批后状态还原成 awaiting_approval', readPendingFile(planId)?.state, 'awaiting_approval')

      // ③ 有资格 + allowed-once → 真执行，审计带 approvedBy。
      const approved = await resumePlan({
        config: { ...config, operatorRoles: ['role:warehouse_manager'] },
        planId,
        approval: { async request() { return 'allowed-once' } },
        agent: { id: AGENT_ID },
      })
      eq('批准后真执行', approved?.status, 'executed')
      eq('批准后库存真减：115 → 103', await moduleOnHand(moduleURL, SKU), before - 12)

      const records = await auditFor(hostURL, planId)
      eq('四段审计：计划级 dispatched + step 两段 + 计划级 result', records.map((record) => `${record.stage}/${String(record.stepId ?? null)}`), [
        'dispatched/null',
        'dispatched/s1',
        'result/s1',
        'result/null',
      ])
      const stepRecords = records.filter((record) => record.stepId === 's1')
      // dispatched 由宿主在签票时写（不含 approvedBy——宿主只知道「谁在调用」，不知道谁批的）；
      // 批准人落在插件写的 result 段，两段共用票上的 auditId。
      const stepResult = stepRecords.find((record) => record.stage === 'result')
      eq('step result 写了审批人', stepResult?.decision?.approvedBy, 'user:alice')
      eq(
        'step result 与宿主 dispatched 共用票上的 auditId',
        stepResult?.auditId,
        stepRecords.find((record) => record.stage === 'dispatched')?.auditId,
      )
      eq('step 级两段的幂等键沿用挂起时的', stepRecords.map((record) => record.idempotencyKey), [
        `tenon:${planId}/s1`,
        `tenon:${planId}/s1`,
      ])
      eq('计划级两段共用挂起时的 auditId', [records[0]?.auditId, records[3]?.auditId], [
        records[0]?.auditId,
        records[0]?.auditId,
      ])

      const archived = readPendingFile(planId, 'archived')
      eq('归档终态是 done', archived?.state, 'done')
      eq('归档记录了审批人', archived?.approvedBy, ON_BEHALF_OF)
      eq('归档后待批目录里没有这一单', readPendingFile(planId), null)

      // 公告板上的那张卡被终态覆盖（否则会一直挂到 expiresAt）。
      const doneCard = await boardCard(hostURL, bobToken, planId)
      eq('公告板卡片被终态覆盖成 done', doneCard?.state, 'done')

      // ④ 再续一次：幂等返回，库存不再动。
      const again = await resumePlan({
        config: { ...config, operatorRoles: ['role:warehouse_manager'] },
        planId,
        approval: { async request() { return 'allowed-once' } },
        agent: { id: AGENT_ID },
      })
      eq('重复续跑返回 executed（归档结论回放）', again?.status, 'executed')
      ok('重复续跑不重复执行', /不重复执行/.test(again?.detail ?? ''), show(again?.detail))
      eq('重复续跑库存不变', await moduleOnHand(moduleURL, SKU), before - 12)
    })
    await section('f) 公告板批准 · 人点批准 → 入站 confirm → 轮询器执行（且从不 ack）', async () => {
      if (!assertAlive(hostProc) || !assertAlive(inventoryProc)) return

      const { createConfirmPoller } = await import(
        pathToFileURL(path.join(PLUGIN_DIR, 'dist', 'confirm.js')).href
      )
      const { createWakeRegistry } = await import(
        pathToFileURL(path.join(PLUGIN_DIR, 'dist', 'wake.js')).href
      )

      const before = await moduleOnHand(moduleURL, SKU)
      const suspended = await runAction({
        config,
        module: 'inventory',
        action: 'DeductStock',
        args: { sku: SKU, qty: 12, referenceNo: ref('G') },
      })
      eq('待批已生成', suspended?.status, 'needs_approval')
      const planId = suspended?.planId

      // 挂起时插件会把这个会话的 agent 句柄按 planId 存下来（apply 里做）；这里用假的验证唤醒链路。
      const woken = []
      const wake = createWakeRegistry((line) => console.log(`    ${line}`))
      wake.remember(planId, { followup: (message) => woken.push(message) })

      // 人在公告板上点「批准」：宿主用 bob 的 token 覆写 by/roles 之后才入队。
      const enqueued = await postJSONAuth(`${hostURL}/tenon/requests`, bobToken, {
        type: 'tenon.confirm',
        confirmId: `cfm_e2e_${stamp}`,
        planId,
        decision: 'approve',
      })
      eq('confirm 帧已入队', enqueued?.ok, true)

      const poller = createConfirmPoller({
        config,
        log: (line) => console.log(`    ${line}`),
        onExecuted: (event) => wake.notify(event),
      })
      const ticked = await poller.tick()
      eq('轮询器读到并处理了一条 confirm', [ticked.confirms, ticked.applied], [1, 1])
      eq('公告板批准后库存真减：103 → 91', await moduleOnHand(moduleURL, SKU), before - 12)

      // 唤醒：业务完成后把结论送回提出计划的会话（M1「DSH 侧批准后唤醒」）。
      ok(
        '批准后唤醒会话：followup 收到 UserMessage 形状',
        woken.length === 1 &&
          woken[0]?.role === 'user' &&
          woken[0]?.source?.kind === 'user' &&
          String(woken[0]?.content?.[0]?.text ?? '').includes(String(planId)) &&
          String(woken[0]?.content?.[0]?.text ?? '').includes('user:bob'),
      )

      const records = await auditFor(hostURL, planId)
      // 批准人落在插件写的 result 段；dispatched 由宿主签票时写，不带 approvedBy。
      const stepResult = records.find(
        (record) => record.stage === 'result' && record.stepId === 's1',
      )
      eq(
        '审计里的批准人来自宿主验过的 token（不是部署配置 operatorRoles）',
        stepResult?.decision?.approvedBy,
        'user:bob',
      )
      eq('计划级两段仍然共用挂起时的 auditId', records.at(-1)?.auditId, records[0]?.auditId)

      const doneCard = await boardCard(hostURL, bobToken, planId)
      eq('公告板卡片被覆盖成 done', doneCard?.state, 'done')

      // ---- 「只读不 ack」的直接证据：帧还在队列里（ack 过就会消失）----
      const agentToken = await loginAs(hostURL, 'agent', 'agent-dev')
      const inbox = await getJSONAuth(`${hostURL}/tenon/requests/poll?after=0`, agentToken)
      const stillThere = Array.isArray(inbox?.frames)
        ? inbox.frames.some((entry) => entry?.frame?.planId === planId)
        : false
      ok('confirm 帧仍留在入站队列里（证明从未 ack）', stillThere)

      // ---- 重放：新轮询器 = 新进程，游标归零再读一遍，靠归档幂等挡住 ----
      const replayWoken = []
      const replay = createConfirmPoller({
        config,
        onExecuted: (event) => replayWoken.push(event),
      })
      const again = await replay.tick()
      eq('重放不产生动作', again.applied, 0)
      eq('重放不重复唤醒', replayWoken.length, 0)
      eq('重放后库存不变', await moduleOnHand(moduleURL, SKU), before - 12)
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
