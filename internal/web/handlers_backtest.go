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
	Lots         string

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
		Lots:         r.FormValue("lots"),
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
	if d.Lots == "" {
		d.Lots = "1"
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
	lots, err := strconv.Atoi(d.Lots)
	if err != nil || lots <= 0 {
		return nil, fmt.Errorf("lots must be a positive whole number")
	}

	store, ok := s.app.Store.(storage.HistoryStore)
	if !ok {
		return nil, fmt.Errorf("this storage backend cannot serve historical data")
	}
	provider, _ := s.historyProvider()
	if provider == nil {
		return nil, fmt.Errorf("no historical data provider available")
	}

	// Strategy parameters come from the descriptor's defaults, with lots
	// overridden. A full parameter form per strategy is the obvious next step;
	// defaults keep the first version usable without one.
	params := desc.Defaults()
	if _, declared := params["lots"]; declared {
		params["lots"] = lots
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
