package web

import (
	"net/url"
	"strings"
	"testing"

	"kite-algo/internal/strategy"
)

var boolDescriptor = strategy.Descriptor{
	Type: "t",
	Params: []strategy.ParamSpec{
		{Key: "lots", Kind: strategy.KindInt, Default: 1},
		{Key: "hedged", Kind: strategy.KindBool, Default: true},
	},
}

// TestParamFieldsUseDefaultsBeforeSubmission covers the fresh form: nothing has
// been submitted, so every field shows what the strategy declares.
func TestParamFieldsUseDefaultsBeforeSubmission(t *testing.T) {
	fields := paramFields(boolDescriptor, url.Values{"lots": {"9"}}, false)

	if got := fields[0].Value; got != "1" {
		t.Errorf("lots = %q, want the declared default 1 — a stray query "+
			"parameter must not seed an unsubmitted form", got)
	}
	if !fields[1].Checked() {
		t.Error("a bool defaulting to true rendered unticked")
	}
}

// TestParamFieldsKeepSubmittedValues is what stops a rejected run from silently
// reverting the operator's settings — the surest way to make someone re-run a
// backtest without noticing it no longer tests what they configured.
func TestParamFieldsKeepSubmittedValues(t *testing.T) {
	fields := paramFields(boolDescriptor, url.Values{"lots": {"9"}}, true)

	if got := fields[0].Value; got != "9" {
		t.Errorf("lots = %q, want the submitted 9", got)
	}
	// An unticked checkbox posts nothing at all. On a submission that means
	// "off"; falling back to the default would make a bool defaulting to true
	// impossible to turn off.
	if fields[1].Checked() {
		t.Error("an unticked checkbox came back ticked, so it could never be cleared")
	}
}

func TestCollectParamsTakesOnlyDeclaredKeys(t *testing.T) {
	raw := collectParams(boolDescriptor, url.Values{
		"lots":     {"3"},
		"capital":  {"100000"}, // a form field of the page, not of the strategy
		"exit_dlt": {"0.4"},    // a typo: must not reach Normalize as a real key
	})

	if len(raw) != 2 {
		t.Fatalf("collected %v, want only the declared lots and hedged", raw)
	}
	if raw["lots"] != "3" {
		t.Errorf("lots = %v, want 3", raw["lots"])
	}
	if raw["hedged"] != "false" {
		t.Errorf("hedged = %v, want false from the missing checkbox", raw["hedged"])
	}
}

func TestParamProblemsNamesEveryBadField(t *testing.T) {
	_, err := sampleDescriptor.Normalize(map[string]any{
		"lots":       "500", // above max
		"exit_delta": "abc", // not a number
	})
	if err == nil {
		t.Fatal("two invalid parameters were accepted")
	}
	msg, ok := paramProblems(err)
	if !ok {
		t.Fatalf("validation error not recognized: %v", err)
	}
	for _, want := range []string{"lots", "exit_delta"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not name %s; the operator would fix one "+
				"field per run", msg, want)
		}
	}
}
