# 容器化

容器分三层，**状态说明比目录结构重要**——只有第一层现在能跑。

| 层 | 文件 | 现在能跑吗 |
|---|---|---|
| ① 检查与文档 | `checks.Dockerfile`（target `spec` / `go`）、`docs.Dockerfile` | ✅ 能 |
| ② 构建契约 | `go-service.Dockerfile`、`node-service.Dockerfile` | ❌ 不能（代码未写，构建会失败——这是预期信号） |
| ③ 拓扑 | 根目录 `compose.yaml`（现状）、`compose.m0.yaml`（M0 目标） | `compose.yaml` ✅ / `compose.m0.yaml` 仅 `config` 可验 |

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

## ② 为什么「构建契约」镜像现在构建会失败

`go-service.Dockerfile` 假定存在 `./cmd/${ENTRY}`，`node-service.Dockerfile` 假定存在 `packages/<pkg>/dist/index.js`。M0 尚未实现任何命令与构建产物，所以构建会在那一刻失败。

**这不是缺陷，是刻意的**：镜像即契约。它把「M0 该产出什么」写成了可执行的断言，而不是写在文档里的愿望。等 `cmd/tenon` 落地，同一个文件不加改动就能构建出镜像。

受限网络下想验证契约仍然成立，只建 build 阶段即可（不需要运行时基础镜像）：

```bash
docker build -f docker/go-service.Dockerfile   --target build --build-arg ENTRY=./cmd/inventory .
docker build -f docker/node-service.Dockerfile --target build --build-arg PKG=@tenon/dsh-plugin-module-host .
```

两者都会在契约点失败，报错分别指向 `go build ./cmd/inventory`（无 Go 文件）与
`ERR_PNPM_RECURSIVE_RUN_NO_SCRIPT`（没有 build 脚本）——正是「未实现」该有的信号。

## ③ M0 拓扑

```bash
docker compose -f compose.m0.yaml config   # 语法现在就能验
docker compose -f compose.m0.yaml build    # 预期失败：go build ./cmd/... 无输入
```

拓扑里有 5 个服务 + 2 个网络（`edge` 对外、`core` internal），端口与端点对照见 [`docs/interfaces.md`](../docs/interfaces.md)。

判据：`host`、`inventory`、`procurement` 的 `/readyz` 全 200 → `conformance` 全绿（退出码 0）→ `lint` 零命中。

## 几个刻意的选择

- **模块不发布端口**：只有 `host` 暴露 8080，`core` 网络 `internal: true`。模块只能被宿主访问，最小暴露面。
- **密钥走文件不走环境变量**：`TENON_HMAC_SECRET_FILE` 指向挂载的 secret 文件，避免密钥出现在 `docker inspect` 与镜像层里。
- **`quote` 模块没有容器**：它是跑在 `host` 进程内的 cordis 插件。用容器包一个进程内插件只会造出假的边界。
- **`conformance` / `lint` 是一次性任务**：退出码就是结论，不常驻、不健康检查。
- **`/healthz` 不检查依赖**：否则 MySQL 挂了会把健康容器一起杀掉。依赖就绪走 `/readyz`。

## 已验证（本次实际构建并运行）

| 验证项 | 结果 |
|---|---|
| `docker compose config` / `-f compose.m0.yaml config` | 两份都通过（exit 0） |
| `docker compose build docs spec go` | 三个镜像构建成功 |
| `docker compose run --rm spec` | `✓ spec 校验通过：11 项检查，0 失败`，exit 0 |
| `docker compose run --rm go` | gofmt / vet / build 全 ok，exit 0 |
| `docker compose up -d docs` | `Up (healthy)`；`/healthz` 返回 `ok`；首页 `<title>Tenon · 规范与架构</title>`；`screens/design-1-layers.html`、`spec/0.1/schemas/action.schema.json` 均 200 |
| `go-service --target build`（`./cmd/tenon`） | ✅ **现已真实构建成功**（M0 代码落地后契约自动兑现） |
| `node-service --target build` | ❌ 仍在 `pnpm run build` 处失败：`ERR_PNPM_RECURSIVE_RUN_NO_SCRIPT`（`packages/*` 尚无 build 脚本） |
| `go-service` 完整镜像 | ⚠️ 未验：运行时基础镜像 `alpine:3.20` 在本机拉不到（网络） |
| `compose.m0.yaml` 端到端启动 | ❌ 未验：缺 `host`（dsh 宿主）实现 |
| 模块 ↔ 宿主 HMAC 实际握手 | ❌ 未验：同上；模块侧验签已有单测覆盖 |

**未验证的部分不许当成已实现来宣称。** `host` 与 `quote` 模块是 M0 剩下的主要工作量。
