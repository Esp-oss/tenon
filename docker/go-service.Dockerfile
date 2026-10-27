# Go 服务镜像：工具（conformance / lint）与 demo 模块共用同一份构建。
#
# 现状（M0）：`cmd/tenon`、`cmd/inventory`、`cmd/procurement` 已实现，构建阶段可正常通过。
# 之前这份文件在 `go build ./cmd/...` 处失败是**刻意的契约信号**；代码落地后它自动变成真实构建。
#
#   docker build -f docker/go-service.Dockerfile --build-arg ENTRY=./cmd/tenon -t tenon/tool .
#   docker build -f docker/go-service.Dockerfile --build-arg ENTRY=./cmd/inventory -t tenon/inventory .
#
# 只验编译（不需要运行时基础镜像、也不拉 alpine）：
#   docker build -f docker/go-service.Dockerfile --target build --build-arg ENTRY=./cmd/tenon .

ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=alpine:3.20

FROM ${GO_IMAGE} AS build
ARG ENTRY=./cmd/tenon
WORKDIR /src

# 依赖先行：go.sum 变化才会让这层失效
COPY go.mod go.sum ./
RUN go mod download

COPY internal ./internal
COPY conformance ./conformance
COPY tools ./tools
COPY cmd ./cmd

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "${ENTRY}"

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 tenon
COPY --from=build /out/app /app
USER tenon
EXPOSE 8080
ENTRYPOINT ["/app"]
