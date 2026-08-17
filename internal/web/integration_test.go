package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"kite-algo/internal/app"
	"kite-algo/internal/auth"
	"kite-algo/internal/config"
	"kite-algo/internal/events"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage/sqlite"
)

const testPassword = "integration-test-password"

// newTestServer boots the real stack — sqlite, app supervisor, engine, hub, and
// HTTP handlers — against a temporary database, and returns an httptest server.
func newTestServer(t *testing.T) (*httptest.Server, *app.App) {
	t.Helper()
	return newTestServerWith(t, nil)
}

// newTestServerWith builds the same server, letting a test adjust the config
// before the app is constructed — capture, risk limits and mode all change what
// the UI renders, and a test needing one of those should not have to rebuild
// the whole harness to get it.
func newTestServerWith(t *testing.T, mutate func(*config.Config)) (*httptest.Server, *app.App) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	cfg := &config.Config{
		Mode: config.ModeDryRun,
		Web: config.WebConfig{
			Addr:           "127.0.0.1:0",
			PasswordHash:   hash,
			SessionTTL:     time.Hour,
			TickIntervalMS: 20, // fast flushes keep the test quick
		},
		Storage: config.StorageConfig{SQLitePath: filepath.Join(t.TempDir(), "test.db")},
	}
	// Load applies defaults; construct them directly for the fields we rely on.
	cfg.Web.PublicURL = "http://127.0.0.1"

	if mutate != nil {
		mutate(cfg)
	}

	store, err := sqlite.New(ctx, cfg.Storage.SQLitePath, quietLogger())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	a, err := app.New(ctx, cfg, store, quietLogger())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	s, err := New(a, quietLogger(), Options{})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	go s.hub.Run(ctx)

	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return ts, a
}

// loginClient returns an HTTP client holding an authenticated session cookie.
func loginClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {testPassword}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login returned HTTP %d", resp.StatusCode)
	}
	return c
}

// dialWS opens the market-data socket using the client's session cookie.
func dialWS(t *testing.T, ts *httptest.Server, c *http.Client) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(ts.URL)

	header := http.Header{}
	for _, ck := range c.Jar.Cookies(u) {
		header.Add("Cookie", ck.Name+"="+ck.Value)
	}
	header.Set("Origin", ts.URL)

	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/ws", header)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("ws dial failed (HTTP %d): %v", code, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestWebSocketDeliversLiveTicks is the end-to-end proof of Phase 2: a browser
// authenticates, opens the socket, declares interest in a symbol, and receives
// coalesced price updates published by the engine.
func TestWebSocketDeliversLiveTicks(t *testing.T) {
	ts, a := newTestServer(t)
	client := loginClient(t, ts)
	conn := dialWS(t, ts, client)

	sub, _ := json.Marshal(clientMessage{Op: "sub", Symbols: []string{"NIFTY 50"}})
	if err := conn.WriteMessage(websocket.TextMessage, sub); err != nil {
		t.Fatalf("send subscribe: %v", err)
	}

	// Publish ticks until a frame arrives. The subscription is processed
	// asynchronously by the hub, so the first few publishes may predate it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		price := 24000.0
		for {
			select {
			case <-stop:
				return
			default:
				price += 1
				a.Bus.Publish(events.Event{
					Kind:   events.KindTick,
					Symbol: "NIFTY 50",
					Tick: &marketdata.Tick{
						TradingSymbol: "NIFTY 50",
						LastPrice:     price,
						OHLC:          marketdata.OHLC{Close: 24000, Open: 24010},
						Timestamp:     time.Now(),
					},
				})
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		var f struct {
			T string     `json:"t"`
			D []tickWire `json:"d"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode frame %s: %v", data, err)
		}
		if f.T != "ticks" {
			continue // status frames etc.
		}
		if len(f.D) == 0 {
			t.Fatal("tick frame carried no data")
		}
		got := f.D[0]
		if got.S != "NIFTY 50" {
			t.Errorf("symbol = %q, want NIFTY 50", got.S)
		}
		if got.P < 24000 {
			t.Errorf("price = %v, want the published series", got.P)
		}
		if got.C == 0 {
			t.Error("percent change not computed from the day close")
		}
		return
	}
}

// TestWebSocketRequiresAuthentication ensures the market-data channel is not
// readable by an anonymous caller. The upgrade is a GET, so CSRF does not cover
// it — the session cookie is the only gate.
func TestWebSocketRequiresAuthentication(t *testing.T) {
	ts, _ := newTestServer(t)
	u, _ := url.Parse(ts.URL)

	header := http.Header{}
	header.Set("Origin", ts.URL)

	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/ws", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("unauthenticated client completed the websocket handshake")
	}
	if resp == nil {
		t.Fatalf("expected an HTTP response, got %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 401 or a redirect to login", resp.StatusCode)
	}
}

// TestWebSocketRejectsForeignOrigin covers the cross-site read. Because the
// browser attaches the session cookie to a WebSocket handshake automatically,
// without this check any page the operator visited could open a socket to their
// trading server and stream their positions.
func TestWebSocketRejectsForeignOrigin(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	u, _ := url.Parse(ts.URL)

	header := http.Header{}
	for _, ck := range client.Jar.Cookies(u) {
		header.Add("Cookie", ck.Name+"="+ck.Value)
	}
	header.Set("Origin", "https://evil.example.com")

	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/ws", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("handshake succeeded from a foreign origin with a valid session cookie")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestPolledFragmentsWorkWithoutWebSocket is the resilience guarantee: the
// socket is a latency optimisation, and every live region must still render
// correctly over plain HTTP when it is unavailable.
func TestPolledFragmentsWorkWithoutWebSocket(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	for _, path := range []string{"/partials/status", "/partials/positions", "/partials/watchlist"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readAll(t, resp)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = HTTP %d, want 200", path, resp.StatusCode)
		}
		if resp.Header.Get("Cache-Control") == "" {
			t.Errorf("GET %s served without a Cache-Control header", path)
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("GET %s returned an empty fragment", path)
		}
	}
}

// TestWatchlistFragmentDeclaresSymbols checks the contract between the polled
// HTML and ws.js: the client derives its subscription set from data-ltp
// attributes, so a fragment without them would render prices that never update.
func TestWatchlistFragmentDeclaresSymbols(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	resp, err := client.Get(ts.URL + "/partials/watchlist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	if !strings.Contains(body, `data-ltp="NIFTY 50"`) {
		t.Errorf("watchlist fragment does not declare data-ltp for NIFTY 50:\n%s", body)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
