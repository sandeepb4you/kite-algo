package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/history"
)

// Timed square-off, per book.
//
// Sitting in front of the screen at 15:20 every day to click "square off" is
// the kind of job a computer should do, and forgetting to do it is expensive in
// exactly one direction. So each book carries its own auto-flatten clock: the
// live desk sets the real book's, the terminal sets the simulated one's.
//
// The two are separate settings rather than one, because they are separate
// decisions. Being flat by the close is a rule about real capital; a simulated
// position is usually meant to keep running so the strategy under evaluation is
// measured on what it actually did. A single time would force one policy onto
// both books.
//
// Distinct from the expiry sweeper next door, which flattens only REAL
// positions in contracts expiring TODAY. This flattens the whole book at the
// given time whether anything is expiring or not.

// squareOffTimesKey is where the operator's chosen times are stored, and
// squareOffRunsKey the IST date each book was last flattened on.
const (
	squareOffTimesKey = "squareoff.times"
	squareOffRunsKey  = "squareoff.lastrun"
)

// squareOffGrace is how late a missed flatten may still run.
//
// The scheduler fires on "the time has passed and today has not run yet", which
// on its own makes every boot a catchup: a process started at 20:00 would decide
// that 15:10 had passed and flatten the book into a closed market, and a Saturday
// restart would do the same to whatever is held over the weekend. Inside the
// window a catchup is what you want — the platform was down at 15:10 and the rule
// still says be flat — and outside it, the moment has gone and the honest thing
// is to say so and leave the book alone.
const squareOffGrace = time.Hour

// SquareOffTimes is the auto square-off clock for both books, as "HH:MM" in IST.
// An empty string means off.
type SquareOffTimes struct {
	Real  string `json:"real"`
	Paper string `json:"paper"`
}

// For reports the time set for one book.
func (t SquareOffTimes) For(b broker.Book) string {
	if b.IsReal() {
		return t.Real
	}
	return t.Paper
}

// with returns a copy with one book's time replaced, so a save that touches the
// real book cannot disturb the paper one.
func (t SquareOffTimes) with(b broker.Book, hhmm string) SquareOffTimes {
	if b.IsReal() {
		t.Real = hhmm
		return t
	}
	t.Paper = hhmm
	return t
}

// SquareOffStatus is one book's auto square-off state, for display.
type SquareOffStatus struct {
	Book broker.Book
	// Time is "HH:MM" IST, or empty when off.
	Time string
	// Positions is how many open positions the flatten would close right now.
	Positions int
	// Done reports that today's slot is used up — the flatten ran, or the
	// process came back too late for it to run — so the time showing on screen
	// is tomorrow's rather than something still pending. Without this the desk
	// said "auto square-off 15:20" all afternoon and gave no way to tell a
	// pending flatten from one that had already happened.
	Done bool
	// In is how long until today's run. Zero when off, already run, or past.
	In time.Duration
}

// Enabled reports whether a time is set at all.
func (s SquareOffStatus) Enabled() bool { return s.Time != "" }

// Label renders the countdown as "2h 14m", for the desk.
func (s SquareOffStatus) Label() string {
	if s.In <= 0 {
		return ""
	}
	h := int(s.In.Hours())
	m := int(s.In.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m+1) // round up: "0m" reads as "already happened"
}

// configuredSquareOffTimes returns the config.yaml starting values.
func configuredSquareOffTimes(cfg *config.Config) SquareOffTimes {
	real, _ := normalizeSquareOffTime(cfg.Risk.Live.SquareOffTime)
	paper, _ := normalizeSquareOffTime(cfg.Risk.Paper.SquareOffTime)
	return SquareOffTimes{Real: real, Paper: paper}
}

