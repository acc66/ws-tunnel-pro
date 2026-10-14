package main

// A 服务器：本地 TCP → WebSocket 隧道客户端
// - 多路复用（-mux N）：所有本地连接复用 N 条 WebSocket 长连接（smux），大幅减少到 B 的 TCP 连接数
//   B 端是旧版（不支持多路复用）时自动退回“一个连接一条 WebSocket”
// - 备用服务器：-server 用逗号分隔多个地址，按顺序优先；当前服务器失败自动切到下一个，
//   主服务器恢复后新连接自动切回
// - 反探测拉黑（-path /mm 开启）：本地端口收到的首个 HTTP 请求路径不是 -path、或不是 HTTP 数据
//   → 拉黑IP（追加到 -blacklist 文件）+ 返回 nginx 蜜罐页；黑名单IP连接时直接断开
//   路径正确但不是 WebSocket → 只返回蜜罐页；非 HTTP 数据 → 断开（-ban-invalid 时也拉黑）
//   拉黑文件每行一个IP（# 后为注释），被修改后 5 秒内自动重新加载，多个端口实例共用同一文件
//   配置 -redis-host 后黑名单与 Redis bl:rank 共享（与 B 服务器 ws-tunnel-server-tcp 同结构，A/B 互通）

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
)

// version 发布时由 -ldflags "-X main.version=vX.Y.Z" 注入
var version = "dev"

var (
	// 命令行参数
	localAddr  = flag.String("local", "127.0.0.1:1080", "本地监听地址")
	serverAddr = flag.String("server", "ws://2.2.2.2:8080/mm", "WebSocket 服务器地址，多个用逗号分隔（第一个为主，其余为备用）")
	password   = flag.String("password", "your_secret_password", "连接密码")
	useTLS     = flag.Bool("tls", false, "使用 WSS (TLS)")
	muxConns   = flag.Int("mux", 4, "每个服务器至少复用的 WebSocket 连接数（0=关闭多路复用，每个连接单独一条 WebSocket）")
	muxStreams = flag.Int("mux-streams", 128, "每条复用 WebSocket 最多承载的连接数，超过自动新开（0=不限）")
	muxMax     = flag.Int("mux-max", 64, "每个服务器最多的复用 WebSocket 数")
	dialTO     = flag.Duration("timeout", 5*time.Second, "连接服务器超时（超时即切换备用服务器）")
	checkEvery = flag.Duration("check", 10*time.Second, "故障服务器的探测间隔（恢复后自动切回）")
	verbose    = flag.Bool("v", false, "输出每个连接的日志（连接数大时建议关闭）")
	showVer    = flag.Bool("version", false, "显示版本号")

	// 反探测拉黑（-path 为空则关闭，原样转发任何数据）
	filterPath = flag.String("path", "", "合法 WebSocket 路径（如 /mm）；设置后路径不对即拉黑IP，留空关闭过滤（原样转发）")
	blFile     = flag.String("blacklist", "/etc/ws-client/blacklist.txt", "本地拉黑文件（每行一个IP）")
	allowList  = flag.String("allow", "", "永不拉黑的IP，逗号分隔（自己的测试IP等）")
	noBan      = flag.Bool("no-ban", false, "只返回蜜罐页，不拉黑")
	banInvalid = flag.Bool("ban-invalid", true, "非 HTTP 数据（GFW 随机字节探测等）也拉黑（ws-client-filter-v1）")
	redisHost  = flag.String("redis-host", "", "共享 Redis 地址（留空则只用本地拉黑文件；与 B 服务器共用同一个库即共享黑名单）")
	redisPort  = flag.String("redis-port", "6379", "Redis 端口")
	redisPass  = flag.String("redis-pass", "", "Redis 密码")
	nodeID     = flag.String("node", "", "节点标识（默认主机名）")
	syncEvery  = flag.Duration("sync", 60*time.Second, "从 Redis 同步黑名单间隔")
)

const (
	muxHeader    = "X-Tunnel-Mux"
	muxVersion   = "smux2"
	pingInterval = 30 * time.Second
	idleTimeout  = 90 * time.Second
)

var (
	bufPool  = sync.Pool{New: func() any { b := make([]byte, 16*1024); return &b }} // 只给 TCP→隧道 方向用
	wbufPool = &sync.Pool{}

	activeConns int64
)

