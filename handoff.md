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
- [x] **M6d 完成（2026-08-31）**：傻瓜式主窗口 GUI — 双击即用、零命令行参数；主窗口（昵称/服务器/好友码/好友列表/状态/设置）+ 托盘同泵，配置持久化 `%AppData%\Eliauk\config.json`，UAC 提权自重启，设置变更自动重建 agent 重连；`cmd/genident` 工具 + `e2e-gui.ps1` 全自动双 GUI 脚本 e2e 全绿（注册/会话/数据面/干净退出）
- [x] **M7a 完成（2026-08-31）**：MCTier 式改造第①档 — 账号+好友+在线状态。服务器端账号（PBKDF2-HMAC-SHA256 密码哈希，100k 迭代，RFC2898）+ 会话 Token（16B 随机 hex，**每次登录轮换**）+ 对称好友图 + 设备指纹绑定（每个账号绑定一组 X25519 指纹）；GUI 登录/注册/退出/按用户名加好友/在线状态。
- [x] **M7b 完成（2026-08-31）**：MCTier 式改造第②档 — 房间系统（一键加入）。5 位房间码（32 字符表，无 I/O/0/1），加入即互白名单 + 互见 + 自动打洞（**不是好友也能直连**）；离开/掉线自动退房；修了「退房者永远不知道已退」的 bug（新增 `room_left` 消息）。
- [x] **M7c 完成（2026-08-31）**：MCTier 式改造第③档 — 游戏启动器集成。`internal/mc`（纯 stdlib）：检测 Java/.minecraft/服务器 jar/官方启动器；一键开服（写 eula.txt + 合并 server.properties + `java -jar server.jar nogui`，stdin 输 stop 优雅关服）；servers.dat 的 NBT 注入（gzip→解析→合并{name,ip}→回写，备份 .bak，保留原有条目）；GUI「游戏」面板（Java/jar 自动检测、开服/停服、复制房主地址、添加到启动器、启动游戏）。房主地址 = 房间 Host 的虚拟 IP:25565（`RoomMember.Host` 标记已上线）
- [x] **UI 焕新完成（2026-09-01）**：深色模式 + 全面焕新（`internal/window/theme.go` 新增，提交 `31f850c` 已推送）— 深色背景 + 六张圆角卡片、靛蓝主题色（#6C5CE7）、owner-draw 按钮（悬停/按下反馈，subclass + TrackMouseEvent）、连接状态指示灯（绿/灰/红三态，`idcStatusDot`）、Segoe UI 字体族、深色标题栏（`DwmSetWindowAttribute` DWMWA_USE_IMMERSIVE_DARK_MODE）。纯 Win32/GDI，零第三方依赖。验证：像素采样 10 点全对 + build/vet/test + e2e-gui.ps1 三阶段全绿。
- [x] **WebView2 GUI 重写完成（2026-09-01）**：纯 Win32/GDI 主窗口太「远古」且偶发「未响应」→ 整体换成 **WebView2（Edge Chromium）桌面应用**（同 oopz/KOOK/Discord 路线，复用 Win11 预装 Edge 运行时、不打 150MB Chromium）。新增 `internal/webviewhost`（宿主 + Go↔JS 桥）+ `internal/winutil`（剪贴板/提权/窗口显示隐藏与子类化）；**删除 `internal/window`**；托盘改 `tray.New()` 自泵（`LockOSThread`）+ 双击开窗；关闭/最小化子类化成进托盘。`go get github.com/jchv/go-webview2`（go-winloader 内嵌 `WebView2Loader.dll`，无 sidecar）。**核心 `internal/agent`/`protocol`/`mc`/`p2p`/`crypto`/`server` 零改动**。详见「WebView2 重写详情」。

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

## 运行指南（Quickstart）
- **协调服务器**（公网 VPS 或本机测试，M7 起带账号目录）：
  `go run ./cmd/server -addr :9090 -relay-listen 0.0.0.0:9091 -relay-public <公网IP>:9091 -accounts <账号目录.json>`
  （注意：本机 8080/8081 被遗留进程占用，测试一律用 9090/9091。不带 `-accounts` 时账号功能不可用，legacy 匿名模式仍可跑。）
