# HFS Go

HFS Go 是一个面向 Windows 的轻量、安全局域网文件分享与在线聊天工具。运行单个可执行文件即可在电脑、手机和平板之间分享文件并实时沟通。

[下载 HFS Go v1.1](https://github.com/aceggbond/hfs-go/releases/tag/v1.1) · [查看全部 Releases](https://github.com/aceggbond/hfs-go/releases)

## 界面预览

### Windows 服务端

![HFS Go Windows 服务端界面](show1.png)

### 浏览器访问端

![HFS Go 浏览器访问界面](show2.png)

## 主要功能

- **局域网文件分享**：分享文件或文件夹，不移动、不复制原始内容；支持搜索、批量管理、拖放添加和断点下载。
- **网页上传与预览**：浏览器支持多文件拖放上传、实时进度显示，并可在线预览图片、音视频、PDF 和文本。
- **HTTP / HTTPS**：可同时监听 HTTP 与 HTTPS，自动生成局域网证书，并可开启 HTTP 自动跳转 HTTPS。
- **在线聊天**：网页与 Windows 后台通过 WebSocket/WSS 实时聊天，支持私聊、多人聊天、长文本及图片粘贴或拖放发送。
- **消息与访客提醒**：按 IP 显示访客、浏览器和系统信息，支持网页通知、提示音及 Windows 右下角提醒。
- **安全与易用性**：支持访问密码、上传/下载权限、网卡地址选择、托盘运行、单实例启动和配置自动保存。

## 使用方法

1. 从 [Releases](https://github.com/aceggbond/hfs-go/releases) 下载 `HFS-Go.exe` 并运行。
2. 在“设置”中选择网卡地址和端口，按需开启上传、下载、聊天或 HTTPS。
3. 在“文件管理”中添加分享内容，然后复制蓝色访问地址给局域网用户。

使用 HTTPS 时，先点击“生成/更新证书”，再将程序目录中的 `hfs-go-ca.crt` 导入访问设备的受信任根证书。请勿分发 `hfs-go-ca-key.pem`。

## 源码构建

需要 Go 1.20 或更高版本：

```powershell
go test ./...
go build -buildvcs=false -ldflags "-H=windowsgui" -o HFS-Go.exe .
```

## 支持作者

如果这个项目对你有帮助，可以通过下面的二维码支持作者继续维护。

<p align="center">
  <img src="dashang.png" width="260" alt="打赏作者二维码">
</p>
