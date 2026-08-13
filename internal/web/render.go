package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Renderer parses and executes the HTML templates.
//
// In production templates are parsed once at startup. With -dev they are
// re-read per request from disk so the UI can be edited without rebuilding.
type Renderer struct {
	dev  bool
	dir  string // on-disk template root, used only in dev mode
	mu   sync.RWMutex
	tmpl *template.Template
}

// NewRenderer parses the embedded templates.
func NewRenderer(dev bool) (*Renderer, error) {
	r := &Renderer{dev: dev, dir: "internal/web/templates"}
	t, err := r.parse()
	if err != nil {
		return nil, err
	}
	r.tmpl = t
	return r, nil
}

func (r *Renderer) parse() (*template.Template, error) {
	var src fs.FS = templateFS
	pattern := "templates/*.html"
	if r.dev {
		if _, err := os.Stat(r.dir); err == nil {
			src = os.DirFS(r.dir)
			pattern = "*.html"
		}
	}
	t, err := template.New("").Funcs(funcMap()).ParseFS(src, pattern)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return t, nil
}

// Render writes a named template as a complete HTML response.
//
// The template is executed into a buffer first: a template error midway through
// writing would otherwise emit a 200 with truncated HTML, which looks to the
// operator like the page simply lost half its content.
func (r *Renderer) Render(w http.ResponseWriter, status int, name string, data any) error {
	t := r.tmpl
	if r.dev {
		fresh, err := r.parse()
		if err != nil {
			return err
		}
		t = fresh
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("render %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

// funcMap holds the formatting helpers templates use. Keeping presentation
// logic here rather than in JavaScript means paper and live views cannot drift.
func funcMap() template.FuncMap {
	return template.FuncMap{
		// money formats rupees with Indian digit grouping (12,34,567.89).
		"money": money,
		// signed renders a PnL figure with an explicit sign.
		"signed": func(f float64) string {
			if f > 0 {
				return "+" + money(f)
			}
			return money(f)
		},
		// pnlClass picks a CSS class from the sign of a number.
		"pnlClass": func(f float64) string {
			switch {
			case f > 0:
				return "pnl-up"
			case f < 0:
				return "pnl-down"
			default:
				return "pnl-flat"
			}
		},
		"num":   func(f float64) string { return fmt.Sprintf("%.2f", f) },
		"pct":   func(f float64) string { return fmt.Sprintf("%.2f%%", f) },
		"upper": strings.ToUpper,
		"ist": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(istZone).Format("02 Jan 15:04:05")
		},
		"istClock": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(istZone).Format("15:04")
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return time.Since(t).Round(time.Second).String() + " ago"
		},
	}
}

var istZone = time.FixedZone("IST", 5*3600+30*60)

// money renders a rupee amount with Indian digit grouping: the last three
// digits are grouped, then every two thereafter (1,23,45,678.90).
func money(f float64) string {
	neg := f < 0
	f = math.Abs(f)
	whole := int64(f)
	frac := int64(math.Round((f - float64(whole)) * 100))
	if frac == 100 { // rounding carried into the rupee
		whole++
		frac = 0
	}

	digits := fmt.Sprintf("%d", whole)
	var grouped string
	if len(digits) <= 3 {
		grouped = digits
	} else {
		head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		grouped = strings.Join(parts, ",") + "," + tail
	}

	out := fmt.Sprintf("%s.%02d", grouped, frac)
	if neg {
		return "-" + out
	}
	return out
}
