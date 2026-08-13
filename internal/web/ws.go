package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket timing. Ping/pong reaps connections that died without a FIN — a
// closed laptop lid produces exactly that, and without a heartbeat its client
// would occupy a slot and a goroutine indefinitely.
const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second
	maxMessage = 8 << 10
)

// Client is one browser connection.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte

	mu   sync.RWMutex
	syms map[string]struct{}

	drops atomic.Uint64
}

// watching reports whether this client wants updates for symbol.
func (c *Client) watching(symbol string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.syms[symbol]
	return ok
}

// symbols returns a snapshot of the client's interest set.
func (c *Client) symbols() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.syms))
	for s := range c.syms {
		out = append(out, s)
	}
	return out
}

func (c *Client) clearSymbols() {
	c.mu.Lock()
	c.syms = make(map[string]struct{})
	c.mu.Unlock()
}

// trySend queues a frame, reporting false if the client is too far behind.
// It never blocks: the caller is the hub goroutine, which must keep draining
// engine events no matter how slow any one browser is.
func (c *Client) trySend(b []byte) bool {
	select {
	case c.send <- b:
		return true
	default:
		c.drops.Add(1)
		return false
	}
}

// clientMessage is the browser→server protocol.
type clientMessage struct {
	Op      string   `json:"op"`      // sub | unsub | ping
	Symbols []string `json:"symbols"` // for sub/unsub
}

// upgrader validates the origin explicitly. gorilla's default same-origin check
// compares against the Host header, which is wrong behind a reverse proxy that
// terminates TLS: the browser reports the public origin while Host may be the
// internal one. Compare against the configured public URL instead.
func (s *Server) upgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin:     s.checkOrigin,
	}
}

// checkOrigin permits only this application's own origin.
//
// This matters more than usual: the WebSocket handshake is a GET, so it is not
// covered by CSRF protection, and the browser attaches the session cookie
// automatically. Without an origin check any website the operator visits could
// open a socket to a localhost-bound trading server and read their positions.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client; the session cookie still gates access
	}
	got, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(got.Host, r.Host) {
		return true
	}
	if pub, err := url.Parse(s.app.Cfg.Web.PublicURL); err == nil && pub.Host != "" {
		return strings.EqualFold(got.Host, pub.Host)
	}
	return false
}

// handleWS upgrades an authenticated request to a WebSocket.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	up := s.upgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written an error response.
		s.log.Debug("websocket upgrade failed", "err", err, "ip", s.clientIP(r))
		return
	}

	c := &Client{
		hub:  s.hub,
		conn: conn,
		send: make(chan []byte, clientBuffer),
		syms: make(map[string]struct{}),
	}
	s.hub.register <- c

	go c.writePump(s)
	go c.readPump(s)
}

// readPump consumes browser messages until the connection dies.
func (c *Client) readPump(s *Server) {
	defer func() {
		c.hub.unregister <- c
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				s.log.Debug("ws read error", "err", err)
			}
			return
		}

		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			s.log.Debug("ws bad message", "err", err)
			continue
		}

		switch msg.Op {
		case "sub":
			added := c.addSymbols(msg.Symbols)
			if len(added) > 0 {
				c.hub.acquire(added)
			}
		case "unsub":
			removed := c.removeSymbols(msg.Symbols)
			if len(removed) > 0 {
				c.hub.release(removed)
			}
		case "ping":
			// The write pump's ping frames handle liveness; this is just a
			// client-initiated no-op.
		}
	}
}

// addSymbols records new interest and returns only the genuinely new ones, so
// a client re-sending its whole watch list does not inflate the refcount.
func (c *Client) addSymbols(symbols []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var added []string
	for _, s := range symbols {
		if s == "" {
			continue
		}
		if _, exists := c.syms[s]; !exists {
			c.syms[s] = struct{}{}
			added = append(added, s)
		}
	}
	return added
}

func (c *Client) removeSymbols(symbols []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var removed []string
	for _, s := range symbols {
		if _, exists := c.syms[s]; exists {
			delete(c.syms, s)
			removed = append(removed, s)
		}
	}
	return removed
}

// writePump drains the send queue and keeps the connection alive.
func (c *Client) writePump(s *Server) {
	ping := time.NewTicker(pingPeriod)
	defer func() {
		ping.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel: say goodbye politely so the
				// browser reconnects rather than reporting an error.
				_ = c.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				s.log.Debug("ws write failed", "err", err)
				return
			}

		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
