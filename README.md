# WebSocket 隧道部署教程（A/B 服务器 + 自动拉黑）

## 架构

```
用户 → A 服务器（ws-tunnel-client，监听本地端口）
     → B 服务器（ws-tunnel-server-tcp，:8080/mm，校验密码）
     → 目标地址（如 127.0.0.1:20001）
```

- A 和 B 之间走 WebSocket，路径和密码都对才放行。
- 访问 B 的其他路径（`/`、`/admin`、`/wp-login.php` 等）会拿到一个假的 nginx 页面，同时这个 IP 被拉黑。
- 连接 A 的某个端口，就相当于连接 B 后面的目标地址。

下文用 `B_IP`、`你的密码` 作占位符，请换成自己的值。

## 1. 部署 B 服务器

```bash
curl -fLO https://raw.githubusercontent.com/acc66/ws-tunnel-pro/main/ws-tunnel.sh
bash ws-tunnel.sh install
```

安装时会依次询问：

| 提示 | 示例 | 说明 |
|---|---|---|
| 目标地址 | `127.0.0.1:20001` | B 收到的流量转发到哪里 |
| 监听端口 | `8080` | 多个用逗号分隔，如 `8080,8443` |
| WebSocket 路径 | `/mm` | 必须和 A 端一致 |
| 连接密码 | `你的密码` | 必须和 A 端一致 |
| 白名单IP | 你自己的公网IP | 永远不会被拉黑，建议填上 |
| 共享 Redis | 直接回车跳过 | 多台 B 共用黑名单时才需要，见第 6 节 |

装完后服务已启动并设为开机自启。云服务器记得在安全组放行监听端口。

## 2. 部署 A 服务器

```bash
curl -fLO https://raw.githubusercontent.com/acc66/ws-tunnel-pro/main/ws-client.sh
bash ws-client.sh add 1080 ws://B_IP:8080/mm 你的密码
```

有备用 B 服务器时，地址用逗号分隔，第一个是主服务器，后面的是备用：

```bash
bash ws-client.sh add 1080 ws://B_IP:8080/mm,ws://备用B_IP:8080/mm 你的密码
bash ws-client.sh server 1080 ws://B_IP:8080/mm,ws://备用B_IP:8080/mm   # 已有端口修改服务器
```

- 主服务器连不上（5 秒超时）就自动切到下一个，新连接不会失败。
- 故障服务器每 10 秒探测一次，主服务器恢复后新连接自动切回主服务器。
- 所有服务器的密码要一致。

每个端口是一个独立的 systemd 服务 `ws-client@端口`，添加后自动启动并开机自启。需要多个入口端口就多执行几次 `add`：

```bash
bash ws-client.sh add 1081 ws://B_IP:8443/mm 你的密码
bash ws-client.sh list
```

### 多路复用（减少 TCP 连接数）

A 默认把所有用户连接复用到每个 B 服务器的 4 条 WebSocket 长连接上（smux），A→B 之间只有 4 条 TCP 连接，不再是一个用户连接一条。好处是 B 的连接数、握手开销和内存都大幅下降，新连接也不用再等 TCP + WebSocket 握手。

- A 和 B 都要更新到新版才会启用；任意一端是旧版时自动退回原来的单连接模式，不影响使用。
- 调整复用条数：编辑 `/etc/ws-client/端口.conf`，例如 `EXTRA="-mux 8"`；`-mux 0` 关闭复用。
- 其他参数：`-timeout 5s`（连接超时，超时即切备用）、`-check 10s`（故障服务器探测间隔）、`-v`（输出每个连接的日志，默认关闭以减少日志量）。

## 3. 更新到新版

旧版脚本没有快捷命令，第一次更新要先下载新脚本。顺序是先更新 B，再更新 A（新 A 连旧 B 也能用，只是暂时不启用多路复用）。

B 服务器（每台都要执行）：

```bash
curl -fLO https://github.com/acc66/ws-tunnel-pro/releases/latest/download/ws-tunnel.sh
bash ws-tunnel.sh update
```

A 服务器：

```bash
curl -fLO https://github.com/acc66/ws-tunnel-pro/releases/latest/download/ws-client.sh
bash ws-client.sh update
```

更新会保留原来的配置（端口、密码、目标地址、拉黑文件），并自动创建快捷命令。之后再更新只要一条命令：

