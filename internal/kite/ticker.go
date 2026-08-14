package kite

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"kite-algo/internal/marketdata"
)

// Kite Connect WebSocket exchange-segment constants (low byte of instrument
// token). Used to pick the correct price divisor when decoding binary ticks.
const (
	segNSE     = 1
	segNFO     = 2
	segCDS     = 3
	segBSE     = 4
	segBFO     = 5
	segBCD     = 6
	segMCX     = 7
	segMCXSX   = 8
	segIndices = 9
)

// priceDivisor returns the price divisor for an instrument token's segment.
// cds (currency) uses 1e7, bcd uses 1e4, everything else (equity/fut/opt/index)
// uses 100.
func priceDivisor(token uint32) float64 {
	switch token & 0xff {
	case segCDS:
		return 10000000.0
	case segBCD:
		return 10000.0
	default:
		return 100.0
	}
}

// OrderUpdate is the JSON payload of an order-update text message.
type OrderUpdate struct {
	OrderID         string  `json:"order_id"`
	ExchangeOrderID string  `json:"exchange_order_id"`
	Tradingsymbol   string  `json:"tradingsymbol"`
	Exchange        string  `json:"exchange"`
	TransactionType string  `json:"transaction_type"`
	Product         string  `json:"product"`
	OrderType       string  `json:"order_type"`
	Quantity        float64 `json:"quantity"`
	FilledQuantity  float64 `json:"filled_quantity"`
	PendingQuantity float64 `json:"pending_quantity"`
	Price           float64 `json:"price"`
	TriggerPrice    float64 `json:"trigger_price"`
	AveragePrice    float64 `json:"average_price"`
	Status          string  `json:"status"`
	StatusMessage   string  `json:"status_message"`
}

// TickHandler is called for every decoded market-data tick.
type TickHandler func(marketdata.Tick)

// OrderHandler is called for order updates streamed over the ticker.
type OrderHandler func(OrderUpdate)

// ConnectHandler is called after a successful (re)connect — used to resubscribe.
type ConnectHandler func()

// Ticker is a Kite WebSocket client with auto-reconnect and resubscribe.
//
// Wire format (verified against pykiteconnect ticker.py):
//   - Outgoing: JSON text frames: {"a":"subscribe","v":[tokens]},
//     {"a":"unsubscribe","v":[tokens]}, {"a":"mode","v":["full",[tokens]]}
//   - Incoming: binary frames. Big-endian. First 2 bytes = number of packets;
//     each packet = 2-byte length prefix + payload. Payload length selects the
//     decode mode: 8=LTP, 28/32=indices quote/full, 44/184=standard quote/full.
type Ticker struct {
	apiKey      string
	accessToken string
	url         string
	instruments *Instruments // optional, used to enrich ticks with symbols
	logger      *slog.Logger

	mu            sync.Mutex
	conn          *websocket.Conn
	subscriptions map[uint32]marketdata.Mode // token -> requested mode
	connected     bool
	stop          chan struct{}
	stopped       bool

	OnTick    TickHandler
	OnOrder   OrderHandler
	OnConnect ConnectHandler
	OnError   func(error)

	// Reconnect tuning.
	reconnectMinDelay time.Duration
	reconnectMaxDelay time.Duration
}

// NewTicker constructs a Ticker. Pass an Instruments master to get trading
// symbols on every tick; otherwise ticks carry only the numeric token.
func NewTicker(apiKey, accessToken, tickerURL string, instruments *Instruments, logger *slog.Logger) *Ticker {
	return &Ticker{
		apiKey:            apiKey,
		accessToken:       accessToken,
		url:               tickerURL,
		instruments:       instruments,
		logger:            logger,
		subscriptions:     make(map[uint32]marketdata.Mode),
		stop:              make(chan struct{}),
		reconnectMinDelay: 1 * time.Second,
		reconnectMaxDelay: 60 * time.Second,
	}
}

// Subscribe requests full-mode streaming for the given instrument tokens.
// Safe to call before connect (the subscription is replayed on connect).
func (t *Ticker) Subscribe(tokens []uint32) {
	t.mu.Lock()
	for _, tok := range tokens {
		t.subscriptions[tok] = marketdata.ModeFull
	}
	conn := t.conn
	t.mu.Unlock()

	if conn != nil {
		t.sendSubscribe(tokens)
		t.sendSetMode(marketdata.ModeFull, tokens)
	}
}

// Unsubscribe stops streaming for the given tokens.
func (t *Ticker) Unsubscribe(tokens []uint32) {
	t.mu.Lock()
	for _, tok := range tokens {
		delete(t.subscriptions, tok)
	}
	conn := t.conn
	t.mu.Unlock()

	if conn != nil {
		t.sendUnsubscribe(tokens)
	}
}

// resubscribeAll replays the current subscription set after a reconnect.
func (t *Ticker) resubscribeAll() {
	t.mu.Lock()
	tokens := make([]uint32, 0, len(t.subscriptions))
	for tok := range t.subscriptions {
		tokens = append(tokens, tok)
	}
	conn := t.conn
	t.mu.Unlock()

	if conn == nil || len(tokens) == 0 {
		return
	}
	t.sendSubscribe(tokens)
	t.sendSetMode(marketdata.ModeFull, tokens)
}

