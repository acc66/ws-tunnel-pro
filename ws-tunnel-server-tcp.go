package main

// B 服务器：WebSocket → TCP 隧道 + 反探测黑名单
// - 路径不是 -path（默认 /mm）→ 拉黑IP（追加到本地拉黑文件，可选同时写入共享 Redis）+ 返回 nginx 蜜罐页
// - 路径正确但密码错误/非 WebSocket → 返回蜜罐页，不拉黑
// - 黑名单IP → 连接建立时直接断开
// 拉黑文件每行一个IP（# 后为注释），文件被修改后自动重新加载（手动删行即解封）
// 配置 -redis-host 后黑名单与 Redis bl:rank 多节点共享（与 gotcp-filter 同结构）

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	listenAddr = flag.String("listen", ":8080", "WebSocket 监听地址，多个用逗号分隔")
	wsPath     = flag.String("path", "/mm", "WebSocket 路径")
	targetAddr = flag.String("target", "xjpk1.help600.com:2086", "目标服务器地址")
	password   = flag.String("password", "your_secret_password", "连接密码")

	blFile    = flag.String("blacklist", "/etc/ws-tunnel/blacklist.txt", "本地拉黑文件（每行一个IP）")
	redisHost = flag.String("redis-host", "", "共享 Redis 地址（留空则只用本地拉黑文件）")
	redisPort = flag.String("redis-port", "6379", "Redis 端口")
	redisPass = flag.String("redis-pass", "", "Redis 密码")
	nodeID    = flag.String("node", "", "节点标识（默认主机名）")
	syncEvery = flag.Duration("sync", 60*time.Second, "从 Redis 同步黑名单间隔")
	allowList = flag.String("allow", "", "永不拉黑的IP，逗号分隔（自己的测试IP等）")
	noBan     = flag.Bool("no-ban", false, "关闭拉黑（仅返回蜜罐页）")
	verbose   = flag.Bool("v", false, "输出每个连接的日志（连接数大时建议关闭）")
	showVer   = flag.Bool("version", false, "显示版本号")
)

const (
	redisTimeout = 3 * time.Second

	// 与 gotcp-filter.go / hp/export_scanners.sh 保持一致
	banLua = `local ip=ARGV[1] local node=ARGV[2] local ts=ARGV[3] local key="bl:"..ip local count=redis.call("HINCRBY",key,"count",1) redis.call("HSET",key,"ip",ip,"node",node,"last",ts) if count==1 then redis.call("HSET",key,"first",ts) end redis.call("ZADD","bl:rank",count,ip) return count`

	honeypotHTML = `<!DOCTYPE html><html><head><title>Welcome to nginx!</title><style>body{width:35em;margin:0 auto;font-family:Tahoma,Verdana,Arial,sans-serif}</style></head><body><h1>Welcome to nginx!</h1><p>If you see this page, the nginx web server is successfully installed and working. Further configuration is required.</p><p>For online documentation and support please refer to <a href="http://nginx.org/">nginx.org</a>.</p><p><em>Thank you for using nginx.</em></p></body></html>`
)