func vlog(format string, v ...any) {
	if *verbose {
		log.Printf(format, v...)
	}
}

// ==================== 服务器（含备用） ====================

type upstream struct {
	url string

	mu    sync.Mutex
	down  bool // 最近一次连接失败
	noMux bool // B 端是旧版，不支持多路复用
	slots []*muxSlot
	next  int
}

var upstreams []*upstream

func dialer() *websocket.Dialer {
	return &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: *dialTO,
		ReadBufferSize:   16 * 1024,
		WriteBufferSize:  16 * 1024,
		WriteBufferPool:  wbufPool,
		NetDialContext:   (&net.Dialer{Timeout: *dialTO, KeepAlive: 30 * time.Second}).DialContext,
	}
}

// dialWS 建立一条 WebSocket；mux=true 时请求多路复用，返回值 muxOK 表示服务端是否同意
func (u *upstream) dialWS(mux bool) (ws *websocket.Conn, muxOK bool, err error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+*password)
	if mux {
		h.Set(muxHeader, muxVersion)
	}
	ws, resp, err := dialer().Dial(u.url, h)
	if err != nil {
		return nil, false, err
	}
	return ws, resp.Header.Get(muxHeader) == muxVersion, nil
}

func (u *upstream) setDown(down bool, err error) {
	u.mu.Lock()
	changed := u.down != down
	u.down = down
	u.mu.Unlock()
	if !changed {
		return
	}
	if down {
		log.Printf("❌ 服务器故障 %s: %v", u.url, err)
	} else {
		log.Printf("✅ 服务器恢复 %s", u.url)
	}
}

func (u *upstream) isDown() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.down
}

// open 在该服务器上打开一条隧道
func (u *upstream) open() (io.ReadWriteCloser, error) {
	u.mu.Lock()
	useMux := *muxConns > 0 && !u.noMux
	u.mu.Unlock()
	if !useMux {
		ws, _, err := u.dialWS(false)
		if err != nil {
			return nil, err
		}
		return newWSConn(ws), nil
	}
	// 会话可能刚好被回收或断开，换一个会话重试，避免误判服务器故障
	var lastErr error
	for try := 0; try < 3; try++ {
		sess, err := u.session()
		if err != nil {
			return nil, err
		}
		if sess == nil { // 服务端不支持多路复用，已退回单连接模式
			return u.open()
		}
		st, err := sess.OpenStream()
		if err == nil {
			return st, nil
		}
		sess.Close()
		lastErr = err
	}
	return nil, lastErr
}

// muxSlot 一条复用 WebSocket。同一槽位同时只拨号一次，其他请求等待结果（防止瞬间大量连接时一起拨号）
type muxSlot struct {
	sess    *smux.Session
	dialing chan struct{} // 非 nil 表示正在拨号，关闭即完成
	err     error         // 最近一次拨号的错误
}

// session 取一个可用的 smux 会话：
// 已连好且未满（流数 < -mux-streams）的会话中挑流数最少的；
// 都满了就新开一个槽位（最多 -mux-max 个）；空闲多出来的会话由 trimSlots 回收
func (u *upstream) session() (*smux.Session, error) {
	u.mu.Lock()
	if len(u.slots) < *muxConns {
		for len(u.slots) < *muxConns {
			u.slots = append(u.slots, &muxSlot{})
		}
	}
	// 优先挑已连好且流数最少的槽位，避免某个槽位断开时新连接都卡在它的重连上
	var best *smux.Session
	for k := 0; k < len(u.slots); k++ {
		s := u.slots[(u.next+k)%len(u.slots)]
		if s.sess == nil || s.sess.IsClosed() {
			continue
		}
		if best == nil || s.sess.NumStreams() < best.NumStreams() {
			best = s.sess
		}
	}
	if best != nil && (*muxStreams <= 0 || best.NumStreams() < *muxStreams) {
		u.next++
		u.mu.Unlock()
		return best, nil
	}
	// 没有可用会话：先找空槽位（未连或已断开）；都在用且都满了就扩容
	var sl *muxSlot
	for k := 0; k < len(u.slots); k++ {
		s := u.slots[(u.next+k)%len(u.slots)]
		if s.sess == nil || s.sess.IsClosed() {
			sl = s
			u.next += k + 1
			break
		}
	}
	if sl == nil {
		if len(u.slots) >= *muxMax {
			// 已达上限：在最少的会话上继续开流
			u.mu.Unlock()
			return best, nil
		}
		sl = &muxSlot{}
		u.slots = append(u.slots, sl)
		vlog("📈 %s 复用连接扩容到 %d 条", u.url, len(u.slots))
	}
	if ch := sl.dialing; ch != nil {
		u.mu.Unlock()
		<-ch
		u.mu.Lock()
		sess, err := sl.sess, sl.err
		u.mu.Unlock()
		if sess == nil && err == nil {
			return nil, nil // 服务端不支持多路复用
		}
		return sess, err
	}
	ch := make(chan struct{})
	sl.dialing = ch
	u.mu.Unlock()

	// 拨号不持锁，避免一个慢连接阻塞所有新连接
	sess, err := u.dialSession()

	u.mu.Lock()
	sl.sess, sl.err, sl.dialing = sess, err, nil
	u.mu.Unlock()
	close(ch)
	return sess, err
}

