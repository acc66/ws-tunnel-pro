# WebSocket 隧道完整部署教程（A/B 服务器 + HAProxy + 自动拉黑）

## 架构

```
用户 → A 服务器（ws-client，监听入口端口，如 88）
     ══ WebSocket 隧道（smux 多路复用，路径 + 密码校验）══
     → B 服务器（ws-tunnel，:8080/mm）
     → 目标：本机 HAProxy（Unix socket 或 127.0.0.1:20001）→ 各后端
```

- A 和 B 之间走 WebSocket，所有用户连接复用几十条长连接，A→B 的 TCP 连接数很少。
- 路径和密码都对才放行；扫描器访问其他路径会看到假的 nginx 页面，IP 被拉黑。
- B 后面可以直接是一个服务，也可以是 HAProxy 按域名分流到多个后端。

下文占位符：`A_IP`、`B_IP`、`你的密码`、`你的IP`，请换成自己的值。系统以 Debian 12/13 为例，所有命令用 root 执行。

## 1. 系统准备（A、B 都建议做）

```bash
apt-get update && apt-get install -y curl socat

# DNS：写入并锁定，防止 DHCP/cloud-init 改回去（国内机器把 1.1.1.1 换成 223.5.5.5）
chattr -i /etc/resolv.conf 2>/dev/null
printf 'nameserver 8.8.8.8\nnameserver 8.8.4.4\nnameserver 1.1.1.1\noptions timeout:2 attempts:2\n' > /etc/resolv.conf
chattr +i /etc/resolv.conf          # 以后要改先 chattr -i

# 2G swap，防止内存打满时服务被 OOM 直接杀掉
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
grep -q swapfile /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

`/etc/resolv.conf` 如果是指向 `/run/systemd/resolve/` 的链接，改用 `/etc/systemd/resolved.conf.d/dns.conf` 写 `[Resolve]` + `DNS=8.8.8.8 8.8.4.4`，再 `systemctl restart systemd-resolved`。

## 2. B 服务器：HAProxy（B 后面直接是单个服务的可跳过）

```bash
apt-get install -y haproxy
mkdir -p /var/lib/haproxy
cp /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.bak
```

在 `/etc/haproxy/haproxy.cfg` 里按自己的后端写配置。下面是最小骨架，标了 ★ 的几行是和 ws-tunnel 配合的关键：

```haproxy
global
  log 127.0.0.1 local0 notice          # ★ 只记重要事件，不要每个连接一条
  nbthread 2                           # ★ 和 CPU 核数一致
  maxconn 200000
  stats socket /var/lib/haproxy/stats mode 600 level admin

defaults
  log global
  mode http
  timeout connect 5s
  timeout client  50s
  timeout server  50s
  timeout tunnel  15m                  # ★ 空闲 WebSocket 15 分钟回收
  timeout client-fin 30s
  timeout server-fin 30s
  option http-server-close
  default-server init-addr last,libc,none   # ★ 开机 DNS 未就绪也能启动
  # 不要开 compression（隧道流量压不了，只会白占 CPU）

resolvers mydns
  nameserver dns1 8.8.8.8:53
  nameserver dns2 1.1.1.1:53
  hold valid 10s

listen admin_stats
  bind 127.0.0.1:12888                 # ★ 统计页只允许本机访问
  stats uri /
  stats auth admin:改成强密码

frontend mmk
  bind :::20001
  bind unix@/run/haproxy-ws.sock mode 666   # ★ ws-tunnel 走 Unix socket 进来
  option dontlognull
  acl is_websocket hdr(Upgrade) -i websocket
  acl valid_path path /mm
  use_backend honeypot if !valid_path
  use_backend honeypot if valid_path !is_websocket
  # 按域名分流示例
  acl site1 hdr_beg(host) -i site1.
  use_backend site1_backend if site1
  default_backend honeypot

backend site1_backend
  server s1 后端域名或IP:端口 resolvers mydns check inter 30s

backend honeypot
  http-request return status 200 content-type "text/html" string "<h1>Welcome to nginx!</h1>"
