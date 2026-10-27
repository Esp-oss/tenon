# Tenon 服务镜像（多阶段）：host / agent / inventory / procurement 共用这一份，
# 靠 build-arg 选要编的包。
#
#   docker build --build-arg ENTRY=./cmd/host -t tenon/host:dev .
#
# ENTRY 是相对仓库根的 Go 包路径：./cmd/host ./cmd/agent ./cmd/inventory
# ./cmd/procurement ./cmd/tenon。冒烟用的 stub LLM 也走同一份：./tools/llmstub。
# 一份 Dockerfile 而不是五份，是因为五个包的构建前提完全相同（go.mod + internal +
# cmd），差异只有「编哪个包」——把它变成一个 ARG，就不会出现五份各自漂移的缓存层。
#
# 只验编译（不产出运行时镜像，也不需要拉运行时基础镜像）：
#   docker build --target build --build-arg ENTRY=./cmd/host .
#
# 运行时是 scratch：镜像里只有两个静态二进制（服务本体 + 健康检查探针）、CA 证书与
# spec/。没有 shell、没有包管理器，也就没有「容器里能 curl 一下」的退路——所以
# compose 的 healthcheck 必须是 exec 形式（["CMD","/app/probe",url]），不能是
# CMD-SHELL；探针只能在这里编出来。
#
# 为什么不复用 docker/go-service.Dockerfile：那份的运行时阶段是 alpine:3.20（本机
# 拉不到，见 docker/README 的「已验证」表），且它没有健康检查手段。这份换成
# scratch + 自编探针，把「运行时基础镜像」这个外部依赖彻底去掉了。

# builder 的默认值按用户指定固定为 golang:1.26；受限网络/内网仓库可用
# TENON_GO_IMAGE 覆盖（compose 里已接成变量，不必改文件）。
ARG GO_IMAGE=golang:1.26

FROM ${GO_IMAGE} AS build
ARG ENTRY=./cmd/host
WORKDIR /src

# 依赖先行：go.mod/go.sum 没变时这层命中缓存，改业务代码不再重下模块。
COPY go.mod go.sum ./
RUN go mod download

# 只搬构建要用的目录。conformance/ 不进这一层：只有 ./cmd/tenon 需要它，而这四个
# 服务都不引用（builder 只编一个包，没被 import 的目录不参与编译）。
COPY internal ./internal
COPY tools ./tools
COPY cmd ./cmd

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "${ENTRY}"

# 健康检查探针：scratch 里没有 wget/curl/sh，所以探针只能是编出来的静态二进制。
# 它放在 /src/probe（构建期临时目录，不进仓库也不进最终镜像的其他层）——
# 放在模块内是为了直接复用 /src 的 go.mod，不必再写一份 go.mod、也不会触碰网络。
RUN mkdir -p /src/probe && cat > /src/probe/main.go <<'PROBE'
// probe 是容器内健康检查探针：GET 一个 URL，2xx 退 0，其余退非 0。
// 只依赖标准库——它要能被 CGO_ENABLED=0 静态编译进 scratch。
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：probe <url>")
		os.Exit(2)
	}
	url := os.Args[1]
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %s 请求失败：%v\n", url, err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "probe: %s 返回 %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
}
PROBE
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/probe ./probe

FROM scratch
WORKDIR /app
COPY --from=build /out/app /app/app
COPY --from=build /out/probe /app/probe
# TLS 根证书：本地冒烟全程 http，但 agent 一旦换成真模型就是 https，
# 少这份文件会表现成「连不上模型」而不是「证书缺失」。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# schema 是判据而不是配置：agent 读不到 plan.schema.json 会拒绝启动（不做静默降级），
# 所以它跟二进制一起进镜像，而不是靠挂载——挂载会让「镜像能跑」依赖宿主机文件。
COPY spec ./spec
# 与 docker/README.md 的运行时用户约定一致（uid 10001）。scratch 没有 /etc/passwd，
# 所以只能用数字 uid/gid。
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/app"]