// dialSession 建一条复用 WebSocket；服务端不支持复用时返回 (nil, nil)
func (u *upstream) dialSession() (*smux.Session, error) {
	ws, ok, err := u.dialWS(true)
	if err != nil {
		return nil, err
	}
	if !ok {
		ws.Close()
		u.mu.Lock()
		if !u.noMux {
			log.Printf("⚠️  %s 不支持多路复用（旧版服务端），退回单连接模式", u.url)
		}
		u.noMux = true
		u.mu.Unlock()
		return nil, nil
	}
	sess, err := smux.Client(newWSConn(ws), smuxConfig())
	if err != nil {
		ws.Close()
		return nil, err
	}
	vlog("🔗 新建复用连接 %s", u.url)
	return sess, nil
}

// probe 探测故障服务器，成功即恢复
func (u *upstream) probe() {
	ws, ok, err := u.dialWS(*muxConns > 0)
	if err != nil {
		return
	}
	ws.Close()
	u.mu.Lock()
	u.noMux = *muxConns > 0 && !ok
	u.mu.Unlock()
	u.setDown(false, nil)
}

// copyStream 单向拷贝：smux 流自带缓冲，直接 WriteTo，不占拷贝缓冲；
// 其他方向用池里的 16KB 缓冲，并屏蔽 WriterTo/ReaderFrom，避免不同 Go 版本下内部再分配 32KB
func copyStream(dst io.Writer, src io.Reader) (int64, error) {
	if st, ok := src.(*smux.Stream); ok {
		return st.WriteTo(dst)
	}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *bp)
}

func smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxReceiveBuffer = 4 * 1024 * 1024 // 每个会话的接收窗口：后端变慢时最多积压这么多
	c.MaxStreamBuffer = 512 * 1024       // 单条流窗口：约 10MB/s@50ms RTT，够用且不至于一条流占满会话
	return c
}