```

检查并启动：

```bash
haproxy -c -f /etc/haproxy/haproxy.cfg && systemctl enable haproxy && systemctl restart haproxy
ls -l /run/haproxy-ws.sock        # 能看到 srw-rw-rw- 开头的文件即可
```

以后改配置一律用 `haproxy -c -f /etc/haproxy/haproxy.cfg && systemctl reload haproxy`，**不要用 restart**：restart 中间有几秒没人监听，reload 是无缝的。

不要加 `option splice-auto`：在 HAProxy 2.6 上测试时它会让 HAProxy 用户态 CPU 空转飙升。

## 3. B 服务器：安装 ws-tunnel

```bash
curl -fLO https://github.com/acc66/ws-tunnel-pro/releases/latest/download/ws-tunnel.sh
bash ws-tunnel.sh install
```

安装时会依次询问：

| 提示 | 示例 | 说明 |
|---|---|---|
| 目标地址 | `unix:/run/haproxy-ws.sock,127.0.0.1:20001` | 见下表 |
| 监听端口 | `8080` | 多个用逗号分隔，如 `8080,8443` |
| WebSocket 路径 | `/mm` | 必须和 A 一致 |
| 连接密码 | `你的密码` | 必须和 A 一致，建议 16 位以上 |
| 白名单IP | `A_IP,你的IP` | 永远不会被拉黑。**一定要填 A 的 IP** |
| 共享 Redis | 直接回车 | 多台服务器共用黑名单时才需要，见第 8 节 |
| 进程名 | 直接回车 | 想在 `ps`/`top` 里显示别的名字就填，如 `nginx-worker` |

目标地址怎么填：

| 写法 | 适用 | 效果 |
|---|---|---|
| `unix:/run/haproxy-ws.sock,127.0.0.1:20001`（推荐） | HAProxy 在本机，配置里有 `bind unix@...` | 平时走 Unix socket 最省 CPU；socket 连不上自动改走 TCP，5 秒后再试 socket |
| `127.0.0.1:20001` | HAProxy 在本机，没加 unix bind | 最简单 |
| `IP:端口` | 目标在别的机器 | Unix socket 只能本机用 |

多个地址用逗号分隔，按顺序故障切换。之后可以随时用 `wst target 新地址` 修改。

装完服务已启动并开机自启，快捷命令是 `wst`。云服务器记得在安全组放行 TCP 8080。

## 4. A 服务器：安装 ws-client

```bash
curl -fLO https://github.com/acc66/ws-tunnel-pro/releases/latest/download/ws-client.sh
bash ws-client.sh add 88 ws://B_IP:8080/mm 你的密码
```

第一次运行会自动安装程序，并询问进程名（直接回车用默认）。快捷命令是 `wsc`。

- 每个入口端口是一个独立服务，需要几个端口就 `add` 几次：`wsc add 2086 ws://B_IP:8080/mm 你的密码`。
- 有备用 B 时，地址用逗号分隔，第一个是主，其余为备用：
  ```bash
  wsc add 88 ws://B_IP:8080/mm,ws://备用B_IP:8080/mm 你的密码
  wsc server 88 ws://B_IP:8080/mm,ws://备用B_IP:8080/mm    # 已有端口修改服务器
  ```
  主服务器 5 秒连不上就切下一个；故障服务器每 10 秒探测一次，恢复后新连接自动切回。所有 B 的密码要一致。
- 安全组放行入口端口（如 TCP 88）。

### 多路复用

A 默认把用户连接复用到每个 B 的 4 条 WebSocket 上；每条超过 128 个连接自动新开一条，最多 64 条，空闲后多开的自动关闭。B 上 `📊` 日志里的「WebSocket 连接」就是这个数，「转发中」是用户连接数。

调整：编辑 `/etc/ws-client/端口.conf`，例如 `EXTRA="-mux 8 -mux-streams 64 -mux-max 128"`，再 `wsc restart`。`-mux 0` 关闭复用。

## 5. 验证

```bash
# B 上
wst ports                         # 配置、进程名、开机启动状态
wst version                       # 脚本 / 程序 / GitHub 最新版本
wst log                           # 实时日志，每分钟一条 📊（WebSocket 连接数、转发中、内存）

# A 上
wsc list                          # 每个端口状态应为 active
wsc log 88                        # 启动时列出主/备用服务器；故障/恢复时会提示
```

用客户端连 A 的入口端口试一下能不能用。

**不要用浏览器访问 `http://B_IP:8080/` 或 `http://A_IP:88/` 来测试**：路径不对会把你自己拉黑。要看伪装页，先 `wst allow A_IP,你的IP` 把自己加进白名单。

## 6. 拉黑规则（B 服务器）

| 请求 | 结果 |
|---|---|
| 路径 `/mm`、密码正确、是 WebSocket | 正常转发 |
| 路径 `/mm`，密码错误或没带密码 | 伪装页 + 拉黑 IP |
| 路径 `/mm`、密码正确，但不是 WebSocket | 伪装页，不拉黑 |
| 其他任何路径（含 `/mm/xxx`） | 伪装页 + 拉黑 IP |
| 已被拉黑的 IP | 连接直接断开 |

拉黑记录在 `/etc/ws-tunnel/blacklist.txt`，每行一个 IP，`#` 后是时间和原因。程序每 5 秒检查一次文件，删一行即解封，不用重启。

```bash
wst bans                  # 查看拉黑的 IP
wst unban 1.2.3.4         # 解封
wst ban 1.2.3.4           # 手动拉黑
wst allow A_IP,你的IP     # 设置白名单（输入 - 清空）
wst banlog                # 拉黑日志
wst clearbans             # 清空拉黑文件
```

