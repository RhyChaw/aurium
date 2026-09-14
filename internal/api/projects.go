package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/store"
)

// getProject returns a project with its repositories, which is what the home
// page's card needs: a project with no repos and a project with three look the
// same without them.
func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.App.Store.GetProject(ctx, r.PathValue("project"))
	if err != nil {
		storeError(w, err)
		return
	}
	repos, err := s.App.Store.ListRepositories(ctx, p.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	containers, err := s.App.Store.ListContainers(ctx, p.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	agents, err := s.App.Store.ListProjectAgents(ctx, p.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": p, "repositories": repos,
		"containers": len(containers), "agents": len(agents),
		"standalone": p.Descriptor == "",
	})
}

// createProject is the dashboard's door in. D21: the CLI's `aurium init` and
// this route call the same provisioning code, so neither can be the one that
// really works.
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	// A container token must never create a project: an agent given the
	// ability to register repositories could attach anything on the host.
	if info, ok := tokenFrom(r); ok && info.ContainerID != "" {
		writeError(w, http.StatusForbidden, "an agent may not create projects")
		return
	}

	var np app.NewProject
	if err := json.NewDecoder(r.Body).Decode(&np); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	p, repos, err := s.App.CreateProject(r.Context(), np)
	if err != nil {
		writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"project": p, "repositories": repos})
}

// addRepository attaches a repository to an existing project and keeps the
// descriptor in step, so the file a human edits by hand says what the database
// believes.
func (s *Server) addRepository(w http.ResponseWriter, r *http.Request) {
	if info, ok := tokenFrom(r); ok && info.ContainerID != "" {
		writeError(w, http.StatusForbidden, "an agent may not attach repositories")
		return
	}

	var body struct {
		app.RepoSpec
		Driver string `json:"driver"`
		Image  string `json:"image"`
		Agent  string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ctx := r.Context()
	p, err := s.App.Store.GetProject(ctx, r.PathValue("project"))
	if err != nil {
		storeError(w, err)
		return
	}
	repo, err := s.App.AttachRepository(ctx, p, body.RepoSpec, body.Driver, body.Image, body.Agent)
	if err != nil {
		writeProvisionError(w, err)
		return
	}
	if err := s.App.SyncDescriptor(ctx, p); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}

// AgentTile is one thin box in the rail: an agent, the container it lives in,
// and everything needed to colour it.
type AgentTile struct {
	Agent     store.Agent `json:"agent"`
	Container struct {
		ID     string `json:"id"`
		Branch string `json:"branch"`
		Status string `json:"status"`
		RepoID string `json:"repo_id"`
	} `json:"container"`
	// State is the colour band (§ status colours): starting | working | done |
	// attention. It is computed here rather than in the browser so the CLI,
	// the desktop shell and any future client agree on what red means.
	State string `json:"state"`
	// Waiting is set when an approval addressed to this agent is pending. It
	// forces attention whatever the stored status says, because the agent is
	// in fact blocked on a human and the row may not have caught up.
	Waiting bool `json:"waiting"`
	// Unread is how many IPC messages are queued for this agent.
	Unread int `json:"unread"`
	// Provider names the company being billed, for the rail's subtitle.
	Provider string `json:"provider,omitempty"`
	// Thinking is a turn in flight right now. Not the same as the agent's
	// status: `running` means alive, which a shell agent is forever.
	Thinking bool `json:"thinking"`
	// CanChat is whether this agent can answer in the chat pane at all. A
	// `shell` agent cannot, and offering a composer that silently does nothing
	// would be worse than saying so.
	CanChat bool `json:"can_chat"`
}

// Agent states, as the rail paints them.
const (
	StateStarting  = "starting"  // amber: spinning up
	StateWorking   = "working"   // accent, pulsing: running, nothing needed
	StateDone      = "done"      // green: idle or exited
	StateAttention = "attention" // red: blocked, errored, or waiting on you
)

// StateOf maps an agent's stored status onto a colour band.
func StateOf(status string) string {
	switch status {
	case store.AgentStarting:
		return StateStarting
	case store.AgentRunning:
		return StateWorking
	case store.AgentBlocked, store.AgentError:
		return StateAttention
	case store.AgentIdle, store.AgentExited:
		return StateDone
	default:
		return StateDone
	}
}

// projectAgents is the rail's feed.
func (s *Server) projectAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID := r.PathValue("project")

	agents, err := s.App.Store.ListProjectAgents(ctx, projectID)
	if err != nil {
		storeError(w, err)
		return
	}
	containers, err := s.App.Store.ListContainers(ctx, projectID)
	if err != nil {
		storeError(w, err)
		return
	}
	byID := make(map[string]store.Container, len(containers))
	for _, c := range containers {
		byID[c.ID] = c
	}

	// Which agents are blocked on a human. Fetched once for the whole rail
	// rather than per tile: the rail redraws on every event.
	waiting := map[string]bool{}
	if s.Gateway != nil {
		if pending, err := s.Gateway.ListApprovals(ctx, "pending"); err == nil {
			for _, ap := range pending {
				if ap.AgentID != "" {
					waiting[ap.AgentID] = true
				}
			}
		}
	}
	unread := s.unreadByAgent(ctx, projectID)

	accounts := map[string]store.ProviderAccount{}
	if list, err := s.App.Store.ListProviderAccounts(ctx); err == nil {
		for _, a := range list {
			accounts[a.ID] = a
		}
	}

	tiles := make([]AgentTile, 0, len(agents))
	for _, a := range agents {
		t := AgentTile{Agent: a, State: StateOf(a.Status)}
		if c, ok := byID[a.ContainerID]; ok {
			t.Container.ID = c.ID
			t.Container.Branch = c.Branch
			t.Container.Status = c.Status
			t.Container.RepoID = c.RepoID
		}
		if waiting[a.ID] {
			t.Waiting, t.State = true, StateAttention
		}
		t.Unread = unread[a.ID]
		t.Thinking = s.App.Manager.IsThinking(a.ID)
		t.CanChat = s.App.Manager.CanConverse(a.Adapter)
		if acct, ok := accounts[a.ProviderAccountID]; ok {
			t.Provider = acct.Provider
		}
		tiles = append(tiles, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": tiles})
}

// unreadByAgent counts queued and delivered-but-unacked messages per agent.
// A read failure yields no counts rather than failing the rail: a missing
// badge is a smaller harm than a blank page.
func (s *Server) unreadByAgent(ctx context.Context, projectID string) map[string]int {
	out := map[string]int{}
	rows, err := s.App.Store.DB().QueryContext(ctx,
		`SELECT to_agent_id, count(*) FROM messages
		 WHERE project_id = ? AND to_agent_id IS NOT NULL AND status != 'acked'
		 GROUP BY to_agent_id`, projectID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return out
		}
		out[id] = n
	}
	return out
}

// writeProvisionError maps provisioning failures onto status codes. Almost all
// of them are the user's to fix — a path that is not a repository, a repo
// already in another project — so they must not read as daemon faults.
func writeProvisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