// openTunnel 按优先级尝试所有服务器：先跳过故障的，全部故障时再逐个硬试
func openTunnel() (io.ReadWriteCloser, *upstream, error) {
	var lastErr error
	for pass := 0; pass < 2; pass++ {
		for _, u := range upstreams {
			if u.isDown() != (pass == 1) {
				continue
			}
			c, err := u.open()
			if err == nil {
				u.setDown(false, nil)
				return c, u, nil
			}
			lastErr = err
			u.setDown(true, err)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用服务器")
	}
	return nil, nil, lastErr
}

func checkLoop() {
	for {
		time.Sleep(*checkEvery)
		for _, u := range upstreams {
			if u.isDown() {
				go u.probe()
			}
			u.trimSlots()
		}
	}
}

// trimSlots 扩容出来的槽位（超过 -mux 的部分）空闲（无流）就关闭回收，已断开的直接移除
func (u *upstream) trimSlots() {
	u.mu.Lock()
	var closing []*smux.Session
	keep := u.slots[:0]
	for i, s := range u.slots {
		extra := i >= *muxConns
		switch {
		case !extra || s.dialing != nil:
			keep = append(keep, s)
		case s.sess == nil || s.sess.IsClosed():
			// 已断开的扩容槽位直接丢弃
		case s.sess.NumStreams() == 0:
			closing = append(closing, s.sess)
		default:
			keep = append(keep, s)
		}
	}
	for i := len(keep); i < len(u.slots); i++ {
		u.slots[i] = nil
	}
	shrunk := len(keep) != len(u.slots)
	u.slots = keep
	n := len(keep)
	u.mu.Unlock()
	for _, s := range closing {
		s.Close()
	}
	if shrunk {
		vlog("📉 %s 复用连接回收到 %d 条", u.url, n)
	}
}

// ==================== WebSocket → io.ReadWriteCloser ====================

// wsConn 把 WebSocket 包装成字节流：每次 Write 一个二进制消息，Read 流式读取消息体（不整条分配内存）
// 带 ping/pong 心跳和读超时，对端掉线（断电、NAT 超时）时能及时回收连接
type wsConn struct {
	ws   *websocket.Conn
	r    io.Reader
	wm   sync.Mutex
	once sync.Once
	stop chan struct{}
}

func newWSConn(ws *websocket.Conn) *wsConn {
	c := &wsConn{ws: ws, stop: make(chan struct{})}
	ws.SetReadDeadline(time.Now().Add(idleTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(idleTimeout)) })
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
					c.Close()
					return
				}
			}
		}
	}()
	return c
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.r == nil {
			t, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			c.ws.SetReadDeadline(time.Now().Add(idleTimeout))
			if t != websocket.BinaryMessage {
				continue
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wm.Lock()
	defer c.wm.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(idleTimeout))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	c.once.Do(func() { close(c.stop) })
	return c.ws.Close()
}

// ==================== 主流程 ====================

func main() {
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}

	for _, s := range strings.Split(*serverAddr, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if *useTLS && strings.HasPrefix(s, "ws://") {
			s = "wss://" + strings.TrimPrefix(s, "ws://")
		}
		upstreams = append(upstreams, &upstream{url: s})
	}
	if len(upstreams) == 0 {
		log.Fatal("未配置服务器地址")
	}

	log.Printf("🚀 WebSocket 隧道客户端启动 %s", version)
	log.Printf("📍 本地监听: %s", *localAddr)
	for i, u := range upstreams {
		role := "主"
		if i > 0 {
			role = "备用"
		}
		log.Printf("🌐 %s服务器: %s", role, u.url)
	}
	if *muxMax < *muxConns {
		*muxMax = *muxConns
	}
	if *muxConns > 0 {
		if *muxStreams > 0 {
			log.Printf("🔀 多路复用: 每个服务器 %d～%d 条 WebSocket，每条最多 %d 个连接", *muxConns, *muxMax, *muxStreams)
		} else {
			log.Printf("🔀 多路复用: 每个服务器 %d 条 WebSocket", *muxConns)
		}
	} else {
		log.Printf("🔀 多路复用: 关闭")
	}

	if *filterPath != "" {
		bl = newBlacklist(*allowList)
		if !*noBan {
			if err := os.MkdirAll(filepath.Dir(*blFile), 0755); err != nil {
				log.Printf("[WARN] 创建目录失败: %v", err)
			}
		}
		bl.reloadFile(true)
		go watchFile()
		if *nodeID == "" {
			*nodeID, _ = os.Hostname()
		}
		if *redisHost != "" && !*noBan {
			go syncLoop()
		}
		switch {
		case *noBan:
			log.Printf("🛡️  反探测: 合法路径 %s，拉黑已关闭（仅蜜罐页）", *filterPath)
		case *redisHost != "":
			log.Printf("🛡️  反探测: 合法路径 %s，拉黑文件 %s + 共享 Redis %s:%s（节点 %s）", *filterPath, *blFile, *redisHost, *redisPort, *nodeID)
		default:
			log.Printf("🛡️  反探测: 合法路径 %s，拉黑文件 %s", *filterPath, *blFile)
		}
	}

	listener, err := net.Listen("tcp", *localAddr)
	if err != nil {
		log.Fatal("监听失败:", err)
	}
	defer listener.Close()

	go checkLoop()
	go statsLoop()

	log.Printf("✅ 等待连接...")

	var delay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			// 文件句柄耗尽等临时错误：退避，避免死循环刷日志占满 CPU
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			log.Printf("❌ 接受连接失败: %v（%v 后重试）", err, delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		go handleConnection(conn)
	}
}

