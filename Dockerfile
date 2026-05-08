FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

WORKDIR /src

# vendor 目录已包含所有依赖，无需联网
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -mod=vendor -trimpath -ldflags="-s -w" \
    -o /out/procman ./cmd/procman

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.18

# ca-certificates: HTTPS请求; tzdata: 时区; bash/coreutils: Terminal功能更完整
RUN apk add --no-cache ca-certificates tzdata bash coreutils

WORKDIR /app

COPY --from=builder /out/procman /app/procman
RUN chmod +x /app/procman && mkdir -p /data

ENV PROCMAN_DATA=/data
ENV PROCMAN_ADDR=:8080
ENV WEB_PASSWORD=

EXPOSE 8080

ENTRYPOINT ["/app/procman"]
