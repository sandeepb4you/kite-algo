package strategy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FieldError names one invalid parameter, so the UI can mark that field rather
// than showing a generic failure.
type FieldError struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// ValidationError collects every problem found in one submission, so the
// operator fixes them in a single pass instead of one per round-trip.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Key+": "+f.Message)
	}
	return strings.Join(parts, "; ")
}

// Defaults returns the descriptor's default parameter map.
func (d Descriptor) Defaults() map[string]any {
	out := make(map[string]any, len(d.Params))
	for _, p := range d.Params {
		if p.Default != nil {
			out[p.Key] = p.Default
		}
	}
	return out
}

// Normalize coerces raw parameters into their declared types, fills in defaults
// for anything missing, and validates ranges and enums.
//
// Raw values arrive from three places with three different Go types for the same
// number: HTML form posts give strings, YAML gives int, and JSON gives float64.
// Normalizing once here means Init can rely on the types being right, and every
// strategy gets identical validation instead of hand-rolling it.
//
// Unknown keys are rejected rather than ignored: silently dropping a misspelled
// parameter would leave a strategy running on a default the operator believes
// they overrode.
func (d Descriptor) Normalize(raw map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(d.Params))
	var errs []FieldError

	known := make(map[string]struct{}, len(d.Params))
	for _, p := range d.Params {
		known[p.Key] = struct{}{}
	}
	for key := range raw {
		if _, ok := known[key]; !ok {
			errs = append(errs, FieldError{Key: key, Message: "unknown parameter"})
		}
	}

	for _, p := range d.Params {
		v, present := raw[p.Key]
		if !present || isBlank(v) {
			if p.Default != nil {
				out[p.Key] = p.Default
			}
			continue
		}

		coerced, err := coerce(p, v)
		if err != nil {
			errs = append(errs, FieldError{Key: p.Key, Message: err.Error()})
			continue
		}
		out[p.Key] = coerced
	}

	if len(errs) > 0 {
		return nil, &ValidationError{Fields: errs}
	}
	return out, nil
}

// coerce converts one raw value to the kind its spec declares.
func coerce(p ParamSpec, v any) (any, error) {
	switch p.Kind {
	case KindInt:
		n, err := toFloat(v)
		if err != nil {
			return nil, fmt.Errorf("must be a whole number")
		}
		if n != float64(int(n)) {
			return nil, fmt.Errorf("must be a whole number")
		}
		if err := checkRange(p, n); err != nil {
			return nil, err
		}
		return int(n), nil

	case KindFloat:
		n, err := toFloat(v)
		if err != nil {
			return nil, fmt.Errorf("must be a number")
		}
		if err := checkRange(p, n); err != nil {
			return nil, err
		}
		return n, nil

	case KindBool:
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			// HTML checkboxes post "on" when ticked and nothing when not.
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "on", "yes", "1":
				return true, nil
			case "false", "off", "no", "0", "":
				return false, nil
			}
		}
		return nil, fmt.Errorf("must be true or false")

	case KindEnum:
		s := fmt.Sprint(v)
		for _, opt := range p.Options {
			if strings.EqualFold(s, opt) {
				return opt, nil
			}
		}
		return nil, fmt.Errorf("must be one of: %s", strings.Join(p.Options, ", "))

	case KindTime:
		s := strings.TrimSpace(fmt.Sprint(v))
		if _, err := time.Parse("15:04", s); err != nil {
			return nil, fmt.Errorf("must be a 24-hour time like 15:15")
		}
		return s, nil

	default: // KindString
		return strings.TrimSpace(fmt.Sprint(v)), nil
	}
}

func checkRange(p ParamSpec, n float64) error {
	if p.Min != nil && n < *p.Min {
		return fmt.Errorf("must be at least %g", *p.Min)
	}
	if p.Max != nil && n > *p.Max {
		return fmt.Errorf("must be at most %g", *p.Max)
	}
	return nil
}

func toFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(t), 64)
	}
	return 0, fmt.Errorf("not numeric")
}

// isBlank reports whether a raw value should be treated as "not supplied", so an
// empty form field falls back to the declared default instead of failing
// validation.
func isBlank(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// Ptr is a convenience for declaring Min/Max bounds in a ParamSpec literal.
func Ptr(f float64) *float64 { return &f }
