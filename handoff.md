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
- [x] **M1 完成（2026-08-31）**：协调服务器 + 客户端注册 + 虚拟 IP 分配 + STUN 探测，本地双客户端端到端测试通过
- [x] **M2 完成（2026-08-31）**：UDP 打洞直连，本地双客户端双向打通（经公网 hairpin，RTT≈243ms）
- [x] **M3 完成（2026-08-31）**：TURN 式中继回退（对称 NAT / 打洞失败自动走中继，`--force-relay` 可强制测中继链路）
- [x] **M4 完成（2026-08-31）**：Wintun 虚拟网卡打通 + 自动连接 + 数据面送达
- [x] **M5 完成（2026-08-31）**：游戏链路 — MC 局域网发现端到端经隧道验证通过（发现 + TCP 连接，双向隧道数据面日志实锤）

## M2 详情（含踩坑）
- 架构：协调服务器交换 candidates（`connect_request` → 双向 `connect_candidates`），双方在同一 socket 上各自打洞。
- 打洞必须用 **STUN 探测过的同一个 socket**（探测到的公网映射绑定在该 socket）→ `stun.DetectOn(conn, ...)`。
- Candidates：public（STUN）+ 各网卡 LAN IP，都用 socket 本地端口。
- **关键 Bug（已修）**：`stun.roundTrip` 用 `SetReadDeadline` 探测但从不清理 → 探测完 socket 残留绝对 read deadline → 先启动一方读循环在打洞前就死了。修复：`roundTrip` 顶部 `defer conn.SetReadDeadline(time.Time{})`。
- 遗留：同机/同网段下 LAN candidate 也可能先连通（`connected to X (172.18.0.1:port)`），无碍。

## M3 详情
- 中继：`internal/server/relay.go`，UDP 信封 `ELKR|sender(8B)|target(8B)|payload`，target 全零 = announce。注册消息带 `relay_addr`。
- 客户端：`punchLoop` 直连打洞 6s 超时后 → 配了 relayAddr 则改走中继再握手 6s，否则 StateFailed。`sendLocked` 按 `p.relayed` 分派 ELK1 或 ELKR。
- 验证：`--force-relay` 双端强制中继，hello/ack/ping/pong 全通，双方日志 `connected ... (relay)`。

## M4 详情
- 虚拟网卡：`internal/vnic`，Wintun，`Open(name, ip, mask)` 创建适配器 + `netsh` 配 IP + **`waitAddressReady` 等 IP 可绑定（~3s）再返回**（否则注入被 tcpip.sys 判「not locally destined」丢弃）。`Read()` 等 `ReadWaitEvent`（ReceivePacket 非阻塞），`Write()` 注入包。`Close()` 先唤醒读循环再 `sess.End()`，避免 0xc0000005。
- 客户端 `--vnic` + `--vnic-name`；注册后开 `Eliauk-<name>` 网卡（虚拟 IP 10.0.0.x），读循环 `forwardFromVnic` 按目的 IP 查路由表（vip→peer id）→ `tunnel.SendData`；对端 `SetDataSink` 收到 frameData 写回自己的网卡。
- **自动连接**：注册 + 每次 peers 列表都 `autoConnect`，只对已上报 punchable endpoint 的 peer 发 `connect_request`。
- **每 peer /32 主机路由（`ensurePeerRoute`，M5 加入）**：`route add <peerVIP> mask 255.255.255.255 0.0.0.0 IF <本机虚拟网卡 ifIndex>`，发现 peer 的 VirtualIP 即自动安装。作用：强制到 peer VIP 的流量进虚拟网卡（否则 OS 可能选真实网卡/其他 VPN 网卡）。**这条路由使同机双向都走隧道** —— 修正了此前「同机 local delivery 短路」的结论（见 M5 详情）。

