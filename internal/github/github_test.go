package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnerRepoHandlesBothRemoteForms(t *testing.T) {
	cases := map[string][2]string{
		"git@github.com:RhyChaw/aurium.git":     {"RhyChaw", "aurium"},
		"git@github.com:RhyChaw/aurium":         {"RhyChaw", "aurium"},
		"https://github.com/RhyChaw/aurium.git": {"RhyChaw", "aurium"},
		"https://github.com/RhyChaw/aurium":     {"RhyChaw", "aurium"},
		"ssh://git@github.com/RhyChaw/aurium":   {"RhyChaw", "aurium"},
	}
	for remote, want := range cases {
		owner, repo, ok := OwnerRepo(remote)
		if !ok || owner != want[0] || repo != want[1] {
			t.Errorf("OwnerRepo(%q) = %q/%q (ok=%v), want %q/%q",
				remote, owner, repo, ok, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "not a url", "https://github.com/", "https://github.com/onlyowner"} {
		if _, _, ok := OwnerRepo(bad); ok {
			t.Errorf("OwnerRepo(%q) should not parse", bad)
		}
	}
}

// A branch with no pull request is the ordinary case. Treating it as an error
// would make every tile in a fresh project report a failure.
func TestPullRequestForBranchReturnsNothingWhenThereIsNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := &Client{Token: "t", BaseURL: srv.URL}
	pr, err := c.PullRequestForBranch(context.Background(), "o", "r", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if pr != nil {
		t.Fatalf("want no PR, got %+v", pr)
	}
}

func TestPullRequestCarriesChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 7, "title": "Add OAuth", "state": "open", "draft": false,
				"html_url": "https://github.com/o/r/pull/7",
				"head":     map[string]any{"sha": "abc123"},
			}})
		case strings.Contains(r.URL.Path, "/status"):
			json.NewEncoder(w).Encode(map[string]any{"state": "failure"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Token: "t", BaseURL: srv.URL}
	pr, err := c.PullRequestForBranch(context.Background(), "o", "r", "oauth")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 7 || pr.State != "open" {
		t.Fatalf("pr = %+v", pr)
	}
	// A red build is the whole point of showing this next to a container.
	if pr.Checks != "failure" {
		t.Fatalf("checks = %q", pr.Checks)
	}
}

// A PR is still worth showing when the checks call fails; failing the badge
// because a second request was slow is the wrong trade.
func TestPullRequestSurvivesAFailingChecksCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls") {
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 1, "state": "open", "head": map[string]any{"sha": "x"},
			}})
			return
		}
		http.Error(w, `{"message":"nope"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{Token: "t", BaseURL: srv.URL}
	pr, err := c.PullRequestForBranch(context.Background(), "o", "r", "b")
	if err != nil {
		t.Fatalf("a failing checks call must not fail the PR lookup: %v", err)
	}
	if pr == nil || pr.Checks != "" {
		t.Fatalf("pr = %+v", pr)
	}
}

// A caller must be able to tell "your token expired" from "no such repo".
func TestErrorCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{Token: "stale", BaseURL: srv.URL}
	_, err := c.Viewer(context.Background())
	var ghErr *Error
	if err == nil {
		t.Fatal("expected an error")
	}
	if !asGitHubError(err, &ghErr) {
		t.Fatalf("want a *github.Error, got %T", err)
	}
	if !ghErr.Unauthorized() || !strings.Contains(ghErr.Message, "Bad credentials") {
		t.Fatalf("err = %+v", ghErr)
	}
	// The message must never carry the credential it failed with.
	if strings.Contains(err.Error(), "stale") {
		t.Fatal("the error leaked the token")
	}
}

func asGitHubError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// Cloning over an existing directory would leave the user guessing whether
// anything was overwritten.
func TestCloneRefusesANonEmptyDestination(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Clone(context.Background(), "https://example.com/o/r.git", dir, "")
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// A token in an error message is a token in a log.
func TestRedactKeepsTheTokenOut(t *testing.T) {
	got := redact("failed to clone https://x:ghp_SECRET@github.com/o/r", "ghp_SECRET")
	if strings.Contains(got, "ghp_SECRET") {
		t.Fatalf("redact left the token in: %q", got)
	}
}

// The rail asks for a token on every repaint, which is every event. Without a
// cache that is a subprocess spawn many times a second.
func TestCLITokenIsCached(t *testing.T) {
	ForgetCLIToken()
	ctx := context.Background()

	start := time.Now()
	first := CLIToken(ctx)
	firstCall := time.Since(start)

	start = time.Now()
	second := CLIToken(ctx)
	cachedCall := time.Since(start)

	if first != second {
		t.Fatal("two calls a moment apart must return the same token")
	}
	// A cached read is a map lookup; an uncached one forks a process. Even on
	// a machine with no gh at all, the second must not be slower.
	if cachedCall > firstCall && cachedCall > 5*time.Millisecond {
		t.Fatalf("the second call was not cached: %v then %v", firstCall, cachedCall)
	}

	// And forgetting must actually forget, or `gh auth login` could never be
	// picked up without restarting the daemon.
	ForgetCLIToken()
	if cliTokenCache.at.IsZero() == false {
		t.Fatal("ForgetCLIToken must clear the timestamp")
	}
}

// A public repository needs no credential, so git never calls GIT_ASKPASS —
// and a script that deleted itself on first run therefore never ran and never
// went away, leaving a file containing a GitHub token in the temp directory.
// Found by a test that swept /tmp after a real clone.
func TestAskpassScriptIsRemovedEvenWhenGitNeverCallsIt(t *testing.T) {
	before := askpassCount(t)

	// A clone that fails immediately: git is handed a URL that resolves to
	// nothing, so it never gets as far as asking for credentials.
	err := Clone(context.Background(), "https://127.0.0.1:1/nope.git",
		filepath.Join(t.TempDir(), "repo"), "ghp_TESTTOKEN")
	if err == nil {
		t.Fatal("expected the clone to fail")
	}
	// And the failure must not carry the token either.
	if strings.Contains(err.Error(), "ghp_TESTTOKEN") {
		t.Fatalf("the error leaked the token: %v", err)
	}

	if after := askpassCount(t); after != before {
		t.Fatalf("an askpass script was left behind: %d then %d", before, after)
	}
}

func TestAskpassScriptIsNotWorldReadable(t *testing.T) {
	path, err := askpassScript("ghp_TESTTOKEN")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("a file holding a token is mode %o; it must not be readable by anyone else", mode)
	}
}

func askpassCount(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "aurium-askpass-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}
