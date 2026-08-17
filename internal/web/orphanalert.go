package web

import (
	"fmt"
	"net/http"
	"strings"
)

// The unmanaged-position warning.
//
// Strategy state lives in memory; positions live in sqlite. So a process that
// stops with a strategy running leaves its positions open with nothing watching
// them — no delta monitoring, no exit_delta, no square-off clock. The UI looks
// perfectly healthy: the position is listed, its P&L updates, and nothing says
// that the logic which was supposed to close it is gone.
//
// Restore (app/strategies.go) fixes the common case by re-adopting those
// positions on the next boot. This banner covers what restore deliberately will
// not do: a strategy that cannot rebuild its state is refused rather than
// started blind, and a strategy stopped without squaring off leaves positions
// behind by the operator's own choice. Both leave real exposure unattended.
//
// It is computed live rather than latched at boot, so it appears whenever the
// condition becomes true and clears the moment the strategy is started or the
// position is closed.

// orphanAlert is the banner state for strategy positions nothing is managing.
type orphanAlert struct {
	Show bool
	// Headline names the strategies, because "unmanaged positions" alone does
	// not tell an operator with several strategies running which one to look at.
	Headline string
	Detail   string
	// Strategy is the single affected instance, for a direct link. Empty when
	// more than one is affected, since the link would have to pick one.
	Strategy string
}

// orphanAlertFor builds the banner.
func (s *Server) orphanAlertFor(r *http.Request) orphanAlert {
	groups := s.app.Orphans(r.Context())
	if len(groups) == 0 {
		return orphanAlert{}
	}

	names := make([]string, 0, len(groups))
	positions, refused := 0, 0
	for _, g := range groups {
		names = append(names, g.StrategyID)
		positions += len(g.Positions)
		if g.Reason != "" {
			refused++
		}
	}

	a := orphanAlert{
		Show: true,
		Headline: fmt.Sprintf("%s open with nothing managing %s — %s",
			plural(positions, "position", "positions"),
			pronoun(positions),
			strings.Join(names, ", ")),
		Detail: "Exits, delta monitoring and the square-off clock are not " +
			"running for these. Start the strategy again to re-adopt them, or " +
			"square them off from the positions panel.",
	}
	if len(groups) == 1 {
		a.Strategy = groups[0].StrategyID
	}
	return a
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// handleOrphanAlertFragment re-renders the banner on a poll, so it appears if a
// strategy dies mid-session and clears as soon as the position is dealt with.
func (s *Server) handleOrphanAlertFragment(w http.ResponseWriter, r *http.Request) {
	if err := s.render.Render(w, http.StatusOK, "orphan_alert.html", pageView{
		Orphan: s.orphanAlertFor(r),
	}); err != nil {
		s.log.Debug("render orphan alert failed", "err", err)
	}
}
