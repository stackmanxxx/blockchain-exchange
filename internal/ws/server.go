package ws

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// MVP 允许跨域；生产环境校验 Origin
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ServeWS 升级 HTTP 为 WebSocket，启动读写循环（阻塞，连接断开后返回）
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := h.NewClient(nil)
	go c.writeLoop(conn)
	c.readLoop(conn)
	c.Close()
}

// readLoop 读客户端消息（订阅/取消订阅），60s 无消息判定超时
func (c *Client) readLoop(conn *websocket.Conn) {
	defer conn.Close()
	conn.SetReadLimit(4096)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		c.HandleMessage(msg)
	}
}

// writeLoop 写推送消息 + 30s 心跳 ping
func (c *Client) writeLoop(conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.Close()
		_ = conn.Close()
	}()
	for {
		select {
		case <-c.closed:
			return
		case msg, ok := <-c.send:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
