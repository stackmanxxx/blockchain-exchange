// Package ws 实现 WebSocket 行情推送（hub 模型）。
// 协议：
//
//	订阅  {"op":"subscribe","channel":"depth:BTCUSDT"}
//	取消  {"op":"unsubscribe","channel":"depth:BTCUSDT"}
//	推送  {"channel":"depth:BTCUSDT","data":{...}}
//
// 频道格式：depth:<symbol> / trade:<symbol> / kline:<symbol>:<interval> / ticker:<symbol>
package ws

import (
	"encoding/json"
	"sync"
)

// Message 推送消息
type Message struct {
	Channel string `json:"channel"`
	Data    any    `json:"data"`
}

// Client WebSocket 客户端
type Client struct {
	hub       *Hub
	send      chan []byte
	channels  map[string]struct{}
	mu        sync.Mutex
	onMessage func([]byte)
	closed    chan struct{}
}

func newClient(h *Hub) *Client {
	return &Client{
		hub:      h,
		send:     make(chan []byte, 256),
		channels: make(map[string]struct{}),
		closed:   make(chan struct{}),
	}
}

// SendChan 待发送队列（由 WS handler 消费）
func (c *Client) SendChan() <-chan []byte { return c.send }

// Closed 连接关闭通知
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Close 关闭客户端
func (c *Client) Close() {
	select {
	case <-c.closed:
		return
	default:
	}
	close(c.closed)
	c.hub.unregister(c)
}

// HandleMessage 处理客户端消息（订阅/取消订阅）
func (c *Client) HandleMessage(data []byte) {
	var req struct {
		Op      string `json:"op"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(data, &req); err != nil || req.Channel == "" {
		c.sendJSON(Message{Channel: "error", Data: "invalid message"})
		return
	}
	switch req.Op {
	case "subscribe":
		c.mu.Lock()
		c.channels[req.Channel] = struct{}{}
		c.mu.Unlock()
	case "unsubscribe":
		c.mu.Lock()
		delete(c.channels, req.Channel)
		c.mu.Unlock()
	default:
		c.sendJSON(Message{Channel: "error", Data: "unknown op: " + req.Op})
	}
}

func (c *Client) sendJSON(m Message) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	select {
	case c.send <- b:
	default: // 队列满则丢弃，避免阻塞（生产环境做慢客户端踢除）
	}
}

// Hub 全局推送中心
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*Client]struct{})}
}

// NewClient 创建并注册客户端
func (h *Hub) NewClient(onMessage func([]byte)) *Client {
	c := newClient(h)
	c.onMessage = onMessage
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// Publish 向订阅了指定频道的客户端推送
func (h *Hub) Publish(channel string, data any) {
	b, err := json.Marshal(Message{Channel: channel, Data: data})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.mu.Lock()
		_, ok := c.channels[channel]
		c.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case c.send <- b:
		default: // 慢客户端丢消息
		}
	}
}

// Broadcast 向所有客户端推送（忽略订阅过滤，用于连接成功提示等）
func (h *Hub) Broadcast(channel string, data any) {
	b, err := json.Marshal(Message{Channel: channel, Data: data})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- b:
		default:
		}
	}
}
