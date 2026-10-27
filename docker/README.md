# 容器化

容器分三层，**状态说明比目录结构重要**。

| 层 | 文件 | 现在能跑吗 |
|---|---|---|
| ① 检查与文档 | `checks.Dockerfile`（target `spec` / `go`）、`docs.Dockerfile` | ✅ 能 |
| ② 服务镜像 | `go-service.Dockerfile`（**4 个 Go 入口的 build 阶段实测通过**）、`node-service.Dockerfile`（仍在契约点失败） | ✅ / ❌ 见下 |
| ③ 拓扑 | 根目录 `compose.yaml`（现状）、`compose.m0.yaml`（M0.5 目标） | 两份 `config` 都能过；端到端 `up` 未验 |

M0.5 之后，`host`、`agent`、`quote` 都是 **Go 服务**，不再有「dsh 宿主 / TS 插件」那两条路径——那部分（进程内接入路径）整体 deferred，见 [`docs/interfaces.md`](../docs/interfaces.md) §1。

## ① 现在就能用的

```bash
# 文档站：规范 schema + 设计期架构图 + 设计文档，只读
docker compose up -d docs          # → http://localhost:8088

# 规范校验（与 CI 的 spec job 同源，零依赖 Node）
docker compose run --rm spec

# Go 工具链（gofmt / vet / build，与 CI 的 go job 同源）
docker compose run --rm go
```

单独构建也可以：

```bash
docker build -f docker/checks.Dockerfile --target spec -t tenon/spec-check .
docker build -f docker/checks.Dockerfile --target go   -t tenon/go-check .
docker build -f docker/docs.Dockerfile -t tenon/docs .
```

### 基础镜像可覆盖

拉不到 `node:22-alpine` 或 `gcr.io` 的环境（国内网络常见），用环境变量换掉基础镜像，不必改文件：

```bash
TENON_NODE_IMAGE=node:24-bookworm-slim \
TENON_NGINX_IMAGE=nginx:alpine \
docker compose build
```

默认值取的是**通用可用**而非最小体积：`node:24-bookworm-slim`、`nginx:alpine`、`golang:1.26-alpine`。生产环境请把 `NGINX_IMAGE` 换成 `nginx:1.27.0-alpine`（或 digest）。

## ② 服务镜像

### `go-service.Dockerfile`：M0.5 起是真实构建

它假定存在 `./cmd/${ENTRY}`，编译成单二进制后放进 `alpine` 运行时。四个入口的 build 阶段本轮都实测通过（见文末「已验证」）：

```bash
docker build -f docker/go-service.Dockerfile --target build --build-arg ENTRY=./cmd/host .
docker build -f docker/go-service.Dockerfile --target build --build-arg ENTRY=./cmd/agent .
docker build -f docker/go-service.Dockerfile --target build --build-arg ENTRY=./cmd/quote .
docker build -f docker/go-service.Dockerfile --target build --build-arg ENTRY=./cmd/tenon .
```

**只验 build 阶段**是有意的：运行时基础镜像 `alpine:3.20` 在本机拉不到（网络），所以「完整镜像 → 容器内启动 → `/readyz`」这条链路仍未验。

### `node-service.Dockerfile`：仍然是契约信号

`packages/*` 至今没有 `build` 脚本、也没有 `dist/index.js`，因此：

```bash
docker build -f docker/node-service.Dockerfile --target build --build-arg PKG=@tenon/dsh-plugin-module-host .
```

会在 `pnpm run build` 处失败（`ERR_PNPM_RECURSIVE_RUN_NO_SCRIPT`）。**这不是缺陷**：镜像即契约，它把「packages/ 该产出什么」写成可执行断言。注意它现在已经**不是** `host` 的构建路径了（`host` 是 `./cmd/host` 的 Go 服务），契约对象只剩 M1 的 dsh 集成。

## ③ M0.5 拓扑

```bash
docker compose -f compose.m0.yaml config --quiet   # 语法与引用完整性，现在就能过

# 只验编译（不拉运行时基础镜像、不碰卷权限）——本条已实测：
docker build -f docker/go-service.Dockerfile --target build --build-arg ENTRY=./cmd/host .

# 完整镜像（docker compose build）还没验：运行时阶段要 alpine:3.20，且命名卷权限未验。
```

7 个服务 + 2 个网络（`edge` 对外、`core` internal）+ 2 个 secret + 5 个命名卷：

| 服务 | 端口 | 说明 |
|---|---|---|
| `host` | **8080 发布** | 控制平面：策略 / 授权 / 审计链 / 注册表 / 共驾帧 REST |
| `agent` | 无端口 | 纯客户端：读注册表 → 轮询发件箱 → 规划 → 过闸门 → 两段审计 → 执行 |
| `quote` | 8083（不发布） | 触发器模块，产出 `quote.completed` 进发件箱 |
| `inventory` | 8081（不发布） | 主写路径：幂等、预演、补偿声明 |
| `procurement` | 8082（不发布） | 差异测试里被摘掉的那一个 |
| `conformance` | 一次性 | `conformance run --target … --host …`，退出码即结论 |
| `lint` | 一次性 | 只读扫描源码（C10） |

端口与端点对照见 [`docs/interfaces.md`](../docs/interfaces.md) §1、§4。

