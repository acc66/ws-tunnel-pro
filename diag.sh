#!/bin/bash
# B 服务器性能诊断：收集 CPU / 网卡 / ws-tunnel / haproxy 关键信息，约 15 秒
# 用法: bash diag.sh            结果保存到 /root/diag-时间.txt（密码已打码）
OUT="/root/diag-$(date +%m%d-%H%M).txt"
S=5   # 采样秒数

pid_of_unit() { systemctl show -p MainPID --value "$1" 2>/dev/null; }
# 进程 CPU(用户态/内核态) 与上下文切换采样：输出 "pid user% sys% ctxsw/s 名字"
sample_procs() {
    local hz; hz=$(getconf CLK_TCK)
    declare -A u1 s1 c1
    for p in /proc/[0-9]*; do
        read -r -a f < "$p/stat" 2>/dev/null || continue
        u1[${p#/proc/}]=${f[13]}; s1[${p#/proc/}]=${f[14]}
        c1[${p#/proc/}]=$(awk '/ctxt_switches/{s+=$2} END{print s+0}' "$p/status" 2>/dev/null)
    done
    sleep "$S"
    for id in "${!u1[@]}"; do
        read -r -a f < "/proc/$id/stat" 2>/dev/null || continue
        local c2; c2=$(awk '/ctxt_switches/{s+=$2} END{print s+0}' "/proc/$id/status" 2>/dev/null)
        local du=$(( f[13] - u1[$id] )) ds=$(( f[14] - s1[$id] ))
        [ $((du + ds)) -gt 0 ] || continue
        printf "%-7s %6.1f %6.1f %8d  %s\n" "$id" "$(echo "$du*100/$hz/$S" | bc -l)" "$(echo "$ds*100/$hz/$S" | bc -l)" \
            $(( (c2 - c1[$id]) / S )) "$(tr '\0' ' ' < /proc/$id/cmdline | cut -c1-60 | sed -E 's/(-password|-redis-pass) [^ ]+/\1 ***/g')"
    done | awk '{print $2+$3, $0}' | sort -nr | cut -d' ' -f2- | head -12
}

command -v bc >/dev/null || apt-get install -y bc >/dev/null 2>&1
NIC=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}')
WSU=$(grep -l -E 'ws-tunnel-server-tcp|/etc/ws-tunnel/ws-tunnel.conf' /etc/systemd/system/*.service 2>/dev/null | head -1 | xargs -r basename | sed 's/\.service$//')
WSP=$(pid_of_unit "$WSU"); HAP=$(pgrep -o -x haproxy)

{
echo "########## 诊断时间 $(date '+%F %T')  采样 ${S}s"
echo "===== 1. 机器"; nproc; free -m | head -2; uptime; uname -r; systemd-detect-virt 2>/dev/null
echo; echo "===== 2. 整机 CPU 分布（${S}s）"
a=($(head -1 /proc/stat)); sleep "$S"; b=($(head -1 /proc/stat))
t=0; for i in 1 2 3 4 5 6 7 8; do d[$i]=$(( b[i] - a[i] )); t=$(( t + d[i] )); done
printf "user %d%%  nice %d%%  system %d%%  idle %d%%  iowait %d%%  irq %d%%  softirq %d%%  steal %d%%\n" \
    $((d[1]*100/t)) $((d[2]*100/t)) $((d[3]*100/t)) $((d[4]*100/t)) $((d[5]*100/t)) $((d[6]*100/t)) $((d[7]*100/t)) $((d[8]*100/t))
echo; echo "===== 3. 各进程 CPU（${S}s）：PID 用户态% 内核态% 切换/秒 命令"; sample_procs
echo; echo "===== 4. 网卡 $NIC（${S}s）"
r1=$(cat /sys/class/net/$NIC/statistics/rx_packets); t1=$(cat /sys/class/net/$NIC/statistics/tx_packets)
rb1=$(cat /sys/class/net/$NIC/statistics/rx_bytes); tb1=$(cat /sys/class/net/$NIC/statistics/tx_bytes)
n1=$(awk '/NET_RX/{for(i=2;i<=NF;i++) printf "%s ",$i}' /proc/softirqs); sleep "$S"
r2=$(cat /sys/class/net/$NIC/statistics/rx_packets); t2=$(cat /sys/class/net/$NIC/statistics/tx_packets)
rb2=$(cat /sys/class/net/$NIC/statistics/rx_bytes); tb2=$(cat /sys/class/net/$NIC/statistics/tx_bytes)
n2=$(awk '/NET_RX/{for(i=2;i<=NF;i++) printf "%s ",$i}' /proc/softirqs)
echo "收 $(( (r2-r1)/S )) 包/s $(( (rb2-rb1)*8/S/1000000 )) Mbps   发 $(( (t2-t1)/S )) 包/s $(( (tb2-tb1)*8/S/1000000 )) Mbps"
echo "平均包大小: 收 $(( (rb2-rb1)/((r2-r1)+1) )) 字节  发 $(( (tb2-tb1)/((t2-t1)+1) )) 字节"
paste <(echo $n1 | tr ' ' '\n') <(echo $n2 | tr ' ' '\n') | awk '{printf "CPU%d NET_RX软中断 %d/s\n", NR-1, ($2-$1)/'"$S"'}'
echo "队列: rx=$(ls -d /sys/class/net/$NIC/queues/rx-* | wc -l) tx=$(ls -d /sys/class/net/$NIC/queues/tx-* | wc -l)  驱动: $(ethtool -i $NIC 2>/dev/null | awk '/driver/{print $2}')"
echo "RPS: $(cat /sys/class/net/$NIC/queues/rx-0/rps_cpus 2>/dev/null)"
grep -E "$NIC|ena|virtio|eth" /proc/interrupts | awk '{printf "%s ", $1; for(i=2;i<=NF-1;i++) if($i ~ /^[0-9]+$/) printf "%s ", $i; print $NF}'
echo; echo "===== 5. ws-tunnel（服务 $WSU，PID $WSP）"
if [ -n "$WSP" ] && [ "$WSP" != 0 ]; then
    BIN=$(readlink /proc/$WSP/exe); echo "程序 $BIN  版本 $($BIN -version 2>/dev/null)"
    grep -E 'VmRSS|Threads' /proc/$WSP/status; echo "fd 数: $(ls /proc/$WSP/fd | wc -l)"
    systemctl cat "$WSU" | grep -E '^(Environment|After|LimitNOFILE)'
    sed -E 's/^(PASSWORD|REDIS_PASS)=.*/\1=***/' /etc/ws-tunnel/ws-tunnel.conf 2>/dev/null | grep -v '^#'
    journalctl -u "$WSU" --no-pager | grep '📊' | tail -5
    echo "最近 10 分钟日志行数: $(journalctl -u "$WSU" --since '-10 min' --no-pager | wc -l)"
    journalctl -u "$WSU" --since '-10 min' --no-pager -o cat | sed -E 's/[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(:[0-9]+)?/IP/g; s/^[0-9/]+ [0-9:]+ //' | sort | uniq -c | sort -rn | head -8
else echo "未找到 ws-tunnel 服务"; fi
echo; echo "===== 6. 连接"
ss -s | head -3
echo "按本地端口（ESTAB 前 8）:"; ss -Htn state established | awk '{n=split($3,a,":"); print a[n]}' | sort | uniq -c | sort -rn | head -8
echo "按对端端口（ESTAB 前 8）:"; ss -Htn state established | awk '{n=split($4,a,":"); print a[n]}' | sort | uniq -c | sort -rn | head -8
echo "发送队列 >64KB 的连接: $(ss -Htn state established | awk '$2>65536' | wc -l)   接收队列 >64KB: $(ss -Htn state established | awk '$1>65536' | wc -l)"
echo; echo "===== 7. haproxy（PID $HAP）"
haproxy -v 2>/dev/null | head -1; [ -n "$HAP" ] && grep -E 'VmRSS|Threads' /proc/$HAP/status
grep -nE '^\s*(nbthread|cpu-map|maxconn|mode|timeout|option|compression|log|bind|http-request capture|tune\.)' /etc/haproxy/haproxy.cfg | head -50
echo "haproxy 最近 1 分钟日志行数: $(journalctl -u haproxy --since '-1 min' --no-pager | wc -l)  rsyslog: $(systemctl is-active rsyslog 2>/dev/null)"
command -v socat >/dev/null && echo "show info" | socat stdio unix-connect:/var/lib/haproxy/stats 2>/dev/null | grep -E '^(Nbthread|CurrConns|ConnRate|SessRate|Idle_pct|Run_queue|Tasks):'
echo; echo "===== 8. nginx"
grep -rhE '^\s*(worker_processes|worker_connections|access_log|proxy_buffering|listen)' /etc/nginx/ 2>/dev/null | sort | uniq -c | head -15
echo; echo "===== 9. 内核网络参数"
sysctl -n net.core.somaxconn net.ipv4.tcp_rmem net.ipv4.tcp_wmem net.ipv4.ip_local_port_range net.ipv4.tcp_tw_reuse net.ipv4.tcp_congestion_control 2>/dev/null
echo "conntrack: $(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo 未启用)/$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null)"
echo "iptables 规则数: $(iptables-save 2>/dev/null | grep -c '^-A')  nft 行数: $(nft list ruleset 2>/dev/null | wc -l)"
echo "docker 容器: $(docker ps -q 2>/dev/null | wc -l)"
echo; echo "===== 10. 内核热点函数（10s，需 perf）"
if command -v perf >/dev/null; then
    perf record -a -g -o /tmp/perf.data -- sleep 10 >/dev/null 2>&1
    perf report -i /tmp/perf.data --stdio --no-children --sort comm,symbol 2>/dev/null | grep -E '^\s+[0-9.]+%' | head -30; rm -f /tmp/perf.data
else echo "未安装 perf（可选: apt-get install -y linux-perf 后重跑）"; fi
} > "$OUT" 2>&1
echo "完成，结果: $OUT"; cat "$OUT"
