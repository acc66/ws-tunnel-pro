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
| 共享 Redis | 直接回车跳过 | 多台 B 共用黑名单时才需要，见第 5 节 |

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

## 3. 验证

```bash
# A 服务器上
bash ws-client.sh list          # 状态应为 active
bash ws-client.sh log 1080      # 启动时列出主/备用服务器，之后每分钟输出活动连接数；服务器故障/恢复时会提示

# B 服务器上
bash ws-tunnel.sh log           # 每分钟输出 WebSocket 连接数和转发中的连接数（加 -v 可看每个连接）
```

注意：不要用浏览器或 `curl http://B_IP:8080/` 去测 B，路径不是 `/mm` 会把你自己拉黑。要测伪装页的话，先把自己的 IP 加进白名单。

## 4. 拉黑规则与管理（B 服务器）

| 请求 | 结果 |
|---|---|
| 路径是 `/mm`、密码正确、是 WebSocket | 正常转发 |
| 路径是 `/mm`，但密码错误或不是 WebSocket | 返回伪装页，不拉黑 |
| 其他任何路径（包括 `/mm/xxx`） | 返回伪装页，并拉黑 IP |
| 已被拉黑的 IP | 连接直接断开 |

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

## 5. 多台 B 共享黑名单（可选）

有多台 B 服务器时，可以让它们共用一个 Redis，一台拉黑，其他几台约 60 秒内同步生效。

```bash
bash ws-tunnel.sh redis    # 选 1，填 REDIS_HOST / PORT / PASS / NODE_ID
```

- 本地拉黑文件照常使用，Redis 只是多一份共享。
- Redis 连不上时服务照常运行，拉黑记录会在 Redis 恢复后补写进去。
- 数据格式（`bl:rank` / `bl:<ip>`）和 gotcp-filter 相同，可以共用同一个库。
- 用脚本解封时，会同时从本地文件和 Redis 里删除。

Redis 一定要设密码，不要直接暴露在公网上。

## 6. 常用命令速查

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
bash ws-client.sh uninstall
```

## 7. 文件位置

| 服务器 | 路径 | 内容 |
|---|---|---|
| B | `/usr/local/bin/ws-tunnel-server-tcp` | 程序 |
| B | `/etc/ws-tunnel/ws-tunnel.conf` | 配置（端口、密码、目标地址、Redis） |
| B | `/etc/ws-tunnel/blacklist.txt` | 拉黑文件 |
| B | `/etc/systemd/system/ws-tunnel.service` | 系统服务 |
| A | `/usr/local/bin/ws-tunnel-client` | 程序 |
| A | `/etc/ws-client/端口.conf` | 每个端口的配置 |
| A | `/etc/systemd/system/ws-client@.service` | 系统服务模板 |

手动改了配置文件后，执行 `restart` 生效。

## 8. 常见问题

**A 端日志出现“连接服务器失败”**
- 检查 B 的服务是否在运行：`bash ws-tunnel.sh status`。
- 检查安全组或防火墙有没有放行端口。
- 检查 A 的 IP 是不是被拉黑了：在 B 上执行 `bash ws-tunnel.sh bans`。

**A 端日志出现 `bad handshake`**
- 路径或密码和 B 不一致。在 B 上执行 `bash ws-tunnel.sh ports` 核对配置。

**自己被拉黑了**
- 换一台机器登录 B，执行 `bash ws-tunnel.sh unban 你的IP`，再执行 `bash ws-tunnel.sh allow 你的IP` 加白名单。

**下载失败**
- 脚本会从 GitHub Releases 下载程序。如果服务器访问不了 GitHub，可以手动下载 `ws-tunnel-server-tcp-linux-amd64` 或 `ws-tunnel-client-linux-amd64`，和脚本放在同一个目录，再运行安装，脚本会优先使用本地文件。
