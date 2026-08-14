package strategy

import (
	"errors"
	"log/slog"
	"testing"
)

func testDescriptor() Descriptor {
	return Descriptor{
		Type: "test",
		Params: []ParamSpec{
			{Key: "name", Kind: KindString, Default: "NIFTY"},
			{Key: "lots", Kind: KindInt, Default: 1, Min: Ptr(1), Max: Ptr(10)},
			{Key: "delta", Kind: KindFloat, Default: 0.25, Min: Ptr(0.01), Max: Ptr(2)},
			{Key: "enabled", Kind: KindBool, Default: false},
			{Key: "product", Kind: KindEnum, Options: []string{"MIS", "NRML"}, Default: "MIS"},
			{Key: "cutoff", Kind: KindTime, Default: "15:15"},
		},
	}
}

// TestNormalizeCoercesFormStrings is the reason this layer exists. HTML posts
// every field as a string, YAML gives ints, JSON gives float64 — and Init needs
// one consistent set of types regardless of where the parameters came from.
func TestNormalizeCoercesFormStrings(t *testing.T) {
	got, err := testDescriptor().Normalize(map[string]any{
		"name":    "BANKNIFTY",
		"lots":    "3",
		"delta":   "0.4",
		"enabled": "on", // an HTML checkbox
		"product": "nrml",
		"cutoff":  "15:20",
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if v, ok := got["lots"].(int); !ok || v != 3 {
		t.Errorf("lots = %#v, want int 3", got["lots"])
	}
	if v, ok := got["delta"].(float64); !ok || v != 0.4 {
		t.Errorf("delta = %#v, want float64 0.4", got["delta"])
	}
	if v, ok := got["enabled"].(bool); !ok || !v {
		t.Errorf("enabled = %#v, want bool true", got["enabled"])
	}
	// Enum matching is case-insensitive but normalizes to the declared casing,
	// so downstream comparisons against "NRML" work.
	if got["product"] != "NRML" {
		t.Errorf("product = %#v, want %q", got["product"], "NRML")
	}
}

// TestNormalizeAppliesDefaults covers the single-source-of-truth property:
// strategies no longer carry their own fallback values.
func TestNormalizeAppliesDefaults(t *testing.T) {
	got, err := testDescriptor().Normalize(map[string]any{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want := map[string]any{
		"name": "NIFTY", "lots": 1, "delta": 0.25,
		"enabled": false, "product": "MIS", "cutoff": "15:15",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
}

// TestNormalizeTreatsBlankAsUnset means clearing a form field falls back to the
// default rather than failing validation.
func TestNormalizeTreatsBlankAsUnset(t *testing.T) {
	got, err := testDescriptor().Normalize(map[string]any{"lots": "", "name": "  "})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got["lots"] != 1 {
		t.Errorf("lots = %#v, want the default 1", got["lots"])
	}
}

// TestNormalizeRejectsUnknownKeys prevents a misspelled parameter from being
// silently ignored, which would leave a strategy running on a default the
// operator believes they overrode.
func TestNormalizeRejectsUnknownKeys(t *testing.T) {
	_, err := testDescriptor().Normalize(map[string]any{"lotz": "3"})
	if err == nil {
		t.Fatal("unknown parameter was accepted")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T, want *ValidationError", err)
	}
	if len(ve.Fields) != 1 || ve.Fields[0].Key != "lotz" {
		t.Errorf("fields = %+v, want one error naming 'lotz'", ve.Fields)
	}
}

func TestNormalizeValidation(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		key  string
	}{
		{"below min", map[string]any{"lots": "0"}, "lots"},
		{"above max", map[string]any{"lots": "99"}, "lots"},
		{"int given a fraction", map[string]any{"lots": "1.5"}, "lots"},
		{"not a number", map[string]any{"delta": "wide"}, "delta"},
		{"float below min", map[string]any{"delta": "0"}, "delta"},
		{"enum not an option", map[string]any{"product": "CNC"}, "product"},
		{"malformed time", map[string]any{"cutoff": "3pm"}, "cutoff"},
		{"time out of range", map[string]any{"cutoff": "25:00"}, "cutoff"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testDescriptor().Normalize(tc.in)
			if err == nil {
				t.Fatalf("%v was accepted", tc.in)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %T, want *ValidationError", err)
			}
			if ve.Fields[0].Key != tc.key {
				t.Errorf("error names %q, want %q", ve.Fields[0].Key, tc.key)
			}
			if ve.Fields[0].Message == "" {
				t.Error("error message is empty; the UI would show nothing useful")
			}
		})
	}
}

// TestNormalizeReportsEveryProblem lets the operator fix all fields in one pass
// rather than discovering them one submission at a time.
func TestNormalizeReportsEveryProblem(t *testing.T) {
	_, err := testDescriptor().Normalize(map[string]any{
		"lots":    "0",
		"delta":   "nope",
		"product": "CNC",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T, want *ValidationError", err)
	}
	if len(ve.Fields) != 3 {
		t.Errorf("reported %d problems, want 3: %+v", len(ve.Fields), ve.Fields)
	}
}

// stubFactory satisfies Descriptor.Factory for registry tests that never build
// an instance.
func stubFactory(string, *slog.Logger) (Strategy, error) { return nil, nil }

// TestRegistryRejectsDuplicates guards the process-wide registry against two
// strategies claiming the same type name.
func TestRegistryRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	valid := Descriptor{Type: "dup", Factory: stubFactory}
	r.Register(valid)

	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate type should panic at startup")
		}
	}()
	r.Register(valid)
}

func TestRegistryRejectsMalformedDescriptors(t *testing.T) {
	cases := map[string]Descriptor{
		"no type":         {Factory: stubFactory},
		"no factory":      {Type: "x"},
		"param no key":    {Type: "x", Factory: stubFactory, Params: []ParamSpec{{Kind: KindString}}},
		"enum no options": {Type: "x", Factory: stubFactory, Params: []ParamSpec{{Key: "k", Kind: KindEnum}}},
		"duplicate param": {Type: "x", Factory: stubFactory, Params: []ParamSpec{{Key: "k", Kind: KindString}, {Key: "k", Kind: KindInt}}},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s should panic at registration", name)
				}
			}()
			NewRegistry().Register(d)
		})
	}
}

func TestRegistryListIsSorted(t *testing.T) {
	r := NewRegistry()
	for _, typ := range []string{"zebra", "alpha", "mike"} {
		r.Register(Descriptor{Type: typ, Factory: stubFactory})
	}
	got := r.List()
	if len(got) != 3 || got[0].Type != "alpha" || got[2].Type != "zebra" {
		t.Errorf("List() = %v, want alphabetical order", got)
	}
}
