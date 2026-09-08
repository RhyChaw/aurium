package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/stack"
	"github.com/RhyChaw/aurium/internal/store"
)

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.App.Store.ListProjects(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": ps})
}

func (s *Server) listContainers(w http.ResponseWriter, r *http.Request) {
	cs, err := s.App.Store.ListContainers(r.Context(), r.PathValue("project"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": cs})
}

func (s *Server) getContainer(w http.ResponseWriter, r *http.Request) {
	c, err := s.App.Store.GetContainer(r.Context(), r.PathValue("container"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	as, err := s.App.Store.ListAgents(r.Context(), r.PathValue("container"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": as})
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.App.Store.ListSnapshots(r.Context(), r.PathValue("container"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	ts, err := s.App.Store.ListTasks(r.Context(), r.PathValue("project"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": ts})
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title  string `json:"title"`
		Parent string `json:"parent_task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}

	projectID := r.PathValue("project")
	t, err := s.App.Store.CreateTask(r.Context(), projectID, body.Title, body.Parent)
	if err != nil {
		storeError(w, err)
		return
	}
	if err := s.App.Events.Emit(r.Context(), events.Event{
		Type: events.TaskCreated, Actor: actorOf(r),
		ProjectID: projectID, TaskID: t.ID,
		Payload: map[string]any{"title": t.Title},
	}); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) patchTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	id := r.PathValue("task")
	if err := s.App.Store.TransitionTask(r.Context(), id, body.Status); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.App.Store.GetTask(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if err := s.App.Events.Emit(r.Context(), events.Event{
		Type: events.TaskTransitioned, Actor: actorOf(r),
		ProjectID: t.ProjectID, TaskID: t.ID,
		Payload: map[string]any{"status": t.Status},
	}); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// projectTree is what the dashboard renders: the forest with live sync status.
func (s *Server) projectTree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID := r.PathValue("project")

	p, err := s.App.Store.GetProject(ctx, projectID)
	if err != nil {
		storeError(w, err)
		return
	}
	cs, err := s.App.Store.ListContainers(ctx, projectID)
	if err != nil {
		storeError(w, err)
		return
	}

	type node struct {
		Container store.Container `json:"container"`
		Agents    []store.Agent   `json:"agents"`
		Status    string          `json:"sync_status"`
		Reason    string          `json:"reason,omitempty"`
		Depth     int             `json:"depth"`
		Orphaned  bool            `json:"orphaned"`
		Children  []*node         `json:"children,omitempty"`
	}

	var build func(n *stack.Node) *node
	build = func(n *stack.Node) *node {
		out := &node{Container: n.Container, Depth: n.Depth, Orphaned: n.Orphaned}
		out.Agents, _ = s.App.Store.ListAgents(ctx, n.Container.ID)

		// Recompute from git rather than trusting the stored status: the
		// parent may have moved since the watcher last looked.
		res, err := stack.CheckEligibility(ctx, gitx.New(n.Container.Worktree), stack.Target{
			Branch: n.Container.Branch, ParentBranch: n.Container.ParentBranch,
			BaseSHA: n.Container.BaseSHA,
		})
		if err == nil {
			out.Status, out.Reason = string(res.Eligibility), res.Reason
			if res.Eligibility == stack.Eligible {
				out.Status = "stale"
			}
		} else {
			out.Status = n.Container.Status
		}

		for _, c := range n.Children {
			out.Children = append(out.Children, build(c))
		}
		return out
	}

	baseBranch := "main"
	if repo, err := s.App.Store.RepositoryByPath(ctx, projectID, p.Root); err == nil {
		baseBranch = repo.BaseBranch
	}

	var roots []*node
	for _, r := range stack.BuildForest(cs, baseBranch) {
		roots = append(roots, build(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": p, "base_branch": baseBranch, "roots": roots,
	})
}

func (s *Server) snapshotContainer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	ctx := r.Context()
	id := r.PathValue("container")
	c, err := s.App.Store.GetContainer(ctx, id)
	if err != nil {
		storeError(w, err)
		return
	}
	p, err := s.App.Store.GetProject(ctx, c.ProjectID)
	if err != nil {
		storeError(w, err)
		return
	}
	_, _, cfg, err := s.App.Project(ctx, p.Root)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	sn, err := s.App.Manager.Snapshot(ctx, id, cfg, body.Label, "manual")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (s *Server) syncContainer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.App.Store.GetContainer(ctx, r.PathValue("container"))
	if err != nil {
		storeError(w, err)
		return
	}

	res, err := stack.Sync(ctx, gitx.New(c.Worktree), stack.Target{
		Branch: c.Branch, ParentBranch: c.ParentBranch, BaseSHA: c.BaseSHA,
	}, stack.Options{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if res.Eligibility == stack.Synced {
		if err := s.App.Store.UpdateContainerBaseSHA(ctx, c.ID, res.NewBaseSHA); err != nil {
			storeError(w, err)
			return
		}
		if err := s.App.Events.Emit(ctx, events.Event{
			Type: events.ContainerSynced, Actor: actorOf(r),
			ProjectID: c.ProjectID, ContainerID: c.ID,
			Payload: map[string]any{"new_base": res.NewBaseSHA, "replayed": res.Replayed},
		}); err != nil {
			storeError(w, err)
			return
		}
	}

	// A conflict is a normal outcome (§6.5), so it is 200 with a body that
	// says so — not an error status the client has to guess about.
	writeJSON(w, http.StatusOK, map[string]any{
		"result":    res.Eligibility,
		"new_base":  res.NewBaseSHA,
		"replayed":  res.Replayed,
		"conflicts": res.ConflictedFiles,
		"reason":    res.Reason,
	})
}

// actorOf attributes an action to the agent that made it, or to the human at
// the CLI. Every event answers "who did this" (§11.1).
func actorOf(r *http.Request) string {
	if info, ok := tokenFrom(r); ok && info.AgentID != "" {
		return events.ActorAgent(info.AgentID)
	}
	if info, ok := tokenFrom(r); ok && info.ContainerID != "" {
		return events.ActorAgent(info.ContainerID)
	}
	return events.ActorHuman
}

func atoiOr(s string, fallback int64) int64 {
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}