- **交互式 CLI 客户端**（调试打洞用）：`bin/client.exe -name host -server ws://<server>:9090/ws [-vnic]`
- **傻瓜式 GUI**（正式使用，M6d+M7，WebView2 深色卡片界面）：双击 `eliaukvpn.exe` 即可（需系统 WebView2 运行时，Win11 自带）。首次使用填「昵称」（服务器地址已默认 `wss://vpn.kaelzfeng.uk/ws`，可改）点「保存并连接」。M7 之后推荐**注册账号**：在「账号」组输用户名+密码点「注册」，再点「登录」（登录后 session token 缓存到 config，重启免密）；「好友」组按用户名加好友；「房间」组创建房间 → 把 5 位房间码发给朋友 → 朋友输入房间码点「加入房间」即**自动互连**（不用互加好友）。「游戏」组：自动检测 Java/服务器 jar →「启动服务器」开服 →「复制地址」把房主地址（虚拟 IP:25565）发给朋友 → 朋友「添加服务器」写进启动器多人在线列表即可一键加入。设置与好友持久化到 `%AppData%\Eliauk\config.json`（`-config` 可覆盖路径），X25519 身份在 `identity.key`。GUI 默认尝试 UAC 提权自重启以建虚拟网卡（`-no-elevate` 跳过）。release 无控制台版：`powershell -File build.ps1`（或 `go build -ldflags "-H windowsgui" -o eliaukvpn.exe ./cmd/gui`）。
  **自动化钩子**（测试用）：`-exit-after <dur>` 到时自动退出；`-vnic-name` 指定网卡名；`-debug-packets` 打数据面日志；`-account`/`-password`/`-create-account` 无头登录/注册（带密码即清 token 强制走密码，不带密码用缓存的 token 登录）；`-game-start <jar>` agent 注册后自动开服。`cmd/genident <keyfile>` 打印/生成身份指纹（e2e 脚本用）。
- **CLI 一键互连（不带 GUI，测试白名单用）**：`cmd/genident` 生成两端指纹 → 各自写 `--friends` 文件（每行一个指纹，`#` 注释）→ `bin/client.exe -name host -server ws://… -vnic --friends friends.txt`。
- **M9 跨机验证步骤**（真机双端）：
  1. 每台机器装同一 `gui.exe` 或 `client.exe` + 各自的 keyfile；
  2. 互加好友白名单；3. 一台开 MC 局域网房（或 `mcprobe.exe -mode server`），另一台 `mcprobe.exe -mode client` 应看到 `discovered world ... at <hostVIP> (port 25565)` 且 `TCP connect OK`；
  4. 验收点：发现包源是虚拟 IP（只能来自隧道）、TCP 握手 `--debug-packets` 双向实锤、真实 NAT 打洞成功（不用 `--force-relay`）。

## Cloudflare Tunnel 异地组网（命名隧道，2026-09-01 已部署验证）

没有公网 IP 的机器也能把信令暴露到固定域名 —— 用 **Cloudflare 命名隧道**（零入站端口、零端口映射）。

**已部署固定地址**：`wss://vpn.kaelzfeng.uk/ws`（`curl` 验证 `HTTP/1.1 101 Switching Protocols` 通过，`Server: cloudflare`）。

- **隧道名** `eliauk`；**隧道 ID** `1278f0c5-2a4d-441b-a91b-fd6936820c95`
- **证书** `C:\Users\zc bx\.cloudflared\cert.pem`（`tunnel login` 生成）
- **凭据** `C:\Users\zc bx\.cloudflared\1278f0c5-2a4d-441b-a91b-fd6936820c95.json`
- **配置** `C:\Users\zc bx\.cloudflared\config.yml`：
  ```yaml
  tunnel: 1278f0c5-2a4d-441b-a91b-fd6936820c95
  credentials-file: C:/Users/zc bx/.cloudflared/1278f0c5-2a4d-441b-a91b-fd6936820c95.json
  ingress:
    - hostname: vpn.kaelzfeng.uk
      service: http://localhost:9090
    - service: http_status:404
  ```
