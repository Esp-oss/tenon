# 文档站镜像：把规范（schema/语义文档）与设计期架构图做成只读静态站。
#
# 当前仓库状态下**可直接构建运行**：
#   docker compose up -d docs   →  http://localhost:8088
#
# 基础镜像用 ARG 暴露：生产环境建议 pin 到具体 minor 或 digest（如 nginx:1.27.0-alpine）。
# 健康检查不写在镜像里——不同变体自带的 HTTP 客户端不同（alpine 有 busybox wget，
# bookworm 两个都没有），写死会一换基础镜像就坏。所以探针放在 compose.yaml。

ARG NGINX_IMAGE=nginx:alpine

FROM ${NGINX_IMAGE}

COPY docker/docs.nginx.conf /etc/nginx/conf.d/default.conf
COPY docker/docs-index.html /usr/share/nginx/html/index.html

# 规范：schema 与语义文档
COPY spec /usr/share/nginx/html/spec
# 可浏览的架构图（设计期快照，见 docs/README.md 的说明）
COPY docs/design-screens /usr/share/nginx/html/screens
# 设计文档（Markdown 原文，浏览器里以纯文本呈现）
COPY docs/superpowers/specs /usr/share/nginx/html/design

EXPOSE 80
