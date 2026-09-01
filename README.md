# Eliauk VPN

像 Radmin VPN / ZeroTier 的 **P2P 虚拟局域网** —— 让跨公网的朋友像在同一个局域网里一样联机游戏（重点：**Minecraft**）。

技术栈：**Go + Wintun（L3 虚拟网卡）**，加密全用标准库，GUI 用 **WebView2（Edge Chromium）** 现代深色界面；外部依赖仅 `gorilla/websocket`（信令）与 `go-webview2`（宿主，复用系统 Edge 运行时，不打 150MB Chromium）。

---

## ✨ 能做什么

- **账号系统** —— 注册 / 登录，PBKDF2-HMAC-SHA256 密码哈希 + 会话 Token，设备指纹绑定。
- **好友 & 在线状态** —— 按用户名互加好友，在线状态实时可见。
- **房间一键加入** —— 创建房间拿 5 位房间码，朋友输入即自动互连（**无需互加好友**）。
- **游戏启动器集成** —— 自动检测 Java / 服务器 jar，一键开服，自动把服务器写进 Minecraft 官方启动器多人在线列表。
- **NAT 穿透** —— STUN 探测 + UDP 打洞直连；对称 NAT 打洞失败自动回退服务器中继。
- **端到端加密** —— X25519 身份 + AES-256-GCM，好友白名单（非白名单 peer 双向拒收）。
- **异地组网** —— 配合 Cloudflare Tunnel，**无需公网 IP** 即可把信令暴露到固定域名（见下文）。

## 🏗 工作原理

```
                协调服务器（信令 / 中继）
                      │  WebSocket (wss)
         ┌────────────┼────────────┐
         │            │            │
      ┌──▼──┐      ┌──▼──┐      ┌──▼──┐
      │ 甲  │◄────►│ 乙  │◄────►│ 丙  │   玩家机器（各在 NAT 后面）
      └──┬──┘  UDP └──┬──┘       └──┬──┘
         │   打洞直连  │             │
      Wintun         Wintun        Wintun        ← 虚拟网卡，模拟同一局域网
     10.0.0.2       10.0.0.3      10.0.0.4
```

- **协调服务器只做信令**：注册、好友、房间、交换打洞候选、分配虚拟 IP。它**不碰游戏流量**（除非打洞失败退化到中继）。
- 真正的游戏数据走两台机器之间的 **点对点 UDP 加密隧道**。
- 每台机器装一块 **Wintun 虚拟网卡**（`10.0.0.x`），系统层面像在同一局域网；Minecraft 的 UDP 广播（4445 端口）由软件层模拟转发。

## 📦 构建

要求：Windows 10/11 + Go 1.27（编译时）；运行需 `gui.exe` + `wintun.dll` + **WebView2 运行时**（Win11 自带、Win10 随 Edge 预装；缺失时启动会提示安装）。

```powershell
go build ./...
go build -ldflags "-H windowsgui" ./cmd/gui   # 无控制台的 GUI 版本
```

## 🚀 快速开始

### 1. 启动协调服务器

```powershell
go run ./cmd/server -addr :9090 -relay-listen 0.0.0.0:9091 -relay-public <公网IP>:9091 -accounts accounts.json
```

- `-accounts` 指向账号数据库文件（缺省则退回 legacy 匿名模式，账号/好友/房间不可用）。
- 本机测试时 `relay-public` 用 `127.0.0.1:9091`。

### 2. 启动 GUI

双击 `bin/gui.exe`（或 `go run ./cmd/gui`），主窗口里：

1. 填「昵称」+「服务器地址」`ws://<主机>:9090/ws` → 「保存并连接」。
2. 「账号」组注册 → 登录（Token 缓存到 config，重启免密）。
3. 「好友」组按用户名加好友，或「房间」组创建房间 → 把 5 位房间码发给朋友加入。
4. 「游戏」组一键开服 → 「复制地址」发给朋友 → 朋友「添加服务器」进 Minecraft 列表。

