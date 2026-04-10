package explorer

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
	wsMaxMsgSize = 8192
)

type ChatHub struct {
	mu          sync.RWMutex
	clients     map[*ChatClient]bool
	userClients map[string][]*ChatClient
	register    chan *ChatClient
	unregister  chan *ChatClient
	broadcast   chan []byte
	onlineCount int
}

type ChatClient struct {
	hub    *ChatHub
	conn   *websocket.Conn
	send   chan []byte
	userID string
}

func NewChatHub() *ChatHub {
	return &ChatHub{
		clients:     make(map[*ChatClient]bool),
		userClients: make(map[string][]*ChatClient),
		register:    make(chan *ChatClient, 16),
		unregister:  make(chan *ChatClient, 16),
		broadcast:   make(chan []byte, 256),
	}
}

func (h *ChatHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			if client.userID != "" {
				h.userClients[client.userID] = append(h.userClients[client.userID], client)
			}
			h.onlineCount = len(h.clients)
			h.mu.Unlock()
			h.broadcastOnlineCount()

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				if client.userID != "" {
					cls := h.userClients[client.userID]
					upd := cls[:0]
					for _, c := range cls {
						if c != client {
							upd = append(upd, c)
						}
					}
					if len(upd) == 0 {
						delete(h.userClients, client.userID)
					} else {
						h.userClients[client.userID] = upd
					}
				}
			}
			h.onlineCount = len(h.clients)
			h.mu.Unlock()
			h.broadcastOnlineCount()

		case msg := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *ChatHub) BroadcastMessage(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.broadcast <- data
}

func (h *ChatHub) SendToUser(userID string, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	clients := h.userClients[userID]
	h.mu.RUnlock()
	for _, c := range clients {
		select {
		case c.send <- data:
		default:
		}
	}
}

func (h *ChatHub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.onlineCount
}

func (h *ChatHub) broadcastOnlineCount() {
	data, _ := json.Marshal(map[string]interface{}{"type": "online_count", "count": h.OnlineCount()})
	h.broadcast <- data
}

func (c *ChatClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