```bash
wst update    # B 服务器
wsc update    # A 服务器
```

确认版本：

```bash
wst version   # B 服务器
wsc version   # A 服务器
```

会显示脚本版本、程序版本和 GitHub 最新版本。显示"程序已是最新"就是更新成功了。

## 4. 验证

```bash
# A 服务器上
bash ws-client.sh list          # 状态应为 active
bash ws-client.sh log 1080      # 启动时列出主/备用服务器，之后每分钟输出活动连接数；服务器故障/恢复时会提示

# B 服务器上
bash ws-tunnel.sh log           # 每分钟输出 WebSocket 连接数和转发中的连接数（加 -v 可看每个连接）
```

注意：不要用浏览器或 `curl http://B_IP:8080/` 去测 B，路径不是 `/mm` 会把你自己拉黑。要测伪装页的话，先把自己的 IP 加进白名单。

## 5. 拉黑规则与管理（B 服务器）

| 请求 | 结果 |
|---|---|
| 路径是 `/mm`、密码正确、是 WebSocket | 正常转发 |
| 路径是 `/mm`，但密码错误或没带密码 | 返回伪装页，并拉黑 IP |
| 路径是 `/mm`、密码正确，但不是 WebSocket | 返回伪装页，不拉黑 |
| 其他任何路径（包括 `/mm/xxx`） | 返回伪装页，并拉黑 IP |
| 已被拉黑的 IP | 连接直接断开 |

密码错误不想拉黑：把 `/etc/ws-tunnel/ws-tunnel.conf` 里的 `EXTRA=""` 改成 `EXTRA="-ban-auth=false"`，再 `bash ws-tunnel.sh restart`。

拉黑的 IP 保存在 `/etc/ws-tunnel/blacklist.txt`，每行一个，`#` 后面是时间和原因：

```
1.2.3.4 # 2026-10-09 21:36:57 GET /wp-login.php
```

程序每 5 秒检查一次这个文件，手动删一行就是解封，加一行就是拉黑，不用重启。也可以用脚本：

```bash
bash ws-tunnel.sh bans            # 查看拉黑的 IP
bash ws-tunnel.sh unban 1.2.3.4   # 解封
bash ws-tunnel.sh ban 1.2.3.4     # 手动拉黑
bash ws-tunnel.sh clearbans       # 清空拉黑文件
bash ws-tunnel.sh allow 1.1.1.1,2.2.2.2   # 设置白名单（输入 - 清空）
bash ws-tunnel.sh banlog          # 查看拉黑日志
```

完全关闭拉黑：编辑 `/etc/ws-tunnel/ws-tunnel.conf`，把 `EXTRA=""` 改成 `EXTRA="-no-ban"`，然后执行 `bash ws-tunnel.sh restart`。

## 6. A 服务器拉黑（可选）

A 服务器的端口默认原样转发任何数据。开启反探测后，浏览器或扫描器直接访问 A（如 `http://A_IP:88/admin`）也会被拉黑：

```bash
bash ws-client.sh filter     # 选 1，填合法路径（和 B 一致，如 /mm）和自己的白名单IP
```

| 访问 A 的请求 | 结果 |
|---|---|
| 路径是 `/mm`、是 WebSocket | 转发给 B（密码由 B 校验，错误时 B 拉黑） |
| 路径是 `/mm`，但不是 WebSocket | 返回伪装页，不拉黑 |
| 其他任何路径（`/`、`/admin` 等） | 返回伪装页，并拉黑 IP |
| 不是 HTTP 的数据（随机字节探测等） | 断开，并拉黑 IP |
| 已被拉黑的 IP | 连接直接断开 |

- 开启后这台 A 所有端口都只接受 WebSocket 隧道流量。端口前面接的是 HAProxy/Xray 等其他协议时不要开。
- 拉黑文件 `/etc/ws-client/blacklist.txt`，所有端口共用，格式和 B 一样，删一行即解封。
- 想让 B 上"密码错误"拉黑的 IP 也在 A 上生效：A 和 B 配同一个 Redis（`bash ws-client.sh filter` 选 3），约 60 秒内同步。

