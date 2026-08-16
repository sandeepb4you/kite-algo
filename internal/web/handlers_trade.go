package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/engine"
	"kite-algo/internal/risk"
)

// defaultChainUnderlying is what the terminal shows before you pick one.
const defaultChainUnderlying = "NIFTY"

// tradeData is the terminal page payload.
type tradeData struct {
	Watchlist []quoteRow
	Positions []broker.Position
	Orders    []broker.Order
	Chain     engine.OptionChain
	ChainErr  string
	Streaming bool
	Routing   string
	LiveMode  bool

	// TicketSymbol pre-fills the order ticket. Clicking a premium in the chain
	// submits it as ?symbol=, so selecting a contract works entirely
	// server-side — no JavaScript required.
	TicketSymbol string
	TicketLot    int
}

// handleTrade renders the manual trading terminal.
func (s *Server) handleTrade(w http.ResponseWriter, r *http.Request) {
	if !s.app.Kite.Snapshot().Connected() {
		http.Redirect(w, r, "/connect", http.StatusSeeOther)
		return
	}
	s.renderPage(w, r, "trade.html", "Terminal", s.tradeData(r))
}

// handleChainFragment is the polled fallback for the option chain.
func (s *Server) handleChainFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r, "chain_fragment.html", s.tradeData(r))
}

func (s *Server) tradeData(r *http.Request) tradeData {
	orders, err := s.app.Engine.OpenOrders(r.Context())
	if err != nil {
		s.log.Debug("fetch open orders failed", "err", err)
	}

	d := tradeData{
		Watchlist: s.watchlist(),
		Positions: s.app.Engine.Positions(),
		Orders:    orders,
		Streaming: s.app.Engine.HasMarketData(),
		Routing:   s.app.Engine.BrokerMode(),
		LiveMode:  s.app.LiveActive(),
	}

	// A contract picked from the chain arrives as ?symbol=.
	if sym := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol"))); sym != "" {
		d.TicketSymbol = sym
		d.TicketLot = s.app.Engine.LotSize(sym)
	}

	underlying := strings.ToUpper(strings.TrimSpace(r.FormValue("underlying")))
	if underlying == "" {
		underlying = defaultChainUnderlying
	}
	var expiry time.Time
	if v := strings.TrimSpace(r.FormValue("expiry")); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			expiry = t
		}
	}

	chain, err := s.app.Engine.OptionChain(underlying, expiry, engine.DefaultChainDepth)
	if err != nil {
		d.ChainErr = err.Error()
		d.Chain = chain // still carries the underlying/expiry lists for the selectors
		return d
	}
	d.Chain = chain

	// Stream exactly the contracts on screen. These are transient — closing the
	// tab releases them — and Engine.Unsubscribe never touches a symbol a
	// strategy pinned, so this cannot blind a running strategy.
	if err := s.app.Engine.SubscribeTransient(chain.ChainSymbols()); err != nil {
		s.log.Debug("subscribe option chain failed", "err", err)
	}
	return d
}

// handleOrdersFragment is the polled fallback for the order book.
func (s *Server) handleOrdersFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r, "orders_fragment.html", s.tradeData(r))
}

