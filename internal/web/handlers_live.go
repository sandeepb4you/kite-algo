package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"kite-algo/internal/broker"
)

// The live desk: the one page in the application where an order can reach the
// exchange.
//
// Separating it from /trade by PAGE rather than by a control on the shared
// ticket is the whole safety argument. Which book an order lands in is decided
// by the endpoint it was posted to, not by a form value — and a form value can
// be mis-set, mis-read, restored by a browser, or replayed, while a URL cannot
// be any of those by accident. /trade posts to /api/orders and is always
// simulated; this page posts to /api/live/orders and is always real.
//
// The arming gate lives here too, rather than on a settings page, so the
// confirmation sits on the screen it governs. Until it is passed this page
// shows the gate and no ticket at all.

// liveData drives the live desk.
//
// It EMBEDS tradeData so the desk renders from the same body template as the
// terminal — same ticket, same option chain, same positions and order book.
// Keeping a second, simplified copy of that markup would let the two drift, and
// a real ticket that has quietly diverged from the paper one is precisely the
// difference that produces a mis-click.
type liveData struct {
	tradeData

	// Armed reports whether live routing is installed. When false the page
	// renders the confirmation gate and no ticket at all.
	Armed bool
	// Configured reports mode: live + live_confirm: true. Without it there is
	// nothing to confirm and the page explains what to change.
	Configured bool
	// SessionOK reports an active Zerodha session, which arming requires.
	SessionOK bool
	Mode      string
	// LockedDay is set when the daily loss cap has barred entries for today.
	LockedDay  string
	LockReason string
}

// handleLive renders the live desk.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "live.html", "Live", s.liveData(r))
}

func (s *Server) liveData(r *http.Request) liveData {
	st := s.app.Status()

	// Force the live page identity regardless of what the query said, so the
	// desk can never render pointing at the paper endpoints.
	q := r.URL.Query()
	q.Set("page", "live")
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()

	d := liveData{
		tradeData:  s.tradeData(r2),
		Armed:      s.app.LiveActive(),
		Configured: st.LiveArmed || st.LiveActive,
		SessionOK:  s.app.Kite.Snapshot().Connected(),
		Mode:       string(st.Mode),
	}
	if s.app.LiveRisk != nil {
		d.LockedDay, d.LockReason = s.app.LiveRisk.Lockout()
	}
	return d
}

// markPageStale tells the browser that this action changed the PAGE, not just a
// panel inside it, and that the response body is therefore not the whole story.
//
// Arming and disarming swap the entire live desk: the gate and the real ticket
// are different branches of live.html, chosen server-side. app.js normally
// renders an action's response into a result div and refreshes the polled
// regions, which is right for placing an order and wrong for this — after a
// disarm the page went on showing the LIVE banner and a real order ticket, with
// the arming form nowhere to be seen, until someone reloaded by hand. The
// engine refused those orders (handlePlaceLiveOrder re-checks LiveActive), so
// nothing could reach the exchange, but a desk that displays REAL MONEY after
// you have stood down is exactly the wrong thing to be wrong about.
//
// Must be called BEFORE the response is written; headers are frozen after that.
func markPageStale(w http.ResponseWriter) {
	w.Header().Set("X-Page-Stale", "1")
}

// handleLiveConfirm is the final gate: it installs live routing.
//
// Guarded by the same login limiter as the password form. Without that this is
// a password oracle with no lockout — an attacker who already has a session
// could brute-force the operator password here at full speed.
func (s *Server) handleLiveConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}
	ip := s.clientIP(r)
	if ok, retry := s.app.Guard.Allow(ip); !ok {
		s.log.Warn("live confirmation attempt while locked out", "ip", ip, "retry_in", retry)
		s.actionResult(w, http.StatusTooManyRequests, "error",
			"Too many failed attempts. Try again in "+retry.Round(time.Second).String()+".")
		return
	}

	phrase := strings.TrimSpace(r.FormValue("phrase"))
	password := r.FormValue("password")

	if err := s.app.ConfirmLive(r.Context(), phrase, password); err != nil {
		if lock := s.app.Guard.Fail(ip); lock > 0 {
			s.log.Warn("live confirmation locked out after repeated failures", "ip", ip, "lockout", lock)
		} else {
			s.log.Warn("live confirmation refused", "ip", ip, "err", err)
		}
		s.actionResult(w, http.StatusOK, "error", err.Error())
		return
	}
	s.app.Guard.Succeed(ip)

	s.log.Warn("LIVE ROUTING ARMED by operator", "ip", ip)
	// Only on success: a refused attempt must keep its error message on screen
	// rather than reloading it away, and the page is not stale in that case
	// because nothing changed.
	markPageStale(w)
	s.actionResult(w, http.StatusOK, "ok",
		"Live routing armed. Orders placed on this page now reach the exchange. "+
			"Strategies remain simulated.")
}

// handleLiveDisarm returns manual routing to the paper broker.
//
// Deliberately asymmetric with confirm: no phrase, no password, one click.
// Standing down is a de-escalation and must never be harder than escalating,
// or an operator who wants to stop is fighting the interface to do it.
func (s *Server) handleLiveDisarm(w http.ResponseWriter, r *http.Request) {
	s.app.DisarmLive(r.Context())
	s.log.Warn("live routing disarmed by operator", "ip", s.clientIP(r))
	markPageStale(w)
	s.actionResult(w, http.StatusOK, "ok",
		"Live routing disarmed. New manual orders are simulated again. "+
			"Open positions are untouched.")
}

// handlePlaceLiveOrder submits a REAL order.
//
// The book is stamped here, by the handler, from the route the request came in
// on. It is never read from the form: an order is real because it was posted to
// this endpoint, which is a fact about the request rather than a value inside
// it. The engine still re-checks that live routing is armed (engine.bookFor),
// so reaching this handler is necessary but not sufficient.
func (s *Server) handlePlaceLiveOrder(w http.ResponseWriter, r *http.Request) {
	if !s.app.LiveActive() {
		s.orderResult(w, http.StatusOK, "error",
			"Live routing is not armed. Confirm on this page first.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.orderResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}
	if halt := s.app.Engine.HaltState(); halt.Halted {
		s.orderResult(w, http.StatusOK, "error",
			"Trading is HALTED ("+halt.Reason+"). Resume on the Strategies page to trade again.")
		return
	}

	req, err := s.parseOrderForm(r)
	if err != nil {
		s.orderResult(w, http.StatusOK, "error", err.Error())
		return
	}
	req.Book = broker.BookReal
	req.Tag = "manual/live"

	o, err := s.app.Engine.PlaceManualOrder(r.Context(), req)
	if err != nil {
		s.orderResult(w, http.StatusOK, "error", err.Error())
		return
	}
	s.log.Warn("REAL ORDER PLACED", "id", o.ID, "symbol", o.TradingSymbol,
		"side", o.Side, "qty", o.Quantity, "ip", s.clientIP(r))
	s.orderResult(w, http.StatusOK, "ok",
		"REAL order "+string(o.Side)+" "+o.TradingSymbol+" x"+itoa(o.Quantity)+
			" sent to the exchange — "+string(o.Status)+".")
}

func itoa(n int) string { return strconv.Itoa(n) }
