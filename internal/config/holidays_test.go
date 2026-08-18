package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The shipped holiday lists must be dates, and must not be weekends.
//
// This list is the one piece of configuration where being WRONG is asymmetric.
// An unlisted holiday costs a few empty API requests and a spurious login nag.
// A date listed here that is actually a trading day makes the capture SKIP it,
// and once that day's contracts expire Kite will not serve their candles again —
// so the mistake is silent, permanent, and only discovered when a backtest over
// that week comes back empty.
//
// A weekend entry is the tell for a transcription slip: weekends are already
// skipped structurally, so listing one is never necessary and usually means a
// date was copied from the wrong row.
func TestShippedHolidayListsAreSane(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config.yaml"),
		filepath.Join("..", "..", "config.example.yaml"),
		filepath.Join("..", "..", "deploy", "config.example.yaml"),
	} {
		t.Run(filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			// Parsed directly rather than through Load: the deploy template
			// deliberately carries no credentials — those live in secrets on the
			// box — so Load rejects it, and this test is about the YAML text.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var doc struct {
				Capture struct {
					Holidays []string `yaml:"holidays"`
				} `yaml:"capture"`
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			days := doc.Capture.Holidays
			if len(days) == 0 {
				t.Fatalf("%s lists no holidays; an unlisted holiday wastes requests "+
					"and makes the missing-session alert nag on a closed day", path)
			}

			seen := map[string]bool{}
			for _, d := range days {
				parsed, err := time.Parse("2006-01-02", d)
				if err != nil {
					t.Errorf("holiday %q is not a YYYY-MM-DD date: %v", d, err)
					continue
				}
				if seen[d] {
					t.Errorf("holiday %q is listed twice", d)
				}
				seen[d] = true

				switch parsed.Weekday() {
				case time.Saturday, time.Sunday:
					t.Errorf("holiday %q is a %s — weekends are skipped structurally, "+
						"so this is almost certainly a mis-transcribed date",
						d, parsed.Weekday())
				}
			}
		})
	}
}
