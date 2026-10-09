#!/bin/bash
#================================================================
# ws-tunnel-server-tcp 一键安装 / 管理脚本（B 服务器）
# 用法: bash ws-tunnel.sh            # 交互菜单
#       bash ws-tunnel.sh install | addport 端口 | delport 端口 | ports
#                         target ip:port | passwd 新密码 | log | banlog
#                         bans | unban IP | ban IP | clearbans | allow IP列表 | redis
#                         restart | stop | status | enable | disable | update | uninstall
#================================================================

REPO="acc66/ws-tunnel-pro"
BIN="/usr/local/bin/ws-tunnel-server-tcp"
CONF_DIR="/etc/ws-tunnel"
CONF="$CONF_DIR/ws-tunnel.conf"
SERVICE="ws-tunnel"
UNIT="/etc/systemd/system/${SERVICE}.service"
BLFILE="$CONF_DIR/blacklist.txt"

red()   { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }
yellow(){ echo -e "\033[33m$*\033[0m"; }

[ "$(id -u)" -ne 0 ] && { red "请用 root 运行"; exit 1; }

load_conf() {
    LISTEN=":8080"; TARGET=""; WSPATH="/mm"; PASSWORD=""; ALLOW=""; EXTRA=""
    REDIS_HOST=""; REDIS_PORT="6379"; REDIS_PASS=""; NODE_ID="$(hostname)"
    [ -f "$CONF" ] && . "$CONF"
}

save_conf() {
    mkdir -p "$CONF_DIR"
    cat > "$CONF" <<EOF
# 修改后执行: bash ws-tunnel.sh restart
LISTEN="$LISTEN"
TARGET="$TARGET"
WSPATH="$WSPATH"
PASSWORD="$PASSWORD"
ALLOW="$ALLOW"
EXTRA="$EXTRA"
# 共享 Redis（可选，REDIS_HOST 留空则只用本地拉黑文件 $BLFILE）
REDIS_HOST="$REDIS_HOST"
REDIS_PORT="$REDIS_PORT"
REDIS_PASS="$REDIS_PASS"
NODE_ID="$NODE_ID"
EOF
    chmod 600 "$CONF"
}

write_unit() {
    cat > "$UNIT" <<EOF
[Unit]
Description=ws-tunnel WebSocket to TCP server with anti-probe blacklist
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
EnvironmentFile=$CONF
ExecStart=/bin/sh -c 'exec $BIN -listen "\$LISTEN" -target "\$TARGET" -path "\$WSPATH" -password "\$PASSWORD" -allow "\$ALLOW" -blacklist $BLFILE -redis-host "\$REDIS_HOST" -redis-port "\$REDIS_PORT" -redis-pass "\$REDIS_PASS" -node "\$NODE_ID" \$EXTRA'
Restart=always
RestartSec=3
LimitNOFILE=1000000

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

download_bin() {
    local arch
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) red "不支持的架构: $(uname -m)"; exit 1 ;;
    esac
    # 本地已有同目录二进制时优先使用（离线安装），但必须是支持多路复用的新版
    local here; here="$(cd "$(dirname "$0")" && pwd)/ws-tunnel-server-tcp-linux-${arch}"
    if [ -f "$here" ]; then
        if grep -q "X-Tunnel-Mux" "$here"; then
            install -m 755 "$here" "$BIN"; green "已安装本地文件 $here → $BIN"; return
        fi
        yellow "本地文件 $here 是旧版（不支持多路复用），改为从 GitHub 下载新版"
    fi
    local url="https://github.com/${REPO}/releases/latest/download/ws-tunnel-server-tcp-linux-${arch}"
    yellow "下载 $url"
    if ! curl -fL --connect-timeout 15 -o "${BIN}.new" "$url"; then
        red "下载失败（检查网络或 Release 是否已生成）"; rm -f "${BIN}.new"; exit 1
    fi
    chmod +x "${BIN}.new" && mv -f "${BIN}.new" "$BIN"
    green "已安装 $BIN"
}

valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }

open_firewall() {
    local p="$1"
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        ufw allow "$p/tcp" >/dev/null && green "ufw 已放行 $p"
    elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
        firewall-cmd --permanent --add-port="$p/tcp" >/dev/null && firewall-cmd --reload >/dev/null && green "firewalld 已放行 $p"
    fi
    yellow "云服务器请在安全组放行 TCP $p"
}

