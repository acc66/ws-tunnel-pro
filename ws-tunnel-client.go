package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	// 命令行参数
	localAddr  = flag.String("local", "127.0.0.1:1080", "本地监听地址（SOCKS5/HTTP 代理）")
	serverAddr = flag.String("server", "ws://2.2.2.2:8080/mm", "WebSocket 服务器地址")
	password   = flag.String("password", "your_secret_password", "连接密码")
	useTLS     = flag.Bool("tls", false, "使用 WSS (TLS)")
)

func main() {
	flag.Parse()

	// 自动调整协议
	if *useTLS && (*serverAddr)[:2] == "ws" {
		*serverAddr = "wss" + (*serverAddr)[2:]
	}

	log.Printf("🚀 WebSocket 隧道客户端启动")
	log.Printf("📍 本地监听: %s", *localAddr)
	log.Printf("🌐 服务器: %s", *serverAddr)

	// 启动本地 TCP 监听
	listener, err := net.Listen("tcp", *localAddr)
	if err != nil {
		log.Fatal("监听失败:", err)
	}
	defer listener.Close()

	log.Printf("✅ 等待连接...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("❌ 接受连接失败: %v", err)
			continue
		}

		go handleConnection(conn)
	}
}

func handleConnection(localConn net.Conn) {
	defer localConn.Close()

	log.Printf("✅ 新本地连接: %s", localConn.RemoteAddr())

	// 连接 WebSocket 服务器
	headers := make(map[string][]string)
	headers["Authorization"] = []string{"Bearer " + *password}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   65536,
		WriteBufferSize:  65536,
	}

	wsConn, _, err := dialer.Dial(*serverAddr, headers)
	if err != nil {
		log.Printf("❌ 连接服务器失败: %v", err)
		return
	}
	defer wsConn.Close()

	log.Printf("🔗 已连接服务器: %s", *serverAddr)

	// 双向转发
	var wg sync.WaitGroup
	wg.Add(2)

	// Local → WebSocket
	go func() {
		defer wg.Done()
		defer wsConn.Close()

		buf := make([]byte, 32768)
		for {
			n, err := localConn.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("⚠️  读取本地连接失败: %v", err)
				}
				return
			}

			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				log.Printf("⚠️  WebSocket 写入错误: %v", err)
				return
			}
		}
	}()

	// WebSocket → Local
	go func() {
		defer wg.Done()
		defer localConn.Close()

		for {
			msgType, data, err := wsConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("⚠️  WebSocket 读取错误: %v", err)
				}
				return
			}

			if msgType != websocket.BinaryMessage {
				continue
			}

			if _, err := localConn.Write(data); err != nil {
				log.Printf("⚠️  写入本地连接失败: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	log.Printf("🔌 连接关闭: %s", localConn.RemoteAddr())
}
