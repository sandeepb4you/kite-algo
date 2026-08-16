package app

import (
	"context"
	"testing"
	"time"

	"kite-algo/internal/history"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 10, 0, 0, 0, history.IST)
}

// The cap is a percentage of the day's OPENING balance.
func TestLiveMaxLossIsAPercentageOfOpeningBalance(t *testing.T) {
	l := NewLiveRisk(1.0)
	l.ObserveBalance(500000, day(2026, 8, 17))

	if got := l.MaxDailyLoss(); got != 5000 {
		t.Errorf("MaxDailyLoss() = %v, want 5000 (1%% of 500000)", got)
	}
}

// Snapshotted, not live. Available margin falls as a position moves against
// you; a limit derived from it would tighten exactly when you are already
// hurting, moving the goalposts mid-trade.
func TestOpeningBalanceIsSnapshottedPerDay(t *testing.T) {
	l := NewLiveRisk(1.0)
	d := day(2026, 8, 17)
	l.ObserveBalance(500000, d)
	l.ObserveBalance(300000, d.Add(3*time.Hour)) // margin consumed intraday

	if got := l.MaxDailyLoss(); got != 5000 {
		t.Errorf("MaxDailyLoss() = %v, want 5000 — the limit moved intraday", got)
	}
	// A new day re-snapshots.
	l.ObserveBalance(300000, day(2026, 8, 18))
	if got := l.MaxDailyLoss(); got != 3000 {
		t.Errorf("MaxDailyLoss() = %v, want 3000 after the day rolled", got)
	}
}

// An unknown balance must refuse live entries, not permit them.
func TestLiveRefusesEntriesBeforeTheBalanceIsKnown(t *testing.T) {
	l := NewLiveRisk(1.0)
	ok, why := l.Allow(day(2026, 8, 17))
	if ok {
		t.Error("allowed a live entry with no balance and therefore no computable limit")
	}
	if why == "" {
		t.Error("refusal gave no reason")
	}
}

func TestLockoutBarsEntriesForTheRestOfTheDay(t *testing.T) {
	l := NewLiveRisk(1.0)
	d := day(2026, 8, 17)
	l.ObserveBalance(500000, d)

	if ok, _ := l.Allow(d); !ok {
		t.Fatal("blocked before any breach")
	}
	if !l.Trip(context.Background(), d, "limit reached", nil) {
		t.Fatal("Trip reported no change")
	}
	if ok, why := l.Allow(d.Add(2 * time.Hour)); ok {
		t.Error("allowed an entry after the lockout tripped")
	} else if why == "" {
		t.Error("lockout gave no reason")
	}
	// Tomorrow is a fresh day.
	next := day(2026, 8, 18)
	l.ObserveBalance(500000, next)
	if ok, why := l.Allow(next); !ok {
		t.Errorf("still locked the next day: %s", why)
	}
}

// A lockout a restart clears is not a lockout — and a restart is exactly what
// an operator reaches for after a bad morning.
func TestLockoutSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	store := &memSettings{v: map[string]string{}}
	d := day(2026, 8, 17)

	first := NewLiveRisk(1.0)
	first.ObserveBalance(500000, d)
	first.Trip(ctx, d, "limit reached", store)

	restarted := NewLiveRisk(1.0)
	restarted.Restore(ctx, store)
	restarted.ObserveBalance(500000, d)

	if ok, _ := restarted.Allow(d); ok {
		t.Error("the lockout did not survive a restart")
	}
}

func TestTripIsIdempotentWithinADay(t *testing.T) {
	ctx := context.Background()
	l := NewLiveRisk(1.0)
	d := day(2026, 8, 17)
	if !l.Trip(ctx, d, "first", nil) {
		t.Fatal("first trip reported no change")
	}
	if l.Trip(ctx, d, "second", nil) {
		t.Error("second trip on the same day reported a change")
	}
}

// memSettings is a minimal settings store.
type memSettings struct{ v map[string]string }

func (m *memSettings) GetSetting(_ context.Context, k string) (string, bool, error) {
	s, ok := m.v[k]
	return s, ok, nil
}
func (m *memSettings) SetSetting(_ context.Context, k, val string) error {
	m.v[k] = val
	return nil
}
