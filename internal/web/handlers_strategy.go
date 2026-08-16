package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"kite-algo/internal/engine"
	"kite-algo/internal/risk"
	"kite-algo/internal/strategy"
)

type strategyData struct {
	Running   []engine.StrategyStatus
	Available []strategy.Descriptor
	Halt      engine.HaltState
}

// handleStrategies renders the algo control panel.
func (s *Server) handleStrategies(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "strategies.html", "Strategies", s.strategyData())
}

func (s *Server) strategyData() strategyData {
	return strategyData{
		Running:   s.app.Engine.ListStrategies(),
		Available: strategy.Default.List(),
		Halt:      s.app.Engine.HaltState(),
	}
}

// handleStrategiesFragment is the polled fallback for the strategy cards.
func (s *Server) handleStrategiesFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r, "strategies_fragment.html", s.strategyData())
}

// strategyFormData drives the strategy start form.
type strategyFormData struct {
	Type    string
	Title   string
	Summary string
	Params  []paramField
}

// handleNewStrategy renders the configuration form for one strategy type.
//
// The form is generated entirely from the descriptor's ParamSpec list, so
// adding a strategy never requires touching a template.
func (s *Server) handleNewStrategy(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	desc, ok := strategy.Default.Get(typ)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Unknown strategy",
			fmt.Sprintf("No strategy type named %q is registered.", typ))
		return
	}
	s.renderPage(w, r, "strategy_new.html", "Configure "+desc.Title, strategyFormData{
		Type:    desc.Type,
		Title:   desc.Title,
		Summary: desc.Summary,
		Params:  paramFields(desc, nil, false),
	})
}

// handleStartStrategy validates the submitted parameters and starts an instance.
func (s *Server) handleStartStrategy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}

	typ := strings.TrimSpace(r.FormValue("type"))
	desc, ok := strategy.Default.Get(typ)
	if !ok {
		s.actionResult(w, http.StatusOK, "error", "Unknown strategy type "+typ+".")
		return
	}

	raw := collectParams(desc, r.Form)

	instanceID := strings.TrimSpace(r.FormValue("instance_id"))
	if instanceID == "" {
		instanceID = typ
	}

	st, err := s.app.Engine.StartStrategy(r.Context(), engine.StrategySpec{
		InstanceID: instanceID,
		Type:       typ,
		Params:     raw,
	})
	if err != nil {
		if msg, ok := paramProblems(err); ok {
			s.actionResult(w, http.StatusOK, "error", msg)
			return
		}
		s.actionResult(w, http.StatusOK, "error", "Could not start: "+err.Error())
		return
	}

	s.log.Warn("strategy started from ui",
		"id", instanceID, "type", typ, "ip", s.clientIP(r))
	s.actionResult(w, http.StatusOK, "ok",
		fmt.Sprintf("Strategy %q started (%s).", st.InstanceID, st.State))
}

// handleStopStrategy stops an instance, with or without squaring off.
func (s *Server) handleStopStrategy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}

	// Square-off is an explicit per-stop choice presented as two buttons, never
	// a default. Leaving short options open unattended is dangerous; closing
	// them without being asked is an unrequested trade. Only the operator can
	// weigh that, and only they know which they meant.
	squareOff := r.FormValue("square_off") == "true"

	st, err := s.app.Engine.StopStrategy(r.Context(), id, engine.StopOptions{
		SquareOff: squareOff,
		Reason:    "stopped by operator",
	})
	if err != nil {
		// A flatten failure still stopped the strategy — say so precisely,
		// because the operator now has open positions nothing is managing.
		s.actionResult(w, http.StatusOK, "error", fmt.Sprintf(
			"Strategy %q stopped, but squaring off FAILED: %s — check your positions now.", id, err))
		return
	}

	s.log.Warn("strategy stopped from ui",
		"id", id, "square_off", squareOff, "ip", s.clientIP(r))

	msg := fmt.Sprintf("Strategy %q stopped.", st.InstanceID)
	if squareOff {
		msg += " Positions squared off."
	} else if len(st.Positions) > 0 {
		msg += fmt.Sprintf(" %d position(s) left open and now unmanaged.", len(st.Positions))
	}
	s.actionResult(w, http.StatusOK, "ok", msg)
}

// handleHalt is the kill switch.
func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}
	squareOff := r.FormValue("square_off") == "true"
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "halted by operator"
	}

	state, errs := s.app.Engine.Halt(r.Context(), engine.HaltOptions{
		Reason:         reason,
		By:             "operator",
		StopStrategies: true,
		SquareOffAll:   squareOff,
	})
	s.log.Warn("KILL SWITCH activated from ui",
		"square_off", squareOff, "reason", reason, "ip", s.clientIP(r))

	if len(errs) > 0 {
		s.actionResult(w, http.StatusOK, "error", fmt.Sprintf(
			"Trading HALTED and strategies stopped, but %d action(s) failed: %s — check your positions now.",
			len(errs), joinErrs(errs)))
		return
	}
	msg := "Trading HALTED. All strategies stopped."
	if state.SquaredOff {
		msg += " All positions squared off."
	} else if squareOff {
		msg += " Nothing was open to square off."
	}
	s.actionResult(w, http.StatusOK, "ok", msg)
}

