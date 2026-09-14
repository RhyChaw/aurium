package api

import (
	"net/http"
	"time"

	"github.com/RhyChaw/aurium/internal/store"
)

// Heartbeat is the centre pane's vitals.
//
// It exists so the pane is alive before the event stream has warmed up. The
// stream is what animates it afterwards; without this, opening the dashboard
// on a quiet fleet shows an empty box until something happens, which reads as
// broken rather than as calm.
type Heartbeat struct {
	TS string `json:"ts"`
	// Agents counts each colour band, so the ring can be drawn from one number
	// per state rather than by walking every tile.
	Agents map[string]int `json:"agents"`
	// Containers counts by stored status.
	Containers map[string]int `json:"containers"`
	// Approvals is how many humans-are-blocking situations exist right now.
	Approvals int `json:"approvals"`
	// Unread is IPC waiting to be read across the fleet.
	Unread int `json:"unread"`
	// EventsPerMinute is the pulse: recent activity, not lifetime totals.
	EventsPerMinute float64 `json:"events_per_minute"`
	// TokensLastHour and CostLastHour are what the fleet has spent recently.
	TokensLastHour int64   `json:"tokens_last_hour"`
	CostLastHour   float64 `json:"cost_last_hour"`
	// Projects is how many projects exist, so the home page badge and this
	// pane cannot disagree.
	Projects int `json:"projects"`
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID := r.URL.Query().Get("project")

	hb := Heartbeat{
		TS:         time.Now().UTC().Format(time.RFC3339),
		Agents:     map[string]int{},
		Containers: map[string]int{},
	}

	projects, err := s.App.Store.ListProjects(ctx)
	if err != nil {
		storeError(w, err)
		return
	}
	hb.Projects = len(projects)

	scope := projects
	if projectID != "" {
		p, err := s.App.Store.GetProject(ctx, projectID)
		if err != nil {
			storeError(w, err)
			return
		}
		scope = []store.Project{p}
	}

	// Which agents are blocked on a human, so the count matches the rail's
	// red tiles rather than only the stored statuses.
	waiting := map[string]bool{}
	if s.Gateway != nil {
		if pending, err := s.Gateway.ListApprovals(ctx, "pending"); err == nil {
			hb.Approvals = len(pending)
			for _, ap := range pending {
				if ap.AgentID != "" {
					waiting[ap.AgentID] = true
				}
			}
		}
	}

	for _, p := range scope {
		agents, err := s.App.Store.ListProjectAgents(ctx, p.ID)
		if err != nil {
			storeError(w, err)
			return
		}
		for _, a := range agents {
			state := StateOf(a.Status)
			if waiting[a.ID] {
				state = StateAttention
			}
			hb.Agents[state]++
		}
		containers, err := s.App.Store.ListContainers(ctx, p.ID)
		if err != nil {
			storeError(w, err)
			return
		}
		for _, c := range containers {
			hb.Containers[c.Status]++
		}
	}

	hour := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if totals, err := s.App.Store.UsageTotals(ctx, store.UsageQuery{
		Since: hour, ProjectID: projectID,
	}); err == nil {
		hb.TokensLastHour = totals.InputTokens + totals.OutputTokens
		hb.CostLastHour = totals.CostUSD
	}

	// Events in the last five minutes, expressed per minute. A rate is what
	// the pane draws; a lifetime total would climb forever and say nothing
	// about now.
	var recent int64
	_ = s.App.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE ts >= ?`,
		time.Now().UTC().Add(-5*time.Minute).Format(time.RFC3339Nano)).Scan(&recent)
	hb.EventsPerMinute = float64(recent) / 5

	var unread int64
	_ = s.App.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE status != 'acked'`).Scan(&unread)
	hb.Unread = int(unread)

	writeJSON(w, http.StatusOK, hb)
}
