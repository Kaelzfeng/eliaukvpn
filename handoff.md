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
- [x] **M6a 完成（2026-08-31）**：加密层 — X25519 身份 + 角色无关会话派生 + **AES-256-GCM** 数据加密（纯 stdlib，无新依赖），同机双客户端经公网 hairpin 加密隧道 e2e 验证（session 双向建立、mcprobe 走加密隧道零鉴权失败）
- [x] **M6b 完成（2026-08-31）**：好友白名单 — 只允许白名单内的静态密钥建立会话；未在白名单的 peer 双向被拒 + 其数据帧被丢弃（含不对称白名单场景的 drop-guard）
- [x] **M6c 完成（2026-08-31）**：Windows 托盘 GUI — 纯 Win32（`syscall.NewLazyDLL`，零新依赖）托盘图标 + 右键状态菜单（身份/VIP/NAT/peers 连接状态/退出），包住 headless agent；`cmd/gui` 与 `cmd/client` 共享 `internal/agent` 内核，双托盘实例 e2e（加密会话 + 数据面 + 退出钩子）验证通过

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

## M6c 详情（Windows 托盘 GUI，已 e2e 验证）
- **`internal/agent`（重构核心）**：把 `cmd/client` 的注册/STUN/隧道/网卡/广播/自动连接逻辑抽成可复用包；`Agent` 暴露 `Peers()/Snapshot()/Connect()/Status()/Run(ctx)`。CLI（交互式）和 GUI（托盘）共用同一内核 —— 之前双端代码是复制粘贴的，现在一份实现。
- **`internal/tray`（纯 Win32，零新依赖）**：`syscall.NewLazyDLL` 直调 user32/shell32 —— `RegisterClassW`+`CreateWindowExW`（**消息窗口 parent = `HWND_MESSAGE` = (HWND)-3，不是 -1！** -1 是 `HWND_TOPMOST`，会报 `ERROR_INVALID_WINDOW_HANDLE` 1400）+ `GetMessageW` 泵 + `Shell_NotifyIconW`（NIM_ADD/MODIFY/DELETE）+ 右键 `TrackPopupMenu`（`TPM_RETURNCMD` 直接拿命令 id）。托盘图标是运行时画的 32x32 ICO（绿色圆盘 + 白色 E），写临时文件 `LoadImageW` 加载，无二进制资产。菜单模型 `Item`（Label/Separator/Disabled/Submenu/ID），每次右键按当前模型重建（`AppendMenuW` 复制字符串）。
- **布局 pin 测试**：`tray_test.go` 断言 MSG=48B、WNDCLASSW=72B、NOTIFYICONDATAW=976B 及关键字段偏移（hIcon=32、szTip=40、hBalloonIcon=968）——低层 Win32 布局错了会静默错读字段。
- **`cmd/gui`**：1s ticker 重建托盘菜单（身份/VIP/Public/NAT/Identity/Friends + Peers 子菜单 + 退出）；`-exit-after <dur>` 是自动化测试钩子（真机弹两个托盘图标，跑完自动退出）。
- **验证**：双 `gui.exe`（host+join，互白名单，`-debug-packets`）同机经公网 hairpin 直连 —— 双方注册、建虚拟网卡（wintun 顺手清了残留的 Eliauk-join/eve/host 1 孤儿适配器）、`session established`、`vnic->tunnel` 数据帧双向流动、`tunnel RTT≈250ms`、退出钩子干净退出（exit 0）。CLI 交互模式回归通过（peers/status/quit）。
- 备注：release 构建用 `go build -ldflags "-H windowsgui" ./cmd/gui` 隐藏控制台；dev 直接跑（有控制台看日志）。

