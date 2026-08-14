package kite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestConcurrentSubscribeDoesNotPanic reproduces the crash that took the whole
// process down.
//
// gorilla/websocket allows one concurrent writer and PANICS on a second. The
// ticker's Subscribe/Unsubscribe are called from HTTP handler goroutines — the
// option chain subscribes on every page render, the WebSocket hub subscribes
// when a browser declares its symbols — so two tabs, or one tab switching
// panels, is enough to race two writes onto the Kite socket.
//
// The panic happened on a goroutine spawned per browser connection, so it killed
// a process that was holding open positions.
func TestConcurrentSubscribeDoesNotPanic(t *testing.T) {
	var upgrader = websocket.Upgrader{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain whatever the client sends until it goes away.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tk := NewTicker("key", "token", wsURL, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tk.Run(ctx) }()

	// Wait for the connection before hammering it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tk.mu.Lock()
		up := tk.conn != nil
		tk.mu.Unlock()
		if up {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Many goroutines subscribing and unsubscribing at once, as several browser
	// tabs would. Without the write lock this panics almost immediately.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens := []uint32{uint32(1000 + i), uint32(2000 + i), 256265}
			for j := 0; j < 40; j++ {
				tk.Subscribe(tokens)
				tk.Unsubscribe(tokens[:1])
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent subscribe deadlocked")
	}

	_ = tk.Close()
}

// TestSubscribeWithoutConnectionIsSafe covers the pre-login window: the UI can
// ask for symbols before the ticker has dialled, and that must record the
// request rather than crash.
func TestSubscribeWithoutConnectionIsSafe(t *testing.T) {
	tk := NewTicker("key", "token", "ws://127.0.0.1:0", nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tk.Subscribe([]uint32{uint32(i), 256265})
			tk.Unsubscribe([]uint32{uint32(i)})
		}(i)
	}
	wg.Wait()
}
