# 任务：用 Go 实现 P2P 端口转发工具（UDP 打洞直连 + 云服务器中转兜底）

## 1. 目标

- 对端（docker 内）有一个 HTTP 服务监听 80 端口。
- 我在本地（Windows）执行 `connect`，指定“连接对端 80 端口，本地监听 8080”，浏览器访问 `http://127.0.0.1:8080` 即可看到对端页面。
- 优先 P2P（UDP 打洞 + QUIC）直连；**P2P 失败或超时，自动切换为经云服务器中转**，浏览器端无需任何改动。
- 日志必须清楚显示当前连接走的是 `p2p` 还是 `relay`，回退时输出原因（探测失败 / NAT 不兼容 / 打洞超时）。

## 2. 环境信息（一律用占位符，真实值只通过命令行参数或环境变量传入，禁止写入代码、脚本、仓库）

| 项目                   | 值                                                                                                                                                                                            |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 开发机                 | Windows，本机运行 Claude Code                                                                                                                                                                 |
| 云服务器公网 IPv4      | `<SERVER_IP>`                                                                                                                                                                                 |
| 云服务器 SSH 用户名    | `<SSH_USER>`（**使用 SSH 密钥登录**，不使用密码；需要口令时提示我手动输入，不要写进任何文件）                                                                                                 |
| 云服务器工作目录       | `/home/ubuntu/p2p`（已装 Go）                                                                                                                                                                 |
| 对端环境               | 云服务器上的 docker 容器 `code-server`，工作目录 `/home/coder/p2p`                                                                                                                            |
| 进入对端               | 交互：`sudo docker exec -it code-server /bin/bash` 后 `source /etc/profile`；自动化：`sudo docker exec code-server bash -lc 'source /etc/profile && cd /home/coder/p2p && ...'`（不加 `-it`） |
| 服务认证令牌           | `<AUTH_TOKEN>`：用于向 server 认证，server 也知道它（环境变量 `P2P_TOKEN` 或 `--token`）                                                                                                      |
| 端到端密钥             | `<E2E_SECRET>`：**仅 connect 与 agent 持有，绝不发送给 server**（环境变量 `P2P_E2E_SECRET` 或 `--e2e-secret`）                                                                                |
| 控制 / 中转端口（TCP） | `<CONTROL_PORT>`，默认 `7000`                                                                                                                                                                 |
| UDP 探测端口（两个）   | `<UDP_PORT_A>` 默认 `7001`，`<UDP_PORT_B>` 默认 `7002`                                                                                                                                        |

其他注意：

- 安全组 / 防火墙需放行 `<CONTROL_PORT>/tcp`、`<UDP_PORT_A>/udp`、`<UDP_PORT_B>/udp`，需要时提醒我并给出 `ufw allow` 命令。
- 开始前先检查服务器、容器、本机的 `go version` 与 `uname -m`（架构），并据此确定 `go.mod` 的 go 版本和 quic-go 的兼容版本（锁定版本）。使用 `log/slog` 要求 Go 1.21+；若依赖拉取失败，先设置 `GOPROXY=https://goproxy.cn,direct`。

## 3. 角色与架构

单一可执行文件 `p2p`，子命令区分角色：

1. **`server`**（云服务器）
   - 信令：接受控制连接（TCP），按 ID 管理在线 peer，转发双方候选地址。
   - UDP 探测：监听**两个** UDP 端口，回复请求者被 NAT 映射后的公网 `ip:port`（类似 STUN）。两个端口用于判断映射是否随目的地变化（对称型 NAT 检测）。
   - 中转：P2P 失败时按 session id 把两条 TCP 数据连接配对并透传字节（server 看到的只有密文）。
2. **`agent`**（对端 docker 内）
   - 以指定 ID（如 `docker-peer`）注册，保持长连接，等待连接请求。
   - 打通后，为每个流连接本机 `127.0.0.1:<目标端口>` 并双向转发。
   - 端口白名单 `--allow 80,443`，白名单外一律拒绝；只转发到 `127.0.0.1`。
   - ID 重复注册：若旧连接心跳仍正常则拒绝新连接，否则替换旧连接。
3. **`connect`**（本地 Windows，也可在云服务器本机测试）
   - 参数：对端 ID、对端端口、本地监听端口；只监听 `127.0.0.1:<本地端口>`。
   - 每接受一个本地 TCP 连接就在通道（p2p 或 relay）上开一个流，转发到对端指定端口。通道尚未就绪时，新连接最多等待 `--dial-wait`（默认 10s），超时关闭。

