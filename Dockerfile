# 三阶段构建：前端产物 → Go 静态二进制 → distroless 运行时。
# 阶段间只传递构建产物，最终镜像不含源码、node_modules 与工具链。

# ---- 阶段 1：前端构建（React + Vite 管理控制台）----
FROM node:24-alpine AS webbuild
WORKDIR /web
# 先拷依赖清单再装依赖：利用 Docker 层缓存，源码改动不触发 npm ci 重跑
COPY web/package.json web/package-lock.json ./
RUN npm ci
# 拷其余源码（.dockerignore 已排除 node_modules，宿主依赖绝不入镜像）
COPY web/ ./
RUN npm run build

# ---- 阶段 2：Go 构建（纯 Go SQLite 驱动，无 CGO）----
FROM golang:1.26-alpine AS gobuild
WORKDIR /src
# 依赖层单独缓存：业务代码改动不触发 go mod download
COPY go.mod go.sum ./
RUN go mod download
# 最小源集：cmd/internal + embed 清单；web/dist 用阶段 1 的构建产物，
# 宿主 web/dist 被 .dockerignore 排除，保证镜像内前端一定与源码同源构建
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/embed.go web/embed.go
COPY --from=webbuild /web/dist web/dist
# CGO_ENABLED=0 匹配 modernc.org/sqlite 的纯 Go 实现，产出可进 distroless
# 的静态二进制；-trimpath/-s/-w 去路径与符号表，减体积且不泄漏构建机路径
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

# ---- 阶段 3：运行时 ----
FROM gcr.io/distroless/static-debian12
# distroless：无 shell、无包管理器，攻击面最小；静态二进制直接运行。
# 不加 healthcheck：distroless 无 shell 也无探针可用的静态 wget/curl，
# 编排层健康探针交给 compose/k8s 的 HTTP 探测配置，不在镜像内过度设计
COPY --from=gobuild /out/proxy /proxy
# 容器内默认落库位置指向声明卷，覆盖代码默认的相对路径 ./data/proxy.db
ENV DB_PATH=/data/proxy.db
EXPOSE 8080
# SQLite 数据独立于容器生命周期；匿名卷保证忘了显式挂载时数据也不落容器层
VOLUME /data
# 不设 USER：distroless 默认 root，/data 卷首次创建时无需处理目录属主，
# 显式降权会引入卷权限配置负担，单服务自托管场景取最简路径
ENTRYPOINT ["/proxy"]
