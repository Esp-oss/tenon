# Go 服务镜像（构建契约）
#
# 当前仓库状态下**不可构建**，且这是有意的：镜像假定存在 ./cmd/${ENTRY}，
# 而 M0 尚未实现任何命令，所以构建会在 `go build ./cmd/...` 处失败并明确告诉你「代码还没写」。
# 它同时是构建契约：M0 落地时应当产出 cmd/tenon（工具）与 demo 模块的 cmd/<模块名>。
#
# 只想验证「构建契约仍然成立且失败在预期位置」时，只建 build 阶段即可（不需要运行时基础镜像）：
#   docker build -f docker/go-service.Dockerfile --target build \
#     --build-arg ENTRY=./cmd/inventory .
#
# 完整构建：
#   docker build -f docker/go-service.Dockerfile --build-arg ENTRY=./cmd/tenon -t tenon/tool .

ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=alpine:3.20

FROM ${GO_IMAGE} AS build
ARG ENTRY=./cmd/tenon
WORKDIR /src
COPY go.mod ./
COPY conformance ./conformance
COPY tools ./tools
# 契约点：M0 落地后这里应能编译出二进制
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "${ENTRY}"

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 tenon
COPY --from=build /out/app /app
USER tenon
EXPOSE 8080
ENTRYPOINT ["/app"]