// normalizeSquareOffTime validates an operator-typed time and canonicalises it.
//
// "off", "none" and blank all mean off, because an operator clearing a field
// types whichever of those comes to mind and none of them should be an error.
//
// Anything else must be a two-digit 24-hour HH:MM, and the two-digit part is not
// pedantry. time.Parse accepts "3:20" and reads it as 03:20 — so a trader who
// typed 3:20 meaning the afternoon would get a flatten scheduled for twenty past
// three in the morning, which fires against a closed market, finds nothing to
// close, and marks the day done. The real square-off then never runs, silently,
// on a day the operator believes it is set.
func normalizeSquareOffTime(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" || strings.EqualFold(v, "off") || strings.EqualFold(v, "none") {
		return "", nil
	}
	bad := fmt.Errorf("%q is not a 24-hour HH:MM time (for example 15:20), or \"off\"", raw)
	if len(v) != 5 || v[2] != ':' {
		return "", bad
	}
	t, err := time.Parse("15:04", v)
	if err != nil {
		return "", bad
	}
	return t.Format("15:04"), nil
}

// minutesSinceMidnight converts "HH:MM" to a duration into the day.
func minutesSinceMidnight(hhmm string) (time.Duration, bool) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, false
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, true
}

// loadSquareOffTimes resolves the times to start with: config.yaml, overridden
// by whatever the operator saved.
//
// A malformed stored value falls back to the config rather than failing the
// boot, the same trade loadRiskLimits makes: refusing to start because a
// settings row is corrupt would leave the operator unable to reach the one
// screen they need in order to flatten by hand.
func loadSquareOffTimes(ctx context.Context, store interface {
	GetSetting(context.Context, string) (string, bool, error)
}, cfg *config.Config, logf func(string, ...any)) SquareOffTimes {
	defaults := configuredSquareOffTimes(cfg)
	if store == nil {
		return defaults
	}

	raw, found, err := store.GetSetting(ctx, squareOffTimesKey)
	if err != nil || !found {
		if err != nil && logf != nil {
			logf("read saved square-off times failed: %v", err)
		}
		return defaults
	}

	var saved SquareOffTimes
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		if logf != nil {
			logf("saved square-off times are unreadable, using config defaults: %v", err)
		}
		return defaults
	}
	// Re-validate on the way in. A hand-edited row could otherwise install a
	// time the scheduler silently ignores, which looks identical to a flatten
	// that is armed and simply has not fired yet.
	saved.Real, _ = normalizeSquareOffTime(saved.Real)
	saved.Paper, _ = normalizeSquareOffTime(saved.Paper)
	return saved
}

// SquareOffTimes returns the active auto square-off clock.
func (a *App) SquareOffTimes() SquareOffTimes {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.squareOffTimes
}

// SaveSquareOffTimes sets one book's time and persists it.
//
// Returns the canonical time actually stored and whether today's run was
// skipped. Skipping matters: a time typed at 15:30 for 15:20 has already passed,
// and treating "flat by 15:20" as due would flatten the book the instant the
// operator pressed Save. Nobody typing a time into a form expects it to be a
// square-off button, so today is marked done and the setting takes effect
// tomorrow — said out loud in the response rather than left to be discovered.
func (a *App) SaveSquareOffTimes(ctx context.Context, book broker.Book, raw string) (string, bool, error) {
	hhmm, err := normalizeSquareOffTime(raw)
	if err != nil {
		return "", false, err
	}

	a.mu.Lock()
	next := a.squareOffTimes.with(book, hhmm)
	a.squareOffTimes = next
	a.mu.Unlock()

	skippedToday := false
	if hhmm != "" {
		if due, ok := minutesSinceMidnight(hhmm); ok {
			now := time.Now().In(history.IST)
			since := time.Duration(now.Hour())*time.Hour + time.Duration(now.Minute())*time.Minute
			if since >= due {
				a.squareOff.markDone(book, now.Format("2006-01-02"))
				skippedToday = true
			}
		}
	}

	if a.Store != nil {
		blob, err := json.Marshal(next)
		if err != nil {
			return hhmm, skippedToday, fmt.Errorf("encode square-off times: %w", err)
		}
		if err := a.Store.SetSetting(ctx, squareOffTimesKey, string(blob)); err != nil {
			// The schedule IS live; only persistence failed. Say which, rather
			// than implying the change did not take.
			return hhmm, skippedToday, fmt.Errorf(
				"square-off time applied but not saved (it will revert on restart): %w", err)
		}
	}

	if a.Log != nil {
		a.Log.Warn("auto square-off time changed", "book", book.String(),
			"at", hhmm, "skipped_today", skippedToday)
	}
	return hhmm, skippedToday, nil
}

