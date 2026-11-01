FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/codearts2api-panel ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app \
    && mkdir -p /data \
    && chown app:app /data
WORKDIR /app
COPY --from=build /out/codearts2api-panel /app/codearts2api-panel
USER app
EXPOSE 7866
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7866/healthz || exit 1
ENTRYPOINT ["/app/codearts2api-panel", "-config", "/data/config.json"]
