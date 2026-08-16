package config

import (
	"slices"
	"testing"
)

// SENSEX options are on BSE. Loading only NFO — which is what the platform did
// before capture existed — makes every BSE contract look nonexistent and drops
// them from the instrument snapshot, so no amount of capturing can make them
// backtestable.
func TestCaptureExchangesIncludesBFOForBSEUnderlyings(t *testing.T) {
	cases := []struct {
		name        string
		underlyings []string
		want        []string
	}{
		{"nse only", []string{"NIFTY"}, []string{"NFO"}},
		{"sensex", []string{"NIFTY", "SENSEX"}, []string{"BFO", "NFO"}},
		{"bankex", []string{"BANKEX"}, []string{"BFO", "NFO"}},
		{"none configured", nil, []string{"NFO"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			for _, u := range tc.underlyings {
				c.Capture.Underlyings = append(c.Capture.Underlyings, CaptureUnderlying{Name: u})
			}
			got := c.CaptureExchanges()
			if !slices.Equal(got, tc.want) {
				t.Errorf("CaptureExchanges() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCaptureDefaultsAreAppliedWhenAbsent(t *testing.T) {
	c := &Config{}
	c.applyDefaults()

	if c.Capture.RunAt != "15:40" {
		t.Errorf("RunAt = %q, want 15:40", c.Capture.RunAt)
	}
	if c.Capture.Interval != "5minute" {
		t.Errorf("Interval = %q, want 5minute", c.Capture.Interval)
	}
	if c.Capture.Strikes != 20 {
		t.Errorf("Strikes = %d, want 20", c.Capture.Strikes)
	}
	if c.Capture.Expiries != 4 {
		t.Errorf("Expiries = %d, want 4", c.Capture.Expiries)
	}
	if c.Capture.Enabled {
		t.Error("capture defaulted to enabled; spending API quota must be opt-in")
	}
	if len(c.Capture.Underlyings) != 2 {
		t.Fatalf("got %d default underlyings, want 2", len(c.Capture.Underlyings))
	}
}

// An underlying typed in lower case must still match the instrument master,
// where names are upper case.
func TestCaptureUnderlyingsAreNormalized(t *testing.T) {
	c := &Config{}
	c.Capture.Underlyings = []CaptureUnderlying{{Name: " sensex ", Index: " SENSEX "}}
	c.applyDefaults()

	u := c.Capture.Underlyings[0]
	if u.Name != "SENSEX" {
		t.Errorf("Name = %q, want SENSEX", u.Name)
	}
	if u.Index != "SENSEX" {
		t.Errorf("Index = %q, want SENSEX", u.Index)
	}
	if got := c.CaptureExchanges(); !slices.Contains(got, "BFO") {
		t.Errorf("CaptureExchanges() = %v, want BFO after normalization", got)
	}
}
