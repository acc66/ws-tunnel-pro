#!/bin/bash
#================================================================
# ws-tunnel-client 一键安装 / 管理脚本（A 服务器）
# 每个本地端口一个独立服务实例: ws-client@端口
# 用法: bash ws-client.sh                    # 交互菜单
#       bash ws-client.sh install            # 安装程序
#       bash ws-client.sh add 端口 ws://B:8080/mm[,ws://备用B:8080/mm] 密码 [监听IP]
#       bash ws-client.sh server 端口 ws://B1:8080/mm,ws://B2:8080/mm   # 修改服务器/备用服务器
#       bash ws-client.sh del 端口 | list | log [端口] | restart [端口]
#                         stop [端口] | start [端口] | enable 端口 | disable 端口
#                         update | uninstall | version
#================================================================

SCRIPT_VER="v1.2.0"
REPO="acc66/ws-tunnel-pro"
BIN="/usr/local/bin/ws-tunnel-client"
CONF_DIR="/etc/ws-client"
UNIT="/etc/systemd/system/ws-client@.service"

red()   { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }
yellow(){ echo -e "\033[33m$*\033[0m"; }

[ "$(id -u)" -ne 0 ] && { red "请用 root 运行"; exit 1; }

# ---------- 版本 ----------
# 已安装程序的版本（v1.1.0 及更早的程序不支持 -version）
bin_ver() {
    [ -x "$BIN" ] || { echo "未安装"; return; }
    local v; v=$("$BIN" -version 2>/dev/null | head -1)
    [[ "$v" =~ ^(v|dev) ]] && echo "$v" || echo "旧版(v1.1.0 或更早)"
}
# GitHub 上的最新版本（查询失败返回空）
latest_ver() {
    curl -fsL --connect-timeout 5 -m 10 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
}
# ver_lt a b：a 比 b 旧时返回 0
ver_lt() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "${1#v}" "${2#v}" | sort -V | head -1)" = "${1#v}" ]; }
do_version() {
    local b l; b=$(bin_ver); l=$(latest_ver)
    echo "脚本版本: $SCRIPT_VER"
    echo "程序版本: $b"
    if [ -z "$l" ]; then
        yellow "最新版本: 查询失败（无法访问 GitHub）"; return
    fi
    case "$b" in
        v[0-9]*) if ver_lt "$b" "$l"; then yellow "最新版本: $l（执行 bash $0 update 更新程序）"; else green "最新版本: $l（程序已是最新）"; fi ;;
        未安装)  yellow "最新版本: $l（执行 bash $0 install 安装）" ;;
        *)       yellow "最新版本: $l（执行 bash $0 update 更新程序）" ;;
    esac
    ver_lt "$SCRIPT_VER" "$l" && yellow "脚本有新版本 $l，重新下载: curl -fLO https://github.com/${REPO}/releases/latest/download/$(basename "$0")"
    return 0
}

valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }

