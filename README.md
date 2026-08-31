# Eliauk VPN

像 Radmin VPN / MCTier 的 P2P 虚拟局域网，让跨公网的朋友像在同一个局域网一样联机游戏（重点：Minecraft）。

**当前阶段：M7 全部完成** —— 账号 + 好友 + 在线状态、房间一键加入、游戏启动器集成。技术栈：**Go + Wintun（纯 stdlib，零第三方依赖）**，GUI 为纯 Win32。

## 它能做什么
- 双击 `bin/gui.exe` → 注册账号 → 好友按用户名互加、在线状态实时可见。
- 「房间」：创建房间拿 5 位房间码，发给朋友一键加入 —— 加入即自动互连（互相白名单 + 打洞），**不用互加好友**。
- 「游戏」：自动检测 Java / 服务器 jar，一键开服（写 eula.txt + server.properties）；把房主地址（虚拟 IP:25565）复制或写进启动器多人在线列表，朋友点一下就能加入你的 MC 服务器。
- 全部加密（X25519 身份 + AES-256-GCM），NAT 打洞失败自动走服务器中继回退。

## 构建
```
go build ./...
go build -ldflags "-H windowsgui" ./cmd/gui   # 无控制台的 GUI 版本
```

## 运行（本机测试）
1. 启动协调服务器（带账号目录）：
   ```
   go run ./cmd/server -addr :9090 -relay-listen 0.0.0.0:9091 -relay-public 127.0.0.1:9091 -accounts accounts.json
   ```
2. 双击 `bin/gui.exe`，填昵称 + 服务器地址 `ws://127.0.0.1:9090/ws` → 保存并连接 → 注册账号 → 建房/加好友。

## 目录结构
```
cmd/
  server/   # 协调服务器（WebSocket 信令 + 中继 + 账号/好友/房间）
  gui/      # 傻瓜式主窗口 GUI（纯 Win32，零依赖）
  client/   # 交互式 CLI 客户端（调试用）
  mcprobe/  # 假 MC 服务端/客户端，测数据面
  genident/ # 生成/打印身份指纹
internal/
  agent/    # 客户端核心（注册/STUN/打洞/隧道/虚拟网卡/房间），GUI 与 CLI 共用
  window/   # 主窗口（原生控件 + 托盘）
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

## 测试
```
go test ./...
powershell -File e2e-gui.ps1   # 三阶段 e2e：匿名 p2p + 账号/token + 游戏面板
```

## 里程碑
M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 ✅ → M4 虚拟网卡 ✅ → M5 游戏链路（MC 局域网发现）✅ → M6 加密+白名单+托盘+主窗口 GUI ✅ → **M7 账号/好友/房间/启动器 ✅** → M9 跨机验证（待做）。
