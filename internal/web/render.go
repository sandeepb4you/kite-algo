package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/options"
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

// positionsView is what the positions partial needs: the rows, plus the CSRF
// token for the per-row close buttons.
//
// An explicit type rather than the enclosing page view, so the partial stays
// usable from any page regardless of that page's data shape — the coupling that
// previously broke the market page at render time.
type positionsView struct {
	Positions []broker.Position
	CSRF      string
	// Book labels the section and decides whether it is styled as real money.
	Book broker.Book
	// Total is this book's P&L. Each section carries its own, because a
	// combined figure would add real rupees to simulated ones.
	Total float64
}

// splitByBook separates positions into the real book and the simulated one.
//
// Real first, and never interleaved. A badge on a blended list is easy to miss
// while scanning, and mistaking a simulated position for a real one is the one
// misreading here that costs actual money.
func splitByBook(positions []broker.Position, csrf string) (real, paper positionsView) {
	real = positionsView{CSRF: csrf, Book: broker.BookReal}
	paper = positionsView{CSRF: csrf, Book: broker.BookPaper}
	for _, p := range positions {
		if p.Book.IsReal() {
			real.Positions = append(real.Positions, p)
			real.Total += p.PnL
		} else {
			paper.Positions = append(paper.Positions, p)
			paper.Total += p.PnL
		}
	}
	return real, paper
}

// symbolLabel is a trading symbol broken into readable parts.
//
// "NIFTY2681824350CE" is seventeen undifferentiated characters and the eye has
// to parse it a digit at a time to find the strike — which is the number you
// are actually looking for in a positions row. Split, it reads at a glance.
type symbolLabel struct {
	Name   string // "NIFTY 24350 CE"
	Expiry string // "18 Aug", empty when unknown
	Raw    string
}

// instrumentLabel decomposes a trading symbol for display.
//
// Falls back to the raw symbol whenever it cannot parse, which is the only safe
// direction: a prettified label that is subtly wrong about the strike would be
// worse than the dense original.
func instrumentLabel(symbol string) symbolLabel {
	out := symbolLabel{Name: symbol, Raw: symbol}

	spec, ok := options.ParseSymbol(symbol)
	if !ok || spec.Underlying == "" || spec.Strike <= 0 {
		return out
	}
	strike := strconv.FormatFloat(spec.Strike, 'f', -1, 64)
	out.Name = spec.Underlying + " " + strike + " " + spec.Type.String()
	if !spec.Expiry.IsZero() {
		out.Expiry = spec.Expiry.Format("02 Jan")
	}
	return out
}

// funcMap holds the formatting helpers templates use. Keeping presentation
// logic here rather than in JavaScript means paper and live views cannot drift.
func funcMap() template.FuncMap {
	return template.FuncMap{
		// posview bundles positions with the CSRF token for the shared partial.
		"posview": func(positions []broker.Position, csrf string) positionsView {
			return positionsView{Positions: positions, CSRF: csrf}
		},
		// realpos / paperpos split a position list by book, so the two are
		// rendered as separate sections with separate totals.
		"realpos": func(positions []broker.Position, csrf string) positionsView {
			r, _ := splitByBook(positions, csrf)
			return r
		},
		"paperpos": func(positions []broker.Position, csrf string) positionsView {
			_, p := splitByBook(positions, csrf)
			return p
		},
		// cap and capMoney render a risk limit, naming a zero as what it
		// actually means.
		//
		// A zero limit is "no limit" throughout risk.Check, but printed as a bare
		// 0 it reads as the opposite — "max open positions: 0" looks like a
		// lockout, and "max order value: 0.00" like a book that can trade
		// nothing. On a read-only policy table that is the whole message, so it
		// has to say the word.
		"cap": func(n int) string {
			if n <= 0 {
				return "no limit"
			}
			return strconv.Itoa(n)
		},
		// capMoney carries its own rupee sign, because the alternative is a
		// literal ₹ in the template sitting in front of the words "no limit".
		"capMoney": func(f float64) string {
			if f <= 0 {
				return "no limit"
			}
			return "₹" + money(f)
		},
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
		// instrument splits a dense trading symbol into readable parts.
		"instrument": instrumentLabel,
		"num":        func(f float64) string { return fmt.Sprintf("%.2f", f) },
		"pct":        func(f float64) string { return fmt.Sprintf("%.2f%%", f) },
		// ivpct renders a volatility given as a FRACTION (0.145) as a percentage
		// (14.50%). Distinct from pct, which takes a number already in percent —
		// passing an IV to that one renders 14% vol as "0.15%".
		"ivpct": func(f float64) string { return fmt.Sprintf("%.2f%%", f*100) },
		// greek renders delta/gamma at the precision they are actually read at.
		// %.2f would round every gamma on an index option to 0.00.
		"greek": func(f float64) string { return fmt.Sprintf("%.4f", f) },
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
