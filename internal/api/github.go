package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/github"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// GitHub, for three things the dashboard could not do: say which repositories
// you have, put one in a project, and show what became of an agent's branch.
//
// All host-only. An agent reaches GitHub through the MCP gateway under a grant
// (§10), which is a different path with different rules; letting it call these
// would route around every one of them.

// githubStatus is what the Providers tab needs to render the GitHub card.
type githubStatus struct {
	Connected bool   `json:"connected"`
	Source    string `json:"source,omitempty"`
	Login     string `json:"login,omitempty"`
	Name      string `json:"name,omitempty"`
	// Error explains a token that exists but does not work, which is a
	// different problem from having none.
	Error string `json:"error,omitempty"`
	// CLIAvailable says whether `gh` could supply one.
	CLIAvailable bool `json:"cli_available"`
}

func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	ctx := r.Context()

	client, err := s.githubClient(ctx)
	out := githubStatus{CLIAvailable: github.CLIToken(ctx) != ""}
	if err != nil {
		out.Error = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if client == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	out.Source = client.Source
	// The token is checked rather than assumed. A stale one that still looks
	// like a token would otherwise show as connected until the first repo
	// listing failed for no visible reason.
	user, err := client.Viewer(ctx)
	if err != nil {
		out.Error = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Connected = true
	out.Login = user.Login
	out.Name = user.Name
	writeJSON(w, http.StatusOK, out)
}

// listGitHubRepos is the picker's feed.
func (s *Server) listGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	ctx := r.Context()

	client, err := s.githubClient(ctx)
	if err != nil || client == nil {
		writeError(w, http.StatusBadRequest, githubMissing(err))
		return
	}

	repos, err := client.ListRepos(ctx, int(atoiOr(r.URL.Query().Get("limit"), 100)))
	if err != nil {
		writeGitHubError(w, err)
		return
	}
	if repos == nil {
		repos = []github.Repo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// cloneGitHubRepo brings a repository onto the machine and attaches it.
//
// Cloning and attaching are one call because they are one intent: a repository
// on disk that no project knows about is not what anybody asked for, and
// leaving the user to do the second step by hand is leaving them halfway.
func (s *Server) cloneGitHubRepo(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}

	var body struct {
		// FullName is owner/repo.
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		// Dest is where to put it. Empty means under the project root, which
		// is where a project's own repositories belong.
		Dest       string `json:"dest"`
		BaseBranch string `json:"base_branch"`
		Driver     string `json:"driver"`
		Image      string `json:"image"`
		Agent      string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.CloneURL) == "" {
		writeError(w, http.StatusBadRequest, "clone_url is required")
		return
	}

	ctx := r.Context()
	p, err := s.App.Store.GetProject(ctx, r.PathValue("project"))
	if err != nil {
		storeError(w, err)
		return
	}

	client, err := s.githubClient(ctx)
	if err != nil || client == nil {
		writeError(w, http.StatusBadRequest, githubMissing(err))
		return
	}

	dest := strings.TrimSpace(body.Dest)
	if dest == "" {
		name := body.FullName
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			name = "repo"
		}
		dest = filepath.Join(p.Root, name)
	}
	dest, err = resolveDest(p.Root, dest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Cloning can take a while on a large repository; the request holds for it
	// rather than returning a handle, because the caller has nothing useful to
	// do until the repository exists.
	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := github.Clone(cloneCtx, body.CloneURL, dest, client.Token); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	repo, err := s.App.AttachRepository(ctx, p,
		app.RepoSpec{Path: dest, BaseBranch: body.BaseBranch},
		body.Driver, body.Image, body.Agent)
	if err != nil {
		// The clone succeeded and the attach did not, which is worth saying
		// precisely: the files are on disk and re-running will not re-clone.
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"cloned to %s but could not attach it: %v", dest, err))
		return
	}
	if err := s.App.SyncDescriptor(ctx, p); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"repository": repo, "path": dest})
}

// resolveDest keeps a clone inside the project root.
//
// Without it, `dest` is an arbitrary filesystem path a request can name, and
// this route would clone anything anywhere as the user — which is a great deal
// more authority than "add a repository to this project" asks for.
func resolveDest(root, dest string) (string, error) {
	abs := dest
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, dest)
	}
	abs = filepath.Clean(abs)

	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("a cloned repository must live under the project root (%s)", root)
	}
	return abs, nil
}

// --- pull request state on containers ---

// prCache keeps GitHub out of the hot path.
//
// The rail refreshes on every event; asking GitHub for a PR state each time
// would exhaust the rate limit in a busy minute and make the dashboard as slow
// as the network. A short TTL is the right trade: a PR badge that is thirty
// seconds stale is useful, and one that costs a round trip per repaint is not.
type prCache struct {
	mu      sync.Mutex
	entries map[string]prEntry
}

type prEntry struct {
	pr  *github.PullRequest
	at  time.Time
	err bool
}

const prTTL = 60 * time.Second

func (c *prCache) get(key string) (*github.PullRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > prTTL {
		return nil, false
	}
	return e.pr, true
}

func (c *prCache) put(key string, pr *github.PullRequest, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]prEntry{}
	}
	c.entries[key] = prEntry{pr: pr, at: time.Now(), err: failed}
}