## M6a 详情（加密层，已 e2e 验证）
- 架构：`internal/crypto`（纯 stdlib）+ `internal/p2p` 集成。长时 X25519 身份 `Identity`（`LoadOrCreate` 持久化到 `%AppData%\Eliauk\identity.key`，0600），启动打印 base64 指纹供好友白名单。
- **角色无关会话派生（关键）**：`NewSession` 算三个对称 DH（static×peer_eph、our_eph×peer_static、our_eph×peer_eph），三个 32B DH 值排序后做规范 IKM，四个公钥排序做规范 transcript（sha256 作 HKDF salt）。**因为同时打洞没有 initiator/responder 之分，双方都跑 responderHandshake** —— 角色相关的 DH 顺序会分叉。
- **每连接一个 ephemeral（第二个半 Bug）**：ephemeral 在 `BeginConnect` 生成一次，hello/helloAck/会话派生共用。responder **绝不重新生成**（曾因覆盖 `p.eph` 导致双方 ephemeral 组合不一致 → 会话分叉 → GCM 鉴权失败）。
- 加密：**AES-256-GCM**（`crypto/aes`+`crypto/cipher`；**不是 chacha20poly1305**——那是 x/crypto 才有，加依赖）。nonce=4B 零||8B 计数器，包格式 = counter(8)||ciphertext(明文+16B tag)，64 包重放窗口；AAD=`"ELK1|" + minID + "|" + maxID` 绑定 peer 对。
- Go 1.27 的 `crypto/hkdf` 是泛型新 API：`hkdf.Key(sha256.New, ikm, salt, "eliaukvpn/session", 32)`，**不是**旧 `hkdf.New`+`Read`。
- 握手消息 = 64B（eph 32||static 32）塞进 hello/helloAck payload；有 session 的 frameData 走 `session.Seal/Open`；身份与无身份 peer 混合 → 拒绝（禁止静默降级）。
- 测试：12 个 crypto 单测 + p2p 加密数据面 / 混合模式拒绝 / 模拟伪造者（TestWrongKeyRejected：冒充 R_pub 但没有 R_priv → 会话必不一致）。

## M6b 详情（好友白名单，已 e2e 验证）
- `tunnel.SetFriends([][]byte)`：把 base64 指纹存进 `friends map[string]bool`；`checkPeerStatic` 验证静态密钥 + 白名单检查（拒绝时错误带出该 peer 指纹，可直接抄进 friends 文件授权）。
- **drop-guard（不对称白名单的关键）**：`frameData` 里 `t.identity != nil && p.session == nil` → 丢弃。堵住「A 拒 B 但 B 收 A」时 B 的密文被 A 当明文转发给虚拟网卡的边角。
- 客户端 `--friends <file>`：每行一个 base64 指纹，`#` 注释、空行跳过。
- 测试：TestWhitelistRejectsUnknownPeer / TestWhitelistAcceptsFriend。e2e：互相白名单的 host/join 连接并交换 MC 流量；未白名单的 eve 被双方拒绝（`peer is not in the friends list (fingerprint …)`），双方各 drop eve 的 11 帧数据（`drop data from eve (no session)`）。
- **已知行为（可接受）**：被拒方（eve）单方面认为自己 connected；数据只单向、在白名单侧被丢弃。

## 下一步（优先级排序）
1. ~~M1~~ ✅ **完成**
2. ~~M2~~ ✅ **完成**
3. ~~M3~~ ✅ **完成**
4. ~~M4~~ ✅ **完成**
5. ~~M5~~ ✅ **完成**（MC 局域网发现经隧道端到端验证通过，含 /32 路由 + BuildDiscovery + 双向 debug 实锤）
6. **M6 已完成** ✅：M6a（加密）+ M6b（白名单）+ M6c（托盘 GUI）全部落地并 e2e 验证 —— **核心里程碑收官**
7. **M9 跨机验证**（下一步）：真机双端跑 `mcprobe`，确认发现 + TCP 走公网隧道（同机已验证，跨机是最终确认；顺带覆盖真实 NAT 打洞）
8. 待定：协调服务器 VPS 选型、`ensurePeerRoute` 的对端下线路由清理（目前 route 只加不删，peer 下线后 VIP 包会发向已失效 peer 被丢弃——对 M5 无碍）

## 里程碑速览
M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 ✅ → M4 虚拟网卡打通 ✅ → M5 游戏链路（MC 局域网发现）✅
→ M6 加密(✅ M6a)+好友白名单(✅ M6b)+GUI(✅ M6c) **全部完成** → M9 跨机验证

## 环境备注
- Windows 11，开发机；Go 在 `C:\Program Files\Go\bin\go.exe`（新 shell 需用全路径或重设 `$env:Path`）。
- `git init` 完成，关联 `https://github.com/Kaelzfeng/eliaukvpn`（origin/main），已推送。
- 协调服务器将来需要一台公网 VPS（暂未部署）。
- **遗留进程**：`eliauk-server.exe`（PID 134868，占 8080/8081）非本会话创建，权限限制不可杀 → 测试一律用 9090/9091。
- 本机多 VPN 网卡（Tailscale 100.x、singbox_tun 172.18.0.1、operator-member-2 10.66.66.128、vEthernet 172.22.48.1），真实 LAN IP 192.168.0.100；测试用公网 hairpin 188.253.121.28。
- 诊断小工具在 `cmd/`：`mcprobe`（假 MC 服务端/客户端）、`twonet`/`readpath`/`iprobe`/`injtest`/`iptime`/`injvar`/`mcast`（M4/M5 调试用，可随时清理）。