// handlePlaceOrder submits a hand-entered order.
//
// Everything goes through Engine.PlaceManualOrder, so a typed order is
// risk-checked, persisted, and published exactly like a strategy's. Responses
// are HTML fragments rendered into the page's result panel.
func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.orderResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}

	// Report a halt before validating the ticket. The engine blocks the order
	// anyway, but only after the form has been checked — so an operator trying
	// to trade during a halt would otherwise be told about a missing lot size
	// or a stray price field and never learn the real reason.
	if halt := s.app.Engine.HaltState(); halt.Halted {
		s.orderResult(w, http.StatusOK, "error",
			"Trading is HALTED ("+halt.Reason+"). Resume on the Strategies page to trade again.")
		return
	}

	req, err := s.parseOrderForm(r)
	if err != nil {
		s.orderResult(w, http.StatusBadRequest, "error", err.Error())
		return
	}

	// Placing real orders demands the operator have deliberately armed live
	// mode; this is a second check behind app.ConfirmLive so a stale page
	// cannot submit into a session that was disarmed since it loaded.
	order, err := s.app.Engine.PlaceManualOrder(r.Context(), req)
	if err != nil {
		var re *risk.RiskError
		if errors.As(err, &re) {
			// Name the rule: "rejected" alone leaves the operator guessing which
			// limit they hit and whether to change the order or the limit.
			s.orderResult(w, http.StatusOK, "error",
				fmt.Sprintf("Blocked by risk rule %q: %s", re.Rule, re.Message))
			return
		}
		s.orderResult(w, http.StatusOK, "error", "Order failed: "+err.Error())
		return
	}

	s.log.Info("manual order placed",
		"symbol", order.TradingSymbol, "side", order.Side, "qty", order.Quantity,
		"mode", order.Mode, "id", order.ID, "ip", s.clientIP(r))

	s.orderResult(w, http.StatusOK, "ok", fmt.Sprintf(
		"%s order placed: %s %s %d @ %s — status %s",
		strings.ToUpper(order.Mode), order.Side, order.TradingSymbol,
		order.Quantity, priceLabel(order), order.Status))
}

// parseOrderForm validates and builds the order request.
func (s *Server) parseOrderForm(r *http.Request) (broker.OrderRequest, error) {
	var req broker.OrderRequest

	symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
	if symbol == "" {
		return req, errors.New("choose an instrument")
	}

	side := broker.Side(strings.ToUpper(r.FormValue("side")))
	if side != broker.SideBuy && side != broker.SideSell {
		return req, errors.New("side must be BUY or SELL")
	}

	orderType := broker.OrderType(strings.ToUpper(r.FormValue("order_type")))
	switch orderType {
	case broker.OrderTypeMarket, broker.OrderTypeLimit, broker.OrderTypeSL, broker.OrderTypeSLM:
	default:
		return req, errors.New("order type must be MARKET, LIMIT, SL or SL-M")
	}

	product := broker.ProductType(strings.ToUpper(r.FormValue("product")))
	switch product {
	case broker.ProductMIS, broker.ProductNRML, broker.ProductCNC:
	default:
		return req, errors.New("product must be MIS, NRML or CNC")
	}

	lots, err := strconv.Atoi(strings.TrimSpace(r.FormValue("lots")))
	if err != nil || lots <= 0 {
		return req, errors.New("lots must be a positive whole number")
	}

	price := parseFloatField(r.FormValue("price"))
	trigger := parseFloatField(r.FormValue("trigger_price"))

	if orderType == broker.OrderTypeLimit && price <= 0 {
		return req, errors.New("a LIMIT order needs a price")
	}
	if (orderType == broker.OrderTypeSL || orderType == broker.OrderTypeSLM) && trigger <= 0 {
		return req, errors.New("a stop-loss order needs a trigger price")
	}

	// The lot-size lookup comes last, deliberately. It is the only check here
	// that can fail for reasons outside the operator's control (no instrument
	// master loaded), and reporting that before "you forgot the limit price"
	// would send them chasing an infrastructure problem instead of fixing a
	// typo. Input mistakes first, environment problems after.
	//
	// Quantity is derived from lots rather than typed directly: entering a raw
	// share count is the classic fat-finger route to an order 75x larger than
	// intended.
	lotSize := s.app.Engine.LotSize(symbol)
	if lotSize <= 0 {
		return req, fmt.Errorf("unknown lot size for %s — is the instrument master loaded?", symbol)
	}

	// The book is an explicit per-order choice, and only "real" spelled out
	// exactly counts. Anything else — absent, misspelled, tampered with —
	// resolves to paper. The engine re-checks this against live-armed state
	// anyway (engine.bookFor); a form value can request the real book but can
	// never be what grants it.
	book := broker.BookPaper
	if strings.EqualFold(strings.TrimSpace(r.FormValue("route")), string(broker.BookReal)) {
		book = broker.BookReal
	}

	return broker.OrderRequest{
		Exchange:      s.app.Engine.ExchangeFor(symbol),
		TradingSymbol: symbol,
		Product:       product,
		OrderType:     orderType,
		Side:          side,
		Quantity:      lots * lotSize,
		Price:         price,
		TriggerPrice:  trigger,
		Validity:      broker.ValidityDay,
		Book:          book,
	}, nil
}