func statsLoop() {
	var last int64 = -1
	for {
		time.Sleep(60 * time.Second)
		if n := atomic.LoadInt64(&activeConns); n != last {
			log.Printf("📊 当前活动连接: %d，内存: %s", n, memUsage())
			last = n
		}
	}
}

func handleConnection(localConn net.Conn) {
	defer localConn.Close()
	atomic.AddInt64(&activeConns, 1)
	defer atomic.AddInt64(&activeConns, -1)

	if tc, ok := localConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	// 反探测过滤：先读首个请求头，不合法就拉黑 / 返回蜜罐，合法才建隧道
	var src io.ReadWriteCloser = localConn
	var head []byte
	if *filterPath != "" {
		var ok bool
		if head, src, ok = filterRequest(localConn); !ok {
			return
		}
	}

	tunnel, u, err := openTunnel()
	if err != nil {
		log.Printf("❌ 连接服务器失败: %v", err)
		return
	}
	defer tunnel.Close()
	vlog("🔗 %s → %s", localConn.RemoteAddr(), u.url)

	// 回放已读的请求头（缓冲区里剩余的数据由 src 继续读出）
	if len(head) > 0 {
		if _, err := tunnel.Write(head); err != nil {
			return
		}
	}
	pipe(src, tunnel)
	vlog("🔌 连接关闭: %s", localConn.RemoteAddr())
}

// pipe 双向转发，任意一端结束即关闭两端
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		copyStream(dst, src)
		dst.Close()
		src.Close()
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// ==================== 反探测拉黑 ====================

const (
	headerTimeout = 10 * time.Second
	maxHeaderSize = 16 * 1024

	honeypotHTML = `<!DOCTYPE html><html><head><title>Welcome to nginx!</title><style>body{width:35em;margin:0 auto;font-family:Tahoma,Verdana,Arial,sans-serif}</style></head><body><h1>Welcome to nginx!</h1><p>If you see this page, the nginx web server is successfully installed and working. Further configuration is required.</p><p>For online documentation and support please refer to <a href="http://nginx.org/">nginx.org</a>.</p><p><em>Thank you for using nginx.</em></p></body></html>`
)

var bl *blacklist

type blacklist struct {
	mu      sync.RWMutex
	ips     map[string]struct{} // 本地拉黑文件中的
	remote  map[string]struct{} // Redis 中的
	pending map[string]struct{} // 写 Redis 失败、待重试的
	allow   map[string]struct{}

	fmu     sync.Mutex // 串行化文件读写
	modTime time.Time
	size    int64
}

func newBlacklist(allow string) *blacklist {
	b := &blacklist{ips: map[string]struct{}{}, remote: map[string]struct{}{}, pending: map[string]struct{}{}, allow: map[string]struct{}{}}
	for _, ip := range strings.Split(allow, ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			b.allow[normalizeIP(ip)] = struct{}{}
		}
	}
	return b
}

func (b *blacklist) has(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, ok := b.allow[ip]; ok {
		return false
	}
	if _, ok := b.ips[ip]; ok {
		return true
	}
	_, ok := b.remote[ip]
	return ok
}

func (b *blacklist) markPending(ip string, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if failed {
		b.pending[ip] = struct{}{}
	} else {
		delete(b.pending, ip)
	}
}

func (b *blacklist) pendingList() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.pending))
	for ip := range b.pending {
		out = append(out, ip)
	}
	return out
}