- **DNS**：`vpn.kaelzfeng.uk` CNAME → 隧道（`cloudflared tunnel route dns eliauk vpn.kaelzfeng.uk`）
- **重起**：`cloudflared tunnel run eliauk`（cert/config 已落盘，不用再 `login`）

**首次搭建流程（换域名/换机器照着来）**：1. `cloudflared tunnel login`（浏览器授权 → cert.pem）→ 2. `cloudflared tunnel create eliauk`（拿隧道 ID + credentials.json）→ 3. `cloudflared tunnel route dns eliauk vpn.<域名>`（加 CNAME）→ 4. 写上面的 `config.yml` → 5. `cloudflared tunnel run eliauk`。

**心跳（防 CF ~100s 空闲断链，提交 `4f30cc8`）**：客户端 `agent.go` 与服务器 `handler.go` 都加了对称 WS 心跳（20s ping / 60s pong / 10s write wait）。客户端写侧用 `writeMu sync.Mutex` 串行化 —— 修了「UI 动作与 autoConnect 并发写同一 conn」的潜在竞态（gorilla 只允许单写者）。

**踩坑**：CF precheck 里 `TCP Connectivity 7844 FAIL` 是正常的（环境回退到 QUIC 传输 `suggested_protocol=quic`），不影响 WebSocket 流量。`tunnel login` 第一次授权 URL 的 `aud=` 可能为空（落在「域名概览」页），停掉重跑即可。

## M6c 详情（Windows 托盘 GUI，已 e2e 验证）
- **`internal/agent`（重构核心）**：把 `cmd/client` 的注册/STUN/隧道/网卡/广播/自动连接逻辑抽成可复用包；`Agent` 暴露 `Peers()/Snapshot()/Connect()/Status()/Run(ctx)`。CLI（交互式）和 GUI（托盘）共用同一内核 —— 之前双端代码是复制粘贴的，现在一份实现。
- **`internal/tray`（纯 Win32，零新依赖）**：`syscall.NewLazyDLL` 直调 user32/shell32 —— `RegisterClassW`+`CreateWindowExW`（**消息窗口 parent = `HWND_MESSAGE` = (HWND)-3，不是 -1！** -1 是 `HWND_TOPMOST`，会报 `ERROR_INVALID_WINDOW_HANDLE` 1400）+ `GetMessageW` 泵 + `Shell_NotifyIconW`（NIM_ADD/MODIFY/DELETE）+ 右键 `TrackPopupMenu`（`TPM_RETURNCMD` 直接拿命令 id）。托盘图标是运行时画的 32x32 ICO（绿色圆盘 + 白色 E），写临时文件 `LoadImageW` 加载，无二进制资产。菜单模型 `Item`（Label/Separator/Disabled/Submenu/ID），每次右键按当前模型重建（`AppendMenuW` 复制字符串）。
- **布局 pin 测试**：`tray_test.go` 断言 MSG=48B、WNDCLASSW=72B、NOTIFYICONDATAW=976B 及关键字段偏移（hIcon=32、szTip=40、hBalloonIcon=968）——低层 Win32 布局错了会静默错读字段。
- **`cmd/gui`**：1s ticker 重建托盘菜单（身份/VIP/Public/NAT/Identity/Friends + Peers 子菜单 + 退出）；`-exit-after <dur>` 是自动化测试钩子（真机弹两个托盘图标，跑完自动退出）。
- **验证**：双 `gui.exe`（host+join，互白名单，`-debug-packets`）同机经公网 hairpin 直连 —— 双方注册、建虚拟网卡（wintun 顺手清了残留的 Eliauk-join/eve/host 1 孤儿适配器）、`session established`、`vnic->tunnel` 数据帧双向流动、`tunnel RTT≈250ms`、退出钩子干净退出（exit 0）。CLI 交互模式回归通过（peers/status/quit）。
- 备注：release 构建用 `go build -ldflags "-H windowsgui" ./cmd/gui` 隐藏控制台；dev 直接跑（有控制台看日志）。