redis_cli() {
    [ -z "$REDIS_HOST" ] && return 2
    command -v redis-cli >/dev/null || { red "未安装 redis-cli（apt install redis-tools / yum install redis）"; return 1; }
    if [ -n "$REDIS_PASS" ]; then
        redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASS" --no-auth-warning "$@"
    else
        redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" "$@"
    fi
}

ask_redis() {
    read -rp "REDIS_HOST [${REDIS_HOST:-127.0.0.1}]: " rh; REDIS_HOST="${rh:-${REDIS_HOST:-127.0.0.1}}"
    read -rp "REDIS_PORT [$REDIS_PORT]: " rp; [ -n "$rp" ] && REDIS_PORT="$rp"
    read -rsp "REDIS_PASS [${REDIS_PASS:+已设置}]: " rpw; echo; [ -n "$rpw" ] && REDIS_PASS="$rpw"
    read -rp "NODE_ID [$NODE_ID]: " nid; [ -n "$nid" ] && NODE_ID="$nid"
}

# 菜单: 开启 / 修改 / 关闭共享 Redis
do_redis() {
    load_conf
    echo "当前 Redis: ${REDIS_HOST:-未启用（只用本地拉黑文件）}"
    read -rp "1=启用/修改  2=关闭  其他=返回: " n
    case "$n" in
        1) ask_redis ;;
        2) REDIS_HOST="" ;;
        *) return ;;
    esac
    save_conf; systemctl restart "$SERVICE"; green "Redis: ${REDIS_HOST:-已关闭}"
}

do_install() {
    command -v curl >/dev/null || { apt-get install -y curl 2>/dev/null || yum install -y curl; }
    load_conf
    read -rp "目标地址 ip:port [${TARGET:-无}]: " t; [ -n "$t" ] && TARGET="$t"
    [ -z "$TARGET" ] && { red "必须填写目标地址"; exit 1; }
    read -rp "监听端口(逗号分隔) [${LISTEN//:/}]: " p
    if [ -n "$p" ]; then
        LISTEN=""
        for x in ${p//,/ }; do valid_port "$x" || { red "端口无效: $x"; exit 1; }; LISTEN="${LISTEN:+$LISTEN,}:$x"; done
    fi
    read -rp "WebSocket 路径 [$WSPATH]: " v; [ -n "$v" ] && WSPATH="$v"
    read -rp "连接密码 [${PASSWORD:+已设置}]: " pw; [ -n "$pw" ] && PASSWORD="$pw"
    [ -z "$PASSWORD" ] && { red "必须设置密码"; exit 1; }
    read -rp "白名单IP(逗号分隔，可留空) [$ALLOW]: " a; [ -n "$a" ] && ALLOW="$a"

    yellow "拉黑IP保存在 $BLFILE"
    read -rp "同时启用共享 Redis 黑名单（多节点共用）？(y/N) [${REDIS_HOST:+当前 $REDIS_HOST}]: " y
    [ "$y" = "y" ] && ask_redis

    download_bin
    save_conf
    write_unit
    systemctl enable "$SERVICE" >/dev/null 2>&1
    systemctl restart "$SERVICE"
    for x in ${LISTEN//,/ }; do open_firewall "${x##*:}"; done
    sleep 1
    do_status
}

do_ports() {
    load_conf
    echo "监听端口: ${LISTEN//:/}"
    echo "WS 路径:  $WSPATH"
    echo "转发到:   $TARGET"
    echo "拉黑文件: $BLFILE（$(bl_count) 个IP）   共享Redis: ${REDIS_HOST:-未启用}"
    echo "开机启动: $(systemctl is-enabled "$SERVICE" 2>/dev/null || echo 未安装)   运行状态: $(systemctl is-active "$SERVICE" 2>/dev/null)"
}

do_addport() {
    load_conf
    local p="$1"; [ -z "$p" ] && read -rp "要添加的端口: " p
    valid_port "$p" || { red "端口无效"; return 1; }
    [[ ",$LISTEN," == *",:$p,"* ]] && { yellow "端口 $p 已存在"; return 0; }
    if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$p\$"; then
        red "端口 $p 已被其他程序占用"; return 1
    fi
    LISTEN="${LISTEN:+$LISTEN,}:$p"
    save_conf; systemctl restart "$SERVICE"
    green "已添加端口 $p，当前: ${LISTEN//:/}"
    open_firewall "$p"
}

do_delport() {
    load_conf
    local p="$1"; [ -z "$p" ] && read -rp "要删除的端口: " p
    [[ ",$LISTEN," != *",:$p,"* ]] && { yellow "端口 $p 不在列表中"; return 1; }
    local new=""
    for x in ${LISTEN//,/ }; do [ "$x" != ":$p" ] && new="${new:+$new,}$x"; done
    [ -z "$new" ] && { red "至少保留一个端口"; return 1; }
    LISTEN="$new"; save_conf; systemctl restart "$SERVICE"
    green "已删除端口 $p，当前: ${LISTEN//:/}"
}

do_target() {
    load_conf
    local t="$1"; [ -z "$t" ] && read -rp "新的目标地址 ip:port: " t
    [ -z "$t" ] && return 1
    TARGET="$t"; save_conf; systemctl restart "$SERVICE"; green "已改为转发到 $TARGET"
}

do_passwd() {
    load_conf
    local p="$1"; [ -z "$p" ] && read -rp "新密码: " p
    [ -z "$p" ] && return 1
    PASSWORD="$p"; save_conf; systemctl restart "$SERVICE"; green "密码已修改，客户端需同步修改"
}

do_allow() {
    load_conf
    local a="$1"; [ -z "$a" ] && read -rp "白名单IP(逗号分隔，输入 - 清空) [$ALLOW]: " a
    [ "$a" = "-" ] && a=""
    ALLOW="$a"; save_conf; systemctl restart "$SERVICE"; green "白名单: ${ALLOW:-无}"
}

bl_count() { [ -f "$BLFILE" ] && grep -cvE '^[[:space:]]*(#|$)' "$BLFILE" || echo 0; }

valid_ip() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ || "$1" =~ ^[0-9a-fA-F:]+$ ]]; }

