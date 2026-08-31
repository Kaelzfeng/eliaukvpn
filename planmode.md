# Eliauk VPN — 项目计划

## 一句话目标
做一款像 Radmin VPN 的 **P2P 虚拟局域网**软件：每台机器装虚拟网卡 + UDP 打洞直连 + 中继兜底 + 加密，
让任何局域网联机游戏（含《我的世界》）跨公网直接玩。

## 为什么选方案 B
- **通用**：所有 LAN 联机游戏都能用，不只 MC。
- **学习价值完整**：NAT 穿透 + 虚拟网卡 + 加密，全链路都亲手实现。
- **可拆分演进**：能力从方案 A 逐层加上来，不浪费（先打洞，再封装，最后上虚拟网卡）。

## 技术要点

### 1. NAT 穿透（原理已讨论）
| 概念 | 作用 |
|---|---|
| NAT / 端口映射 | 路由器把内网机器映射到公网 IP:端口 |
| NAT 四类型 | Full-cone / Restricted / Port-restricted / **Symmetric**（对称型是打洞天敌） |
| STUN | 公网服务器照镜子，拿对外 IP:端口 + 判定 NAT 类型 |
| UDP 打洞 | 双方同时向对方发 UDP，各自 NAT 留下洞，达成直连 |
| TURN 中继 | 打洞失败（尤其对称 NAT）时的兜底，流量经公网服务器转发 |
| ICE | 直连优先、中继兜底、自动选最优路径（WebRTC 同款） |
| 协调服务器 | 在线注册 / 好友 / 交换候选地址 / 打洞协商，只做信令 |

### 2. 虚拟网卡层（方案 B 的核心增量）
- **为什么需要**：让操作系统以为机器在同一个局域网，游戏代码零改动。
- **驱动选型（关键决策点）**：
  | 驱动 | 层次 | 广播/ARP | MC 局域网发现 | 备注 |
  |---|---|---|---|---|
  | **Wintun** | L3 (IP包) | 无 | 需软件层模拟广播转发 | 现代、性能好、生态新 |
  | **tap-windows6** | L2 (以太网帧) | 有 | 天然支持 | OpenVPN 老驱动，签名/兼容性繁琐 |
- **MC 局域网发现原理**：服务器开房后向 UDP 4445 端口发广播，客户端靠广播发现世界 → 驱动/L2 能力直接决定这项体验。
- **IP 分配**：协调服务器给每台机器分配虚拟 IP（如 `10.0.0.x`），客户端写死或走简单 DHCP 逻辑。
- **路由**：加一条到虚拟网段（如 `10.0.0.0/24`）的路由，下一跳指向虚拟网卡。
- **封装**：从网卡读帧（L2）或 IP 包（L3）→ 塞进加密 UDP 隧道 → 对端解包写回自己的网卡。

### 3. 加密（M6a 已实现，纯 stdlib）
- **身份**：每台机器一个 X25519 长时密钥对（`internal/crypto`，`LoadOrCreate` 持久化到 `%AppData%\Eliauk\identity.key`），base64 指纹即「好友 ID」。
- **握手**：hello/helloAck 各带 64B（ephemeral 32 || static 32）。**无 initiator/responder 之分**（同时打洞双方都跑 responder 逻辑）→ 会话派生**角色无关**：三个对称 DH 排序取规范 IKM + 四个公钥排序取规范 transcript，HKDF-SHA256 派生 32B 会话密钥。
- **每连接一个 ephemeral**：`BeginConnect` 生成一次，握手与派生共用；responder 绝不重生成（重生成会导致双方 ephemeral 组合分叉 → 会话不一致 → GCM 鉴权失败）。
- **数据加密**：AES-256-GCM（`crypto/aes`+`crypto/cipher`，不是 chacha20poly1305——那在 x/crypto）。8B 计数器 nonce + 64 包重放窗口，AAD 绑定 peer 对。
- **好友白名单（M6b）**：`--friends` 文件列 base64 指纹；未在白名单的静态密钥直接拒绝握手；其密文在 frameData 层被 drop-guard 丢弃（堵住不对称白名单下「密文当明文转发」）。
- 有身份 client 拒绝与无身份（legacy 明文）peer 连接 —— 禁止静默降级。

### 4. 广播模拟（M5，已实现）
Wintun 是 L3 无广播 → MC 局域网发现（UDP 224.0.2.60:4445）在软件层模拟：
- **host 端**：`lan.Listen` 绑定 4445（SO_REUSEADDR）并在全部 up 网卡 JoinGroup（含 wintun）→ 嗅探本地广播 → `lan.BuildDiscovery` 把**裸 UDP 负载**（`lan.Listen` 回调无 IP 头）包成完整 IPv4/UDP 包、源改为自己的虚拟 IP → `SendDataBroadcast` 扇出给所有 peer。
- **join 端**：dataSink 收到发现包 → 注入原组播 + 一份改写目的为自身 VIP 的副本（兼容未 JoinGroup 的 0.0.0.0:4445 客户端）。
- **loop guard**：源在虚拟子网的包丢弃（防回声再转发）。
- **每 peer /32 主机路由**（`route add <peerVIP> mask 255.255.255.255 0.0.0.0 IF <网卡>`，自动安装）：保证到 peer VIP 的流量进虚拟网卡、经隧道 —— 同机双向 TCP 握手都因此真实穿越隧道（debug 日志实锤）。

