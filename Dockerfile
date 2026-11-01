FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/codearts2api-panel ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app \
    && mkdir -p /app/auths /app/data \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/codearts2api-panel /app/codearts2api-panel
# 示例配置放在旁边而不是 /app/config.json：若把文件预置在挂载点，宿主机缺
# ./config.json 时 Docker 会建同名目录，runc 在「目录挂到文件上」这一步直接
# 失败（exit 125），用户看不到任何可操作提示。留空挂载点，目录能挂进来，
# 程序自己会识别「配置路径是目录」并打印处理办法。
COPY config.example.json /app/config.example.json
USER app
EXPOSE 7866
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7866/healthz || exit 1
ENTRYPOINT ["/app/codearts2api-panel", "-config", "/app/config.json"]
