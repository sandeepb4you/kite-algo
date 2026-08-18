package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Every spelling an operator would reach for must parse.
//
// A bare `repeat_every: 0` is the one that mattered: it is what the documentation
// for this field told people to write for "do not repeat", and a plain
// time.Duration field rejects it — yaml.v3 decodes durations only from strings. The
// result was not a warning or an ignored setting, it was the whole process failing
// to parse its config and crash-looping at startup. A notification interval must
// not be able to take a trading platform down.
func TestIntervalAcceptsEverySensibleSpelling(t *testing.T) {
	cases := []struct {
		yaml string
		want time.Duration
	}{
		{"d: 30m", 30 * time.Minute},
		{`d: "30m"`, 30 * time.Minute},
		{"d: 90s", 90 * time.Second},
		{"d: 1h30m", 90 * time.Minute},
		// The two ways of saying "never repeat".
		{"d: 0", 0},
		{"d: 0s", 0},
	}
	for _, tc := range cases {
		t.Run(tc.yaml, func(t *testing.T) {
			var v struct {
				D Interval `yaml:"d"`
			}
			if err := yaml.Unmarshal([]byte(tc.yaml), &v); err != nil {
				t.Fatalf("unmarshal %q: %v", tc.yaml, err)
			}
			if v.D.D != tc.want {
				t.Errorf("got %v, want %v", v.D.D, tc.want)
			}
			if !v.D.Set {
				t.Error("Set is false for a key that was present")
			}
		})
	}
}

// A bare non-zero number has no defensible reading — seconds? minutes? — and must
// be refused with a message that says how to write it.
func TestIntervalRejectsAUnitlessNumber(t *testing.T) {
	var v struct {
		D Interval `yaml:"d"`
	}
	err := yaml.Unmarshal([]byte("d: 30"), &v)
	if err == nil {
		t.Fatal("30 was accepted; it could mean seconds or minutes")
	}
	if got := err.Error(); !strings.Contains(got, "30m") {
		t.Errorf("the error does not show the correct form: %s", got)
	}
}

// Absent must be distinguishable from zero, or the default overwrites the
// operator's explicit "off" — the max_lots_per_trade bug in another costume.
func TestIntervalDistinguishesAbsentFromZero(t *testing.T) {
	base := func(body string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "c.yaml")
		writeFile(t, path, "mode: paper\nkite:\n  api_key: k\n  api_secret: s\n"+body)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	// Absent → the 30m default.
	cfg := base("notify:\n  telegram:\n    enabled: true\n    chat_id: \"1\"\n")
	if got := cfg.Notify.Telegram.RepeatEvery.D; got != 30*time.Minute {
		t.Errorf("absent repeat_every = %v, want the 30m default", got)
	}

	// Explicit 0 → no repeats, NOT the default.
	cfg = base("notify:\n  telegram:\n    enabled: true\n    chat_id: \"1\"\n    repeat_every: 0\n")
	if got := cfg.Notify.Telegram.RepeatEvery.D; got != 0 {
		t.Errorf("repeat_every: 0 became %v — an explicit \"never repeat\" was overwritten "+
			"by the default", got)
	}

	// And an explicit value is left alone.
	cfg = base("notify:\n  telegram:\n    enabled: true\n    chat_id: \"1\"\n    repeat_every: 2h\n")
	if got := cfg.Notify.Telegram.RepeatEvery.D; got != 2*time.Hour {
		t.Errorf("repeat_every: 2h = %v", got)
	}
}
