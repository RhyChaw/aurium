// Package api is the daemon's HTTP surface (§11.2).
//
// D21: this API is the product boundary. The CLI, the embedded dashboard, a
// Tauri shell and a future cloud runtime are all clients of it, so anything
// only the CLI can do is a design mistake.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/store"
)

// Server serves the Aurium API.
type Server struct {
	App *app.App
	Log *slog.Logger
	// HostToken authenticates local clients (the CLI, the dashboard). It is
	// written to ~/.aurium/token with mode 0600.
	HostToken string

	// Gateway serves the MCP endpoint containers talk to. Optional: without
	// it /mcp reports itself unavailable rather than half-working.
	Gateway *gateway.Gateway

	mux *http.ServeMux

	// prs caches pull-request lookups. The rail repaints on every event and
	// GitHub has a rate limit; a badge that is a minute stale is useful, one
	// that costs a round trip per repaint is not.
	prs prCache
}

// contextKey namespaces values this package puts on a request context.
type contextKey string

const tokenKey contextKey = "aurium.token"

// New builds a server with all routes registered.
func New(a *app.App, hostToken string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{App: a, Log: log, HostToken: hostToken, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Routes returns the registered route patterns, used by a test that keeps
// api/openapi.yaml honest.
func (s *Server) Routes() []string { return registeredRoutes }

var registeredRoutes []string

func (s *Server) handle(pattern string, h http.HandlerFunc, scope string) {
	registeredRoutes = appendUnique(registeredRoutes, pattern)
	s.mux.Handle(pattern, s.authenticate(scope, h))
}

func appendUnique(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}

func (s *Server) routes() {
	// Health is deliberately unauthenticated: the CLI polls it to decide
	// whether the daemon it just spawned is up, before it has read a token.
	s.mux.HandleFunc("GET /v1/health", s.health)

	s.handle("GET /v1/events", s.eventsSSE, "")
	s.handle("GET /v1/projects", s.listProjects, "")
	s.handle("POST /v1/projects", s.createProject, "")
	s.handle("GET /v1/projects/{project}", s.getProject, "")
	s.handle("POST /v1/projects/{project}/repos", s.addRepository, "")
	s.handle("GET /v1/projects/{project}/agents", s.projectAgents, "")
	s.handle("POST /v1/projects/{project}/agents", s.spawnAgent, "")
	s.handle("GET /v1/projects/{project}/containers", s.listContainers, "")
	s.handle("GET /v1/projects/{project}/tasks", s.listTasks, "")
	s.handle("POST /v1/projects/{project}/tasks", s.createTask, "task:*")
	s.handle("GET /v1/projects/{project}/tree", s.projectTree, "")
	s.handle("GET /v1/containers/{container}", s.getContainer, "")
	s.handle("GET /v1/containers/{container}/snapshots", s.listSnapshots, "")
	s.handle("POST /v1/containers/{container}/snapshot", s.snapshotContainer, "snapshot:self")
	s.handle("POST /v1/containers/{container}/sync", s.syncContainer, "")
	s.handle("GET /v1/containers/{container}/agents", s.listAgents, "")
	s.handle("PATCH /v1/tasks/{task}", s.patchTask, "task:*")
	s.handle("GET /v1/agents/{agent}", s.getAgent, "")
	s.handle("GET /v1/agents/{agent}/messages", s.agentMessages, "")
	s.handle("POST /v1/agents/{agent}/message", s.sendToAgent, "ipc:*")
	s.handle("GET /v1/approvals", s.listApprovals, "")
	s.handle("POST /v1/approvals/{approval}/decide", s.decideApproval, "")

	// Provider accounts and what they spend. Host-only, enforced in the
	// handlers: an agent that could connect an account could hand itself a
	// credential.
	s.handle("GET /v1/providers", s.listProviders, "")
	s.handle("POST /v1/providers", s.connectProvider, "")
	s.handle("GET /v1/providers/detect", s.detectProviders, "")
	s.handle("DELETE /v1/providers/{account}", s.disconnectProvider, "")
	// GitHub: what repositories you have, putting one in a project, and what
	// became of an agent's branch. Host-only — an agent reaches GitHub through
	// the MCP gateway under a grant (§10), which is a different path with
	// different rules.
	s.handle("GET /v1/drivers", s.listDrivers, "")
	s.handle("GET /v1/github", s.githubStatus, "")
	s.handle("GET /v1/github/repos", s.listGitHubRepos, "")
	s.handle("POST /v1/projects/{project}/github/clone", s.cloneGitHubRepo, "")
	s.handle("POST /v1/projects/{project}/github/tools", s.enableGitHubTools, "")
	s.handle("GET /v1/usage", s.usageReport, "")
	s.handle("GET /v1/usage/series", s.usageSeries, "")
	s.handle("GET /v1/heartbeat", s.heartbeat, "")
	// The same check table `aurium doctor` renders, for the dashboard's
	// setup wizard.
	s.handle("GET /v1/preflight", s.preflight, "")

	// The MCP endpoint every container reaches through the aurium-mcp shim.
	s.handle("POST /mcp", s.handleMCP, "gateway:*")

	// The dashboard, embedded so the daemon is one binary.
	s.mux.Handle("GET /", s.dashboard())
}

// authenticate checks the bearer token and, when scope is non-empty, that the
// token carries it.
//
// Host clients present the host token and are trusted fully. Containers
// present a scoped token; everything they can reach is bounded by it, which is
// what makes a leaked container token a limited problem rather than a total one.
func (s *Server) authenticate(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := bearer(r)
		if presented == "" {
			unauthorized(w)
			return
		}

		if s.HostToken != "" && presented == s.HostToken {
			next(w, r)
			return
		}

		info, err := s.App.Store.AuthenticateToken(r.Context(), presented)
		if err != nil {
			unauthorized(w)
			return
		}
		if scope != "" && !info.Has(scope) {
			// 403, not 404: the caller is authenticated, just not permitted.
			writeError(w, http.StatusForbidden, fmt.Sprintf(
				"token lacks the %s scope", scope))
			return
		}

		// A container token may only ever act on its own container. Without
		// this, any container could snapshot or read any other.
		if info.ContainerID != "" {
			if id := r.PathValue("container"); id != "" && id != info.ContainerID {
				writeError(w, http.StatusForbidden,
					"this token is scoped to a different container")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), tokenKey, info)))
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if token, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	// EventSource cannot set request headers, so the SSE endpoint alone also
	// accepts the token as a query parameter. It is confined to that one route
	// deliberately: query strings land in logs and browser history, so this is
	// a concession to the browser API, not a general alternative.
	if r.Method == http.MethodGet && r.URL.Path == "/v1/events" {
		return strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return ""
}

// tokenFrom returns the authenticated container token, if any. A host client
// has none, which is how handlers tell "the user" from "an agent".
func tokenFrom(r *http.Request) (store.TokenInfo, bool) {
	info, ok := r.Context().Value(tokenKey).(store.TokenInfo)
	return info, ok
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
		// Identity, so a client can tell this daemon from a stale one left
		// running by an older build. Version alone cannot: a dev build keeps
		// the same string across every rebuild, which is exactly when a
		// forgotten daemon is most confusing.
		"pid":        os.Getpid(),
		"started_at": StartedAt.UTC().Format(time.RFC3339),
		"uptime":     time.Since(StartedAt).Truncate(time.Second).String(),
	})
}

// Version is stamped at build time.
var Version = "0.1.0-dev"

// StartedAt is when this process began serving. It is package state rather
// than a Server field because health is the one route that must answer before
// anything else is wired up.
var StartedAt = time.Now()

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("api: encoding response", "err", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="aurium"`)
	writeError(w, http.StatusUnauthorized, "a bearer token is required")
}

// storeError maps store errors onto status codes so clients can branch.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrUnauthorized):
		unauthorized(w)
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
