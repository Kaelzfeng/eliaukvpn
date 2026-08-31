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
- [x] **M3 完成（2026-08-31）**：TURN 式中继回退（对称 NAT / 打洞失败自动走中继，`--force-relay` 可强制测中继链路）
- [x] **M4 完成（2026-08-31）**：Wintun 虚拟网卡打通 + 自动连接（虚拟 IP 10.0.0.7/.8，双向隧道直连/中继，数据面 frameData 送达 dataSink）
- [ ] M5 游戏链路（MC 联机：先手动直连验证 → 再软件层广播模拟，实现 MC 局域网发现）

## M2 详情（含踩坑）
- 架构：协调服务器交换 candidates（`connect_request` → 双向 `connect_candidates`），双方在同一 socket 上各自打洞。
- 打洞必须用 **STUN 探测过的同一个 socket**（探测到的公网映射绑定在该 socket）→ `stun.DetectOn(conn, ...)`。
- Candidates：public（STUN）+ 各网卡 LAN IP，都用 socket 本地端口；`gatherCandidates` 采集，服务器 `registry.UpdateEndpoint` 保存。
- **关键 Bug（已修）**：`stun.roundTrip` 用 `SetReadDeadline` 探测，但从不清理 → 探测完 socket 上残留绝对 read deadline → `tunnel.Run()` 首次 `ReadFromUDP` 阻塞到 deadline 到期即报 i/o timeout 永久退出 → 先启动的一方读循环在打洞前就死了，表现为「A 连上 B，B 全程只发不收」。修复：`roundTrip` 顶部 `defer conn.SetReadDeadline(time.Time{})`。
- 测试流程：先起 bob 等完全注册，再起 alice `connect bob`（约 3s 后），读双方日志。同机测试走公网 hairpin（188.253.121.28 自家公网 IP），不同机器/对称 NAT 需真机验证。
- 遗留：同机/同网段下 LAN candidate 也可能先连通（`connected to X (172.18.0.1:port)`），无碍。

## M3 详情
- 中继：`internal/server/relay.go`，UDP 端口 8081（`--relay-listen`），信封 `ELKR|sender(8B)|target(8B)|payload`，target 全零 = announce（学习对端地址）。服务器记住每个 id 的 UDP 来源地址（`registry.SetRelayAddr`），注册消息带 `relay_addr` 通知客户端。
- 客户端：`punchLoop` 直连打洞 6s 超时后 → 若配了 relayAddr 则 `p.relayed=true`，改走中继再握手 6s，否则 StateFailed。`sendLocked` 按 `p.relayed` 分派直连（ELK1）或中继（ELKR 包裹）。收到来源 == relayAddr 的包即判定中继路径。
- 验证：`--force-relay` 双端强制中继，hello/ack/ping/pong 全通（127.0.0.1:8081），双方日志 `connected ... (relay)`。

## M4 详情
- 虚拟网卡：`internal/vnic`，Wintun（`golang.zx2c4.com/wintun`），`Open(name, ip, mask)` 创建适配器 + `netsh` 配 IP，`Read()` 等 `ReadWaitEvent`（ReceivePacket 非阻塞，空环返回 `ERROR_NO_MORE_ITEMS`），`Write()` 注入包。
- 客户端 `--vnic`（默认开）+ `--vnic-name`；注册后开 `Eliauk-<name>` 网卡（虚拟 IP 10.0.0.x），读循环 `forwardFromVnic` 按目的 IP 查路由表（vip→peer id）→ `tunnel.SendData`；对端 `SetDataSink` 收到 frameData 写回自己的网卡。
- **自动连接**：注册 + 每次 peers 列表都 `autoConnect`，只对已上报 punchable endpoint（`PublicIP != ""`）的 peer 发 `connect_request`，避免「peer has not reported candidates yet」后不重试的竞态。
- **同机限制**：Windows 对同主机两个本地虚拟 IP 的流量走 loopback 短路，不经隧道 → 同机 `ping 10.0.0.x` 不通属预期，真实验证需两台机器。数据面已用单元测试 `internal/p2p/tunnel_test.go` 证明（frameData 送达 dataSink、未连接 peer SendData 报错）。
- 验证：Eliauk-alice(10.0.0.8) / Eliauk-bob(10.0.0.7)，双向 `connected (direct)`，RTT 0.5–1ms（LAN）/243ms（公网）。

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
5. ~~M3~~ ✅ **完成**（TURN 中继回退，`--force-relay` 验证通过）
6. ~~M4~~ ✅ **完成**（Wintun 虚拟网卡打通 + 自动连接 + 数据面送达）
7. **M5 开始**：游戏链路 — ① MC Java 直接连虚拟 IP（25565 经隧道）② 软件层模拟 UDP 4445 广播 → MC 局域网发现「直接连接服务器」
8. 剩余待定：协调服务器 VPS 选型、好友身份机制（M6）

## 里程碑速览
M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 ✅ → M4 虚拟网卡打通 ✅
→ M5 游戏链路（MC 局域网发现）→ M6 加密 + 好友 + GUI

## 环境备注
- Windows 11，开发机；Go 1.27.0（`C:\Program Files\Go\bin`，每次新 shell 需重设 `$env:Path`）
- `git init` 完成，关联 `https://github.com/Kaelzfeng/eliaukvpn`（origin/main），已推送
- 协调服务器将来需要一台公网 VPS（暂未部署）