判据：`host`、三个模块的 `/readyz` 全 200 → `conformance`（带 `--host`）里 C4/C5 判 PASS → `lint` 零命中。D1 不在这些容器里跑：它要真 LLM 凭据（`docker/secrets/llm.key` + `tenon d1 run`）。

## 几个刻意的选择

- **模块不发布端口**：只有 `host` 暴露 8080，`core` 网络 `internal: true`。模块只能被宿主与 agent 访问，最小暴露面。
- **agent 不发布端口，但必须挂在 `edge` 上**：它没有任何入站接口；挂 `edge` 纯粹是为了出网调 LLM——`core` 是 `internal: true`，待在里面的进程连不出去。
- **密钥与 key 走文件不走环境变量**：`TENON_HMAC_SECRET_FILE`、`TENON_LLM_API_KEY_FILE` 都指向挂载的 secret 文件，避免进 `docker inspect` 与镜像层。
- **`quote` 是容器，不是进程内插件**：本轮宿主选了 Go，没有 TS 宿主，进程内路径 deferred。用容器包一个不存在的进程内插件只会造出假的边界。
- **`conformance` / `lint` 是一次性任务**：退出码就是结论，不常驻、不健康检查。
- **`/healthz` 不检查依赖**：否则依赖挂了会把健康容器一起杀掉。依赖就绪走 `/readyz`（两侧检查项不同，见 `interfaces.md` §7）。
- **测试态端点只在测试拓扑里给**：`host` 与三个模块都写了 `TENON_TEST_MODE: "1"`（宿主挂 `POST /tenon/admin/modules`，模块挂 `POST /tenon/admin/reset`）——差异测试的「摘模块 + 两轮同输入」全靠这两个端点。生产部署必须把它们全部删掉：宿主侧能改调度面，模块侧会抹掉幂等记账与两张事件表。
- **agent 的观测产物也落盘**：`TENON_RESULT_FILE` 指向 `/var/lib/tenon/agent-result.jsonl`（每回合一行：结局、planId、规划出来的动作序列）。它是 `tenon d1 run --agent-result` 的输入；与审计、游标同一块卷，才有 A/B 两轮可比的起点。
- **审计与游标都要落盘**：命名卷承载 JSONL 与游标文件。运行时用户是 uid 10001，而命名卷首次挂载的属主是 root，**可写性还没在容器里验过**。

## 已验证（本次实际执行）

| 验证项 | 结果 | 验于 |
|---|---|---|
| `docker compose config --quiet`（`compose.yaml`） | ✅ exit 0 | 本轮 |
| `docker compose -f compose.m0.yaml config --quiet` | ✅ exit 0 | 本轮 |
| `go-service --target build`，`./cmd/host` | ✅ exit 0 | 本轮 |
| `go-service --target build`，`./cmd/agent` | ✅ exit 0 | 本轮 |
| `go-service --target build`，`./cmd/quote` | ✅ exit 0 | 本轮 |
| `go-service --target build`，`./cmd/tenon` | ✅ exit 0 | 本轮 |
| `docs` 容器 | ✅ `Up (healthy)`；`/healthz` → 200 `ok` | 容器此前起的，本轮复查探针 |
| `docker compose run --rm spec` | ✅ `✓ spec 校验通过：11 项检查，0 失败` | M0 轮 |
| `docker compose run --rm go` | ✅ gofmt / vet / build 全 ok | M0 轮 |
| `node-service --target build` | ❌ 预期失败：`ERR_PNPM_RECURSIVE_RUN_NO_SCRIPT` | M0 轮 |
| 模块 `TENON_TEST_MODE=1` 挂 `admin/reset` | ✅ 设 `1` → 200 `{"reset":true}`；设 `0` → 404 | 本轮（本机进程，非容器） |
| 对话入口四服务（`inventory`/`procurement`/`host`/`agent`）端到端 | ✅ 跑通：投一句话 → 挂起 → bob 批准 → 库存 120→108 → 归档 `done`；审计 step 级 `approvedBy=user:bob`、哈希链连续 | 2026-10-27，`compose.yaml` + `--profile smoke` |
| `compose.m0.yaml` 端到端 `up`（M0.5 拓扑） | ❌ 未验 | — |
| 命名卷对 uid 10001 的可写性 | ❌ 未验（对话入口冒烟用宿主目录而非命名卷） | — |
| 模块 ↔ 宿主 HMAC 实际握手 | ❌ 未验：M0.5 走的是发件箱轮询，这个方向没有调用方 | — |

> 对话入口那次冒烟用的是 `tools/llmstub`（stub 规划器），它证明的是**编排与落盘链路**，不是模型能力；D1 判定必须用真模型（见根 README 的「D1 判定结果」一节，结论仍是未判定）。

补充一条不属于容器、但决定本层判据的实测：真宿主（`cmd/host`）+ 真模块（`cmd/inventory`）在**本机进程**上跑了宿主模式一致性，C4/C5 判 PASS、pass 8 / fail 0 / skip 1（D1），exit 0。证据与复现命令在 [`docs/interfaces.md`](../docs/interfaces.md) §11——它证明的是**协议能用**，不是**容器能起**。

**未验证的部分不许当成已实现来宣称。** 本层剩下的工作量是：完整镜像（运行时基础镜像 + 卷权限）、端到端 `up`。D1 的执行器已有（`internal/d1` + `scripts/d1-e2e.ps1`），缺的是让规划器产出合法计划的入参契约（见根 README）。
