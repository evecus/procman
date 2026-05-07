# 使用轻量级的 Alpine 作为运行环境
FROM alpine:3.18

# 安装基础库
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# 核心修改：利用 TARGETARCH 变量从 build 目录拷贝对应的二进制
# build 目录由 GitHub Action 在构建镜像前准备好
ARG TARGETARCH
COPY build/procman-${TARGETARCH} /app/procman

# 赋予执行权限
RUN chmod +x /app/procman

# 设置环境变量默认值
# 注意：现在不需要 COPY web 目录了，因为它已经嵌入在二进制里了
ENV PROCMAN_DATA=/data/services
ENV PROCMAN_ADDR=:8080

EXPOSE 8080

# 启动
CMD ["./procman"]