## M6d 详情（傻瓜式主窗口 GUI，已 e2e 验证）
- **架构**：`cmd/gui` 重写为「一个窗口 + 一个托盘 + 一个 agent 生命周期」。主窗口（`internal/window`）是纯 Win32 原生控件窗口（STATIC/EDIT/BUTTON/LISTBOX + 分组框，系统字体，520×566 固定），由 `win.Run()` 独享 UI 线程消息泵；`window.SetView` 用 PostMessage(wmRefresh) 把渲染模型跨 goroutine 送到 UI 线程；用户动作从 `win.Events()` 读。托盘（`internal/tray.NewOnWindow`）挂同一个窗口，`MsgHook` 在 wndProc 前截获 `tray.CallbackMsg`（0x8001，LDblClk→Show 窗口，其余→HandleTrayMsg）。
- **消息槽位**：`window.wmRefresh = wmApp+3`（0x8003），**不是** wmApp+1 —— 0x8001 被托盘回调占用且被 MsgHook 先截，同值会被吞掉导致窗口永不刷新（已踩坑修复）。
- **生命周期**：`agentLoop` 无设置时空闲等 `restartCh`；有设置则 `agent.New`→`Run`→出错 3s 自动重连；`signalRestart`（保存设置）取消当前 agent 并在 `restartCh` 排队重建（带 stale-agent 丢弃：dial 期间设置变了就关掉刚建的）。`tickLoop` 每秒 SetView + 更新托盘 tooltip。主 select 循环同泵 `win.Events`/`selCh`（托盘菜单转发）/`exitTimer`（`-exit-after`）/`quitCh`。
- **配置驱动**：`config.Load` 读 `config.json`（昵称/服务器/好友列表）；`-name`/`-server` 仅测试用覆盖。`crypto.LoadOrCreate` 启动即载身份 → 好友码在连接前就显示。UAC：非提权且未 `-no-elevate` → `RelaunchElevated`（同命令行提权重启，原进程退出；用户拒了则无虚拟网卡继续跑并显示红字）。
- **`internal/window` 的 uintptr→unsafe.Pointer 坑**：`GlobalLock`/`GetCommandLineW` 返回的指针装进 uintptr，`go vet` 拒绝一切直接转换（含包装函数）→ 用标准 launder 惯用法 `*(*unsafe.Pointer)(unsafe.Pointer(&p))` 再 `unsafe.Slice`。控制台窗口 GetCommandLineW 单缓冲没问题（提权重启用同命令行）。
- **`cmd/genident <keyfile>`**：载入/创建 X25519 身份并打印 base64 指纹 —— 让 e2e 脚本和想预生成 keyfile 的用户不用先跑 GUI。
- **e2e 验证**（`e2e-gui.ps1` 全自动，等价 bash 编排全绿）：只预写 host/join 两个 `config.json`（互指对方指纹）+ `-exit-after`，双击式启动两个 `gui.exe`（各自 `-vnic-name Eliauk-e2eh/-e2ej`）→ 双方 `registered`（10.0.0.4/10.0.0.5）→ `session established` → mcprobe 数据面（client 发现 `at 10.0.0.4 (port 25565)` + `TCP connect OK`，server `TCP accepted from 10.0.0.5`，`vnic->tunnel`/`tunnel->vnic` 双向实锤）→ 退出码 0/0，config/identity 落盘。脚本 finally 清理：杀进程、删 Eliauk-e2e* 适配器、删 workspace。

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

