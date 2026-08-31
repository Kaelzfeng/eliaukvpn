# Eliauk VPN

像 Radmin VPN 的 P2P 虚拟局域网，用于跨公网联机局域网游戏（重点：Minecraft）。
当前阶段：**M1**（协调服务器 + STUN 探测）。技术栈：**Go + Wintun**。

## 目录结构
```
cmd/
  server/   # 协调服务器（WebSocket 信令 + /peers 调试接口）
  client/   # 客户端 CLI（注册 + STUN 探测 + 上报端点 + 显示 peer）
internal/
  protocol/ # 控制协议消息定义（JSON envelope）
  server/   # 注册表 + 虚拟 IP 分配 + WebSocket 处理
  stun/     # STUN(RFC 5389) 客户端 + NAT 类型检测
```

## 构建
```
go build ./...
```

## 运行（M1 调试）
1. 启动协调服务器：
   ```
   go run ./cmd/server
   ```
2. 开两个终端，各起一个客户端：
   ```
   go run ./cmd/client -name alice
   go run ./cmd/client -name bob
   ```
   客户端会打印自己的公网端点 + NAT 类型，并显示在线 peer。
3. 查看服务端注册表：`http://127.0.0.1:8080/peers`

## 里程碑
见 `planmode.md`。下一步 M2：UDP 打洞直连。
