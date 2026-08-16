package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kite-algo/internal/history"
)

// captureView is the status panel on /options.
type captureView struct {
	Enabled bool
	// Ready reports whether a Zerodha session exists to capture through.
	Ready bool
	// Target is the day a manual run would capture — the most recent trading
	// day, which on a weekend is NOT today.
	Target   string
	IsToday  bool
	RunAt    string
	Status   history.CaptureStatus
	CSRF     string
	Interval string
}

// captureStatusView assembles the panel.
func (s *Server) captureStatusView(r *http.Request) captureView {
	sess, _ := sessionFrom(r)
	v := captureView{
		Enabled:  s.app.Cfg.Capture.Enabled,
		RunAt:    s.app.Cfg.Capture.RunAt,
		Interval: s.app.Cfg.Capture.Interval,
		CSRF:     sess.CSRFToken,
		Ready: s.app.Kite.Snapshot().Connected() &&
			s.app.Kite.Client() != nil && s.app.Kite.Instruments() != nil,
	}
	if sched := s.app.Capture(); sched != nil {
		v.Status = sched.Status()
	}

	cal := history.NSE()
	cal.SetHolidays(s.app.Cfg.Capture.Holidays)
	now := time.Now().In(history.IST)
	if day, ok := cal.MostRecentTradingDay(now); ok {
		v.Target = day.Format("2006-01-02")
		v.IsToday = v.Target == now.Format("2006-01-02")
	}
	return v
}

// handleCaptureFragment re-renders the status panel for polling.
func (s *Server) handleCaptureFragment(w http.ResponseWriter, r *http.Request) {
	v := s.captureStatusView(r)
	if err := s.render.Render(w, http.StatusOK, "capture_fragment.html", v); err != nil {
		s.log.Error("render capture fragment failed", "err", err)
	}
}

// handleCaptureRun triggers a capture immediately.
//
// The run is started in the background and the handler returns at once. A full
// pass is ~660 rate-limited requests — minutes, not milliseconds — and holding
// the response open for it would hit every proxy and browser timeout in the
// path, leaving the operator unable to tell a slow capture from a failed one.
// Progress is read back from the polling panel instead.
func (s *Server) handleCaptureRun(w http.ResponseWriter, r *http.Request) {
	sched := s.app.Capture()
	if sched == nil {
		s.actionResult(w, http.StatusOK, "error",
			"Capture is disabled — set capture.enabled: true in config.yaml and restart.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}

	cal := history.NSE()
	cal.SetHolidays(s.app.Cfg.Capture.Holidays)

	day, err := captureTargetDay(cal, r.FormValue("date"))
	if err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", err.Error())
		return
	}

	// Detach from the request: the response is written in a moment, and a
	// context tied to it would cancel the capture partway through the first
	// contract. Values (request-scoped logging) are preserved.
	ctx := context.WithoutCancel(r.Context())

	go func() {
		rep, err := sched.RunNow(ctx, day)
		switch {
		case err != nil:
			s.log.Error("manual capture failed", "day", day.Format("2006-01-02"), "err", err)
		case rep.Skipped != "":
			s.log.Warn("manual capture skipped", "day", day.Format("2006-01-02"), "reason", rep.Skipped)
		default:
			s.log.Info("manual capture complete",
				"day", day.Format("2006-01-02"), "contracts", rep.Contracts,
				"candles", rep.Candles, "failures", rep.Failures,
				"took", rep.Duration.Round(time.Second))
		}
	}()

	s.actionResult(w, http.StatusOK, "ok", fmt.Sprintf(
		"Capturing %s in the background. With a 30-day lookback this is a few "+
			"minutes of rate-limited requests — the panel updates as it runs.",
		day.Format("2006-01-02")))
}

// captureTargetDay resolves the requested day, defaulting to the most recent
// trading day.
//
// An explicitly requested non-trading day is rejected rather than silently
// snapped to a neighbour: capture would skip it, and reporting success for a
// day that was never captured is the failure mode this whole subsystem exists
// to avoid.
func captureTargetDay(cal *history.Calendar, raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		day, ok := cal.MostRecentTradingDay(time.Now())
		if !ok {
			return time.Time{}, fmt.Errorf("no trading day found in the last fortnight")
		}
		return day, nil
	}

	day, err := time.ParseInLocation("2006-01-02", raw, history.IST)
	if err != nil {
		return time.Time{}, fmt.Errorf("'date' must be a date like 2026-08-14")
	}
	if !cal.IsTradingDay(day) {
		return time.Time{}, fmt.Errorf(
			"%s is a %s — the exchange was shut, so there is nothing to capture",
			day.Format("2006-01-02"), day.Weekday())
	}
	if day.After(time.Now().In(history.IST)) {
		return time.Time{}, fmt.Errorf("%s is in the future", day.Format("2006-01-02"))
	}
	return day, nil
}