- 密码错误不拉黑：`/etc/ws-tunnel/ws-tunnel.conf` 里 `EXTRA="-ban-auth=false"`，再 `wst restart`。
- 完全关闭拉黑：`EXTRA="-no-ban"`。

## 7. A 服务器拉黑（可选）

A 的端口默认原样转发。开启后，浏览器或扫描器直接访问 A（如 `http://A_IP:88/admin`）也会被拉黑：

```bash
wsc filter                # 选 1：合法路径填 /mm，白名单填你的IP
```

| 访问 A 的请求 | 结果 |
|---|---|
| 路径 `/mm`、是 WebSocket | 转发给 B（密码由 B 校验） |
| 路径 `/mm`，不是 WebSocket | 伪装页，不拉黑 |
| 其他路径 | 伪装页 + 拉黑 IP |
| 非 HTTP 数据（随机字节探测） | 断开 + 拉黑 IP |

- 开启后这台 A 所有端口只接受 WebSocket 隧道流量；端口前面是其他协议时不要开。
- 拉黑文件 `/etc/ws-client/blacklist.txt`，所有端口共用。
- 管理命令和 B 一样：`wsc bans`、`wsc unban IP`、`wsc ban IP`、`wsc banlog`、`wsc clearbans`、`wsc filter off`。

## 8. 多台服务器共享黑名单（可选）

多台 A/B 共用一个 Redis，一台拉黑，其他约 60 秒内同步：

```bash
wst redis                 # B：选 1，填 REDIS_HOST / PORT / PASS / NODE_ID
wsc filter                # A：选 3
```

- 本地拉黑文件照常生效，Redis 只是多一份共享；Redis 连不上时服务照常运行，恢复后补写。
- 用脚本解封会同时删本地文件和 Redis。
- Redis 一定要设密码，不要暴露在公网。

## 9. 更新

```bash
wst update                # B
wsc update                # A
```

`update` 会同时更新脚本、程序和 systemd 服务文件，保留所有配置（端口、密码、目标、拉黑文件）。A、B 新旧版本可以混用，先更新哪台都行；更新时经过这台的连接会断几秒后自动重连。

还没有快捷命令的旧版本，先下载新脚本再更新：

```bash
curl -fLO https://github.com/acc66/ws-tunnel-pro/releases/latest/download/ws-tunnel.sh && bash ws-tunnel.sh update   # B
curl -fLO https://github.com/acc66/ws-tunnel-pro/releases/latest/download/ws-client.sh && bash ws-client.sh update   # A
```

更新后删掉 home 目录里下载的旧 `ws-*.sh`，以后只用 `wst` / `wsc`。

## 10. 进程名和快捷命令

| 名字 | 是什么 | 修改 |
|---|---|---|
| 快捷命令（默认 B `wst`、A `wsc`） | 管理脚本，输入它打开菜单 | `wst shortcut 新名字` |
| 进程名（默认 B `ws-tunnel-server-tcp`、A `ws-tunnel-client`） | 程序本身，`ps`/`top` 里显示的名字，也是 systemd 服务名 | `wst name 新名字`，`-` 恢复默认 |

两者不要起成同一个名字。进程名就是程序本身，直接输入它会在前台再启动一个程序，不是打开菜单。查看当前的名字：`wst ports` / `wsc` 菜单第一行。

## 11. 性能调优

- **内存**：程序设置了 `GOMEMLIMIT`（B 物理内存 70%，A 每端口 30%）。要手动指定，在 `/etc/ws-tunnel/ws-tunnel.conf` 或 `/etc/ws-client/端口.conf` 加 `GOMEMLIMIT="1GiB"`。正常每条用户连接约 60~70KB。
- **CPU**：主要花在内核收发包上。最有效的几项：
  1. B 到 HAProxy 走 Unix socket（第 3 节推荐写法）；
  2. HAProxy 关掉逐连接日志（`option httplog`、`http-request capture`）和 `compression`；
  3. `nbthread` 等于 CPU 核数；
  4. 不要开 `splice-auto`。
- **诊断**：B 上一键收集 CPU、网卡、连接、HAProxy 信息（只读，密码已打码）：
  ```bash
  apt-get install -y bc linux-perf
  curl -fsSL https://raw.githubusercontent.com/acc66/ws-tunnel-pro/main/diag.sh -o diag.sh && bash diag.sh
  ```
  对比优化效果时用「每万包/秒占多少 CPU」（第 2 节 CPU 合计 ÷ 第 4 节包速），流量不同时直接比 CPU 百分比不准。