// Run connects and blocks until Close is called or the context is canceled.
// It handles reconnection with exponential backoff internally.
func (t *Ticker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.stop:
			return nil
		default:
		}

		err := t.connectAndServe(ctx)
		if t.isStopped() {
			return nil
		}
		if t.OnError != nil && err != nil {
			t.OnError(err)
		}
		if !t.shouldReconnect(err) {
			return err
		}

		// Backoff and retry.
		delay := t.reconnectMinDelay
		for {
			if t.logger != nil {
				t.logger.Warn("kite ticker reconnecting", "delay", delay, "err", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.stop:
				return nil
			case <-time.After(delay):
			}
			if delay < t.reconnectMaxDelay {
				delay *= 2
				if delay > t.reconnectMaxDelay {
					delay = t.reconnectMaxDelay
				}
			}
			break
		}
	}
}

// Close stops the ticker and closes the underlying connection.
func (t *Ticker) Close() error {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	close(t.stop)
	conn := t.conn
	t.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

func (t *Ticker) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

// shouldReconnect decides whether the given error is recoverable.
func (t *Ticker) shouldReconnect(err error) bool {
	if err == nil {
		return true // closed cleanly; keep reconnecting per Kite guidance
	}
	// We always reconnect; Kite connections drop ~daily and must be re-established.
	return true
}

// connectAndServe dials the WebSocket, marks connected, calls OnConnect
// (which triggers resubscribe), and reads messages until the connection drops.
func (t *Ticker) connectAndServe(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	endpoint := fmt.Sprintf("%s/?api_key=%s&access_token=%s", t.url, t.apiKey, t.accessToken)
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("kite ticker dial: %w", err)
	}
	conn.SetReadLimit(1 << 20) // 1 MiB; ticks come in larger batches

	t.mu.Lock()
	t.conn = conn
	t.connected = true
	t.mu.Unlock()

	if t.logger != nil {
		t.logger.Info("kite ticker connected")
	}
	if t.OnConnect != nil {
		t.OnConnect()
	}
	// Always resubscribe, even if OnConnect already did.
	t.resubscribeAll()

	defer func() {
		t.mu.Lock()
		t.connected = false
		t.conn = nil
		t.mu.Unlock()
		_ = conn.Close()
	}()

	// Read loop. gorilla/websocket pings are answered automatically by setting
	// a pong handler; defaults are fine. We set a read deadline reset per message.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.stop:
			return nil
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("ticker read: %w", err)
		}
		switch msgType {
		case websocket.TextMessage:
			t.handleText(data)
		case websocket.BinaryMessage:
			ticks := parseBinaryTicks(data)
			if t.OnTick != nil {
				for _, tk := range ticks {
					t.enrichTick(&tk)
					t.OnTick(tk)
				}
			}
		}
	}
}

// handleText parses a text frame: order updates and error messages.
func (t *Ticker) handleText(data []byte) {
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		if t.logger != nil {
			t.logger.Debug("ticker text parse failed", "err", err)
		}
		return
	}
	switch env.Type {
	case "order":
		if t.OnOrder != nil {
			var ou OrderUpdate
			if err := json.Unmarshal(env.Data, &ou); err == nil {
				t.OnOrder(ou)
			}
		}
	case "error":
		if t.logger != nil {
			t.logger.Warn("ticker error frame", "data", string(env.Data))
		}
	}
}

// enrichTick fills in the trading symbol/exchange if an instruments master is
// available. Kite ticks only carry the numeric token.
func (t *Ticker) enrichTick(tk *marketdata.Tick) {
	if t.instruments == nil {
		return
	}
	if inst, ok := t.instruments.LookupToken(tk.InstrumentToken); ok {
		tk.TradingSymbol = inst.TradingSymbol
		tk.Exchange = inst.Exchange
	}
}

// --- outgoing JSON commands ---

func (t *Ticker) sendJSON(v any) error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("ticker not connected")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (t *Ticker) sendSubscribe(tokens []uint32) {
	if len(tokens) == 0 {
		return
	}
	// The wire payload wants a plain JSON array of integers.
	_ = t.sendJSON(struct {
		A string   `json:"a"`
		V []uint32 `json:"v"`
	}{"subscribe", tokens})
}

func (t *Ticker) sendUnsubscribe(tokens []uint32) {
	if len(tokens) == 0 {
		return
	}
	_ = t.sendJSON(struct {
		A string   `json:"a"`
		V []uint32 `json:"v"`
	}{"unsubscribe", tokens})
}

func (t *Ticker) sendSetMode(mode marketdata.Mode, tokens []uint32) {
	if len(tokens) == 0 {
		return
	}
	_ = t.sendJSON(struct {
		A string `json:"a"`
		V []any  `json:"v"`
	}{"mode", []any{string(mode), toAny(tokens)}})
}

// toAny converts []uint32 to []any so it can sit in a heterogeneous JSON array.
func toAny(tokens []uint32) []any {
	out := make([]any, len(tokens))
	for i, t := range tokens {
		out[i] = t
	}
	return out
}