// pullRequestsFor looks up the PR behind each container's branch.
//
// Best effort throughout: no GitHub connection, a repository with no remote, a
// rate limit — all of them mean "no badge", never "no rail".
func (s *Server) pullRequestsFor(ctx context.Context, containers []store.Container) map[string]*github.PullRequest {
	out := map[string]*github.PullRequest{}
	if len(containers) == 0 {
		return out
	}
	client, err := s.githubClient(ctx)
	if err != nil || client == nil {
		return out
	}

	remotes := map[string][2]string{} // repoID -> owner, repo
	for _, c := range containers {
		if _, seen := remotes[c.RepoID]; seen {
			continue
		}
		repo, err := s.App.Store.GetRepository(ctx, c.RepoID)
		if err != nil || repo.Remote == "" {
			continue
		}
		if owner, name, ok := github.OwnerRepo(repo.Remote); ok {
			remotes[c.RepoID] = [2]string{owner, name}
		}
	}

	for _, c := range containers {
		nwo, ok := remotes[c.RepoID]
		if !ok {
			continue
		}
		key := nwo[0] + "/" + nwo[1] + "#" + c.Branch
		if pr, fresh := s.prs.get(key); fresh {
			if pr != nil {
				out[c.ID] = pr
			}
			continue
		}
		// Bounded per lookup: a slow GitHub must not hold the rail.
		lookup, cancel := context.WithTimeout(ctx, 4*time.Second)
		pr, err := client.PullRequestForBranch(lookup, nwo[0], nwo[1], c.Branch)
		cancel()
		s.prs.put(key, pr, err != nil)
		if err == nil && pr != nil {
			out[c.ID] = pr
		}
	}
	return out
}

// githubClient resolves a token: the machine's `gh` login first, then anything
// the user stored (§D28).
func (s *Server) githubClient(ctx context.Context) (*github.Client, error) {
	if token := github.CLIToken(ctx); token != "" {
		return &github.Client{Token: token, Source: github.SourceCLI}, nil
	}
	ref := ""
	err := s.App.Store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(secret_ref,'') FROM integrations
		 WHERE name = 'github' AND secret_ref != '' LIMIT 1`).Scan(&ref)
	if err != nil || ref == "" {
		return nil, nil
	}
	token, err := s.App.Secrets.Get(ref)
	if err != nil {
		return nil, fmt.Errorf("the stored GitHub token could not be read: %w", err)
	}
	return &github.Client{Token: token, Source: github.SourceStored}, nil
}

func githubMissing(err error) string {
	if err != nil {
		return err.Error()
	}
	return "no GitHub connection. Run `gh auth login`, or connect a token on the Providers tab."
}

func writeGitHubError(w http.ResponseWriter, err error) {
	var ghErr *github.Error
	if errors.As(err, &ghErr) && ghErr.Unauthorized() {
		// The cached token is the one that just failed; asking `gh` again is
		// the whole recovery path after `gh auth login`.
		github.ForgetCLIToken()
		writeError(w, http.StatusBadRequest,
			"GitHub refused the token: "+ghErr.Message+". Try `gh auth login`.")
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

// enableGitHubTools gives a project's agents github_* capabilities.
//
// This is the other GitHub — the MCP gateway's (§10). Agents never touch the
// token: the daemon holds it, spawns the upstream server, and filters which
// tools each container may call. Ungranted tools are not listed at all, so an
// agent cannot try one it does not hold.
//
// The default grants are deliberately unequal. Reading is allowed outright;
// anything that writes to somebody else's repository is held for a human,
// because "an agent opened a pull request while I was at lunch" and "an agent
// merged one" are different sentences.
func (s *Server) enableGitHubTools(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	ctx := r.Context()

	p, err := s.App.Store.GetProject(ctx, r.PathValue("project"))
	if err != nil {
		storeError(w, err)
		return
	}
	if token := github.CLIToken(ctx); token == "" {
		writeError(w, http.StatusBadRequest,
			"`gh` is not logged in on this machine. Run `gh auth login` first — "+
				"Aurium borrows that token rather than storing a second copy of it.")
		return
	}

	var existing string
	_ = s.App.Store.DB().QueryRowContext(ctx,
		`SELECT id FROM integrations WHERE project_id = ? AND name = 'github'`,
		p.ID).Scan(&existing)

	if existing == "" {
		cfg, _ := json.Marshal(map[string]any{
			"command": "npx",
			"args": []string{
				"-y", "@modelcontextprotocol/server-github",
			},
			"secret_env": "GITHUB_PERSONAL_ACCESS_TOKEN",
			// No secret_ref: the token is borrowed from `gh` at connect time
			// and never written down (§D28).
			"token_from": app.TokenFromGitHubCLI,
		})
		id := newIntegrationID()
		if _, err := s.App.Store.DB().ExecContext(ctx,
			`INSERT INTO integrations (id, project_id, kind, name, config_json, secret_ref, status)
			 VALUES (?,?,?,?,?,?,?)`,
			id, p.ID, "mcp_stdio", "github", string(cfg), nil, "connecting"); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		existing = id
	}

	// Connect it now rather than at the next daemon start, so the button does
	// what it says while the user is looking at it.
	if err := s.App.ConnectUpstreams(ctx, s.Log); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	var status, lastErr string
	_ = s.App.Store.DB().QueryRowContext(ctx,
		`SELECT status, COALESCE(last_error,'') FROM integrations WHERE id = ?`,
		existing).Scan(&status, &lastErr)

	caps := 0
	_ = s.App.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM capabilities WHERE integration_id = ? AND removed = 0`,
		existing).Scan(&caps)

	writeJSON(w, http.StatusOK, map[string]any{
		"integration":  existing,
		"status":       status,
		"last_error":   lastErr,
		"capabilities": caps,
	})
}

// newIntegrationID exists so this file does not import internal/ids purely for
// one call in one place.
func newIntegrationID() string { return ids.New(ids.Integration) }