## M7 详情（账号/好友/房间/启动器，MCTier 式，已单测 + 集成测试 + e2e 验证）
- **服务器端账号**（`internal/server`）：账号按用户名存，密码 PBKDF2-HMAC-SHA256（RFC2898，100k 迭代，格式 `pbkdf2$<salt hex>$<hash hex>`）；**会话 Token** 16B 随机 hex，**每次登录轮换**（`RotateToken`）——旧 token 立即失效；设备 = 账号绑定的 base64 X25519 指纹集合（防换机伪冒，也用于白名单）。认证顺序：**先验 Token 后验密码**。
- **可见性规则**：账号客户端只看到 好友 ∪ 同房间成员；legacy 匿名客户端（Account==""）仍看到所有人。`connect_request` 受 `reg.VisibleTo` 门控。
- **对称好友图**：`friend_add` 是双向的（加别人 = 对方也加你）；在线状态 `presence` 推送。好友列表进 `friend_list`。
- **房间**（`internal/server/rooms.go`）：5 位码（32 字符表无 I/O/0/1）；`Room{Host, Members}` 本来就有 Host（创建者），M7c 才把它暴露到线上（`RoomMember.Host`）；成员 = 在线账号（离线自动退房）。加入房间 → 服务器把每个成员 KeyFP/VirtualIP 下发 → 客户端 `syncWhitelistLocked()` 把 房间成员指纹 并进白名单 → `autoConnect` 自动打洞（**不是好友也能直连 = 一键加入的实质**）。
- **退房 bug（已修）**：服务器原来只给剩余成员发 `room_update`，退房者自己永远不知道已退出 → 客户端残留房间状态/指纹。修复：新增 `room_left` 消息发给退房者，agent `clearRoom()` 清空房间+roomFP 并重算白名单。
- **`internal/mc`（纯 stdlib，M7c）**：
  - 检测：`MinecraftDir`（%AppData%\.minecraft）、`FindJava`（JAVA_HOME → Program Files 浅层 glob（Adoptium/Java/Microsoft/Corretto）→ .minecraft\runtime 深度受限 walk → PATH）、`FindServerJar`（cwd/server.jar 或 .minecraft）、`LauncherExe`。
  - 一键开服：`PrepareServerDir`（写 eula.txt；server.properties **只补缺省** server-port=25565/online-mode=false/motd，用户已有键保留）+ `StartServer`（`java -Xmx1G -jar <jar> nogui`，stdin 输 `stop` 优雅关服 + Kill 兜底）。
  - **servers.dat NBT 注入**：手写最小 NBT 读写（gzip → TAG 树，支持全部类型含 Int/LongArray）→ 在 `servers` 列表合并/更新 {name, ip} → 回写前备份 `.bak`，其他条目与根标签原样保留。官方启动器多人在线列表即刻出现该服务器。
- **GUI 游戏面板（M7c，`internal/window` 520×796，UI 焕新后为深色卡片界面）**：Java 路径/服务器 jar 两输入框 + 自动检测、启动/停止服务器、复制地址（= 房主虚拟 IP:25565）、添加到启动器（写 servers.dat）、启动游戏（拉起官方启动器）。路径持久化到 config（`java`/`server_jar`）。`-game-start <jar>` 是自动化钩子：agent 注册后自动开服（e2e 用）。
- **e2e（`e2e-gui.ps1` 三阶段）**：A) legacy 匿名双 GUI p2p + mcprobe 数据面（不变）；B) `-create-account` 注册 → config 落盘 session token（脱敏检查字节数，不打印 token）→ 仅 `-account` 免密重启登录成功；C) 现场 javac 编译一个「假 MC 服务器」（绑定 25565 的 Java 程序）→ `-game-start` 让 GUI 开服 → 断言 server.properties/eula.txt 写入 + 25565 端口监听 + config 持久化 java/server_jar + **退出后进程和端口一并清干净**。
- **安全边界**：密码明文从不落盘（只进 `pendingPass`，拿到 token 即忘）；GUI 永不打印 token 原文（e2e 用 `token.Length > 0` 这类脱敏断言）；`online-mode=false` 只写进新建的 server.properties（已存在的用户配置保留）。

