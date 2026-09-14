// Package github is Aurium's own view of GitHub: which repositories you have,
// and what has become of the branch an agent is working on.
//
// It is deliberately separate from the MCP gateway's GitHub integration. That
// one exists so an *agent* can call github_* tools under a grant; this one
// exists so *Aurium* can show you a repo list and a PR badge. Same token,
// different consumers, and conflating them would mean every repo listing went
// through a per-agent permission check that has nothing to say about it.
//
// D28: the token comes from `gh` when `gh` is logged in.
//
// Storing a second copy of a credential the machine already holds creates a
// second thing to revoke and a second thing to go stale. `gh auth token` is a
// documented command whose whole purpose is handing the token to other tools,
// and it keeps Aurium out of the business of minting GitHub credentials.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// API is the GitHub REST base. Overridable so tests never touch the network.
const API = "https://api.github.com"

// TokenSource says where a token came from, which is what a user needs to know
// when it stops working.
const (
	// SourceCLI is `gh auth token` — the machine's existing login.
	SourceCLI = "gh_cli"
	// SourceStored is a token the user handed Aurium, held in the keyring.
	SourceStored = "stored"
)

// Client talks to GitHub as the user.
type Client struct {
	// Token is the credential. Never logged, never returned through the API.
	Token string
	// Source records where it came from.
	Source string
	// BaseURL defaults to API.
	BaseURL string
	// HTTP defaults to a client with a timeout — an unbounded GitHub call
	// would hang a dashboard request behind it.
	HTTP *http.Client
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return API
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// cliTokenCache holds the `gh` token briefly.
//
// Without it this is a subprocess spawn per call, and the rail asks on every
// repaint — which is every event, which on a busy fleet is many times a second.
// A minute is short enough that `gh auth logout` is noticed promptly and long
// enough that the cost disappears.
var cliTokenCache struct {
	sync.Mutex
	token string
	at    time.Time
}

// CLITokenTTL is how long a borrowed token is reused before `gh` is asked again.
const CLITokenTTL = time.Minute

// CLIToken reads the token `gh` already holds.
//
// It returns "" rather than an error when gh is absent or logged out, because
// "you have no GitHub connection" is a state the caller renders, not a fault.
func CLIToken(ctx context.Context) string {
	cliTokenCache.Lock()
	defer cliTokenCache.Unlock()
	if time.Since(cliTokenCache.at) < CLITokenTTL {
		return cliTokenCache.token
	}

	token := readCLIToken(ctx)
	cliTokenCache.token = token
	cliTokenCache.at = time.Now()
	return token
}

// ForgetCLIToken drops the cached token, so a caller that just saw GitHub
// refuse it asks `gh` again rather than reusing the one that failed.
func ForgetCLIToken() {
	cliTokenCache.Lock()
	defer cliTokenCache.Unlock()
	cliTokenCache.at = time.Time{}
	cliTokenCache.token = ""
}

func readCLIToken(ctx context.Context) string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	// Bounded: `gh` can block on a keyring prompt, and a rail repaint must not
	// wait on one.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "auth", "token")
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// Repo is a repository as the picker needs it.
type Repo struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	Description   string `json:"description"`
	UpdatedAt     string `json:"updated_at"`
	// Fork and Archived are surfaced because a list of eighty repositories is
	// mostly things you do not want to start an agent in.
	Fork     bool `json:"fork"`
	Archived bool `json:"archived"`
}

// User is who the token belongs to.
type User struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

// Viewer returns the authenticated user, which is also how a token is checked.
func (c *Client) Viewer(ctx context.Context) (User, error) {
	var u User
	err := c.get(ctx, "/user", &u)
	return u, err
}

// ListRepos returns the repositories the token can see, most recently pushed
// first — which is very nearly always the order somebody wants them in.
func (c *Client) ListRepos(ctx context.Context, limit int) ([]Repo, error) {
	if limit <= 0 || limit > 300 {
		limit = 100
	}

	var out []Repo
	for page := 1; len(out) < limit && page <= 3; page++ {
		var batch []struct {
			Repo
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		}
		path := fmt.Sprintf("/user/repos?per_page=100&page=%d&sort=pushed&affiliation=owner,collaborator,organization_member", page)
		if err := c.get(ctx, path, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, b := range batch {
			r := b.Repo
			r.Owner = b.Owner.Login
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// PullRequest is what a container's branch has become on GitHub.
type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"` // open | closed
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	URL     string `json:"html_url"`
	HeadSHA string `json:"head_sha"`
	// Checks is the combined CI conclusion: success | failure | pending | "".
	Checks string `json:"checks"`
	// Reviews is the review decision: approved | changes_requested | "".
	Reviews string `json:"reviews"`
}

// PullRequestForBranch finds the open PR whose head is branch, if any.
//
// Returns (nil, nil) when there is none: a branch with no PR is the ordinary
// case, not an error, and treating it as one would make every tile in a fresh
// project report a failure.
func (c *Client) PullRequestForBranch(ctx context.Context, owner, repo, branch string) (*PullRequest, error) {
	var list []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		State   string `json:"state"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
		Merged  bool   `json:"merged"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls?head=%s:%s&state=all&per_page=1",
		owner, repo, owner, branch)
	if err := c.get(ctx, path, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	p := list[0]
	pr := &PullRequest{
		Number: p.Number, Title: p.Title, State: p.State, Draft: p.Draft,
		URL: p.HTMLURL, Merged: p.Merged, HeadSHA: p.Head.SHA,
	}

	// Checks are a second call and a best effort: a PR with an unknown CI
	// state is still worth showing, and failing the whole badge because the
	// checks endpoint was slow would be the wrong trade.
	if pr.HeadSHA != "" {
		var status struct {
			State string `json:"state"`
		}
		if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s/status",
			owner, repo, pr.HeadSHA), &status); err == nil {
			pr.Checks = status.State
		}
	}
	return pr, nil
}

// Error is a GitHub API failure with its status, so a caller can tell "your
// token expired" from "that repository does not exist".
type Error struct {
	Status  int
	Message string
	Path    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github: %s: %d %s", e.Path, e.Status, e.Message)
}

// Unauthorized reports whether the token is the problem.
func (e *Error) Unauthorized() bool { return e.Status == 401 || e.Status == 403 }

func (c *Client) get(ctx context.Context, path string, into any) error {
	if c.Token == "" {
		return &Error{Status: 401, Message: "no GitHub token", Path: path}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	res, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		if body.Message == "" {
			body.Message = res.Status
		}
		return &Error{Status: res.StatusCode, Message: body.Message, Path: path}
	}
	return json.NewDecoder(res.Body).Decode(into)
}