// handleResume lifts a halt.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.app.Engine.Resume("operator")
	s.log.Warn("trading resumed from ui", "ip", s.clientIP(r))
	s.actionResult(w, http.StatusOK, "ok",
		"Trading resumed. Strategies stopped by the halt were NOT restarted — start them explicitly.")
}

// riskData drives the risk-limit editor.
type riskData struct {
	// Limits and Defaults are the SIMULATED book's — the only ones this page
	// may change.
	Limits     risk.Limits
	Defaults   risk.Limits
	Overridden bool

	// LiveLimits and LivePolicy are shown READ-ONLY. The real book's daily loss
	// is derived from the account's opening balance, and none of it is editable
	// here: a limit you can loosen from a browser at the moment it starts
	// hurting is not a limit.
	LiveLimits risk.Limits
	LivePolicy string
	LockedDay  string
	LockReason string
}

// handleRisk renders the risk-limit editor.
func (s *Server) handleRisk(w http.ResponseWriter, r *http.Request) {
	d := riskData{
		Limits:     s.app.PaperRisk.Limits(),
		Defaults:   s.app.ConfiguredRiskLimits(),
		Overridden: s.app.RiskOverridden(),
		LiveLimits: s.app.Risk.Limits(),
	}
	if s.app.LiveRisk != nil {
		d.LivePolicy = s.app.LiveRisk.Describe()
		d.LockedDay, d.LockReason = s.app.LiveRisk.Lockout()
	}
	s.renderPage(w, r, "risk.html", "Risk", d)
}

// handleSetRiskLimits applies edited limits at runtime.
func (s *Server) handleSetRiskLimits(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.actionResult(w, http.StatusBadRequest, "error", "Malformed form submission.")
		return
	}

	current := s.app.PaperRisk.Limits()
	next := current
	var problems []string

	if v, err := parseNonNegative(r.FormValue("max_daily_loss")); err != nil {
		problems = append(problems, "daily loss limit "+err.Error())
	} else {
		next.MaxDailyLoss = v
	}
	if v, err := parseNonNegative(r.FormValue("max_order_value")); err != nil {
		problems = append(problems, "order value limit "+err.Error())
	} else {
		next.MaxOrderValue = v
	}
	if v, err := parseNonNegativeInt(r.FormValue("max_lots_per_trade")); err != nil {
		problems = append(problems, "lots per trade "+err.Error())
	} else {
		next.MaxLotsPerTrade = v
	}
	if v, err := parseNonNegativeInt(r.FormValue("max_open_positions")); err != nil {
		problems = append(problems, "open positions "+err.Error())
	} else {
		next.MaxOpenPositions = v
	}

	if len(problems) > 0 {
		s.actionResult(w, http.StatusOK, "error", "Check these: "+strings.Join(problems, "; "))
		return
	}

	if err := s.app.SaveRiskLimits(r.Context(), next); err != nil {
		// The limits are live either way; only persistence failed. Report that
		// precisely rather than implying the change did not take.
		s.log.Error("persist risk limits failed", "err", err)
		s.actionResult(w, http.StatusOK, "error", err.Error())
		return
	}
	s.log.Warn("risk limits changed from ui", "ip", s.clientIP(r))

	msg := "Risk limits updated and saved. They apply to the next order."
	if loosened(current, next) {
		// Loosening a limit is the change most likely to be regretted, so it is
		// called out rather than acknowledged with a generic success message.
		msg += " Note: you LOOSENED at least one limit."
	}
	s.actionResult(w, http.StatusOK, "ok", msg)
}

// handleResetRiskLimits discards the saved override and returns to config.yaml.
func (s *Server) handleResetRiskLimits(w http.ResponseWriter, r *http.Request) {
	if err := s.app.ResetRiskLimits(r.Context()); err != nil {
		s.actionResult(w, http.StatusOK, "error", err.Error())
		return
	}
	s.log.Warn("risk limits reset to config defaults", "ip", s.clientIP(r))
	s.actionResult(w, http.StatusOK, "ok",
		"Risk limits reset to the values in config.yaml.")
}

func loosened(before, after risk.Limits) bool {
	return after.MaxDailyLoss > before.MaxDailyLoss ||
		after.MaxOrderValue > before.MaxOrderValue ||
		after.MaxLotsPerTrade > before.MaxLotsPerTrade ||
		after.MaxOpenPositions > before.MaxOpenPositions
}

func parseNonNegative(v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, errors.New("must be a number")
	}
	if f < 0 {
		return 0, errors.New("cannot be negative")
	}
	return f, nil
}

func parseNonNegativeInt(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, errors.New("must be a whole number")
	}
	if n < 0 {
		return 0, errors.New("cannot be negative")
	}
	return n, nil
}

// actionResult renders the outcome banner for a control-panel action.
func (s *Server) actionResult(w http.ResponseWriter, status int, kind, message string) {
	v := struct {
		Kind    string
		Message string
	}{Kind: kind, Message: message}
	if err := s.render.Render(w, status, "order_result.html", v); err != nil {
		s.log.Error("render action result failed", "err", err)
		http.Error(w, message, http.StatusInternalServerError)
	}
}
