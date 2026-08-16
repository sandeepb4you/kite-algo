package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kite-algo/internal/backtest"
	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"
)

// backtestData drives the backtest page.
type backtestData struct {
	Available []strategy.Descriptor
	Intervals []kite.Interval
	Paths     []backtest.BarPath

	// Submitted form values, echoed back so a re-run keeps its settings.
	StrategyType string
	Symbols      string
	Interval     string
	From         string
	To           string
	BarPath      string
	Capital      string

	// Params are the chosen strategy's declared parameters, rendered as form
	// fields. Empty until a strategy is chosen — a strategy's parameters are
	// not knowable before then.
	Params []paramField

	Result *backtest.Result
	Error  string
}

// handleBacktest renders the form and, when submitted, the run.
func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	today := time.Now().In(history.IST)

	d := backtestData{
		Available:    strategy.Default.List(),
		Intervals:    kite.Intervals,
		Paths:        []backtest.BarPath{backtest.PathPessimist, backtest.PathOHLC, backtest.PathCloseOnly},
		StrategyType: r.FormValue("strategy"),
		Symbols:      r.FormValue("symbols"),
		Interval:     r.FormValue("interval"),
		From:         r.FormValue("from"),
		To:           r.FormValue("to"),
		BarPath:      r.FormValue("bar_path"),
		Capital:      r.FormValue("capital"),
	}
	if d.Interval == "" {
		d.Interval = string(kite.Interval5Minute)
	}
	if d.From == "" {
		d.From = today.AddDate(0, 0, -7).Format("2006-01-02")
	}
	if d.To == "" {
		d.To = today.Format("2006-01-02")
	}
	if d.BarPath == "" {
		d.BarPath = string(backtest.PathPessimist)
	}
	if d.Capital == "" {
		d.Capital = "100000"
	}
	// Echo the parameters back on a POST so a run rejected for any reason —
	// bad dates, missing data, an out-of-range parameter — returns the form the
	// operator submitted rather than one silently reset to the defaults.
	if desc, ok := strategy.Default.Get(d.StrategyType); ok {
		d.Params = paramFields(desc, r.Form, r.Method == http.MethodPost)
	}

	if r.Method == http.MethodPost {
		res, err := s.runBacktest(r, &d)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Result = res
		}
	}

	s.renderPage(w, r, "backtest.html", "Backtest", d)
}

// handleBacktestParamsFragment renders the parameter fields for one strategy.
//
// app.js swaps this in when the strategy select changes. An unknown or empty
// type renders the prompt rather than an error: the fragment is a convenience,
// and the page it enhances still works without it.
func (s *Server) handleBacktestParamsFragment(w http.ResponseWriter, r *http.Request) {
	var fields []paramField
	if desc, ok := strategy.Default.Get(r.URL.Query().Get("strategy")); ok {
		fields = paramFields(desc, nil, false)
	}
	s.renderFragment(w, r, "backtest_params.html", fields)
}

// runBacktest validates the form and executes the run.
//
// The run is synchronous. A few days of 5-minute bars completes in well under a
// second, and a synchronous request keeps the result trivially correlated with
// the form that produced it. A long minute-resolution run over months would want
// a job queue; that is worth building when someone actually needs it, not before.
func (s *Server) runBacktest(r *http.Request, d *backtestData) (*backtest.Result, error) {
	if d.StrategyType == "" {
		return nil, fmt.Errorf("choose a strategy")
	}
	desc, ok := strategy.Default.Get(d.StrategyType)
	if !ok {
		return nil, fmt.Errorf("unknown strategy %q", d.StrategyType)
	}

	// Strategy parameters come from the form, defaulted and range-checked by the
	// descriptor — the same path the live start form takes, so a backtest cannot
	// be configured in a way the live instance would reject.
	//
	// Checked before the data plumbing on purpose: a mistyped parameter is the
	// operator's to fix either way, and reporting it should not depend on
	// whether a history provider happens to be connected. Normalizing here
	// rather than leaving it to the runner is what names the offending fields.
	params, err := desc.Normalize(collectParams(desc, r.Form))
	if err != nil {
		if msg, ok := paramProblems(err); ok {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, err
	}

	interval, ok := kite.ParseInterval(d.Interval)
	if !ok {
		return nil, fmt.Errorf("%q is not a Kite interval", d.Interval)
	}

	from, err := time.ParseInLocation("2006-01-02", d.From, history.IST)
	if err != nil {
		return nil, fmt.Errorf("'from' must be a date like 2024-08-01")
	}
	to, err := time.ParseInLocation("2006-01-02", d.To, history.IST)
	if err != nil {
		return nil, fmt.Errorf("'to' must be a date like 2024-08-08")
	}
	to = to.AddDate(0, 0, 1) // make the end date inclusive
	if !to.After(from) {
		return nil, fmt.Errorf("'to' must be on or after 'from'")
	}

	var symbols []string
	for _, s := range strings.Split(d.Symbols, ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			symbols = append(symbols, s)
		}
	}
	if len(symbols) == 0 {
		return nil, fmt.Errorf("give at least one seed symbol for the strategy to watch")
	}

	capital, err := strconv.ParseFloat(d.Capital, 64)
	if err != nil || capital <= 0 {
		return nil, fmt.Errorf("capital must be a positive number")
	}
	store, ok := s.app.Store.(storage.HistoryStore)
	if !ok {
		return nil, fmt.Errorf("this storage backend cannot serve historical data")
	}
	provider, _ := s.historyProvider()
	if provider == nil {
		return nil, fmt.Errorf("no historical data provider available")
	}

	cfg := backtest.Config{
		StrategyType:   d.StrategyType,
		InstanceID:     d.StrategyType + "-backtest",
		Params:         params,
		Symbols:        symbols,
		Interval:       interval,
		From:           from,
		To:             to,
		BarPath:        backtest.BarPath(d.BarPath),
		Costs:          backtest.DefaultNSEOptionCosts(),
		Risk:           s.app.Risk.Limits(),
		InitialCapital: capital,
	}
	// A backtest must not inherit a daily-loss halt from the live session: the
	// limit protects today's capital, not a simulation of last month.
	cfg.Risk = risk.Limits{
		MaxOrderValue:    cfg.Risk.MaxOrderValue,
		MaxLotsPerTrade:  cfg.Risk.MaxLotsPerTrade,
		MaxOpenPositions: cfg.Risk.MaxOpenPositions,
	}

	runner, err := backtest.New(cfg, strategy.Default, provider, store, s.log)
	if err != nil {
		return nil, err
	}

	s.log.Info("running backtest",
		"strategy", d.StrategyType, "symbols", symbols,
		"from", d.From, "to", d.To, "interval", interval)

	res, err := runner.Run(r.Context())
	if err != nil {
		return nil, err
	}
	return res, nil
}