// replaceRemote 用 Redis 数据整体替换（Redis 中删除的IP即视为解封，本地文件中的不受影响）
func (b *blacklist) replaceRemote(redisIPs []string) int {
	next := make(map[string]struct{}, len(redisIPs))
	for _, ip := range redisIPs {
		next[normalizeIP(ip)] = struct{}{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remote = next
	return len(next)
}

// add 返回 true 表示是新拉黑的IP（已追加到拉黑文件）
func (b *blacklist) add(ip, reason string) bool {
	b.mu.Lock()
	if _, ok := b.allow[ip]; ok {
		b.mu.Unlock()
		return false
	}
	if _, ok := b.ips[ip]; ok {
		b.mu.Unlock()
		return false
	}
	b.ips[ip] = struct{}{}
	b.mu.Unlock()

	b.fmu.Lock()
	defer b.fmu.Unlock()
	f, err := os.OpenFile(*blFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[WARN] 写入拉黑文件失败: %v", err)
		return true
	}
	// 注释里不能带换行，避免请求里的特殊字符破坏文件格式
	reason = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, reason)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	fmt.Fprintf(f, "%s # %s %s\n", ip, time.Now().Format("2006-01-02 15:04:05"), reason)
	f.Close()
	if st, err := os.Stat(*blFile); err == nil {
		b.modTime, b.size = st.ModTime(), st.Size() // 自己写的不触发重载
	}
	return true
}

// reloadFile 文件被外部修改（手动删行解封 / 手动加IP / 其他端口实例拉黑）时重新加载
func (b *blacklist) reloadFile(force bool) {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	st, err := os.Stat(*blFile)
	if err != nil {
		if os.IsNotExist(err) {
			b.mu.Lock()
			n := len(b.ips)
			b.ips = map[string]struct{}{}
			b.mu.Unlock()
			if n > 0 {
				log.Printf("拉黑文件已删除，黑名单清空")
			}
			b.modTime, b.size = time.Time{}, 0
		}
		return
	}
	if !force && st.ModTime().Equal(b.modTime) && st.Size() == b.size {
		return
	}
	f, err := os.Open(*blFile)
	if err != nil {
		log.Printf("[WARN] 读取拉黑文件失败: %v", err)
		return
	}
	defer f.Close()
	next := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if fields := strings.Fields(line); len(fields) > 0 {
			next[normalizeIP(fields[0])] = struct{}{}
		}
	}
	b.mu.Lock()
	b.ips = next
	b.mu.Unlock()
	b.modTime, b.size = st.ModTime(), st.Size()
	log.Printf("已加载拉黑文件 %s，共 %d 个IP", *blFile, len(next))
}

func watchFile() {
	for {
		time.Sleep(5 * time.Second)
		bl.reloadFile(false)
	}
}

func normalizeIP(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return strings.TrimSpace(s)
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return normalizeIP(host)
}

func ban(ip, reason string) {
	if *noBan || !bl.add(ip, reason) {
		return
	}
	log.Printf("🚫 拉黑 %s（%s）", ip, reason)
	if *redisHost == "" {
		return
	}
	go func() {
		if err := redisBan(ip); err != nil {
			log.Printf("[WARN] 写入 Redis 失败 %s: %v（稍后重试）", ip, err)
			bl.markPending(ip, true)
		}
	}()
}

// ==================== 极简 Redis 客户端（RESP，无第三方依赖，与 B 服务器相同） ====================

const (
	redisTimeout = 3 * time.Second

	// 与 ws-tunnel-server-tcp.go 保持一致：bl:<ip> 记录详情，bl:rank 按触发次数排序
	banLua = `local ip=ARGV[1] local node=ARGV[2] local ts=ARGV[3] local key="bl:"..ip local count=redis.call("HINCRBY",key,"count",1) redis.call("HSET",key,"ip",ip,"node",node,"last",ts) if count==1 then redis.call("HSET",key,"first",ts) end redis.call("ZADD","bl:rank",count,ip) return count`
)

func redisDo(args ...string) (interface{}, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(*redisHost, *redisPort), redisTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(redisTimeout * 3))
	r := bufio.NewReader(conn)
	if *redisPass != "" {
		if _, err := redisCmd(conn, r, "AUTH", *redisPass); err != nil {
			return nil, fmt.Errorf("AUTH: %w", err)
		}
	}
	return redisCmd(conn, r, args...)
}

func redisCmd(w io.Writer, r *bufio.Reader, args ...string) (interface{}, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&buf, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return nil, err
	}
	return readRESP(r)
}