const (
	muxHeader    = "X-Tunnel-Mux"
	muxVersion   = "smux2"
	pingInterval = 30 * time.Second
	idleTimeout  = 90 * time.Second // 超过这个时间收不到任何数据/心跳即断开，回收死连接
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	WriteBufferPool: &sync.Pool{},
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var (
	bufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

	wsCount     int64 // 当前 WebSocket 连接数
	streamCount int64 // 当前转发中的目标连接数
)

func vlog(format string, v ...any) {
	if *verbose {
		log.Printf(format, v...)
	}
}

var bl *blacklist

// ==================== 黑名单 ====================

type blacklist struct {
	mu      sync.RWMutex
	file    map[string]struct{} // 本地拉黑文件中的
	remote  map[string]struct{} // Redis 中的
	pending map[string]struct{} // 写 Redis 失败、待重试的
	allow   map[string]struct{}

	fmu     sync.Mutex // 串行化文件写入
	modTime time.Time
	size    int64
}

func newBlacklist(allow string) *blacklist {
	b := &blacklist{
		file:    map[string]struct{}{},
		remote:  map[string]struct{}{},
		pending: map[string]struct{}{},
		allow:   map[string]struct{}{},
	}
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
	if _, ok := b.file[ip]; ok {
		return true
	}
	_, ok := b.remote[ip]
	return ok
}

// add 返回 true 表示是新拉黑的IP（已写入本地文件）
func (b *blacklist) add(ip, reason string) bool {
	b.mu.Lock()
	if _, ok := b.allow[ip]; ok {
		b.mu.Unlock()
		return false
	}
	if _, ok := b.file[ip]; ok {
		b.mu.Unlock()
		return false
	}
	b.file[ip] = struct{}{}
	b.mu.Unlock()

	b.fmu.Lock()
	defer b.fmu.Unlock()
	f, err := os.OpenFile(*blFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[WARN] 写入拉黑文件失败: %v", err)
		return true
	}
	// 注释里不能带换行，避免路径中的特殊字符破坏文件格式
	reason = strings.NewReplacer("\r", " ", "\n", " ").Replace(reason)
	fmt.Fprintf(f, "%s # %s %s\n", ip, time.Now().Format("2006-01-02 15:04:05"), reason)
	f.Close()
	if st, err := os.Stat(*blFile); err == nil {
		b.modTime, b.size = st.ModTime(), st.Size() // 自己写的不触发重载
	}
	return true
}

// reloadFile 文件被外部修改（手动删行解封 / 手动加IP）时重新加载
func (b *blacklist) reloadFile(force bool) {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	st, err := os.Stat(*blFile)
	if err != nil {
		if os.IsNotExist(err) && len(b.fileSnapshot()) > 0 {
			b.mu.Lock()
			b.file = map[string]struct{}{}
			b.mu.Unlock()
			log.Printf("拉黑文件已删除，本地黑名单清空")
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
	b.file = next
	b.mu.Unlock()
	b.modTime, b.size = st.ModTime(), st.Size()
	log.Printf("已加载拉黑文件 %s，共 %d 个IP", *blFile, len(next))
}

func (b *blacklist) fileSnapshot() map[string]struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.file
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

func (b *blacklist) pendingList() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.pending))
	for ip := range b.pending {
		out = append(out, ip)
	}
	return out
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

// ==================== 极简 Redis 客户端（RESP，无第三方依赖） ====================

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

func syncLoop() {
	for {
		for _, ip := range bl.pendingList() {
			if err := redisBan(ip); err == nil {
				bl.markPending(ip, false)
			}
		}
		if ips, err := redisFetchAll(); err != nil {
			log.Printf("[WARN] Redis 同步失败: %v（继续使用本地拉黑文件）", err)
		} else {
			n := bl.replaceRemote(ips)
			log.Printf("Redis 同步完成，共享黑名单 %d 个IP", n)
		}
		time.Sleep(*syncEvery)
	}
}

// watchFile 每 5 秒检查拉黑文件是否被修改
func watchFile() {
	for {
		time.Sleep(5 * time.Second)
		bl.reloadFile(false)
	}
}

// ==================== 监听：黑名单IP在 Accept 时直接断开 ====================

type filterListener struct{ net.Listener }

func (l filterListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if bl.has(remoteIP(c.RemoteAddr().String())) {
			c.Close()
			continue
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
		}
		return c, nil
	}
}