流的约定：connect 侧开流，agent 侧接受；流首部为 `目标端口(uint16)`，agent 返回 1 字节状态（成功 / 端口被拒 / 连接失败），之后是纯数据转发。

## 4. 连接流程

1. `agent` 与 `connect` 向 server 注册（TCP 控制连接，认证见第 5 节），并**各自用同一个 UDP socket** 向 server 的两个探测端口发包，得到两个映射地址。若两个映射端口不同，判定为对称型 NAT 并记录日志。同时收集本机局域网候选地址。
2. `connect` 通过 server 向 `agent` 发起连接请求，server 互相转发双方的候选地址（内网地址 + 公网映射）与随机 session id。
3. 双方**同时**向对方所有候选地址以约 200ms 间隔发送打洞包，超时可配置（`--punch-timeout`，默认 8s）。打洞包带固定魔数前缀和 session id。
   - 支持 **peer-reflexive 候选**：从收到的打洞包中学习对方真实源地址并加入候选。这对 docker 场景很关键：容器向宿主机公网 IP 探测时，server 看到的可能是 `172.17.x.x` 内网地址而非真实映射。
   - 若双方都是对称型 NAT，直接进入中转，并在日志中说明原因；仅一方对称时仍尝试打洞。
   - 若探测本身失败（server UDP 不可达），不要等待打洞超时，直接进入中转并记录原因。
4. 打洞成功：在**同一个 UDP socket**上建立 **QUIC** 连接（`github.com/quic-go/quic-go`）。connect 为 QUIC client，agent 为 QUIC server。打洞包与 QUIC 包需在同一 socket 上区分，优先使用 `quic.Transport` 的非 QUIC 包读写能力（若所选版本提供），否则用包装 `net.PacketConn` 按魔数过滤。QUIC 配置 keepalive 15s、空闲超时 60s 以维持 NAT 映射。每个本地 TCP 连接对应一个 QUIC stream。
5. 打洞失败或超时：回退中转。双方各自向 server 的 TCP 中转端口建立数据连接并带上 session id，server 配对后透传；链路为 **TCP → TLS（端到端）→ yamux**，行为与 P2P 一致。server 对未配对连接设超时（如 15s）并限制并发 pending 数量。
6. 通道断开时，`connect` 自动重新协商（先 P2P，再中转），无需重启；已建立的 TCP 连接随通道断开，这是预期行为。可选：处于中转时每隔 `--upgrade-interval`（默认关闭）尝试升级到 P2P。

## 5. 安全设计

- **服务认证（token）**：不在网络上明文传输 token。所有连接（控制、中转数据）使用 HMAC 挑战-应答：server 发随机 nonce，客户端回 `HMAC-SHA256(token, nonce ‖ id ‖ role ‖ session_id)`。认证通过前不分配任何资源。
- **端到端加密与互认（E2E_SECRET）**：P2P 的 QUIC 与中转的 TLS 都使用各自生成的临时自签证书。握手完成后双方互发 `HMAC(e2e_secret, 角色 ‖ session_id ‖ 本端证书指纹 ‖ 对端证书指纹)` 并校验，不一致立即断开。因为 server 不持有 `E2E_SECRET`，所以无法在中转链路上做中间人或读取明文。README 中说明该机制与其局限。
- **控制连接**可选再套一层 TLS（`--server-fp` 固定 server 证书指纹），推荐开启；未开启时 README 注明候选地址等元数据是明文。
- **UDP 探测**无认证，因此响应包不得大于请求包（请求补齐到响应大小），并对单个来源 IP 限速，避免被用作反射放大。
- `connect` 本地只绑定 `127.0.0.1`；agent 只允许白名单端口、只转发到 `127.0.0.1`；relay session id 为 128 位随机数。

## 6. 命令行

```
# 云服务器
p2p server --listen :<CONTROL_PORT> --udp-ports <UDP_PORT_A>,<UDP_PORT_B>   # token 从 P2P_TOKEN 读取

# 对端 docker
p2p agent --server <SERVER_IP>:<CONTROL_PORT> --udp-ports <UDP_PORT_A>,<UDP_PORT_B> --id docker-peer --allow 80

# 本地 Windows
p2p connect --server <SERVER_IP>:<CONTROL_PORT> --udp-ports <UDP_PORT_A>,<UDP_PORT_B> --peer docker-peer --remote 80 --local 8080
#   调试开关：--force-relay（强制中转）、--force-p2p（禁用回退）、--punch-timeout 8s、--dial-wait 10s
```

token 与 e2e 密钥默认从环境变量 `P2P_TOKEN`、`P2P_E2E_SECRET` 读取，也可用 `--token`、`--e2e-secret` 覆盖。

