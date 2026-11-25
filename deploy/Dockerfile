# 多阶段构建：只把编译好的二进制带进运行镜像。
#
# 为什么值得写：runner 镜像要能在云端 spot 实例上快速拉起，
# 镜像越小、拉起越快，「云上冲量」的尾延迟越低（F1 / F7）。

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=sutd
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/${CMD}

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/app /app/bin/app
# 配置文件与负载计划通过 compose 挂载进来，镜像里不放实验参数。
ENTRYPOINT ["/app/bin/app"]
