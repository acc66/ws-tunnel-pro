#!/bin/bash
#================================================================
# ws-tunnel-client 一键安装 / 管理脚本（A 服务器）
# 每个本地端口一个独立服务实例: ws-client@端口（进程名可自定义: name 命令）
# 用法: bash ws-client.sh                    # 交互菜单
#       bash ws-client.sh install            # 安装程序
#       bash ws-client.sh add 端口 ws://B:8080/mm[,ws://备用B:8080/mm] 密码 [监听IP]
#       bash ws-client.sh server 端口 ws://B1:8080/mm,ws://B2:8080/mm   # 修改服务器/备用服务器
#       bash ws-client.sh del 端口 | list | log [端口] | restart [端口]
#                         stop [端口] | start [端口] | enable 端口 | disable 端口
#                         update | uninstall | version | shortcut [名字]   # 快捷命令，默认 wsc
#                         name 进程名   # 自定义进程名（- 恢复默认）
#================================================================

SCRIPT_VER="v1.4.0"
REPO="acc66/ws-tunnel-pro"
SCRIPT_NAME="ws-client.sh"
SHORT_DEFAULT="wsc"
CONF_DIR="/etc/ws-client"

NAME_FILE="$CONF_DIR/name"   # 自定义进程名（ps/top/systemctl 中显示的名字）

# 读取进程名: 未设置用默认名；设置后程序文件名和 systemd 服务名都用它
load_name() {
    PNAME=""; [ -s "$NAME_FILE" ] && PNAME="$(head -n1 "$NAME_FILE")"
    BIN="/usr/local/bin/${PNAME:-ws-tunnel-client}"
    SERVICE="${PNAME:-ws-client}"
    UNIT="/etc/systemd/system/${SERVICE}@.service"
}
load_name

# 进程名限制: 字母数字._- ，最长 15（Linux 进程名 comm 上限）
valid_name() { [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$ ]]; }

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
        v[0-9]*) if ver_lt "$b" "$l"; then yellow "最新版本: $l（执行 $(CMD) update 更新程序）"; else green "最新版本: $l（程序已是最新）"; fi ;;
        未安装)  yellow "最新版本: $l（执行 $(CMD) install 安装）" ;;
        *)       yellow "最新版本: $l（执行 $(CMD) update 更新程序）" ;;
    esac
    ver_lt "$SCRIPT_VER" "$l" && yellow "脚本有新版本 $l，重新下载: curl -fLO https://github.com/${REPO}/releases/latest/download/${SCRIPT_NAME}（或执行 $(CMD) update）"
    return 0
}

# ---------- 快捷命令（把脚本装到 /usr/local/bin，之后直接输入命令名即可打开菜单） ----------
SHORT_FILE="$CONF_DIR/.shortcut"
short_name() { [ -f "$SHORT_FILE" ] && cat "$SHORT_FILE" || echo ""; }
# 提示里显示的命令：有快捷命令用快捷命令，否则 bash 脚本路径
CMD() { local n; n=$(short_name); [ -n "$n" ] && [ -x "/usr/local/bin/$n" ] && echo "$n" || echo "bash $0"; }

# 把当前脚本写到 /usr/local/bin/$1（脚本不是普通文件时从 GitHub 下载）
write_script() {
    local dst="/usr/local/bin/$1" tmp="/usr/local/bin/.$1.tmp.$$"
    if [ -f "$0" ] && [ "$(readlink -f "$0")" != "$(readlink -f "$dst" 2>/dev/null)" ]; then
        cp -f "$0" "$tmp"
    elif [ -f "$0" ]; then
        return 0
    else
        curl -fsL --connect-timeout 15 -o "$tmp" "https://github.com/${REPO}/releases/latest/download/${SCRIPT_NAME}" || { rm -f "$tmp"; return 1; }
    fi
    chmod 755 "$tmp" && mv -f "$tmp" "$dst"
}

