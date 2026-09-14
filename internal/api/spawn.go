package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/store"
)

// Creating an agent was CLI-only: `aurium container create <branch>`. That
// leaves the dashboard able to make a project and then unable to put anything
// in it, which is half a loop. This is the other half.
//
// "Add an agent" is the user's unit, not "create a container": a container is
// the mechanism — a worktree on a branch — and it is created here because an
// agent needs one, not because anybody asked for one.

func (s *Server) spawnAgent(w http.ResponseWriter, r *http.Request) {
	// An agent that could spawn agents could fan out without bound, and the
	// delegation path (§9.4) exists for that with a depth cap. This route is
	// for a human.
	if !s.hostOnly(w, r) {
		return
	}

	var body struct {
		// RepoID picks which repository in the project. Empty means the only
		// one, or the first — with several, the caller should say.
		RepoID string `json:"repo_id"`
		// Name is what the rail shows. Empty gets "Agent N".
		Name string `json:"name"`
		// Branch defaults to a slug of the name, which keeps the git history
		// legible: a branch called `agent-3` says nothing six weeks later.
		Branch  string `json:"branch"`
		Adapter string `json:"adapter"`
		Model   string `json:"model"`
		Role    string `json:"role"`
		// Task creates a task and attaches the container to it.
		Task string `json:"task"`
		// ParentContainerID stacks the new container on an existing one.
		ParentContainerID string `json:"parent_container_id"`
		ProviderAccountID string `json:"provider_account_id"`
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

	repo, err := s.pickRepo(ctx, p.ID, body.RepoID)
	if err != nil {
		writeProvisionError(w, err)
		return
	}
	cfg, err := config.Load(filepath.Join(repo.Path, config.Filename))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := s.App.Store.ListProjectAgents(ctx, p.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = fmt.Sprintf("Agent %d", len(existing)+1)
	}

	branch := strings.TrimSpace(body.Branch)
	if branch == "" {
		branch = uniqueBranch(ctx, s.App.Store, repo.ID, runtime.Slugify(strings.ToLower(name)))
	}

	adapter := body.Adapter
	if adapter == "" {
		adapter = cfg.Sandbox.Agent
	}
	role := body.Role
	if role == "" {
		role = store.RolePrimary
	}

	parentBranch := repo.BaseBranch
	if body.ParentContainerID != "" {
		parent, err := s.App.Store.GetContainer(ctx, body.ParentContainerID)
		if err != nil {
			storeError(w, err)
			return
		}
		parentBranch = parent.Branch
	}

	taskID := ""
	if title := strings.TrimSpace(body.Task); title != "" {
		t, err := s.App.Store.CreateTask(ctx, p.ID, title, "")
		if err != nil {
			storeError(w, err)
			return
		}
		taskID = t.ID
	}

	c, err := s.App.Manager.Create(ctx, runtime.CreateOpts{
		ProjectID: p.ID, RepoID: repo.ID, RepoRoot: repo.Path, TaskID: taskID,
		Branch: branch, ParentBranch: parentBranch,
		ParentContainerID: body.ParentContainerID,
		Config:            cfg, Adapter: adapter, Role: role, Model: body.Model,
		ProviderAccountID: body.ProviderAccountID,
	})
	if err != nil {
		// Almost every failure here is the user's to fix: a branch that
		// already exists, a dirty worktree, a driver that is not running.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	agents, err := s.App.Store.ListAgents(ctx, c.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	if len(agents) == 0 {
		writeError(w, http.StatusInternalServerError,
			"the container was created but no agent started in it")
		return
	}
	a := agents[0]
	if err := s.App.Store.SetAgentDisplayName(ctx, a.ID, name); err != nil {
		storeError(w, err)
		return
	}
	a.DisplayName = name

	writeJSON(w, http.StatusCreated, map[string]any{
		"agent": a, "container": c, "state": StateOf(a.Status),
	})
}

// pickRepo resolves which repository an agent belongs in.
//
// With one repository the choice is made; with several it is a real decision
// and guessing would put an agent in the wrong tree, so the caller is asked.
func (s *Server) pickRepo(ctx context.Context, projectID, repoID string) (store.Repository, error) {
	repos, err := s.App.Store.ListRepositories(ctx, projectID)
	if err != nil {
		return store.Repository{}, err
	}
	switch {
	case len(repos) == 0:
		return store.Repository{}, fmt.Errorf(
			"this project has no repositories yet; add one before creating an agent")
	case repoID == "" && len(repos) == 1:
		return repos[0], nil
	case repoID == "":
		names := make([]string, 0, len(repos))
		for _, r := range repos {
			names = append(names, filepath.Base(r.Path))
		}
		return store.Repository{}, fmt.Errorf(
			"this project has %d repositories (%s); say which one with repo_id",
			len(repos), strings.Join(names, ", "))
	}
	for _, r := range repos {
		if r.ID == repoID {
			return r, nil
		}
	}
	return store.Repository{}, fmt.Errorf("%w: repository %s is not in this project",
		store.ErrNotFound, repoID)
}

// uniqueBranch avoids colliding with a branch that already exists, so clicking
// "+ agent" twice does not fail the second time on a name the user never chose.
func uniqueBranch(ctx context.Context, st *store.Store, repoID, base string) string {
	if base == "" {
		base = "agent"
	}
	candidate := base
	for n := 2; n < 100; n++ {
		var count int
		err := st.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM containers WHERE repo_id = ? AND branch = ?`,
			repoID, candidate).Scan(&count)
		if err != nil || count == 0 {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
	return base + "-" + store.ContainerCreating
}
