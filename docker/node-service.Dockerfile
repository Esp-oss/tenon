# TypeScript 服务镜像（构建契约）
#
# 当前仓库状态下**不可构建**，且这是有意的：packages/* 都还没有 src，`pnpm build`
# 无从产出 dist/index.js，运行期 node 会以 MODULE_NOT_FOUND 退出——即「服务未实现」的诚实信号。
#
#   docker build -f docker/node-service.Dockerfile \
#     --build-arg PKG=@tenon/dsh-plugin-module-host -t tenon/host .

ARG NODE_IMAGE=node:24-bookworm-slim

FROM ${NODE_IMAGE} AS build
ARG PKG
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY packages ./packages
RUN corepack enable && pnpm install --frozen-lockfile
# 契约点：M0 落地后每个包应有 build 脚本并产出 dist/
RUN pnpm --filter "${PKG}" run build

FROM ${NODE_IMAGE}
ARG PKG
ENV TENON_PKG=${PKG}
WORKDIR /src
COPY --from=build /src /src
USER node
# 运行期入口：M0 实现后应存在 packages/<pkg>/dist/index.js
CMD ["sh", "-c", "exec node /src/packages/${TENON_PKG}/dist/index.js"]