## 架构蓝图
```
┌─ 协调服务器（VPS，WebSocket）──────────────┐
│  注册 / 好友 / 交换候选 / 判定NAT类型 / 分配虚拟IP │
└──────┬───────────────────────────┬────────┘
       │ 信令                        │ 信令
┌──────▼────────┐             ┌─────▼────────┐
│ 客户端 10.0.0.1 │◄──打洞/中继──►│ 客户端 10.0.0.2 │
│ ┌────────────┐ │  加密UDP隧道   │ ┌────────────┐ │
│ │ 虚拟网卡     │ │              │ │ 虚拟网卡     │ │
│ └─────┬──────┘ │              │ └─────┬──────┘ │
│ 游戏→25565/局域网 │             │ 局域网/25565→游戏 │
└───────────────┘              └───────────────┘
```

## 里程碑
- [x] **M0** 项目结构 + 文档（planmode / handoff）
- [x] **M1** 协调服务器 + 客户端注册 + STUN 探测（拿对外地址、判定 NAT 类型、分配虚拟 IP）
- [x] **M2** UDP 打洞直连（最小打通：两客户端经服务器交换候选后互发 UDP，本地双向验证通过）
- [x] **M3** TURN 中继回退（打洞失败时流量走服务器）
- [x] **M4** 虚拟网卡打通（绑定 Wintun，虚拟 IP 分配 + 自动连接 + 数据面送达）
- [x] **M5** 游戏链路（MC 局域网发现端到端经隧道验证：4445 嗅探 → 源改写为 VIP → SendDataBroadcast → dataSink 双路注入；每 peer /32 路由强制走隧道；mcprobe 假 MC 端到端 + `--debug-packets` 双向实锤）
- [x] **M6a** 加密层（X25519 身份 + 角色无关会话派生 + AES-256-GCM 数据加密；纯 stdlib；同机双客户端 e2e 验证）
- [x] **M6b** 好友白名单（`--friends` 指纹白名单 + checkPeerStatic 拒绝 + frameData drop-guard；e2e 验证被拒方数据被丢）
- [x] **M6c** Windows 托盘 GUI（`internal/agent` 共享内核 + `internal/tray` 纯 Win32 托盘 + `cmd/gui`；双托盘 e2e 通过）—— **M6 全部完成**
- [ ] **M9** 跨机验证（真实双机跑 mcprobe，覆盖真实 NAT 打洞；同机已验证通过）

## 技术栈（已确定 ✅）
- **语言**：**Go**（goroutine 适合网络层、上手快、编译为单个 exe）
- **虚拟网卡驱动**：**Wintun**（`golang.zx2c4.com/wintun`，WireGuard 官方签名驱动，L3 IP 层）
- **广播/ARP**：Wintun 无广播 → 软件层模拟广播转发（MC 局域网发现依赖）
- **异步**：标准库 + goroutine
- **协调服务器**：WebSocket + 轻量 HTTP，可部署在任何云 VPS
- **加密库**：纯 **stdlib**（`crypto/ecdh` X25519 + `crypto/hkdf` + `crypto/aes`/`crypto/cipher` AES-256-GCM）——M6a 已落地，零新依赖
- **GUI（M6c，已实现）**：`internal/tray` 纯 Win32（`syscall.NewLazyDLL` 直调 user32/shell32，零新依赖）+ `cmd/gui`；`internal/agent` 抽共享内核供 CLI/GUI 复用

## 已定决策
- ✅ 语言：**Go**
- ✅ 驱动：**Wintun**（L3，软件层模拟广播）

## 已定决策（M6 追加）
- ✅ 加密方案：**X25519 + HKDF + AES-256-GCM**（纯 stdlib，角色无关会话派生）
- ✅ 好友身份机制：**密钥白名单**（`--friends` base64 指纹文件），非账号系统
- ✅ GUI 方案：**纯 Win32**（`internal/tray`，`syscall.NewLazyDLL`，零新依赖）——放弃 `getlantern/systray`（引入第三方依赖）
- ✅ 架构：**`internal/agent` 共享内核**——CLI 与 GUI 同一份实现，UI 只是壳

## 待决策（剩余）
- 虚拟 IP 网段与分配方式（写死 vs 协调服务器动态分配）——已用协调服务器动态分配（10.0.0.x），无争议
- 协调服务器部署方案（哪家 VPS / 域名）

## 项目信息
- 创建日期：2026-08-31
- 位置：`E:\eliaukvpn`
- 环境：Windows 11
