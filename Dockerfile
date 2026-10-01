# 静态二进制 + 内嵌前端/词表，运行时镜像里只有一个可执行文件，
# 唯一的外部依赖是挂进来的 config.yaml 和 SQLite 数据目录。
FROM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wordle ./cmd/wordle

FROM alpine
WORKDIR /app
COPY --from=build /out/wordle /app/wordle
# config.yaml 与 data/*.db 都不进镜像，部署时从宿主机挂载到 /app/config.yaml 和 /app/data。
RUN mkdir -p /app/data
EXPOSE 8080
ENTRYPOINT ["/app/wordle", "-config", "/app/config.yaml"]