package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RhyChaw/aurium/internal/store"
	"github.com/RhyChaw/aurium/internal/usage"
)

// defaultWindow is what the Usage tab opens on. A day is long enough to cover
// a working session and short enough that the numbers still mean something.
const defaultWindow = 24 * time.Hour

// usageReport is the Usage tab's whole payload: one request, because four
// round trips to draw one screen is four chances for the parts to disagree.
func (s *Server) usageReport(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	ctx := r.Context()

	window, err := parseWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := store.UsageQuery{
		Since:     time.Now().UTC().Add(-window).Format(time.RFC3339Nano),
		ProjectID: r.URL.Query().Get("project"),
		AgentID:   r.URL.Query().Get("agent"),
		Provider:  r.URL.Query().Get("provider"),
	}

	totals, err := s.App.Store.UsageTotals(ctx, q)
	if err != nil {
		storeError(w, err)
		return
	}

	groups := map[string][]store.UsageGroup{}
	for _, by := range []string{"provider", "agent", "model", "project"} {
		g, err := s.App.Store.AggregateUsage(ctx, q, by)
		if err != nil {
			storeError(w, err)
			return
		}
		groups[by] = s.labelled(ctx, by, g)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window": window.String(),
		"since":  q.Since,
		"totals": totals,
		"groups": groups,
		"pricing": map[string]any{
			// The Usage tab states both of these on the page. A cost figure
			// whose staleness and coverage are invisible invites more trust
			// than it has earned.
			"verified": usage.PricesVerified,
			"models":   usage.Models(),
			"note": "List prices only. Discounts, batch pricing and cache tiers are not " +
				"modelled, and a subscription seat has no per-token price at all — those " +
				"tokens are counted and shown as unpriced.",
		},
		"coverage": "Metered where a provider reports it: headless runs through the claude " +
			"adapter. Interactive REPL turns are not counted — nothing surfaces their usage " +
			"to the daemon (ERD §9.1's provider proxy is what would).",
	})
}

// usageSeries is the sparkline behind the heartbeat.
func (s *Server) usageSeries(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	window, err := parseWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Aim for roughly 48 points whatever the window, so the shape of the line
	// is comparable between an hour and a week.
	bucket := int(window.Minutes() / 48)
	if bucket < 1 {
		bucket = 1
	}

	series, err := s.App.Store.UsageSeries(r.Context(), store.UsageQuery{
		Since:     time.Now().UTC().Add(-window).Format(time.RFC3339Nano),
		ProjectID: r.URL.Query().Get("project"),
	}, bucket)
	if err != nil {
		storeError(w, err)
		return
	}
	if series == nil {
		series = []store.UsageBucket{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window": window.String(), "bucket_minutes": bucket, "series": series,
	})
}

// labelled turns opaque ids into names. The store groups by id because that is
// what it has; a page showing "a_01J9Z…: $4.10" is a page nobody can read.
func (s *Server) labelled(ctx context.Context, by string, groups []store.UsageGroup) []store.UsageGroup {
	for i, g := range groups {
		if g.Key == "" {
			groups[i].Label = "unattributed"
			continue
		}
		switch by {
		case "agent":
			if a, err := s.App.Store.GetAgent(ctx, g.Key); err == nil {
				name := a.DisplayName
				if name == "" {
					name = a.Adapter
				}
				groups[i].Label = fmt.Sprintf("%s · %s", name, shortID(g.Key))
			}
		case "project":
			if p, err := s.App.Store.GetProject(ctx, g.Key); err == nil {
				groups[i].Label = p.Name
			}
		default:
			groups[i].Label = g.Key
		}
		if groups[i].Label == "" {
			groups[i].Label = shortID(g.Key)
		}
	}
	return groups
}

func shortID(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 && len(id) > i+7 {
		return id[:i+7]
	}
	return id
}

// parseWindow accepts a Go duration, defaulting to a day and refusing anything
// absurd — a ten-year window is a request that scans the whole table to draw
// forty-eight points.
func parseWindow(s string) (time.Duration, error) {
	if s == "" {
		return defaultWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("window %q is not a duration (try 1h, 24h, 168h)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("window must be positive")
	}
	if d > 365*24*time.Hour {
		return 0, fmt.Errorf("window must be a year or less")
	}
	return d, nil
}
