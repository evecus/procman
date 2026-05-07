FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /out/procman ./cmd/procman

FROM alpine:3.18

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/procman /app/procman
COPY web/static /app/web/static

RUN chmod +x /app/procman
RUN mkdir -p /app/data
ENV PROCMAN_DATA=/app/data
ENV PROCMAN_ADDR=:8080
ENV WEB_PASSWORD=1234

EXPOSE 8080

ENTRYPOINT ["/app/procman"]