## 7. 工程要求

- Go modules，模块名自定，目录建议 `cmd/p2p`、`internal/{proto,server,agent,client,punch,relay,secure}`。
- 用 `context` 控制生命周期，支持 Ctrl+C 优雅退出，避免 goroutine 泄漏。
- 控制协议使用带长度前缀的 JSON 消息，保持简单。
- 控制连接有心跳，断开后自动重连（指数退避）。
- 日志使用 `log/slog`：注册、候选地址、NAT 检测结果、打洞开始/成功/失败、切换中转及原因、每个转发连接的建立与关闭（含 `mode=p2p|relay`）。
- 单元测试至少覆盖：协议编解码、HMAC 挑战-应答、中转配对逻辑（含未配对超时、重复 session id、错误角色）、端口白名单。

## 8. 实现阶段（每阶段先在对应环境验证再继续）

1. 协议、认证与控制面：server 注册与心跳、agent / connect 注册与重连。
2. **中转通路**：TLS + yamux + 端口转发，跑通 `--force-relay` 全流程，得到可用基线。
3. UDP 探测、NAT 检测、候选收集与打洞。
4. QUIC 通道与 P2P 转发。
5. 自动回退、通道断开后的重新协商，以及稳定性测试。

## 9. 构建与部署

1. 在本地 Windows 项目目录开发，用 scp / rsync 同步源码到 `<SSH_USER>@<SERVER_IP>:/home/ubuntu/p2p`。
2. 服务器上构建静态二进制：`cd /home/ubuntu/p2p && CGO_ENABLED=0 go build -o p2p ./cmd/p2p`；再交叉编译 Windows 版：`GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o p2p.exe ./cmd/p2p`，用 scp 下载到本机。（本机若已装 Go，也可直接本地构建。）
3. 确认架构一致后，直接把二进制放进容器，不必在容器内重新编译：`sudo docker cp /home/ubuntu/p2p/p2p code-server:/home/coder/p2p/p2p`。
4. 在服务器用 `nohup` 或 systemd 运行 `server`（令牌通过 `Environment=` 或 EnvironmentFile 注入，文件权限 600），确认端口已放行。

## 10. 测试与验收

先在容器内启动测试 HTTP 服务：`python3 -m http.server 80`（无 python 则用 Go 写最小测试服务）。

1. **中转**：`connect --force-relay`，浏览器访问 `http://127.0.0.1:8080` 正常，日志显示 `relay`。
2. **P2P**：不加参数，日志显示打洞成功、`p2p`，页面可访问；用大文件下载并比较 sha256。
3. **回退**（两种情况都要测）：
   - 3a 探测失败：在服务器上临时 `iptables -I INPUT -p udp --dport <UDP_PORT_A> -j DROP`，应立即回退中转，日志说明“探测失败”。
   - 3b 探测成功但打洞失败：在服务器上用 `DOCKER-USER` 链丢弃容器与 Windows 公网 IP 之间的 UDP（如 `iptables -I DOCKER-USER -p udp -s <WIN_PUBLIC_IP> -j DROP` 及对应 `-d` 规则），应在 `--punch-timeout` 后自动切到中转，浏览器无需改动。测试后删除规则。
4. **并发**：浏览器同时打开多个页面 / 资源，多连接并行转发正常。
5. **鉴权**：token 错误被拒绝；`E2E_SECRET` 不一致时通道建立失败；请求白名单外端口（如 22）被 agent 拒绝；用抓包确认 token 与业务数据均未以明文出现在网络上。
6. **稳定性**：重启 agent 或 server 后 `connect` 自动恢复；P2P 通道断开后自动重新协商。

> 说明：容器与云服务器在同一台主机，经过 docker 桥接 NAT，不等于真实跨公网 NAT 环境。真正的 P2P 验证以“本地 Windows ↔ 容器”为准；打洞失败（如对称型 NAT）走中转属于预期行为，需在日志里输出原因。

## 11. 交付物

- 完整源码、`go.mod`、`README.md`（架构图、用法、端口说明、安全设计与已知局限）。
- 部署脚本或命令清单（仅含占位符，不含真实凭据）。
- 测试结果简要总结：各场景实际使用 p2p 还是 relay，以及遇到的问题。

## 12. 工作方式

- 先给出简短的设计方案与目录结构，确认无歧义后再实现。
- 需要 `<SERVER_IP>`、`<SSH_USER>`、`<AUTH_TOKEN>`、`<E2E_SECRET>`、端口等信息时，先列出清单再向我索取，不要猜测，也不要写入代码仓库。
- SSH 优先使用密钥；需要口令的步骤提示我手动完成。
