# LanChatGo

LanChatGo（原 **hfs-go**）是一个轻量、安全的局域网聊天与文件分享系统。运行 Windows 客户端后，局域网用户通过浏览器访问服务地址即可聊天、发送文件和管理个人资料；桌面端负责服务控制、用户管理和历史归档。

[下载 LanChatGo v2.0.1](https://github.com/aceggbond/LanChatGo/releases/tag/v2.0.1) · [Releases](https://github.com/aceggbond/LanChatGo/releases) · [项目地址](https://github.com/aceggbond/LanChatGo)

## 主要功能

- 基于 WebSocket/WSS 的实时聊天，支持系统群和私信。
- 支持用户发起私有群聊：创建者选择成员，成员可退出，群主可解散；群聊消息、文件和图片归档彼此隔离。
- 会话可置顶或取消置顶；系统群固定第一，置顶会话随后显示，其他会话按最新消息自动前移。
- IP 作为用户唯一标识，可设置自己的名称，管理员可修改备注、移除用户或加入黑名单。
- 会话中心与在线用户分组，未读消息数量、闪烁提示、浏览器通知和声音提醒。
- 提供独立 Windows 客户端：完整复用网页版聊天功能，支持局域网自动发现、手动地址、开机启动、原生托盘通知和独立声音开关。
- 客户端关闭窗口后驻留托盘；右击托盘图标可以显示窗口、重新发现服务、调整通知或彻底退出。
- 私信支持未读/已读状态；只有接收方实际打开对应私信且页面可见时才会标记已读，系统群和群聊不使用已读回执。
- 每个聊天窗口可单独开启或关闭消息提醒，全局浏览器提醒也可以随时关闭。
- 支持长文本、常用表情以及粘贴或拖拽图片和文件；附件会先进入待发送区，确认发送后才会上传。
- 图片可放大预览，并支持切换上一张/下一张。
- 用户发送的文件和图片自动归档，可按名称、发送人、IP、日期和类型搜索，并分页浏览。
- 图片最大 100 MiB，普通文件最大 1 GiB；上传显示实时进度。
- 消息发送后 2 分钟内可以撤回，聊天记录按批次加载，减少长会话卡顿。
- 管理端只负责服务控制：独立管理在线/全部用户、群聊成员、群名称、成员剔除、群聊解散、黑名单和历史归档。
- 支持选择网卡地址、HTTP/HTTPS 监听（默认 80/443）、自动生成证书和 HTTP 自动跳转 HTTPS。
- 配置、用户资料、访问记录、聊天记录、归档索引及证书统一保存到程序目录的 `lanchatgo.db`。

## 界面预览

### Windows 管理端

![LanChatGo Windows 管理端](show1.png)

### 浏览器聊天端

![LanChatGo 浏览器聊天端](show2.png)

## 使用

1. 从 [Releases](https://github.com/aceggbond/LanChatGo/releases) 下载并运行 `LanChatGo.exe`。
2. 在“设置”中选择监听地址，按需配置 HTTP/HTTPS 端口、访问密码、私信和群聊权限。
3. 启动服务后，点击管理端显示的蓝色地址复制访问链接，并发送给局域网用户。
4. 首次进入聊天端时设置自己的名称；文件和图片会自动归档到聊天附件库。

`Alt+F4` 会退出程序；点击窗口右上角关闭按钮会最小化到系统托盘。程序使用 `logo.png` 作为窗口、托盘和浏览器图标。

### 桌面客户端

1. 管理端在“设置 → 网络与访问”中开启“允许下载桌面客户端”。
2. 服务启动后，网页会根据浏览器平台自动提供 Windows x64 或 macOS ARM64 客户端；在客户端内不会重复显示下载按钮。
3. 客户端启动时通过一次局域网 UDP 广播发现同一 C 段服务，不逐个扫描 IP 或端口；也可以手动输入 HTTP/HTTPS 地址。
4. 客户端可设置开机自动启动和原生消息通知；Windows 客户端同时支持提示声音。客户端会自动接受局域网服务端生成的 HTTPS 证书。
5. Windows 点击窗口 `×` 仅隐藏到托盘，右击托盘选择“退出客户端”才会完全退出；macOS 点击 `×` 隐藏窗口，可从菜单栏图标重新显示或退出。

## 数据与安全

- `lanchatgo.db` 不存在时会自动创建；旧版 `hfs-go.db` 会在启动时迁移。
- 聊天附件保存在 `chat_files/`，按日期创建子目录。
- 黑名单 IP 会被拒绝访问、上传、下载和建立聊天连接。
- HTTPS 证书直接保存到数据库，重新生成会覆盖旧证书，不再生成散落的证书文件。

## 从源码构建

需要 Go 1.20 或更高版本：

```powershell
go test ./...
go build -trimpath -buildvcs=false -ldflags "-H=windowsgui -s -w" -o LanChatGo.exe .
Copy-Item LanChatGo.exe LanChatGo-Client-windows-amd64.exe
```

Windows 的服务端和客户端使用同一套代码：文件名包含 `Client` 时自动进入客户端模式，也可使用 `LanChatGo.exe --client` 启动客户端。推送版本标签后，GitHub Actions 会同时生成 `LanChatGo-Client-windows-amd64.exe` 和 `LanChatGo-Client-macos-arm64.zip`。

## 项目与支持

项目地址：https://github.com/aceggbond/LanChatGo

<p align="center">
  <img src="dashang.png" width="260" alt="打赏作者二维码">
</p>