## UI 焕新详情（深色模式，2026-09-01，提交 `31f850c`）
- **布局契约**：主窗口 client 固定 520×796（`winW`/`winH`），由 `AdjustWindowRectEx` 把外层窗口撑到正好（不同主题 caption/border 尺寸差异不会裁剪卡片）。六张圆角卡片 `cardRects`（`theme.go`）每张 = 顶部标题带（top+14..top+32，左缘 13px 靛蓝 accent bar + 标题）+ 下方控件区；控件坐标（`window.go createControls`）与卡片严格对应。
- **Owner-draw 按钮**：`BS_OWNERDRAW`，父窗口 `WM_DRAWITEM` 里 `drawButton` 画圆角矩形（`RoundRect` r=7）+ 文字（`DrawTextW` 居中）。**关键常量：`ODT_BUTTON=4`、`ODT_STATIC=5`（不是 2/1）**——之前写错导致 `switch ds.ctlType` 永不匹配、按钮回退成系统默认浅色（240,240,240）。`DRAWITEMSTRUCT` 布局 pin 见 `window_test.go`（ctlID@4、hwndItem@24、hDC@32、rcItem@40，共 64B）。
- **悬停/按下反馈**：每个按钮 `SetWindowLongPtrW` subclass（`subclassButton`），WM_MOUSEMOVE/WM_MOUSELEAVE（`TrackMouseEvent` TME_LEAVE）/WM_LBUTTONDOWN/UP 更新 `btnState` 后 `InvalidateRect`；callback 必须保活（`w.btnCallbacks`）。primary 按钮（`primaryBtn` map：登录/注册/保存/创建房间/加入房间/添加/启动服务器）用靛蓝填充，其余次级灰。
- **状态指示灯**：`idcStatusDot` 是 `SS_OWNERDRAW` static（14×14），`drawStatusDot` 画圆点；颜色 = `setStatusDot`：1=绿(colGreen)、2=红(colRed)、0/默认=灰(colMuted)。`applyView` 里根据 `v.Good`/状态文本含「连接」映射。旁边 `statusTxt` 用 `dotColor()` 同步文字色。
- **深色控件**：`WM_CTLCOLORSTATIC/EDIT/LISTBOX` 统一返回深色 brush（`wmEraseBkgnd` 先刷全窗背景，`hbrBgnd` 类背景也设为深色）；native 控件 `SetWindowTheme("DarkMode_Explorer")` 让输入框边框/列表框选中变深。STATIC 颜色/字体按控件存 map（`label()` 辅助）。字体族 = Segoe UI（标题 700/20、卡片标题 600/15、正文 400/14、标签 400/13、小字 400/12）。
- **深色标题栏**：`applyImmersiveDark` 调 `DwmSetWindowAttribute(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE=20, &1, 4)`。
- **启动可见性修复**：`WS_VISIBLE`(0x10000000) 并进 `winStyle`——首次 `ShowWindow` 会忽略 nCmdShow 改用 STARTUPINFO 的 show state，隐藏/最小化启动态曾导致窗口不可见。`SetProcessDPIAware` 在创建窗口前调用，固定布局在高 DPI 下不糊。
- **踩坑记录**：`PAINTSTRUCT.rcPaint` 偏移是 12（RECT 4 对齐，不是 16）；`TRACKMOUSEEVENT` Go 结构按 uintptr 对齐补到 24B，但 `cbSize=20` 让 Windows 只读 20B；hDC/rcItem 在 DRAWITEMSTRUCT 里是控件本地坐标（rcItem=(0,0,w,h)），直接在 ds.hDC 上画即可。
- **验证方法**：`C:\Users\zc bx\AppData\Local\Temp\eliauk-ui\verify.ps1`（拉窗口 → CopyFromScreen client 区 → 采样 10 个点：header 背景/卡片填充/卡片缝隙/accent bar/login 按钮靛蓝/次级按钮/输入框/状态点/退出按钮）全对；`scan.ps1` 数整窗靛蓝 vs btnface 像素。GUI 自检：先起协调服务器（9090/9091，`-accounts` 带路径要预加引号——`Start-Process -ArgumentList` 会按空格拆参数），再 `bin\gui.exe -config <cfg> -keyfile <key> -account host -password x -create-account -no-elevate -vnic=false` 注册后绿灯。

