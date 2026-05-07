FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o procman ./cmd/procman

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/procman .
COPY --from=builder /app/web/static ./web/static
# 运行
CMD ["./procman"]