## M5 详情（游戏链路：MC 局域网发现）
- **架构**：`internal/lan` 做软件层广播模拟。host 端 `lan.Listen` 绑定 4445（SO_REUSEADDR）+ **在全部 up 网卡上 JoinGroup**（wintun 不报 FlagMulticast 但 JoinGroup 成功）→ 嗅探到 MC 服务器广播 → 经隧道 fanout；join 端 dataSink 收到发现包后注入虚拟网卡。
- **关键：`lan.Listen` 回调给的是裸 UDP 负载（无 IP 头）**。`lan.RewriteSource` 需要完整 IP 包，对负载直接 no-op → 早期「源地址没改成 VIP、join 端收到 192.168.0.100」就是它造成的。修复：新增 `lan.BuildDiscovery(src, payload)` 把负载包成完整 IPv4/UDP 包（src=hostVIP、dst=224.0.2.60:4445、sport=dport=4445、TTL=64、**UDP checksum=0**——IPv4 接受，mcast/injvar 同样注入成功）。host 端 forwardDiscovery 对嗅探路径用 BuildDiscovery、对网卡读路径（完整 IP 包）用 RewriteSource。
- **loop guard**：源地址在虚拟子网（`lan.InVirtualSubnet`）的发现包一律丢弃（自己已转发过的或 peer 回声，再转发会循环）。
- **join 端双路投递**：dataSink 收到发现包 → `a.Write(原组播)`（给在 wintun 上 JoinGroup 的客户端）+ `a.Write(RewriteDest→自己VIP)`（给绑 0.0.0.0:4445 但没加入组的客户端）。不用 LocalEmit（Windows 上 socket→组不环回）。
- **验证方法学**：同机会有真实网卡组播环回把测试污染（mcprobe client 直接收到 192.168.0.100 的广播）。mcprobe client 改成**只接受虚拟子网源**的广告 → 报出的 hostVIP 就是隧道穿越的实锤。
- **`--debug-packets` 诊断开关**（默认关）：记 `vnic->tunnel`（网卡读出→进隧道）和 `tunnel->vnic`（隧道到→注入网卡）每包 src→dst，能直接看出 TCP 握手是否走隧道。
- **测试结果（同机双客户端，host=10.0.0.4 / join=10.0.0.5，直连隧道 RTT≈245ms）**：
  - mcprobe client：`discovered world "Eliauk Test World..." at 10.0.0.4 (port 25565)` —— host 的 VIP，只能来自隧道（源改写只在 forwardDiscovery 发生）。192.168.0.100 直连广播被过滤。
  - mcprobe client：`TCP connect to 10.0.0.4:25565 OK`；server `TCP accepted from 10.0.0.5`。
  - **debug 实锤双向都走隧道**：13:13:45 join 侧 `vnic->tunnel: 52/40 B 10.0.0.5 -> 10.0.0.4`（SYN 进网卡→隧道）+ host 侧 `tunnel->vnic: 52/40 B 10.0.0.5 -> 10.0.0.4`（SYN 到→注入）；回程 SYN-ACK 同样有 host `vnic->tunnel 10.0.0.4 -> 10.0.0.5` + join `tunnel->vnic 10.0.0.4 -> 10.0.0.5`。**同机 /32 路由下 TCP 握手真的穿越了隧道，不是本地短路。**
  - join 端还看到 `at 10.0.0.5` 的世界 —— 同机假象（join 的 socket 也在 host 网卡上加入了组，host 注入的 10.0.0.5 广告被它收到），跨真机不会发生。

## 关键技术上下文（速记，下次不用重新学）
- 玩家都在 NAT 后面，需要 NAT 穿透才能直连。
- **STUN**：公网服务器照镜子，告诉客户端对外 IP:端口 + 判定 NAT 类型。
- **UDP 打洞**：双方同时向对方发 UDP，各自 NAT 留洞达成直连；**对称 NAT 打洞失败** → 回退 **TURN 中继**。
- **协调服务器**只做信令（注册 / 好友 / 交换候选 / 分配虚拟 IP），不碰游戏流量（除非退化中继）。
- **虚拟网卡**是方案 B 核心：让操作系统以为在同一个局域网。
- **驱动选型**：MC 局域网发现靠 UDP 广播（4445 端口）→ Wintun（L3，无广播）需软件层模拟广播转发；tap-windows6（L2，真广播/ARP）天然支持但驱动签名繁琐。
- **加密**：Noise / WireGuard 模式，兼做好友白名单。
- MC Java 走 TCP（25565），隧道把 IP 包/帧塞进 UDP 带走。

## 下一步（优先级排序）
1. ~~M1~~ ✅ **完成**
2. ~~M2~~ ✅ **完成**
3. ~~M3~~ ✅ **完成**
4. ~~M4~~ ✅ **完成**
5. ~~M5~~ ✅ **完成**（MC 局域网发现经隧道端到端验证通过，含 /32 路由 + BuildDiscovery + 双向 debug 实锤）
6. **M6**：加密（Noise/WireGuard 模式）+ 好友白名单 + GUI —— 见 planmode.md
7. **M9 跨机验证**（尚未做）：真机双端跑 `mcprobe`，确认发现 + TCP 走公网隧道（同机已验证，跨机是最终确认；顺带覆盖真实 NAT 打洞）
8. 待定：协调服务器 VPS 选型、好友身份机制、`ensurePeerRoute` 的对端下线路由清理（目前 route 只加不删，peer 下线后 VIP 包会发向已失效 peer 被丢弃——对 M5 无碍）

## 里程碑速览
M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 ✅ → M4 虚拟网卡打通 ✅ → M5 游戏链路（MC 局域网发现）✅
→ M6 加密 + 好友 + GUI → M9 跨机验证

## 环境备注
- Windows 11，开发机；Go 在 `C:\Program Files\Go\bin\go.exe`（新 shell 需用全路径或重设 `$env:Path`）。
- `git init` 完成，关联 `https://github.com/Kaelzfeng/eliaukvpn`（origin/main），已推送。
- 协调服务器将来需要一台公网 VPS（暂未部署）。
- **遗留进程**：`eliauk-server.exe`（PID 134868，占 8080/8081）非本会话创建，权限限制不可杀 → 测试一律用 9090/9091。
- 本机多 VPN 网卡（Tailscale 100.x、singbox_tun 172.18.0.1、operator-member-2 10.66.66.128、vEthernet 172.22.48.1），真实 LAN IP 192.168.0.100；测试用公网 hairpin 188.253.121.28。
- 诊断小工具在 `cmd/`：`mcprobe`（假 MC 服务端/客户端）、`twonet`/`readpath`/`iprobe`/`injtest`/`iptime`/`injvar`/`mcast`（M4/M5 调试用，可随时清理）。