# 校验服务器地址列表（逗号分隔，第一个为主，其余为备用）
valid_servers() {
    [ -z "$1" ] && { red "服务器地址不能为空"; return 1; }
    local x
    for x in ${1//,/ }; do
        [[ "$x" =~ ^wss?://[^[:space:]\"\']+$ ]] || { red "地址无效: $x（必须以 ws:// 或 wss:// 开头）"; return 1; }
    done
}
ports() { ls "$CONF_DIR"/*.conf 2>/dev/null | sed 's#.*/##; s#\.conf$##' | sort -n; }

write_unit() {
    cat > "$UNIT" <<EOF
[Unit]
Description=ws-tunnel client on port %i
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
EnvironmentFile=$CONF_DIR/%i.conf
ExecStart=/bin/sh -c 'exec $BIN -local "\$BIND:%i" -server "\$SERVER" -password "\$PASSWORD" \$EXTRA'
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
        i386|i686) arch="386" ;;
        *) red "不支持的架构: $(uname -m)"; exit 1 ;;
    esac
    local here; here="$(cd "$(dirname "$0")" && pwd)/ws-tunnel-client-linux-${arch}"
    if [ -f "$here" ]; then
        if grep -q "X-Tunnel-Mux" "$here"; then
            install -m 755 "$here" "$BIN"; green "已安装本地文件 $here → $BIN"; return
        fi
        yellow "本地文件 $here 是旧版（不支持多路复用/备用服务器），改为从 GitHub 下载新版"
    fi
    command -v curl >/dev/null || { apt-get install -y curl 2>/dev/null || yum install -y curl; }
    local url="https://github.com/${REPO}/releases/latest/download/ws-tunnel-client-linux-${arch}"
    yellow "下载 $url"
    if ! curl -fL --connect-timeout 15 -o "${BIN}.new" "$url"; then
        red "下载失败（检查网络或 Release 是否已生成）"; rm -f "${BIN}.new"; exit 1
    fi
    chmod +x "${BIN}.new" && mv -f "${BIN}.new" "$BIN"
    green "已安装 $BIN"
}

open_firewall() {
    local p="$1"
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        ufw allow "$p/tcp" >/dev/null && green "ufw 已放行 $p"
    elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
        firewall-cmd --permanent --add-port="$p/tcp" >/dev/null && firewall-cmd --reload >/dev/null && green "firewalld 已放行 $p"
    fi
    yellow "云服务器请在安全组放行 TCP $p"
}

do_install() {
    download_bin
    mkdir -p "$CONF_DIR"; chmod 700 "$CONF_DIR"
    write_unit
    green "安装完成，接下来添加端口: bash $0 add"
}

# 旧版程序（v1.1.0 之前）不认识逗号分隔的多个服务器，会把整串当成一个地址导致连不上
ensure_new_bin() {
    [ -x "$BIN" ] && grep -q "X-Tunnel-Mux" "$BIN" && return
    yellow "当前程序是旧版，不支持备用服务器，先更新程序..."
    download_bin
}

do_add() {
    [ -x "$BIN" ] || do_install
    [ -f "$UNIT" ] || write_unit
    ensure_new_bin
    local p="$1" s="$2" pw="$3" b="${4:-0.0.0.0}"
    [ -z "$p" ]  && read -rp "本地监听端口: " p
    valid_port "$p" || { red "端口无效"; return 1; }
    [ -f "$CONF_DIR/$p.conf" ] && { red "端口 $p 已存在，先删除再添加"; return 1; }
    if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$p\$"; then
        red "端口 $p 已被其他程序占用"; return 1
    fi
    [ -z "$s" ]  && read -rp "B 服务器地址，多个用逗号分隔，第一个为主其余为备用 (如 ws://1.2.3.4:8080/mm,ws://5.6.7.8:8080/mm): " s
    valid_servers "$s" || return 1
    [ -z "$pw" ] && read -rp "连接密码: " pw
    [ -z "$pw" ] && { red "密码不能为空"; return 1; }
    [ -z "$4" ] && { read -rp "监听IP [0.0.0.0]: " bb; [ -n "$bb" ] && b="$bb"; }
    mkdir -p "$CONF_DIR"
    cat > "$CONF_DIR/$p.conf" <<EOF
# 修改后执行: bash ws-client.sh restart $p
BIND="$b"
SERVER="$s"
PASSWORD="$pw"
EXTRA=""
EOF
    chmod 600 "$CONF_DIR/$p.conf"
    systemctl enable --now "ws-client@$p" >/dev/null 2>&1
    sleep 1
    if systemctl is-active -q "ws-client@$p"; then
        green "已添加 $b:$p → $s（已开机启动）"
        open_firewall "$p"
    else
        red "启动失败，查看日志: bash $0 log $p"
    fi
}

do_del() {
    local p="$1"; [ -z "$p" ] && { do_list; read -rp "要删除的端口: " p; }
    [ -f "$CONF_DIR/$p.conf" ] || { red "端口 $p 不存在"; return 1; }
    systemctl disable --now "ws-client@$p" >/dev/null 2>&1
    rm -f "$CONF_DIR/$p.conf"
    green "已删除端口 $p"
}

# 修改某端口的服务器地址（含备用服务器）
do_server() {
    ensure_new_bin
    local p="$1" s="$2"
    [ -z "$p" ] && { do_list; read -rp "端口: " p; }
    [ -f "$CONF_DIR/$p.conf" ] || { red "端口 $p 不存在"; return 1; }
    if [ -z "$s" ]; then
        ( . "$CONF_DIR/$p.conf"; echo "当前: $SERVER" )
        read -rp "新地址，多个用逗号分隔（第一个为主，其余为备用）: " s
    fi
    valid_servers "$s" || return 1
    local tmp="$CONF_DIR/$p.conf.tmp.$$"
    awk -v s="$s" '/^SERVER=/{print "SERVER=\"" s "\""; next} {print}' "$CONF_DIR/$p.conf" > "$tmp" \
        && chmod 600 "$tmp" && mv -f "$tmp" "$CONF_DIR/$p.conf"
    systemctl restart "ws-client@$p" && green "端口 $p → $s"
}

do_list() {
    local list; list=$(ports)
    [ -z "$list" ] && { yellow "暂无端口，使用 add 添加"; return; }
    printf "%-7s %-9s %-9s %s\n" "端口" "状态" "开机" "转发到"
    for p in $list; do
        ( . "$CONF_DIR/$p.conf"
          printf "%-7s %-9s %-9s %s\n" "$p" "$(systemctl is-active "ws-client@$p")" \
              "$(systemctl is-enabled "ws-client@$p" 2>/dev/null)" "$SERVER" )
    done
}

# 对单个端口或全部端口执行 systemctl 动作
each() {
    local act="$1" p="$2"
    if [ -n "$p" ]; then
        [ -f "$CONF_DIR/$p.conf" ] || { red "端口 $p 不存在"; return 1; }
        systemctl "$act" "ws-client@$p" && green "$act $p 完成"
    else
        for x in $(ports); do systemctl "$act" "ws-client@$x" && green "$act $x 完成"; done
    fi
}

do_log() {
    local p="$1"
    if [ -n "$p" ]; then
        journalctl -u "ws-client@$p" -n 100 -f --no-pager
    else
        journalctl -u "ws-client@*" -n 100 -f --no-pager
    fi
}

do_update() { download_bin; green "当前程序版本: $(bin_ver)"; each restart; }

do_uninstall() {
    read -rp "确认卸载 ws-client（停止所有端口）？(y/N): " y; [ "$y" != "y" ] && return
    for p in $(ports); do systemctl disable --now "ws-client@$p" >/dev/null 2>&1; done
    rm -f "$UNIT" "$BIN"; systemctl daemon-reload
    green "已卸载（保留配置目录 $CONF_DIR）"
}

menu() {
    while true; do
        echo
        green "======== ws-tunnel 客户端管理（A 服务器）  脚本 $SCRIPT_VER  程序 $(bin_ver) ========"
        do_list
        echo
        echo " 1. 安装 / 更新程序      2. 添加端口"
        echo " 3. 删除端口             4. 查看实时日志"
        echo " 5. 重启                 6. 停止"
        echo " 7. 启动                 8. 开机启动 开"
        echo " 9. 开机启动 关         10. 卸载"
        echo "11. 修改服务器地址 / 备用服务器  12. 查看版本"
        echo " 0. 退出"
        read -rp "选择: " n
        case "$n" in
            1) do_install; each restart ;; 2) do_add ;; 3) do_del ;;
            4) read -rp "端口(留空=全部): " p; do_log "$p" ;;
            5) read -rp "端口(留空=全部): " p; each restart "$p" ;;
            6) read -rp "端口(留空=全部): " p; each stop "$p" ;;
            7) read -rp "端口(留空=全部): " p; each start "$p" ;;
            8) read -rp "端口(留空=全部): " p; each enable "$p" ;;
            9) read -rp "端口(留空=全部): " p; each disable "$p" ;;
            10) do_uninstall ;; 11) do_server ;; 12) do_version ;; 0) exit 0 ;;
        esac
    done
}

case "$1" in
    install) do_install ;; add) do_add "$2" "$3" "$4" "$5" ;; del) do_del "$2" ;; server) do_server "$2" "$3" ;;
    list) do_list ;; log) do_log "$2" ;;
    restart|stop|start|enable|disable) each "$1" "$2" ;;
    update) do_update ;; uninstall) do_uninstall ;; version|-v|--version) do_version ;; *) menu ;;
esac
