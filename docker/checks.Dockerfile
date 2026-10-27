# 检查镜像：本地与 CI 使用的规范/工具链检查。
#
# 这两个 target 在当前仓库状态下**可以直接构建并运行**（不需要业务代码）：
#   docker compose build spec go
#   docker compose run --rm spec
#   docker compose run --rm go
#
# 基础镜像用 ARG 暴露，便于在受限网络/内网镜像仓库下替换：
#   docker build -f docker/checks.Dockerfile --target spec \
#     --build-arg NODE_IMAGE=node:22-alpine -t tenon/spec-check .
# 体积更小的选择：node:22-alpine（默认用 bookworm-slim 是为了在
# 拉不到 alpine 变体的环境里也能构建）。

ARG NODE_IMAGE=node:24-bookworm-slim
ARG GO_IMAGE=golang:1.26-alpine

# ---- 规范校验：零依赖 Node 脚本，只读 spec/ 与 scripts/ ----
FROM ${NODE_IMAGE} AS spec
WORKDIR /src
COPY scripts ./scripts
COPY spec ./spec
CMD ["node", "scripts/validate-spec.mjs"]

# ---- Go 工具链：gofmt / vet / build 三道 ----
FROM ${GO_IMAGE} AS go
WORKDIR /src
COPY go.mod ./
# go.sum 必须一起拷：conformance 依赖 jsonschema/v6，缺 go.sum 时 go vet 会在
# `missing go.sum entry` 处失败，而镜像里连 go.sum 都没有——报错会误导成"依赖缺失"。
COPY go.sum ./
COPY conformance ./conformance
COPY tools ./tools
COPY docker/checks-go.sh /usr/local/bin/checks-go
RUN chmod +x /usr/local/bin/checks-go
CMD ["/usr/local/bin/checks-go"]