do_bans() {
    load_conf
    if [ -s "$BLFILE" ]; then
        green "本地拉黑文件 $BLFILE 共 $(bl_count) 个IP（最近 50 条）："
        grep -vE '^[[:space:]]*(#|$)' "$BLFILE" | tail -50 | sed 's/^/  /'
    else
        yellow "本地拉黑文件为空"
    fi
    [ -z "$REDIS_HOST" ] && return
    local total; total=$(redis_cli ZCARD bl:rank) || { red "Redis 连接失败"; return 1; }
    echo
    green "共享 Redis 黑名单共 $total 个IP（按触发次数排序，前 50）："
    redis_cli ZREVRANGE bl:rank 0 49 WITHSCORES | paste - - | while read -r ip cnt; do
        printf "  %-40s 次数:%-5s 节点:%s\n" "$ip" "$cnt" "$(redis_cli HGET "bl:$ip" node)"
    done
}

do_unban() {
    load_conf
    local ip="$1"; [ -z "$ip" ] && read -rp "要解封的IP: " ip
    valid_ip "$ip" || { red "IP 格式无效"; return 1; }
    if [ -f "$BLFILE" ]; then
        local tmp="$BLFILE.tmp.$$"
        # 精确匹配行首IP，避免 1.2.3.4 误删 1.2.3.45
        awk -v ip="$ip" '{ split($0, a, /[ \t#]/); if (a[1] != ip) print }' "$BLFILE" > "$tmp" && mv -f "$tmp" "$BLFILE"
    fi
    if [ -n "$REDIS_HOST" ]; then
        redis_cli ZREM bl:rank "$ip" >/dev/null && redis_cli DEL "bl:$ip" >/dev/null || red "Redis 解封失败"
    fi
    green "已解封 $ip（约 5 秒内生效${REDIS_HOST:+，其他节点在下次 Redis 同步时生效}）"
}