set_shortcut() {
    local n="$1" old; old=$(short_name)
    [ -z "$n" ] && { read -rp "快捷命令名 [${old:-$SHORT_DEFAULT}]: " n; n="${n:-${old:-$SHORT_DEFAULT}}"; }
    [[ "$n" =~ ^[a-zA-Z][a-zA-Z0-9_-]{0,31}$ ]] || { red "名字只能用字母、数字、_ -，且以字母开头"; return 1; }
    # 不能覆盖系统已有命令（自己装过的除外）
    local p; p=$(command -v "$n" 2>/dev/null)
    if [ -n "$p" ] && ! grep -q "$REPO" "$p" 2>/dev/null; then
        red "命令 $n 已存在（$p），换一个名字"; return 1
    fi
    write_script "$n" || { red "写入 /usr/local/bin/$n 失败"; return 1; }
    [ -n "$old" ] && [ "$old" != "$n" ] && grep -q "$REPO" "/usr/local/bin/$old" 2>/dev/null && rm -f "/usr/local/bin/$old"
    mkdir -p "$CONF_DIR"; echo "$n" > "$SHORT_FILE"
    green "快捷命令已设置：以后直接输入 $n 打开菜单（例如 $n list、$n log）"
}

# 首次安装时自动创建默认快捷命令
ensure_shortcut() {
    local n; n=$(short_name)
    [ -n "$n" ] && [ -x "/usr/local/bin/$n" ] && return 0
    set_shortcut "${n:-$SHORT_DEFAULT}" >/dev/null 2>&1 && green "快捷命令: 以后直接输入 $(short_name) 打开菜单（$(CMD) shortcut 可改名）"
    return 0
}

# 更新脚本自身（快捷命令指向的那份）
update_script() {
    local n; n=$(short_name); [ -z "$n" ] && return 0
    local tmp; tmp=$(mktemp)
    if curl -fsL --connect-timeout 15 -o "$tmp" "https://github.com/${REPO}/releases/latest/download/${SCRIPT_NAME}" && bash -n "$tmp" && grep -q "$REPO" "$tmp"; then
        chmod 755 "$tmp" && mv -f "$tmp" "/usr/local/bin/$n" && green "脚本已更新: $(grep -m1 '^SCRIPT_VER=' "/usr/local/bin/$n" | cut -d'"' -f2)"
    else
        rm -f "$tmp"; yellow "脚本更新失败（不影响程序更新）"
    fi
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
    [ -x "$BIN" ] || do_name   # 首次安装时可设置进程名
    download_bin
    mkdir -p "$CONF_DIR"; chmod 700 "$CONF_DIR"
    write_unit
    ensure_shortcut
    green "安装完成，接下来添加端口: $(CMD) add"
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
    systemctl enable --now "${SERVICE}@$p" >/dev/null 2>&1
    sleep 1
    if systemctl is-active -q "${SERVICE}@$p"; then
        green "已添加 $b:$p → $s（已开机启动）"
        open_firewall "$p"
    else
        red "启动失败，查看日志: $(CMD) log $p"
    fi
}

do_del() {
    local p="$1"; [ -z "$p" ] && { do_list; read -rp "要删除的端口: " p; }
    [ -f "$CONF_DIR/$p.conf" ] || { red "端口 $p 不存在"; return 1; }
    systemctl disable --now "${SERVICE}@$p" >/dev/null 2>&1
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
    systemctl restart "${SERVICE}@$p" && green "端口 $p → $s"
}

do_list() {
    local list; list=$(ports)
    [ -z "$list" ] && { yellow "暂无端口，使用 add 添加"; return; }
    printf "%-7s %-9s %-9s %s\n" "端口" "状态" "开机" "转发到"
    for p in $list; do
        ( . "$CONF_DIR/$p.conf"
          printf "%-7s %-9s %-9s %s\n" "$p" "$(systemctl is-active "${SERVICE}@$p")" \
              "$(systemctl is-enabled "${SERVICE}@$p" 2>/dev/null)" "$SERVER" )
    done
}

# 对单个端口或全部端口执行 systemctl 动作
each() {
    local act="$1" p="$2"
    if [ -n "$p" ]; then
        [ -f "$CONF_DIR/$p.conf" ] || { red "端口 $p 不存在"; return 1; }
        systemctl "$act" "${SERVICE}@$p" && green "$act $p 完成"
    else
        for x in $(ports); do systemctl "$act" "${SERVICE}@$x" && green "$act $x 完成"; done
    fi
}

