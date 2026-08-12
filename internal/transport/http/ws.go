package http

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// hub menyiarkan perubahan meeting/event LiveKit ke klien WebSocket.
// Klien mengautentikasi diri lewat query `?token=<access token SSO>`.
type hub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	up      websocket.Upgrader
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

type wsMessage struct {
	Kind    string    `json:"kind"`
	Payload any       `json:"payload,omitempty"`
	At      time.Time `json:"at"`
}

func newHub() *hub {
	return &hub{
		clients: make(map[*wsClient]struct{}),
		up: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
	}
}

// Broadcast mengirim satu pesan ke semua klien yang tersambung.
func (h *hub) Broadcast(kind string, payload any) {
	b, err := json.Marshal(wsMessage{Kind: kind, Payload: payload, At: time.Now()})
	if err != nil {
		log.Printf("livekit: ws marshal %s: %v", kind, err)
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- b:
		default: // klien lambat — lewati, jangan blokir server
		}
	}
}

// serve meng-upgrade koneksi setelah token divalidasi oleh authorize.
func (h *hub) serve(w http.ResponseWriter, r *http.Request, authorize func(token string) bool) {
	token := r.URL.Query().Get("token")
	if token == "" {
		token = bearer(r)
	}
	if !authorize(token) {
		writeError(w, http.StatusUnauthorized, "token tidak valid")
		return
	}
	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade sudah menulis respons error
	}
	c := &wsClient{conn: conn, send: make(chan []byte, 32)}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	n := len(h.clients)
	h.mu.Unlock()
	log.Printf("livekit: ws tersambung (%d klien)", n)

	go h.writePump(c)
	h.readPump(c)
}

func (h *hub) readPump(c *wsClient) {
	defer h.drop(c)
	c.conn.SetReadLimit(4096)
	_ = c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (h *hub) writePump(c *wsClient) {
	ping := time.NewTicker(30 * time.Second)
	defer func() {
		ping.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *hub) drop(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
	_ = c.conn.Close()
}