// SquareOffStatusFor describes one book's auto square-off, for the desk.
func (a *App) SquareOffStatusFor(book broker.Book) SquareOffStatus {
	st := SquareOffStatus{Book: book, Time: a.SquareOffTimes().For(book)}

	for _, p := range a.Engine.Positions() {
		if p.IsOpen() && p.Book.IsReal() == book.IsReal() {
			st.Positions++
		}
	}
	if st.Time == "" {
		return st
	}

	now := time.Now().In(history.IST)
	today := now.Format("2006-01-02")
	st.Done = a.squareOff.doneOn(book) == today

	if due, ok := minutesSinceMidnight(st.Time); ok && !st.Done {
		since := time.Duration(now.Hour())*time.Hour + time.Duration(now.Minute())*time.Minute
		if due > since {
			st.In = due - since
		}
	}
	return st
}

// squareOffScheduler flattens each book at its configured time, once per day.
type squareOffScheduler struct {
	app *App

	mu   sync.Mutex
	done map[string]string // book -> IST date the flatten last ran on
}

// newSquareOffScheduler builds the scheduler, restoring which books have already
// been flattened today.
//
// The restore is the point. Held only in memory, the marker was lost on every
// restart, and the server is restarted from the IDE several times a day: a
// process coming back up at 15:25 saw that 15:10 had passed with no run recorded
// and flattened the book a second time — including a position the operator had
// deliberately re-opened after the first flatten. The daily-loss lockout next
// door persists for exactly this reason.
func newSquareOffScheduler(ctx context.Context, a *App) *squareOffScheduler {
	s := &squareOffScheduler{app: a, done: make(map[string]string)}
	if a.Store == nil {
		return s
	}
	raw, found, err := a.Store.GetSetting(ctx, squareOffRunsKey)
	if err != nil || !found {
		if err != nil && a.Log != nil {
			a.Log.Warn("read square-off run history failed; a restart today may "+
				"repeat a flatten that already ran", "err", err)
		}
		return s
	}
	if err := json.Unmarshal([]byte(raw), &s.done); err != nil {
		s.done = make(map[string]string)
		if a.Log != nil {
			a.Log.Warn("square-off run history is unreadable", "err", err)
		}
	}
	return s
}

// markDone claims a book's flatten for one IST date, and persists the claim.
//
// Called BEFORE the flatten, never after: a square-off that partially fails must
// not be retried a minute later on the positions that did close, because the
// second closing order would open the opposite position in a book the first one
// already flattened.
func (s *squareOffScheduler) markDone(book broker.Book, date string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.done[book.String()] = date
	blob, err := json.Marshal(s.done)
	s.mu.Unlock()

	if err != nil || s.app == nil || s.app.Store == nil {
		return
	}
	// Best effort, and loud when it fails: the flatten still happens, but a
	// restart could repeat it.
	if err := s.app.Store.SetSetting(context.Background(), squareOffRunsKey, string(blob)); err != nil {
		if s.app.Log != nil {
			s.app.Log.Warn("could not persist the square-off run marker; a restart "+
				"today may repeat this flatten", "book", book.String(), "err", err)
		}
	}
}

func (s *squareOffScheduler) doneOn(book broker.Book) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done[book.String()]
}