do_ban() {
    load_conf
    local ip="$1"; [ -z "$ip" ] && read -rp "要拉黑的IP: " ip
    valid_ip "$ip" || { red "IP 格式无效"; return 1; }
    mkdir -p "$CONF_DIR"
    if grep -qE "^$ip([ \t#]|\$)" "$BLFILE" 2>/dev/null; then
        yellow "$ip 已在拉黑文件中"
    else
        echo "$ip # $(date '+%F %T') manual" >> "$BLFILE"
    fi
    if [ -n "$REDIS_HOST" ]; then
        local ts; ts=$(date +%s)
        redis_cli HINCRBY "bl:$ip" count 1 >/dev/null || { red "Redis 写入失败"; return 1; }
        redis_cli HSET "bl:$ip" ip "$ip" node "manual" last "$ts" >/dev/null
        redis_cli HSETNX "bl:$ip" first "$ts" >/dev/null
        redis_cli ZINCRBY bl:rank 1 "$ip" >/dev/null
    fi
    green "已拉黑 $ip（约 5 秒内生效）"
}

do_clearbans() {
    read -rp "确认清空本地拉黑文件 $BLFILE？(y/N): " y; [ "$y" != "y" ] && return
    : > "$BLFILE"; green "已清空（共享 Redis 中的不受影响）"
}

do_status()  { systemctl --no-pager status "$SERVICE" | head -15; }
do_restart() { systemctl restart "$SERVICE" && green "已重启"; do_status; }
do_stop()    { systemctl stop "$SERVICE" && green "已停止"; }
do_enable()  { systemctl enable "$SERVICE" && green "已开启开机启动"; }
do_disable() { systemctl disable "$SERVICE" && green "已关闭开机启动"; }
do_log()     { journalctl -u "$SERVICE" -n 100 -f --no-pager; }
do_banlog()  { journalctl -u "$SERVICE" --no-pager | grep -E "拉黑|非法访问" | tail -50; }
do_update()  { download_bin; systemctl restart "$SERVICE"; do_status; }

do_uninstall() {
    read -rp "确认卸载 ws-tunnel？(y/N): " y; [ "$y" != "y" ] && return
    systemctl disable --now "$SERVICE" 2>/dev/null
    rm -f "$UNIT" "$BIN"; systemctl daemon-reload
    green "已卸载（保留配置目录 $CONF_DIR，含拉黑文件）"
}

menu() {
    while true; do
        echo
        green "======== ws-tunnel 服务端管理 ========"
        do_ports 2>/dev/null
        echo " 1. 安装 / 重新配置      2. 添加端口"
        echo " 3. 删除端口             4. 修改目标地址"
        echo " 5. 修改密码             6. 设置白名单"
        echo " 7. 查看实时日志         8. 查看拉黑日志"
        echo " 9. 查看拉黑IP          10. 解封IP"
        echo "11. 手动拉黑IP          12. 清空拉黑文件"
        echo "13. 共享 Redis 设置     14. 重启"
        echo "15. 停止                16. 状态"
        echo "17. 开机启动 开         18. 开机启动 关"
        echo "19. 更新程序            20. 卸载"
        echo " 0. 退出"
        read -rp "选择: " n
        case "$n" in
            1) do_install ;; 2) do_addport ;; 3) do_delport ;; 4) do_target ;;
            5) do_passwd ;; 6) do_allow ;; 7) do_log ;; 8) do_banlog ;;
            9) do_bans ;; 10) do_unban ;; 11) do_ban ;; 12) do_clearbans ;;
            13) do_redis ;; 14) do_restart ;; 15) do_stop ;; 16) do_status ;;
            17) do_enable ;; 18) do_disable ;; 19) do_update ;; 20) do_uninstall ;;
            0) exit 0 ;;
        esac
    done
}

case "$1" in
    install) do_install ;; addport) do_addport "$2" ;; delport) do_delport "$2" ;;
    ports) do_ports ;; target) do_target "$2" ;; passwd) do_passwd "$2" ;; allow) do_allow "$2" ;;
    log) do_log ;; banlog) do_banlog ;; bans) do_bans ;; unban) do_unban "$2" ;; ban) do_ban "$2" ;;
    clearbans) do_clearbans ;; redis) do_redis ;;
    restart) do_restart ;; stop) do_stop ;; status) do_status ;;
    enable) do_enable ;; disable) do_disable ;;
    update) do_update ;; uninstall) do_uninstall ;; *) menu ;;
esac
