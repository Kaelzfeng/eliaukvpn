# Handoff — 会话交接文档

> 目的：下次打开这个项目（新会话）时，不用重新聊一遍背景，直接从这里续上。

## 项目
**Eliauk VPN** —— 像 Radmin VPN 的 P2P 虚拟局域网软件，让局域网联机游戏（含 MC）跨公网直接玩。

## 位置
`E:\eliaukvpn`

## 当前进度（2026-08-31）
- [x] 创建项目文件夹
- [x] 学习完整原理：NAT / STUN / UDP打洞 / TURN中继 / ICE / 协调服务器 / 虚拟网卡 / 加密
- [x] **选定方向：方案 B（完整 P2P 虚拟局域网）**，从方案 A 的能力逐层演进，不浪费
- [x] **技术栈已定：Go + Wintun（L3，软件层模拟广播做 MC 局域网发现）**
- [x] **M1 完成（2026-08-31）**：协调服务器 + 客户端注册 + 虚拟 IP 分配 + STUN 探测，本地双客户端端到端测试通过（STUN 探测到公网端点、NAT 类型 restricted_cone、peer 列表双向同步）
- [x] **M2 完成（2026-08-31）**：UDP 打洞直连，本地双客户端双向打通（hello/ack/ping/pong 握手，双向 RTT≈243ms，经公网 hairpin）
- [ ] M3 TURN 中继回退（下一步）

## M2 详情（含踩坑）
- 架构：协调服务器交换 candidates（`connect_request` → 双向 `connect_candidates`），双方在同一 socket 上各自打洞。
- 打洞必须用 **STUN 探测过的同一个 socket**（探测到的公网映射绑定在该 socket）→ `stun.DetectOn(conn, ...)`。
- Candidates：public（STUN）+ 各网卡 LAN IP，都用 socket 本地端口；`gatherCandidates` 采集，服务器 `registry.UpdateEndpoint` 保存。
- **关键 Bug（已修）**：`stun.roundTrip` 用 `SetReadDeadline` 探测，但从不清理 → 探测完 socket 上残留绝对 read deadline → `tunnel.Run()` 首次 `ReadFromUDP` 阻塞到 deadline 到期即报 i/o timeout 永久退出 → 先启动的一方读循环在打洞前就死了，表现为「A 连上 B，B 全程只发不收」。修复：`roundTrip` 顶部 `defer conn.SetReadDeadline(time.Time{})`。
- 测试流程：先起 bob 等完全注册，再起 alice `connect bob`（约 3s 后），读双方日志。同机测试走公网 hairpin（188.253.121.28 自家公网 IP），不同机器/对称 NAT 需真机验证。
- 遗留：同机/同网段下 LAN candidate 也可能先连通（`connected to X (172.18.0.1:port)`），无碍。

## 关键技术上下文（速记，下次不用重新学）
- 玩家都在 NAT 后面，需要 NAT 穿透才能直连。
- **STUN**：公网服务器照镜子，告诉客户端对外 IP:端口 + 判定 NAT 类型。
- **UDP 打洞**：双方同时向对方发 UDP，各自 NAT 留洞达成直连；**对称 NAT 打洞失败** → 回退 **TURN 中继**。
- **协调服务器**只做信令（注册 / 好友 / 交换候选 / 分配虚拟 IP），不碰游戏流量（除非退化中继）。
- **虚拟网卡**是方案 B 核心：让操作系统以为在同一个局域网。
- **驱动选型关键点**：MC 局域网发现靠 UDP 广播（4445 端口）→
  - Wintun（L3，无广播）：需软件层模拟广播转发；
  - tap-windows6（L2，真广播/ARP）：天然支持，但驱动签名繁琐。
- **加密**：Noise / WireGuard 模式，兼做好友白名单。
- MC Java 走 TCP（25565），隧道把 IP 包/帧塞进 UDP 带走。

## 下一步（优先级排序）
1. ~~定技术栈~~ ✅ **Go**
2. ~~定驱动~~ ✅ **Wintun**（L3，软件层模拟广播做 MC 局域网发现）
3. ~~M1~~ ✅ **完成**（协调服务器 + 客户端注册 + STUN 探测 + 虚拟 IP 分配，本地测试通过）
4. ~~M2~~ ✅ **完成**（UDP 打洞直连，本地双客户端双向打通）
5. **M3 开始**：TURN 中继回退（对称 NAT 时打洞失败 → 走中继）；对称 NAT 判定已在 STUN 探测里
6. 剩余待定：虚拟 IP 网段与分配方式、协调服务器 VPS 选型、好友身份机制

## 里程碑速览
M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 → M4 虚拟网卡打通
→ M5 游戏链路（MC 局域网发现）→ M6 加密 + 好友 + GUI

## 环境备注
- Windows 11，开发机；Go 1.27.0（`C:\Program Files\Go\bin`，每次新 shell 需重设 `$env:Path`）
- `git init` 完成，关联 `https://github.com/Kaelzfeng/eliaukvpn`（origin/main），已推送
- 协调服务器将来需要一台公网 VPS（暂未部署）