```bash
bash ws-client.sh bans            # 查看拉黑的 IP（含共享 Redis）
bash ws-client.sh unban 1.2.3.4   # 解封（本地文件 + Redis）
bash ws-client.sh ban 1.2.3.4     # 手动拉黑
bash ws-client.sh banlog          # 查看拉黑日志
bash ws-client.sh clearbans       # 清空拉黑文件
bash ws-client.sh filter off      # 关闭过滤
```

## 7. 多台服务器共享黑名单（可选）

有多台 A/B 服务器时，可以让它们共用一个 Redis，一台拉黑，其他几台约 60 秒内同步生效（A 端用 `bash ws-client.sh filter` 选 3 配置）。

```bash
bash ws-tunnel.sh redis    # 选 1，填 REDIS_HOST / PORT / PASS / NODE_ID
```

- 本地拉黑文件照常使用，Redis 只是多一份共享。
- Redis 连不上时服务照常运行，拉黑记录会在 Redis 恢复后补写进去。
- 数据格式（`bl:rank` / `bl:<ip>`）和 gotcp-filter 相同，可以共用同一个库。
- 用脚本解封时，会同时从本地文件和 Redis 里删除。

Redis 一定要设密码，不要直接暴露在公网上。

## 8. 常用命令速查

### B 服务器 `ws-tunnel.sh`

```bash
bash ws-tunnel.sh                 # 交互菜单
bash ws-tunnel.sh addport 8443    # 添加监听端口
bash ws-tunnel.sh delport 8443    # 删除监听端口
bash ws-tunnel.sh ports           # 查看当前配置
bash ws-tunnel.sh target 1.2.3.4:20001   # 修改目标地址
bash ws-tunnel.sh passwd 新密码   # 修改密码（A 端要同步修改）
bash ws-tunnel.sh log             # 实时日志
bash ws-tunnel.sh restart | stop | status
bash ws-tunnel.sh enable | disable       # 开机启动 开/关
bash ws-tunnel.sh update          # 更新程序
bash ws-tunnel.sh version         # 查看脚本 / 程序 / 最新版本
bash ws-tunnel.sh uninstall       # 卸载（保留配置和拉黑文件）
```

### A 服务器 `ws-client.sh`

```bash
bash ws-client.sh                 # 交互菜单
bash ws-client.sh add 端口 ws://B_IP:8080/mm[,ws://备用B:8080/mm] 密码 [监听IP，默认 0.0.0.0]
bash ws-client.sh server 1080 ws://B1:8080/mm,ws://B2:8080/mm   # 修改服务器 / 备用服务器
bash ws-client.sh del 1080        # 删除端口
bash ws-client.sh list            # 查看所有端口
bash ws-client.sh log [端口]      # 实时日志，不带端口则显示全部
bash ws-client.sh restart | stop | start | enable | disable [端口]   # 不带端口则作用于全部
bash ws-client.sh update          # 更新程序并重启全部端口
bash ws-client.sh version         # 查看脚本 / 程序 / 最新版本
bash ws-client.sh uninstall
```

### 快捷命令

第一次运行脚本后会自动创建快捷命令，以后不用再输入 `bash xxx.sh`：

| 服务器 | 默认快捷命令 | 示例 |
|---|---|---|
| A | `wsc` | `wsc`（打开菜单）、`wsc list`、`wsc log 1080`、`wsc update` |
| B | `wst` | `wst`（打开菜单）、`wst bans`、`wst log`、`wst update` |

想换成自己习惯的名字（比如 `ws`）：

```bash
wsc shortcut ws     # A 服务器
wst shortcut ws     # B 服务器
```

名字不能和系统已有命令重名（比如装了 PHP 的机器上 `php` 就不能用）。`update` 会同时更新脚本和程序。

## 9. 连接数大时的调优

