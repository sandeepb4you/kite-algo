package web

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"kite-algo/internal/strategy"
)

// paramField pairs a declared parameter with the value its form control should
// show.
//
// A template cannot ask "what did the operator type, or failing that what is
// the default?", so the handler resolves that once and the partial renders
// whatever it is handed. That is what lets one partial serve a blank form, a
// re-submitted one, and the fragment swapped in when the strategy changes.
type paramField struct {
	Spec  strategy.ParamSpec
	Value string
}

// Checked reports whether a bool field renders ticked.
func (f paramField) Checked() bool { return f.Value == "true" }

// paramFields builds the form model for a descriptor.
//
// When submitted is true the values come from the form, so a run rejected by
// validation comes back with the operator's settings intact rather than
// silently reset to defaults — the surest way to make someone re-run a
// backtest without noticing their parameters were reverted. It also decides
// what a missing checkbox means: nothing at all on a fresh form (use the
// default), deliberately unticked on a submission.
func paramFields(desc strategy.Descriptor, form url.Values, submitted bool) []paramField {
	out := make([]paramField, 0, len(desc.Params))
	for _, p := range desc.Params {
		value := defaultString(p)
		if submitted {
			switch v := strings.TrimSpace(form.Get(p.Key)); {
			case v != "":
				value = v
			case p.Kind == strategy.KindBool:
				value = "false" // an unticked checkbox posts nothing
			}
		}
		out = append(out, paramField{Spec: p, Value: value})
	}
	return out
}

// defaultString renders a declared default for an HTML value attribute.
func defaultString(p strategy.ParamSpec) string {
	if p.Default == nil {
		return ""
	}
	return fmt.Sprintf("%v", p.Default)
}

// collectParams pulls a descriptor's declared parameters out of a submitted
// form.
//
// Only declared keys are read, and Normalize rejects anything unrecognized, so
// a renamed field cannot silently fall back to a default the operator believes
// they overrode.
func collectParams(desc strategy.Descriptor, form url.Values) map[string]any {
	raw := make(map[string]any, len(desc.Params))
	for _, p := range desc.Params {
		if v := form.Get(p.Key); v != "" {
			raw[p.Key] = v
		} else if p.Kind == strategy.KindBool {
			raw[p.Key] = "false" // an unticked checkbox posts nothing
		}
	}
	return raw
}

// paramProblems turns a parameter validation failure into one line naming every
// bad field, so the operator fixes them in a single pass instead of discovering
// them one run at a time.
func paramProblems(err error) (string, bool) {
	var ve *strategy.ValidationError
	if !errors.As(err, &ve) {
		return "", false
	}
	parts := make([]string, 0, len(ve.Fields))
	for _, f := range ve.Fields {
		parts = append(parts, f.Key+" "+f.Message)
	}
	return "Check these settings: " + strings.Join(parts, "; "), true
}
