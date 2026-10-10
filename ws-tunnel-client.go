package main

// A 服务器：本地 TCP → WebSocket 隧道客户端
// - 多路复用（-mux N）：所有本地连接复用 N 条 WebSocket 长连接（smux），大幅减少到 B 的 TCP 连接数
//   B 端是旧版（不支持多路复用）时自动退回“一个连接一条 WebSocket”
// - 备用服务器：-server 用逗号分隔多个地址，按顺序优先；当前服务器失败自动切到下一个，
//   主服务器恢复后新连接自动切回

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
	muxConns   = flag.Int("mux", 4, "每个服务器复用的 WebSocket 连接数（0=关闭多路复用，每个连接单独一条 WebSocket）")
	dialTO     = flag.Duration("timeout", 5*time.Second, "连接服务器超时（超时即切换备用服务器）")
	checkEvery = flag.Duration("check", 10*time.Second, "故障服务器的探测间隔（恢复后自动切回）")
	verbose    = flag.Bool("v", false, "输出每个连接的日志（连接数大时建议关闭）")
	showVer    = flag.Bool("version", false, "显示版本号")
)

const (
	muxHeader    = "X-Tunnel-Mux"
	muxVersion   = "smux2"
	pingInterval = 30 * time.Second
	idleTimeout  = 90 * time.Second
)

var (
	bufPool  = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}
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
	sess, err := u.session()
	if err != nil {
		return nil, err
	}
	if sess == nil { // 服务端不支持多路复用，已退回单连接模式
		return u.open()
	}
	st, err := sess.OpenStream()
	if err != nil {
		sess.Close()
		return nil, err
	}
	return st, nil
}

// muxSlot 一条复用 WebSocket。同一槽位同时只拨号一次，其他请求等待结果（防止瞬间大量连接时一起拨号）
type muxSlot struct {
	sess    *smux.Session
	dialing chan struct{} // 非 nil 表示正在拨号，关闭即完成
	err     error         // 最近一次拨号的错误
}

// session 轮询取一个可用的 smux 会话，槽位为空或已断开则新建
func (u *upstream) session() (*smux.Session, error) {
	u.mu.Lock()
	if len(u.slots) != *muxConns {
		u.slots = make([]*muxSlot, *muxConns)
		for i := range u.slots {
			u.slots[i] = &muxSlot{}
		}
	}
	// 优先挑已连好的槽位，避免某个槽位断开时新连接都卡在它的重连上
	var sl *muxSlot
	for k := 0; k < len(u.slots); k++ {
		s := u.slots[(u.next+k)%len(u.slots)]
		if s.sess != nil && !s.sess.IsClosed() {
			sl = s
			u.next += k + 1
			break
		}
	}
	if sl != nil {
		sess := sl.sess
		u.mu.Unlock()
		return sess, nil
	}
	sl = u.slots[u.next%len(u.slots)]
	u.next++
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

func smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxReceiveBuffer = 16 * 1024 * 1024
	c.MaxStreamBuffer = 1024 * 1024
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
	if *muxConns > 0 {
		log.Printf("🔀 多路复用: 每个服务器 %d 条 WebSocket", *muxConns)
	} else {
		log.Printf("🔀 多路复用: 关闭")
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
			log.Printf("📊 当前活动连接: %d", n)
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

	tunnel, u, err := openTunnel()
	if err != nil {
		log.Printf("❌ 连接服务器失败: %v", err)
		return
	}
	defer tunnel.Close()
	vlog("🔗 %s → %s", localConn.RemoteAddr(), u.url)

	pipe(localConn, tunnel)
	vlog("🔌 连接关闭: %s", localConn.RemoteAddr())
}

// pipe 双向转发，任意一端结束即关闭两端
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		bp := bufPool.Get().(*[]byte)
		io.CopyBuffer(dst, src, *bp)
		bufPool.Put(bp)
		dst.Close()
		src.Close()
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