do_log() {
    local p="$1"
    if [ -n "$p" ]; then
        journalctl -u "${SERVICE}@$p" -n 100 -f --no-pager
    else
        journalctl -u "${SERVICE}@*" -n 100 -f --no-pager
    fi
}

do_update() { ensure_shortcut; update_script; download_bin; green "当前程序版本: $(bin_ver)"; each restart; }

do_uninstall() {
    read -rp "确认卸载 ws-client（停止所有端口）？(y/N): " y; [ "$y" != "y" ] && return
    for p in $(ports); do systemctl disable --now "${SERVICE}@$p" >/dev/null 2>&1; done
    rm -f "$UNIT" "$BIN"; systemctl daemon-reload
    local n; n=$(short_name); [ -n "$n" ] && grep -q "$REPO" "/usr/local/bin/$n" 2>/dev/null && rm -f "/usr/local/bin/$n"; rm -f "$SHORT_FILE"
    green "已卸载（保留配置目录 $CONF_DIR）"
}

# 修改进程名: 停所有端口 → 改程序文件名 → 重写 systemd 模板 → 按原状态启动
do_name() {
    local n="$1"
    [ -z "$n" ] && read -rp "新进程名(字母数字._-，最长15，输入 - 恢复默认) [当前 $SERVICE]: " n
    [ -z "$n" ] && return
    if [ "$n" = "-" ]; then n=""; else valid_name "$n" || { red "进程名无效: $n"; return 1; }; fi
    local old_bin="$BIN" old_svc="$SERVICE" old_unit="$UNIT"
    local new_bin="/usr/local/bin/${n:-ws-tunnel-client}" new_unit="/etc/systemd/system/${n:-ws-client}@.service"
    [ "$new_unit" = "$old_unit" ] && { yellow "进程名未变化: $SERVICE"; return 0; }
    [ -e "$new_bin" ] && [ "$new_bin" != "$old_bin" ] && { red "$new_bin 已存在，换个名字"; return 1; }
    [ -e "$new_unit" ] && { red "$new_unit 已存在，换个名字"; return 1; }
    if [ -n "$n" ] && { systemctl cat "$n@.service" >/dev/null 2>&1 || systemctl cat "$n.service" >/dev/null 2>&1 || command -v "$n" >/dev/null; }; then
        red "系统已有同名服务或命令: $n，换个名字"; return 1
    fi
    local p en_list="" act_list=""
    for p in $(ports); do
        [ "$(systemctl is-enabled "${old_svc}@$p" 2>/dev/null)" = "enabled" ] && en_list="$en_list $p"
        systemctl is-active -q "${old_svc}@$p" && act_list="$act_list $p"
        systemctl disable --now "${old_svc}@$p" >/dev/null 2>&1
    done
    mkdir -p "$CONF_DIR"
    if [ -n "$n" ]; then echo "$n" > "$NAME_FILE"; else rm -f "$NAME_FILE"; fi
    load_name
    [ -f "$old_bin" ] && mv -f "$old_bin" "$BIN"
    if [ -f "$old_unit" ]; then
        rm -f "$old_unit"; write_unit
        for p in $en_list;  do systemctl enable "${SERVICE}@$p" >/dev/null 2>&1; done
        for p in $act_list; do systemctl start "${SERVICE}@$p"; done
    fi
    green "进程名已改为 $SERVICE（程序 $BIN，服务 ${SERVICE}@端口）"
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
        echo "13. 设置快捷命令（当前: $(short_name)）  14. 修改进程名（当前: $SERVICE）"
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
            10) do_uninstall ;; 11) do_server ;; 12) do_version ;; 13) set_shortcut ;; 14) do_name ;; 0) exit 0 ;;
        esac
    done
}

case "$1" in
    install) do_install ;; add) do_add "$2" "$3" "$4" "$5" ;; del) do_del "$2" ;; server) do_server "$2" "$3" ;;
    list) do_list ;; log) do_log "$2" ;;
    restart|stop|start|enable|disable) each "$1" "$2" ;;
    update) do_update ;; uninstall) do_uninstall ;; version|-v|--version) do_version ;; shortcut) set_shortcut "$2" ;; name) do_name "$2" ;;
    *) ensure_shortcut; menu ;;
esac