## 下一步（优先级排序）
1. ~~M1~~ ✅ **完成**
2. ~~M2~~ ✅ **完成**
3. ~~M3~~ ✅ **完成**
4. ~~M4~~ ✅ **完成**
5. ~~M5~~ ✅ **完成**（MC 局域网发现经隧道端到端验证通过，含 /32 路由 + BuildDiscovery + 双向 debug 实锤）
6. **M6 已完成** ✅：M6a（加密）+ M6b（白名单）+ M6c（托盘 GUI）+ M6d（傻瓜式主窗口 GUI）全部落地并 e2e 验证 —— **核心里程碑收官**
7. **M7 已完成** ✅（MCTier 式改造，2026-08-31）：M7a 账号+好友+在线状态 → M7b 房间一键加入 → M7c 游戏启动器集成，全部落地（单测 + `TestRoomIntegration` 集成测试 + `e2e-gui.ps1` 三阶段 e2e 验证）
8. **UI 焕新已完成** ✅（2026-09-01，提交 `31f850c`）：深色模式 + 全面焕新（见上方「UI 焕新详情」），像素采样/单测/e2e 全绿，已推送
9. **M9 跨机验证**（下一步，需用户两台真机）：真机双端跑 `gui.exe`（账号注册→建房→分享房间码，或互加好友码）或 `mcprobe`，确认发现 + TCP 走公网隧道（同机已验证，跨机是最终确认；顺带覆盖真实 NAT 打洞）
10. ~~协调服务器 VPS 选型~~ ✅ 已用 Cloudflare 命名隧道解决（2026-09-01，固定地址 `wss://vpn.kaelzfeng.uk/ws`，见「Cloudflare Tunnel 异地组网」节）；待定：`ensurePeerRoute` 的对端下线路由清理（目前 route 只加不删，peer 下线后 VIP 包会发向已失效 peer 被丢弃——对 M5 无碍）

## 里程碑速览
M0 文档 ✅ → M1 协调服务器/STUN ✅ → M2 打洞直连 ✅ → M3 中继回退 ✅ → M4 虚拟网卡打通 ✅ → M5 游戏链路（MC 局域网发现）✅
→ M6 加密(✅ M6a)+好友白名单(✅ M6b)+托盘GUI(✅ M6c)+傻瓜式主窗口GUI(✅ M6d) **全部完成**
→ M7 MCTier 式改造（✅ M7a 账号/好友/在线状态 + ✅ M7b 房间一键加入 + ✅ M7c 游戏启动器集成）**全部完成**
→ **UI 焕新 ✅**（深色模式 + 全面焕新，提交 `31f850c`）→ M9 跨机验证（最终确认，需用户两台真机）

## 环境备注
- Windows 11，开发机；Go 在 `C:\Program Files\Go\bin\go.exe`（新 shell 需用全路径或重设 `$env:Path`）。
- `git init` 完成，关联 `https://github.com/Kaelzfeng/eliaukvpn`（origin/main），已推送。
- 协调服务器的公网暴露：~~需要公网 VPS~~ 已用 **Cloudflare 命名隧道**解决（见「Cloudflare Tunnel 异地组网」节），固定地址 `wss://vpn.kaelzfeng.uk/ws`，无需公网 IP。
- **遗留进程**：`eliauk-server.exe`（PID 134868，占 8080/8081）非本会话创建，权限限制不可杀 → 测试一律用 9090/9091。
- 本机多 VPN 网卡（Tailscale 100.x、singbox_tun 172.18.0.1、operator-member-2 10.66.66.128、vEthernet 172.22.48.1），真实 LAN IP 192.168.0.100；测试用公网 hairpin 188.253.121.28。
- 诊断小工具在 `cmd/`：`mcprobe`（假 MC 服务端/客户端）、`twonet`/`readpath`/`iprobe`/`injtest`/`iptime`/`injvar`/`mcast`（M4/M5 调试用，可随时清理）。