func main() {
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	if *nodeID == "" {
		*nodeID, _ = os.Hostname()
	}

	bl = newBlacklist(*allowList)
	if !*noBan {
		if err := os.MkdirAll(filepath.Dir(*blFile), 0755); err != nil {
			log.Printf("[WARN] 创建目录失败: %v", err)
		}
		bl.reloadFile(true)
		go watchFile()
		if *redisHost != "" {
			go syncLoop()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc(*wsPath, handleWebSocketTunnel)
	mux.HandleFunc("/", handleFakePage)

	log.Printf("🚀 WebSocket TCP 隧道服务器启动 %s", version)
	log.Printf("🔑 WebSocket 路径: %s", *wsPath)
	log.Printf("🎯 目标地址: %s", *targetAddr)
	switch {
	case *noBan:
		log.Printf("🛡️  拉黑已关闭")
	case *redisHost != "":
		log.Printf("🛡️  拉黑文件: %s + 共享 Redis %s:%s（节点 %s）", *blFile, *redisHost, *redisPort, *nodeID)
	default:
		log.Printf("🛡️  拉黑文件: %s", *blFile)
	}

	go statsLoop()

	errCh := make(chan error, 1)
	for _, a := range strings.Split(*listenAddr, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Fatalf("监听 %s 失败: %v", a, err)
		}
		log.Printf("📍 监听地址: %s", a)
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() { errCh <- srv.Serve(filterListener{ln}) }()
	}
	log.Fatal("启动失败:", <-errCh)
}

func handleWebSocketTunnel(w http.ResponseWriter, r *http.Request) {
	// 路径必须精确匹配（ServeMux 对 "/mm/" 这类前缀也会命中）
	if r.URL.Path != *wsPath {
		handleFakePage(w, r)
		return
	}
	// 验证密码：错误只给蜜罐，不拉黑（避免误封自己）；常量时间比较防止计时侧信道
	authHeader := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(authHeader), []byte("Bearer "+*password)) != 1 || !websocket.IsWebSocketUpgrade(r) {
		log.Printf("❌ 未授权访问: %s", r.RemoteAddr)
		writeFakePage(w)
		return
	}

	// 客户端请求多路复用时在响应头里确认；旧版客户端不带这个头，走单连接模式
	useMux := r.Header.Get(muxHeader) == muxVersion
	var respHeader http.Header
	if useMux {
		respHeader = http.Header{muxHeader: []string{muxVersion}}
	}

	wsConn, err := upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		log.Printf("❌ WebSocket 升级失败: %v", err)
		return
	}
	conn := newWSConn(wsConn)
	defer conn.Close()
	atomic.AddInt64(&wsCount, 1)
	defer atomic.AddInt64(&wsCount, -1)

	if !useMux {
		vlog("✅ 新连接: %s → %s", r.RemoteAddr, *targetAddr)
		forward(conn)
		vlog("🔌 连接关闭: %s", r.RemoteAddr)
		return
	}

	sess, err := smux.Server(conn, smuxConfig())
	if err != nil {
		log.Printf("❌ 创建复用会话失败: %v", err)
		return
	}
	defer sess.Close()
	vlog("✅ 新复用连接: %s", r.RemoteAddr)
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			break
		}
		go func() {
			defer st.Close()
			forward(st)
		}()
	}
	vlog("🔌 复用连接关闭: %s", r.RemoteAddr)
}

// forward 连接目标并双向转发，任意一端结束即关闭两端
func forward(c io.ReadWriteCloser) {
	atomic.AddInt64(&streamCount, 1)
	defer atomic.AddInt64(&streamCount, -1)

	tcpConn, err := net.DialTimeout("tcp", *targetAddr, 10*time.Second)
	if err != nil {
		log.Printf("❌ 连接目标失败 %s: %v", *targetAddr, err)
		return
	}
	if tc, ok := tcpConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	done := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		bp := bufPool.Get().(*[]byte)
		_, err := io.CopyBuffer(dst, src, *bp)
		bufPool.Put(bp)
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			vlog("⚠️  转发结束: %v", err)
		}
		dst.Close()
		src.Close()
		done <- struct{}{}
	}
	go cp(tcpConn, c)
	go cp(c, tcpConn)
	<-done
	<-done
}

func smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxReceiveBuffer = 16 * 1024 * 1024
	c.MaxStreamBuffer = 1024 * 1024
	return c
}

func statsLoop() {
	var lw, ls int64 = -1, -1
	for {
		time.Sleep(60 * time.Second)
		w, s := atomic.LoadInt64(&wsCount), atomic.LoadInt64(&streamCount)
		if w != lw || s != ls {
			log.Printf("📊 WebSocket 连接: %d，转发中: %d", w, s)
			lw, ls = w, s
		}
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

func handleFakePage(w http.ResponseWriter, r *http.Request) {
	log.Printf("🕵️  非法访问: %s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
	ban(remoteIP(r.RemoteAddr), r.Method+" "+r.URL.Path)
	writeFakePage(w)
}

func writeFakePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Server", "nginx/1.18.0")
	w.Header().Set("Connection", "close")
	fmt.Fprint(w, honeypotHTML)
}