设置持久化在 `%AppData%\Eliauk\config.json`，身份在 `identity.key`。

## 🌍 异地组网（无需公网 IP）

协调服务器通常需要一台公网 VPS。没有公网 IP 的机器，可用 **Cloudflare Tunnel** 把本机信令服务暴露到固定域名 —— 零入站端口、零端口映射：

```powershell
# 1. 登录（需域名已在 Cloudflare 托管）
cloudflared tunnel login

# 2. 创建命名隧道
cloudflared tunnel create eliauk

# 3. 绑定域名
cloudflared tunnel route dns eliauk vpn.<你的域名>

# 4. 写 ~/.cloudflared/config.yml
```

```yaml
tunnel: <隧道ID>
credentials-file: <credentials.json 路径>
ingress:
  - hostname: vpn.<你的域名>
    service: http://localhost:9090
  - service: http_status:404
```

```powershell
# 5. 运行（证书/config 已落盘，之后直接 run 即可）
cloudflared tunnel run eliauk
```

GUI 服务器地址填 **`wss://vpn.<你的域名>/ws`**。

> 客户端与服务器都内置 WebSocket 心跳（20s ping / 60s pong），可跨过 Cloudflare 的 ~100s 空闲超时不断链。

## 📁 目录结构

```
cmd/
  server/   # 协调服务器（WebSocket 信令 + 中继 + 账号/好友/房间）
  gui/      # 傻瓜式主窗口 GUI（WebView2 / Edge Chromium，深色卡片主题）
  client/   # 交互式 CLI 客户端（调试用）
  mcprobe/  # 假 MC 服务端/客户端，测数据面
  genident/ # 生成/打印身份指纹
internal/
  agent/    # 客户端核心（注册/STUN/打洞/隧道/虚拟网卡/房间），GUI 与 CLI 共用
  webviewhost/ # WebView2 宿主 + Go↔JS 桥（状态推送 / 动作分发 / 关闭进托盘）
  winutil/  # 剪贴板 / 提权 / 窗口显示隐藏与子类化（纯 syscall）
  tray/     # 纯 Win32 托盘
  p2p/      # UDP 打洞 + 加密隧道
  crypto/   # X25519 + HKDF + AES-256-GCM（stdlib）
  server/   # 注册表 + 虚拟 IP + 账号/好友/房间
  mc/       # Minecraft 集成（Java 检测、开服、servers.dat NBT 注入）
  lan/      # MC 局域网发现广播模拟
  stun/     # STUN 客户端 + NAT 类型检测
  vnic/     # Wintun 虚拟网卡
  config/   # GUI 配置持久化
```

## 🧪 测试

```powershell
go test ./...
powershell -File e2e-gui.ps1   # 三阶段 e2e：匿名 p2p + 账号/token + 游戏面板
```

## 🔒 安全

- **身份**：X25519 长期密钥（`identity.key`），base64 指纹用于好友白名单。
- **会话**：三角色无关 DH（static↔eph、eph↔static、eph↔eph）+ HKDF 派生 AES-256-GCM 会话密钥，握手同时发起无 initiator/responder 之分。
- **白名单**：只允许白名单内指纹建立会话；非白名单 peer 数据帧被丢弃（含不对称白名单的 drop-guard）。
- **密码**：PBKDF2-HMAC-SHA256（100k 迭代），明文从不落盘；会话 Token 每次登录轮换。

## 🗺 里程碑

M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 ✅ → M4 虚拟网卡 ✅ → M5 游戏链路（MC 局域网发现）✅ → M6 加密/白名单/托盘/主窗口 GUI ✅ → **M7 账号/好友/房间/启动器 ✅** → M9 跨机验证（待做）。

> 详细的开发记录、踩坑与交接文档见 [`handoff.md`](handoff.md)。