- **容量参考**：2 核 4G 的 B（本机带 HAProxy）在约 1.2Gbps 双向、6000~7000 条用户连接时 CPU 约 60~70%。再高就要升配置，或把 HAProxy 拆到另一台机器。
- 密码通过环境变量传给程序，`ps` 里看不到。
- 连接目标失败时日志每 10 秒汇总一条，不会刷屏。

## 12. 常用命令速查

### B 服务器 `wst`

```bash
wst                       # 交互菜单
wst install               # 安装 / 重新配置
wst ports                 # 查看配置
wst addport 8443 | delport 8443
wst target unix:/run/haproxy-ws.sock,127.0.0.1:20001
wst addroute 8181 127.0.0.1:7789   # 8181 单独转发到 7789（不走 target），同路径同密码
wst delroute 8181
wst passwd 新密码         # A 要同步修改
wst allow A_IP,你的IP
wst log | banlog | bans
wst ban IP | unban IP | clearbans
wst redis                 # 共享 Redis
wst restart | stop | status | enable | disable
wst update | version
wst name 进程名 | shortcut 快捷名
wst uninstall             # 卸载（保留配置和拉黑文件）
```

### A 服务器 `wsc`

```bash
wsc                       # 交互菜单
wsc add 端口 ws://B_IP:8080/mm[,ws://备用B:8080/mm] 密码 [监听IP]
wsc server 端口 ws://B1:8080/mm,ws://B2:8080/mm
wsc del 端口 | list
wsc log [端口]
wsc restart | stop | start | enable | disable [端口]    # 不带端口作用于全部
wsc filter [on|off|redis]
wsc bans | ban IP | unban IP | banlog | clearbans
wsc update | version
wsc name 进程名 | shortcut 快捷名
wsc uninstall
```

## 13. 文件位置

| 服务器 | 路径 | 内容 |
|---|---|---|
| B | `/usr/local/bin/<进程名>` | 程序 |
| B | `/etc/ws-tunnel/ws-tunnel.conf` | 配置（端口、密码、目标、白名单、Redis、GOMEMLIMIT） |
| B | `/etc/ws-tunnel/blacklist.txt` | 拉黑文件 |
| B | `/etc/systemd/system/<进程名>.service` | 系统服务 |
| A | `/usr/local/bin/<进程名>` | 程序 |
| A | `/etc/ws-client/端口.conf` | 每个端口的配置 |
| A | `/etc/ws-client/filter.conf` | 拉黑配置（合法路径、白名单、Redis） |
| A | `/etc/ws-client/blacklist.txt` | 拉黑文件（所有端口共用） |
| A | `/etc/systemd/system/<进程名>@.service` | 系统服务模板 |

手动改配置文件后执行 `restart` 生效。

## 14. 常见问题

**A 日志「连接服务器失败」**
- B 服务是否在运行：`wst status`；安全组是否放行 8080。
- A 的 IP 是否被 B 拉黑：`wst bans`。A、B 密码不一致时 B 会拉黑 A，所以 A 的 IP 一定要在 B 的白名单里。

**A 日志 `bad handshake`**
- 路径或密码和 B 不一致，`wst ports` 核对。

**B 日志「目标故障」或「连接目标失败」**
- HAProxy 没运行或在 restart：`systemctl status haproxy`。目标写了两个地址时会自动切到备用。
- 填了 `unix:/run/haproxy-ws.sock`，但 HAProxy 没有 `bind unix@/run/haproxy-ws.sock`：`ls -l /run/haproxy-ws.sock` 确认。

**HAProxy 开机起不来**
- `journalctl -u haproxy -b --no-pager -o cat | grep ALERT` 看原因。
- `could not resolve address`：开机时 DNS 还没好。`defaults` 加 `default-server init-addr last,libc,none`，并检查第 1 节的 DNS。
- `cannot bind socket`：端口被占用，`ss -ltnp | grep 端口` 看是谁；不要再手动运行 `haproxy -f`。
- 改完后 `systemctl reset-failed haproxy && systemctl start haproxy`。

**自己被拉黑了**
- 换一台机器登录，`wst unban 你的IP` 再 `wst allow A_IP,你的IP`。A 上同理用 `wsc unban`。

**CPU 很高**
- 跑 `bash diag.sh`，按第 11 节逐项检查。`haproxy` 用户态 CPU 突然很高时，先确认没开 `splice-auto`。

**下载失败**
- 服务器访问不了 GitHub 时，手动下载 `ws-tunnel-server-tcp-linux-amd64` / `ws-tunnel-client-linux-amd64`，和脚本放在同一目录再安装，脚本会优先使用本地文件（必须是新版）。

**输入快捷命令却启动了一个程序**
- 快捷命令和进程名起成了同一个名字，见第 10 节，用 `cat /etc/ws-tunnel/.shortcut`（A 是 `/etc/ws-client/.shortcut`）查看真正的快捷名。