// handleCancelOrder cancels a pending order.
func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.orderResult(w, http.StatusBadRequest, "error", "No order id given.")
		return
	}
	if err := s.app.Engine.CancelOrder(r.Context(), id); err != nil {
		s.orderResult(w, http.StatusOK, "error", "Cancel failed: "+err.Error())
		return
	}
	s.log.Info("manual order cancelled", "id", id, "ip", s.clientIP(r))
	s.orderResult(w, http.StatusOK, "ok", "Order "+id+" cancelled.")
}

// handleSquareOff flattens one position, or every position when no symbol is
// given.
func (s *Server) handleSquareOff(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.orderResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))

	if symbol == "" {
		placed, errs := s.app.Engine.SquareOffAll(r.Context())
		s.log.Warn("square off all requested",
			"placed", len(placed), "failed", len(errs), "ip", s.clientIP(r))

		switch {
		case len(placed) == 0 && len(errs) == 0:
			s.orderResult(w, http.StatusOK, "ok", "Nothing to square off — the book is already flat.")
		case len(errs) > 0:
			// Report partial success explicitly: believing everything closed
			// when some of it did not is how a position gets carried overnight.
			s.orderResult(w, http.StatusOK, "error", fmt.Sprintf(
				"Squared off %d position(s), but %d FAILED: %s", len(placed), len(errs), joinErrs(errs)))
		default:
			s.orderResult(w, http.StatusOK, "ok",
				fmt.Sprintf("Squared off %d position(s).", len(placed)))
		}
		return
	}

	order, err := s.app.Engine.SquareOff(r.Context(), r.FormValue("strategy"), symbol)
	if err != nil {
		if errors.Is(err, engine.ErrNoPosition) {
			s.orderResult(w, http.StatusOK, "error", "No open position in "+symbol+".")
			return
		}
		s.orderResult(w, http.StatusOK, "error", "Square off failed: "+err.Error())
		return
	}
	s.log.Warn("manual square off", "symbol", symbol, "id", order.ID, "ip", s.clientIP(r))
	s.orderResult(w, http.StatusOK, "ok",
		fmt.Sprintf("Squaring off %s: %s %d.", symbol, order.Side, order.Quantity))
}

// handleInstrumentSearch powers the order ticket's typeahead.
func (s *Server) handleInstrumentSearch(w http.ResponseWriter, r *http.Request) {
	results := s.app.Engine.SearchInstruments(r.URL.Query().Get("q"), 20)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		s.log.Debug("write search results failed", "err", err)
	}
}

// orderResult renders the outcome banner shown beside the order ticket.
func (s *Server) orderResult(w http.ResponseWriter, status int, kind, message string) {
	v := struct {
		Kind    string
		Message string
	}{Kind: kind, Message: message}
	if err := s.render.Render(w, status, "order_result.html", v); err != nil {
		s.log.Error("render order result failed", "err", err)
		http.Error(w, message, http.StatusInternalServerError)
	}
}

// parseFloatField reads an optional numeric form field, treating blanks as zero.
func parseFloatField(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	return f
}

func priceLabel(o *broker.Order) string {
	if o.OrderType == broker.OrderTypeMarket || o.Price == 0 {
		return "MKT"
	}
	return fmt.Sprintf("%.2f", o.Price)
}

func joinErrs(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}