// startSquareOffScheduler launches the per-book flatten.
//
// Always started, even with both times off: the times are editable at runtime,
// so a scheduler that only existed when something was configured at boot would
// mean a time set from the desk never fired until a restart.
func (a *App) startSquareOffScheduler(ctx context.Context) {
	if a.squareOff == nil {
		a.squareOff = newSquareOffScheduler(ctx, a)
	}
	if a.Log != nil {
		t := a.SquareOffTimes()
		a.Log.Info("auto square-off scheduler started",
			"real", orOff(t.Real), "paper", orOff(t.Paper))
	}
	go a.squareOff.run(ctx)
}

func orOff(v string) string {
	if v == "" {
		return "off"
	}
	return v
}

// run ticks each minute, for the same reason the capture scheduler and the
// expiry sweeper do: a machine resuming from suspend, or a clock correction,
// converges within a minute instead of missing a timer set against the old wall
// clock. It also means a time changed from the desk is picked up on the next
// tick with no restart.
func (s *squareOffScheduler) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.tick(ctx, now)
		}
	}
}

func (s *squareOffScheduler) tick(ctx context.Context, now time.Time) {
	local := now.In(history.IST)
	today := local.Format("2006-01-02")
	since := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute

	times := s.app.SquareOffTimes()
	// Real first. If both books are set to the same minute, the money that can
	// actually be lost goes first.
	for _, book := range []broker.Book{broker.BookReal, broker.BookPaper} {
		hhmm := times.For(book)
		if hhmm == "" {
			continue
		}
		due, ok := minutesSinceMidnight(hhmm)
		if !ok || since < due {
			continue
		}
		s.mu.Lock()
		claimed := s.done[book.String()] == today
		s.mu.Unlock()
		if claimed {
			continue
		}

		// Too late to be the flatten that was asked for. Claim the day anyway,
		// so this is said once rather than every minute until midnight.
		if since > due+squareOffGrace {
			s.markDone(book, today)
			if s.app.Log != nil {
				s.app.Log.Warn("auto square-off NOT run: its time passed too long ago",
					"book", book.String(), "at", hhmm, "now", local.Format("15:04"),
					"grace", squareOffGrace.String())
			}
			continue
		}

		s.markDone(book, today)
		s.flatten(ctx, book, local)
	}
}

// flatten squares off one book and reports what happened.
func (s *squareOffScheduler) flatten(ctx context.Context, book broker.Book, now time.Time) {
	a := s.app

	open := 0
	for _, p := range a.Engine.Positions() {
		if p.IsOpen() && p.Book.IsReal() == book.IsReal() {
			open++
		}
	}
	if open == 0 {
		return
	}

	if a.Log != nil {
		a.Log.Warn("auto square-off: flattening the book",
			"book", book.String(), "positions", open, "at", now.Format("15:04"))
	}

	placed, errs := a.Engine.SquareOffBook(ctx, book)

	if a.Log != nil {
		if len(errs) > 0 {
			// Partial failure is the case that must be loud. Believing the book
			// went flat when some of it did not is how a position is carried
			// overnight.
			a.Log.Error("auto square-off did NOT fully flatten the book",
				"book", book.String(), "placed", len(placed), "failed", len(errs),
				"err", joinErrors(errs))
		} else {
			a.Log.Warn("auto square-off complete",
				"book", book.String(), "placed", len(placed))
		}
	}

	// Alert on the real book always, and on the paper book only when something
	// failed. A simulated flatten that worked is not news; a real one is, and so
	// is any flatten that left positions open.
	if a.alerts != nil && (book.IsReal() || len(errs) > 0) {
		msg := fmt.Sprintf("Auto square-off %s book at %s IST: %d order(s) placed",
			book.String(), now.Format("15:04"), len(placed))
		if len(errs) > 0 {
			msg += fmt.Sprintf(", %d FAILED — %s", len(errs), joinErrors(errs))
		}
		if err := a.alerts.Send(ctx, msg); err != nil && a.Log != nil {
			a.Log.Warn("square-off alert not delivered", "err", err)
		}
	}
}

// joinErrors flattens a square-off's per-position failures into one line.
func joinErrors(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}
