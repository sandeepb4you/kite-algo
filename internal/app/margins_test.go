package app

import (
	"errors"
	"testing"
	"time"

	"kite-algo/internal/kite"
)

// TestMarginsPreferLiveBalanceButFallBack covers Kite's two cash fields:
// live_balance is populated during market hours, cash outside them. Reading only
// one would show a zero balance for half the day.
func TestMarginsPreferLiveBalanceButFallBack(t *testing.T) {
	var during kite.Margin
	during.Available.LiveBalance = 125000
	during.Available.Cash = 120000
	during.Used.Debits = 45000

	if got := toMargins(during).Available; got != 125000 {
		t.Errorf("available = %v, want the live balance 125000", got)
	}

	var afterHours kite.Margin
	afterHours.Available.LiveBalance = 0
	afterHours.Available.Cash = 120000

	if got := toMargins(afterHours).Available; got != 120000 {
		t.Errorf("available = %v, want the cash figure 120000 when live_balance is absent", got)
	}
}

func TestMarginsCarryUsedAndTimestamp(t *testing.T) {
	var raw kite.Margin
	raw.Available.LiveBalance = 100
	raw.Used.Debits = 40
	raw.Available.OpeningBalance = 150

	m := toMargins(raw)
	if m.Used != 40 {
		t.Errorf("used = %v, want 40", m.Used)
	}
	if m.OpeningBalance != 150 {
		t.Errorf("opening balance = %v, want 150", m.OpeningBalance)
	}
	if !m.Known() {
		t.Error("a freshly fetched margin should report itself as known")
	}
	if m.Stale() {
		t.Error("a freshly fetched margin should not be stale")
	}
}

// TestUnknownMarginsRenderAsUnknown keeps the header from showing a confident
// ₹0.00 before the first fetch — zero balance and unknown balance are very
// different things to a trader.
func TestUnknownMarginsRenderAsUnknown(t *testing.T) {
	var m Margins
	if m.Known() {
		t.Error("a zero-value Margins must not claim to be known")
	}
	if m.Stale() {
		t.Error("an unknown balance is not 'stale'; it was simply never fetched")
	}
}

func TestMarginsGoStale(t *testing.T) {
	m := Margins{Available: 100, UpdatedAt: time.Now().Add(-5 * marginRefreshInterval)}
	if !m.Stale() {
		t.Error("a reading several refresh intervals old should be reported as stale")
	}
}

// TestMarginErrorKeepsTheLastGoodReading: a transient API failure should not
// blank the balance, because a stale number clearly labelled stale is more
// useful than nothing at all.
func TestMarginErrorKeepsTheLastGoodReading(t *testing.T) {
	var c marginCache
	c.set(Margins{Available: 99000, Used: 1000, UpdatedAt: time.Now()})

	c.setErr(errors.New("network is unreachable"))

	got := c.get()
	if got.Available != 99000 {
		t.Errorf("available = %v; a failed refresh discarded the last good reading", got.Available)
	}
	if got.Err == "" {
		t.Error("the failure was not recorded, so the UI cannot flag it")
	}
	if !got.Known() {
		t.Error("the reading should still count as known")
	}
}
