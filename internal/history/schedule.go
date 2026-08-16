package history

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// CaptureScheduler runs a capture once per trading day at a fixed IST time.
//
// It is deliberately not a cron library. The only schedule needed is "after the
// close, every day the exchange traded", and the two things that actually
// matter are both things a cron expression gets wrong:
//
//   - A missed day must be noticed. If the process was down at 15:40, or the
//     operator had not logged in yet, the run has to happen when it next can.
//     Data not captured today cannot be captured tomorrow.
//   - A day must not be captured twice. Re-running is harmless for correctness
//     because coverage dedupes, but it burns historical quota for nothing.
type CaptureScheduler struct {
	runAt   time.Duration // offset into the IST day
	capture func(ctx context.Context, day time.Time) (CaptureReport, error)
	ready   func() bool // is there a live session to capture through?
	logger  *slog.Logger

	mu     sync.Mutex
	lastOK string // IST date of the last successful run
	last   CaptureReport
	lastAt time.Time
	lastEr string
	// running guards against a manual trigger racing the timer, which would
	// double every request in flight.
	running bool
}

// NewCaptureScheduler builds a scheduler firing at runAt ("15:40", IST).
//
// capture does the work; ready reports whether it currently can (a Kite session
// with an instrument master). Both are injected rather than reached for, so the
// scheduler can be tested without a broker.
func NewCaptureScheduler(runAt string, capture func(context.Context, time.Time) (CaptureReport, error), ready func() bool, logger *slog.Logger) (*CaptureScheduler, error) {
	off, err := parseClock(runAt)
	if err != nil {
		return nil, err
	}
	if capture == nil {
		return nil, fmt.Errorf("capture scheduler: no capture function")
	}
	if ready == nil {
		ready = func() bool { return true }
	}
	return &CaptureScheduler{runAt: off, capture: capture, ready: ready, logger: logger}, nil
}

// parseClock converts "15:40" to an offset into the day.
func parseClock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("capture: run_at %q must be HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Run drives the scheduler until ctx ends.
//
// The tick is one minute rather than a timer sleeping until the next run, so
// that a laptop resuming from suspend, a clock correction, or a login that
// arrives hours late all converge within a minute instead of waiting out a
// timer that was set against the old wall clock.
func (s *CaptureScheduler) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()

	s.tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.tick(ctx, now)
		}
	}
}

// tick runs the capture if the day is due and nothing else is running.
func (s *CaptureScheduler) tick(ctx context.Context, now time.Time) {
	local := now.In(IST)
	today := local.Format("2006-01-02")

	// Before the trigger time there is nothing to capture: the session has not
	// finished, and a partial day would be stored as a complete one.
	sinceMidnight := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute
	if sinceMidnight < s.runAt {
		return
	}

	s.mu.Lock()
	if s.running || s.lastOK == today {
		s.mu.Unlock()
		return
	}
	if !s.ready() {
		s.mu.Unlock()
		// Warned once a minute would be noise; the daily summary below is the
		// signal. Log at debug so it is available when someone goes looking.
		if s.logger != nil {
			s.logger.Debug("capture due but no Zerodha session; will retry",
				"day", today)
		}
		return
	}
	s.running = true
	s.mu.Unlock()

	rep, err := s.capture(ctx, local)

	s.mu.Lock()
	s.running = false
	s.last, s.lastAt = rep, now
	if err != nil {
		s.lastEr = err.Error()
	} else {
		s.lastEr = ""
		// A skipped day (weekend, holiday) still counts as done, or the
		// scheduler would retry it every minute until midnight.
		s.lastOK = today
	}
	s.mu.Unlock()

	if err != nil && s.logger != nil {
		s.logger.Error("option capture failed; today's option data may be unrecoverable",
			"day", today, "err", err)
	}
}

// RunNow captures a specific day immediately, ignoring the schedule. It still
// refuses to run concurrently with the timer.
func (s *CaptureScheduler) RunNow(ctx context.Context, day time.Time) (CaptureReport, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return CaptureReport{}, fmt.Errorf("a capture is already running")
	}
	if !s.ready() {
		s.mu.Unlock()
		return CaptureReport{}, fmt.Errorf("no Zerodha session; log in first")
	}
	s.running = true
	s.mu.Unlock()

	rep, err := s.capture(ctx, day)

	s.mu.Lock()
	s.running = false
	s.last, s.lastAt = rep, time.Now()
	if err != nil {
		s.lastEr = err.Error()
	} else {
		s.lastEr = ""
		if day.In(IST).Format("2006-01-02") == time.Now().In(IST).Format("2006-01-02") {
			s.lastOK = day.In(IST).Format("2006-01-02")
		}
	}
	s.mu.Unlock()
	return rep, err
}

// CaptureStatus is what the UI needs to show whether capture is keeping up.
type CaptureStatus struct {
	Running   bool          `json:"running"`
	LastRunAt time.Time     `json:"last_run_at"`
	LastDay   string        `json:"last_day"`
	Contracts int           `json:"contracts"`
	Candles   int           `json:"candles"`
	Failures  int           `json:"failures"`
	Skipped   string        `json:"skipped"`
	Duration  time.Duration `json:"duration"`
	Error     string        `json:"error"`
}

// Status reports the last run.
func (s *CaptureScheduler) Status() CaptureStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CaptureStatus{
		Running:   s.running,
		LastRunAt: s.lastAt,
		LastDay:   s.lastOK,
		Contracts: s.last.Contracts,
		Candles:   s.last.Candles,
		Failures:  s.last.Failures,
		Skipped:   s.last.Skipped,
		Duration:  s.last.Duration,
		Error:     s.lastEr,
	}
}
