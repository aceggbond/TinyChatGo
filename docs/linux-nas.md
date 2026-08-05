# Linux / NAS 服务端部署

Linux 构建产物是无界面的服务进程，网页端功能、账号、聊天、文件和数据库与 Windows 服务端共用同一套核心代码。

## Docker Compose（NAS 推荐）

先修改 `docker-compose.yml` 中的访问密码，然后运行：

```sh
docker compose up -d --build
```

浏览器访问 `http://NAS-IP:8080`。数据保存在当前目录的 `tinychatgo-data/`，升级容器不会丢失。

默认使用三个互相独立的端口：HTTP `8080`、HTTPS `8443`、管理后台 `8081`。管理地址是 `http://NAS-IP:8081/admin/`；用户端口不会提供管理页面。使用 `TINYCHATGO_ADMIN_PASSWORD` 配置的独立密码登录，密码为空时管理监听不会启动。

HTTPS 未指定证书文件时会在数据目录自动生成并复用自签名证书。首次访问会出现浏览器安全提示；本地 CA 文件是 `tinychatgo-data/hfs-go-ca.crt`。正式公网部署仍建议换成可信证书或使用 NAS 反向代理。

## 直接运行

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o TinyChatGoServer-linux-amd64 .
mkdir -p ./tinychatgo-data
TINYCHATGO_ACCESS_PASSWORD='请换成强密码' ./TinyChatGoServer-linux-amd64 \
  -listen :8080 -data-dir ./tinychatgo-data
```

ARM64 NAS 将 `GOARCH` 改成 `arm64`。进程收到 `SIGTERM` 或 `Ctrl+C` 时会优雅停止。

## 参数与环境变量

运行 `./TinyChatGoServer-linux-amd64 -help` 可查看全部参数。常用环境变量：

- `TINYCHATGO_LISTEN`：HTTP 监听地址，默认 `:8080`
- `TINYCHATGO_DATA_DIR`：数据库及上传目录，默认当前目录
- `TINYCHATGO_ACCESS_PASSWORD`：网页入口密码
- `TINYCHATGO_ADMIN_PASSWORD`：独立管理员密码，用于 `/admin/`
- `TINYCHATGO_ADMIN_LISTEN`：独立管理页面监听地址，默认 `:8081`
- `TINYCHATGO_TRUSTED_PROXIES`：反向代理 IP/CIDR，多个值用逗号分隔
- `TINYCHATGO_REQUIRE_APPROVAL`：新账号是否需要审批
- `TINYCHATGO_SHOW_USERS`、`TINYCHATGO_PRIVATE_CHAT`、`TINYCHATGO_ALLOW_GROUPS`：聊天功能开关

HTTPS 通常建议交给 NAS 自带的反向代理处理。如果要由程序直接提供 HTTPS，增加：

```sh
-https-listen :8443 -tls-cert /data/server.crt -tls-key /data/server.key
```

反向代理需要转发 WebSocket，并设置 `X-Forwarded-For`；同时用 `TINYCHATGO_TRUSTED_PROXIES` 明确配置代理地址。
