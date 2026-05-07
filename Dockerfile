FROM alpine:3.18

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

ARG TARGETARCH
COPY build/procman-${TARGETARCH} /app/procman
COPY web/static /app/web/static

RUN chmod +x /app/procman

ENV PROCMAN_DATA=/data/services
ENV PROCMAN_ADDR=:8080

EXPOSE 8080

ENTRYPOINT ["/app/procman"]
