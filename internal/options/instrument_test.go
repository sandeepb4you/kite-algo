package options

import "testing"

// Date-coded weeklies are the commonest NFO option symbol there is, and they
// were parsing wrong: scanning trailing digits for the strike consumed the
// expiry too, so NIFTY2681824350CE reported a strike of 2,681,824,350 and no
// expiry at all. Anything keyed on expiry silently excluded almost every
// weekly contract.
func TestParseSymbolWeeklyDateCoded(t *testing.T) {
	cases := []struct {
		symbol     string
		underlying string
		strike     float64
		expiry     string
		typ        OptionType
	}{
		// Real symbols captured from the instrument master.
		{"NIFTY2681824350CE", "NIFTY", 24350, "2026-08-18", Call},
		{"NIFTY2681824350PE", "NIFTY", 24350, "2026-08-18", Put},
		{"SENSEX2682075700CE", "SENSEX", 75700, "2026-08-20", Call},
		// Monthly, letter-coded — the form that already worked.
		{"NIFTY26AUG24900CE", "NIFTY", 24900, "", Call},
		// Oct/Nov/Dec weeklies use a letter for the month, because "1015"
		// could not be told apart from "115".
		{"NIFTY26O1524350CE", "NIFTY", 24350, "2026-10-15", Call},
		{"NIFTY26N0524350PE", "NIFTY", 24350, "2026-11-05", Put},
		{"NIFTY26D3124350CE", "NIFTY", 24350, "2026-12-31", Call},
	}

	for _, tc := range cases {
		t.Run(tc.symbol, func(t *testing.T) {
			got, ok := ParseSymbol(tc.symbol)
			if !ok {
				t.Fatalf("ParseSymbol(%q) failed", tc.symbol)
			}
			if got.Underlying != tc.underlying {
				t.Errorf("underlying = %q, want %q", got.Underlying, tc.underlying)
			}
			if got.Strike != tc.strike {
				t.Errorf("strike = %v, want %v", got.Strike, tc.strike)
			}
			if got.Type != tc.typ {
				t.Errorf("type = %v, want %v", got.Type, tc.typ)
			}
			if tc.expiry != "" {
				if got.Expiry.IsZero() {
					t.Fatalf("expiry not parsed from %q", tc.symbol)
				}
				if g := got.Expiry.Format("2006-01-02"); g != tc.expiry {
					t.Errorf("expiry = %s, want %s", g, tc.expiry)
				}
			}
		})
	}
}

// The strike must never swallow the expiry digits.
func TestParseSymbolStrikeIsPlausible(t *testing.T) {
	for _, sym := range []string{
		"NIFTY2681824350CE", "SENSEX2682075700CE", "NIFTY26AUG24900PE",
	} {
		got, ok := ParseSymbol(sym)
		if !ok {
			t.Fatalf("ParseSymbol(%q) failed", sym)
		}
		if got.Strike > 1_000_000 {
			t.Errorf("%s: strike %v is implausible — the expiry was absorbed into it",
				sym, got.Strike)
		}
	}
}
