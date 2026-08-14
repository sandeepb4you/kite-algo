// Package web serves the operator's trading UI: a manual terminal, an algo
// control panel, a market dashboard, and the Zerodha login round-trip.
//
// It is deliberately server-rendered — Go html/template plus htmx — so there is
// no separate build toolchain and no client/server model drift. The only thing
// pushed to the browser as raw data is the market-data stream, which is far too
// fast to re-render as HTML.
//
// Routing uses the standard library's method-aware ServeMux (Go 1.22+); no
// third-party router is needed.
package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"kite-algo/internal/app"
)

// Options configures the server.
type Options struct {
	// Dev re-reads templates and static assets from disk per request.
	Dev bool
}

// Server is the HTTP front end.
type Server struct {
	app    *app.App
	log    *slog.Logger
	render *Renderer
	hub    *Hub
	opts   Options
	http   *http.Server
}

// New builds the server and its routes.
func New(a *app.App, log *slog.Logger, opts Options) (*Server, error) {
	r, err := NewRenderer(opts.Dev)
	if err != nil {
		return nil, err
	}
	s := &Server{
		app:    a,
		log:    log,
		render: r,
		hub:    NewHub(a.Bus, a.Engine, a.Cfg.TickInterval(), log),
		opts:   opts,
	}
	s.http = &http.Server{
		Addr:              a.Cfg.Web.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the market-data WebSocket is a long-lived hijacked
		// connection and a write deadline here would sever it. Per-write
		// deadlines are applied on that connection instead.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// routes builds the mux. Middleware is applied per group rather than globally
// so the login page and the health probe stay reachable when unauthenticated.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// --- public ---
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.Handle("GET /static/", s.staticHandler())

	// --- authenticated ---
	page := func(h http.HandlerFunc) http.Handler {
		return chain(h, s.noStore, s.requireAuth, s.requireCSRF)
	}

	mux.Handle("GET /{$}", page(s.handleDashboard))
	mux.Handle("GET /market", page(s.handleMarket))
	mux.Handle("GET /trade", page(s.handleTrade))
	mux.Handle("POST /logout", page(s.handleLogout))

	// Order entry and position management. Every one of these is a
	// state-changing POST and therefore CSRF-protected by the `page` chain.
	// Algo control panel.
	mux.Handle("GET /strategies", page(s.handleStrategies))
	mux.Handle("GET /strategies/new", page(s.handleNewStrategy))
	mux.Handle("GET /risk", page(s.handleRisk))
	mux.Handle("GET /research", page(s.handleResearch))
	mux.Handle("GET /backtest", page(s.handleBacktest))
	mux.Handle("POST /backtest", page(s.handleBacktest))
	mux.Handle("GET /api/candles", page(s.handleCandlesJSON))
	mux.Handle("POST /api/strategies", page(s.handleStartStrategy))
	mux.Handle("POST /api/strategies/{id}/stop", page(s.handleStopStrategy))
	mux.Handle("POST /api/halt", page(s.handleHalt))
	mux.Handle("POST /api/resume", page(s.handleResume))
	mux.Handle("POST /api/risk/limits", page(s.handleSetRiskLimits))

	mux.Handle("POST /api/orders", page(s.handlePlaceOrder))
	mux.Handle("POST /api/orders/{id}/cancel", page(s.handleCancelOrder))
	mux.Handle("POST /api/positions/squareoff", page(s.handleSquareOff))
	mux.Handle("GET /api/instruments", page(s.handleInstrumentSearch))

	// Live market-data channel. The upgrade is a GET so CSRF does not apply;
	// it is gated by requireAuth and by an explicit Origin check in ws.go.
	mux.Handle("GET /ws", chain(http.HandlerFunc(s.handleWS), s.requireAuth))

	// Zerodha login round-trip.
	mux.Handle("GET /connect", page(s.handleConnect))
	mux.Handle("GET /kite/login", page(s.handleKiteLogin))
	mux.Handle("GET /kite/callback", page(s.handleKiteCallback))
	mux.Handle("POST /kite/disconnect", page(s.handleKiteDisconnect))

	// Fragments polled by app.js — the correctness baseline behind the socket.
	mux.Handle("GET /partials/status", page(s.handleStatusFragment))
	mux.Handle("GET /partials/positions", page(s.handlePositionsFragment))
	mux.Handle("GET /partials/watchlist", page(s.handleWatchlistFragment))
	mux.Handle("GET /partials/orders", page(s.handleOrdersFragment))
	mux.Handle("GET /partials/strategies", page(s.handleStrategiesFragment))

	return chain(mux, s.recoverPanic, s.accessLog, s.secureHeaders)
}

// staticHandler serves embedded assets, or disk assets in dev mode.
func (s *Server) staticHandler() http.Handler {
	var src fs.FS = staticFS
	if s.opts.Dev {
		if _, err := os.Stat("internal/web/static"); err == nil {
			src = os.DirFS("internal/web")
		}
	}
	sub, err := fs.Sub(src, "static")
	if err != nil {
		// Only reachable if the embed directive and the directory disagree,
		// which is a build-time mistake rather than a runtime condition.
		panic(fmt.Sprintf("web: static assets unavailable: %v", err))
	}
	h := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Dev {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		h.ServeHTTP(w, r)
	})
}

// Start listens and serves until Shutdown. It returns nil on a clean shutdown.
//
// ctx bounds the lifetime of the event hub; cancel it to release the hub's
// engine subscription and disconnect every browser.
func (s *Server) Start(ctx context.Context) error {
	go s.hub.Run(ctx)
	return s.serve()
}

func (s *Server) serve() error {
	s.log.Info("web ui listening",
		"addr", s.app.Cfg.Web.Addr,
		"public_url", s.app.Cfg.Web.PublicURL,
		"kite_redirect_url", s.app.Cfg.KiteRedirectURL())

	if !s.app.Cfg.HasWebPassword() {
		s.log.Warn("NO WEB PASSWORD SET — run 'tradebot -set-password' before exposing this beyond localhost")
	}

	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
