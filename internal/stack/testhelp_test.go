package stack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/gitx"
)

func ctx() context.Context { return context.Background() }

// repo is a git fixture with an Aurium-style worktree layout.
type repo struct {
	t    *testing.T
	root string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, root: t.TempDir()}
	r.git(r.root, "init", "-q", "-b", "main")
	r.git(r.root, "config", "user.email", "test@aurium.dev")
	r.git(r.root, "config", "user.name", "Aurium Test")
	r.git(r.root, "config", "commit.gpgsign", "false")
	r.write(r.root, "README.md", "# fixture\n")
	r.git(r.root, "add", "-A")
	r.git(r.root, "commit", "-qm", "initial")
	if err := gitx.EnsureExcluded(r.root); err != nil {
		t.Fatal(err)
	}
	return r
}

// worktree creates a container-style worktree for branch, started at startRef.
func (r *repo) worktree(branch, startRef string) string {
	r.t.Helper()
	wt, err := gitx.AddWorktree(ctx(), r.root, branch, branch, startRef)
	if err != nil {
		r.t.Fatal(err)
	}
	return wt
}

// commit writes a file in a worktree and commits it.
func (r *repo) commit(wt, name, content, msg string) string {
	r.t.Helper()
	r.write(wt, name, content)
	r.git(wt, "add", "-A")
	r.git(wt, "commit", "-qm", msg)
	return r.rev(wt, "HEAD")
}

func (r *repo) rev(dir, ref string) string {
	r.t.Helper()
	return r.git(dir, "rev-parse", ref)
}

func (r *repo) branchSHA(branch string) string {
	r.t.Helper()
	return r.git(r.root, "rev-parse", branch)
}

func (r *repo) write(dir, name, content string) {
	r.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func (r *repo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fsck asserts the object database is intact. Every destructive test ends with
// this: the one outcome Aurium can never produce is a corrupt repository.
func (r *repo) fsck() {
	r.t.Helper()
	r.git(r.root, "fsck", "--no-progress")
}

// log returns the subject lines of a branch, newest first.
func (r *repo) log(branch string) []string {
	r.t.Helper()
	out := r.git(r.root, "log", "--format=%s", branch)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