func readRESP(r *bufio.Reader) (interface{}, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("redis: 空响应")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, errors.New("redis: " + line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		data := make([]byte, n+2)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		return string(data[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		arr := make([]interface{}, 0, n)
		for i := 0; i < n; i++ {
			v, err := readRESP(r)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	}
	return nil, fmt.Errorf("redis: 未知响应 %q", line)
}

func redisBan(ip string) error {
	_, err := redisDo("EVAL", banLua, "0", ip, *nodeID, strconv.FormatInt(time.Now().Unix(), 10))
	return err
}

func redisFetchAll() ([]string, error) {
	v, err := redisDo("ZRANGE", "bl:rank", "0", "-1")
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, errors.New("redis: ZRANGE 返回类型错误")
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// syncLoop 补写失败的拉黑记录，并定期拉取共享黑名单
func syncLoop() {
	var last = -1
	for {
		for _, ip := range bl.pendingList() {
			if err := redisBan(ip); err == nil {
				bl.markPending(ip, false)
			}
		}
		if ips, err := redisFetchAll(); err != nil {
			log.Printf("[WARN] Redis 同步失败: %v（继续使用本地拉黑文件）", err)
			last = -1
		} else if n := bl.replaceRemote(ips); n != last {
			// 只在数量变化时输出，避免每分钟刷日志
			log.Printf("Redis 同步完成，共享黑名单 %d 个IP", n)
			last = n
		}
		time.Sleep(*syncEvery)
	}
}

func writeFakePage(c net.Conn) {
	c.SetWriteDeadline(time.Now().Add(headerTimeout))
	fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nServer: nginx/1.18.0\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(honeypotHTML), honeypotHTML)
}

// readHeader 读取首个 HTTP 请求头（到空行），返回已读字节（转发时原样回放）
func readHeader(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := r.ReadSlice('\n')
		buf.Write(line)
		if err != nil && err != bufio.ErrBufferFull {
			return buf.Bytes(), err
		}
		if buf.Len() > maxHeaderSize {
			return buf.Bytes(), errors.New("请求头过大")
		}
		if err == nil && (len(line) == 2 && line[0] == '\r' || len(line) == 1) {
			return buf.Bytes(), nil
		}
	}
}

// parseRequest 解析请求行和 Upgrade 头
func parseRequest(head []byte) (method, path string, isWS, ok bool) {
	lines := strings.Split(string(head), "\n")
	parts := strings.Fields(lines[0])
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/") {
		return "", "", false, false
	}
	method, path = parts[0], parts[1]
	if u, err := url.ParseRequestURI(path); err == nil {
		path = u.Path // 去掉 ?query
	}
	for _, l := range lines[1:] {
		k, v, found := strings.Cut(l, ":")
		if found && strings.EqualFold(strings.TrimSpace(k), "Upgrade") &&
			strings.EqualFold(strings.TrimSpace(v), "websocket") {
			isWS = true
		}
	}
	return method, path, isWS, true
}

// bufConn 先读完 bufio 里已缓冲的数据，再读底层连接
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// filterRequest 检查首个请求；ok=false 表示已拒绝（调用方直接关闭连接）
func filterRequest(c net.Conn) (head []byte, src io.ReadWriteCloser, ok bool) {
	ip := remoteIP(c.RemoteAddr().String())
	if bl.has(ip) {
		vlog("⛔ 黑名单IP断开: %s", ip)
		return nil, nil, false
	}

	c.SetReadDeadline(time.Now().Add(headerTimeout))
	r := bufio.NewReaderSize(c, 4096)
	head, err := readHeader(r)
	if err != nil {
		// 超时/断开/请求头过大：不转发；收到过数据但不是完整 HTTP 头视为探测
		if *banInvalid && len(head) > 0 {
			ban(ip, "非HTTP数据")
		}
		return nil, nil, false
	}
	c.SetReadDeadline(time.Time{})

	method, path, isWS, valid := parseRequest(head)
	switch {
	case !valid:
		if *banInvalid {
			ban(ip, "非法请求行")
		}
		return nil, nil, false
	case path != *filterPath:
		log.Printf("🕵️  非法访问: %s %s %s", c.RemoteAddr(), method, path)
		ban(ip, method+" "+path)
		writeFakePage(c)
		return nil, nil, false
	case !isWS:
		// 路径正确但不是 WebSocket：只给蜜罐，不拉黑（客户端可能先发普通探测）
		writeFakePage(c)
		return nil, nil, false
	}
	return head, &bufConn{Conn: c, r: r}, true
}

// memUsage Go 运行时向系统申请的内存（约等于进程常驻内存）和堆上正在使用的内存
func memUsage() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("%dMB（堆 %dMB）", (m.Sys-m.HeapReleased)>>20, m.HeapInuse>>20)
}
