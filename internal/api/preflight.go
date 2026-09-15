package api

import (
	"net/http"

	"github.com/RhyChaw/aurium/internal/preflight"
)

// preflight serves the same check table `aurium doctor` renders, so the
// dashboard's setup wizard never develops a second opinion about what
// Aurium needs.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	// Host-only: this payload describes the host machine (its ~/.aurium home
	// path, its own git/docker/tmux failure text), not the container's
	// environment, which comes from the image regardless of what the host
	// reports.
	if !s.hostOnly(w, r) {
		return
	}
	// An empty addr omits the port check: over HTTP a port check is
	// meaningless, because if a caller reached this route the port is
	// necessarily held — by the very daemon answering the request.
	// Reporting that as a failure would report success as a problem.
	results := preflight.Run(r.Context(), preflight.Checks(""))
	writeJSON(w, http.StatusOK, map[string]any{"checks": results})
}
