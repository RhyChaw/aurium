package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RhyChaw/aurium/internal/events"
)

// heartbeat keeps intermediaries and browsers from closing an idle stream.
// A quiet Aurium is normal — agents think for minutes at a time — so without
// this the dashboard would appear to disconnect whenever nothing happened.
const heartbeat = 25 * time.Second

// eventsSSE streams the event log (§11.2).
//
// A client reconnecting passes the last id it saw, either as ?since= or via
// the standard Last-Event-ID header, and gets everything after it before the
// live stream resumes. That replay-then-subscribe order is what makes the
// stream lossless across a dropped connection.
func (s *Server) eventsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	filter := events.Filter{
		ProjectID:   r.URL.Query().Get("project"),
		ContainerID: r.URL.Query().Get("container"),
		AgentID:     r.URL.Query().Get("agent"),
	}
	if types := r.URL.Query().Get("types"); types != "" {
		filter.Types = strings.Split(types, ",")
	}

	since := atoiOr(r.URL.Query().Get("since"), -1)
	if since < 0 {
		since = atoiOr(r.Header.Get("Last-Event-ID"), -1)
	}

	// Subscribe BEFORE replaying. The other order has a gap: an event emitted
	// between the replay query and the subscription would be lost forever.
	// Subscribing first can duplicate an event instead, which clients dedupe
	// by id — a far better failure than silently missing one.
	live, cancel := s.App.Events.Subscribe(filter)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var lastSent int64
	send := func(e events.Event) bool {
		if e.ID <= lastSent {
			return true // already delivered by the replay
		}
		body, err := json.Marshal(e)
		if err != nil {
			return true
		}
		// Deliberately no `event:` field. Naming the SSE event after the
		// Aurium event type would make the browser dispatch a typed event,
		// which EventSource can only receive through a per-type listener —
		// and the set of types is open, so no client could register them all.
		// The type is already in the JSON payload, so nothing is lost and
		// every client receives every event through onmessage.
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, body); err != nil {
			return false
		}
		lastSent = e.ID
		flusher.Flush()
		return true
	}

	if since >= 0 {
		past, err := s.App.Events.Replay(r.Context(), since, filter, 1000)
		if err == nil {
			for _, e := range past {
				if !send(e) {
					return
				}
			}
		}
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-live:
			if !open {
				// The bus dropped us for falling behind. Say so rather than
				// leaving the client believing it has a live stream.
				fmt.Fprint(w, "data: {\"type\":\"aurium.disconnected\",\"reason\":\"subscriber fell behind\"}\n\n")
				flusher.Flush()
				return
			}
			if !send(e) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