- A 到 B 的复用连接会按负载自动增减：平时每个服务器 4 条；每条超过 128 个连接就自动新开一条，最多 64 条；空闲下来后多开的会自动关掉。可以在 A 的 `/etc/ws-client/端口.conf` 里改，例如 `EXTRA="-mux 8 -mux-streams 64 -mux-max 128"`，再执行 `restart`。
- 程序启动时设置了 `GOMEMLIMIT`：B 按物理内存的 70%，A 每个端口按 30%。快到这个数时 GC 会回收得更积极，但它不是硬上限，不会因此杀进程。要手动指定，在配置文件（B 是 `/etc/ws-tunnel/ws-tunnel.conf`，A 是 `/etc/ws-client/端口.conf`）里加一行 `GOMEMLIMIT="1GiB"`。
- B 的服务排在 haproxy 之后启动，开机时不会刷"连接目标失败"。
- 日志里每分钟一条 `📊`，带当前连接数和内存占用。
- B 和 HAProxy 在同一台机器时，可以改走 Unix socket，跳过本机回环 TCP 协议栈，省一部分内核 CPU：
  1. 在 HAProxy 接收 B 流量的 frontend（比如 `mmk`）加一行 `bind unix@/run/haproxy-ws.sock mode 666`，执行 `haproxy -c -f /etc/haproxy/haproxy.cfg && systemctl reload haproxy`。
  2. B 上执行 `wst target unix:/run/haproxy-ws.sock,127.0.0.1:20001`。逗号后面是备用：socket 连不上（比如 HAProxy restart 的那几秒）会自动改走 TCP 20001，5 秒后再试 socket。
  3. 改回来：`wst target 127.0.0.1:20001`。
- 密码通过环境变量传给程序，`ps` 里看不到。升级后执行一次 `update`，服务文件会自动更新。
- 改 HAProxy 配置后用 `systemctl reload haproxy`，不要用 `restart`。reload 是无缝切换，restart 中间会有几秒没人监听。
- 连接目标失败时，日志每 10 秒汇总一条，不会刷屏。

## 10. 文件位置

| 服务器 | 路径 | 内容 |
|---|---|---|
| B | `/usr/local/bin/ws-tunnel-server-tcp` | 程序 |
| B | `/etc/ws-tunnel/ws-tunnel.conf` | 配置（端口、密码、目标地址、Redis） |
| B | `/etc/ws-tunnel/blacklist.txt` | 拉黑文件 |
| B | `/etc/systemd/system/ws-tunnel.service` | 系统服务 |
| A | `/usr/local/bin/ws-tunnel-client` | 程序 |
| A | `/etc/ws-client/端口.conf` | 每个端口的配置 |
| A | `/etc/ws-client/filter.conf` | 反探测拉黑配置（合法路径、白名单、Redis） |
| A | `/etc/ws-client/blacklist.txt` | 拉黑文件（所有端口共用） |
| A | `/etc/systemd/system/ws-client@.service` | 系统服务模板 |

手动改了配置文件后，执行 `restart` 生效。

## 11. 常见问题

**A 端日志出现“连接服务器失败”**
- 检查 B 的服务是否在运行：`bash ws-tunnel.sh status`。
- 检查安全组或防火墙有没有放行端口。
- 检查 A 的 IP 是不是被拉黑了：在 B 上执行 `bash ws-tunnel.sh bans`。A 和 B 的密码不一致时，B 会把 A 的 IP 拉黑，建议把 A 的 IP 加进 B 的白名单：`bash ws-tunnel.sh allow A_IP`。

**A 端日志出现 `bad handshake`**
- 路径或密码和 B 不一致。在 B 上执行 `bash ws-tunnel.sh ports` 核对配置。

**自己被拉黑了**
- 换一台机器登录 B，执行 `bash ws-tunnel.sh unban 你的IP`，再执行 `bash ws-tunnel.sh allow 你的IP` 加白名单。A 上同理：`bash ws-client.sh unban 你的IP`，再 `bash ws-client.sh filter` 选 1 填白名单。

**下载失败**
- 脚本会从 GitHub Releases 下载程序。如果服务器访问不了 GitHub，可以手动下载 `ws-tunnel-server-tcp-linux-amd64` 或 `ws-tunnel-client-linux-amd64`，和脚本放在同一个目录，再运行安装，脚本会优先使用本地文件。

**加了备用服务器后连不上**
- A 上的程序是旧版（v1.1.0 之前），不认识逗号分隔的多个地址。执行 `bash ws-client.sh version` 查看，再执行 `bash ws-client.sh update` 更新。新版脚本在 `add` / `server` 时会自动检查并更新。

**查看版本**
- `bash ws-tunnel.sh version` / `bash ws-client.sh version` 会显示脚本版本、已安装程序版本和 GitHub 最新版本。程序本身也可以用 `-version` 参数查看。
