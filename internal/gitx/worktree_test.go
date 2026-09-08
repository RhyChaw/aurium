package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddWorktreeCreatesBranchAtParent(t *testing.T) {
	root := initRepo(t)
	parentSHA := New(root).mustRev(t, "main")

	wt, err := AddWorktree(ctx(), root, "implement-oauth", "implement-oauth", "main")
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, ".aurium", "wt", "implement-oauth")
	if wt != want {
		t.Fatalf("worktree at %q, want %q (D4: worktrees live inside the repo)", wt, want)
	}
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatalf("worktree not checked out: %v", err)
	}

	g := New(wt)
	if b, _ := g.CurrentBranch(ctx()); b != "implement-oauth" {
		t.Fatalf("worktree is on branch %q, want implement-oauth", b)
	}
	if sha := g.mustRev(t, "HEAD"); sha != parentSHA {
		t.Fatalf("new branch starts at %s, want the parent tip %s", sha, parentSHA)
	}
}

func TestEnsureExcludedIsIdempotent(t *testing.T) {
	root := initRepo(t)
	for i := 0; i < 3; i++ {
		if err := EnsureExcluded(root); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), ".aurium/"); n != 1 {
		t.Fatalf(".aurium/ appears %d times in info/exclude, want exactly 1", n)
	}
}

func TestWorktreeDoesNotShowUpAsUntrackedInTheParent(t *testing.T) {
	root := initRepo(t)
	if err := EnsureExcluded(root); err != nil {
		t.Fatal(err)
	}
	if _, err := AddWorktree(ctx(), root, "feature", "feature", "main"); err != nil {
		t.Fatal(err)
	}

	clean, err := New(root).IsClean(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		out, _ := New(root).Run(ctx(), "status", "--porcelain", "-uall")
		t.Fatalf("creating a worktree must not dirty the parent repo; status:\n%s", out)
	}
}

func TestContainerEnvSetsIdenticalPathHooksAndGuard(t *testing.T) {
	env := ContainerEnv("/Users/a/app", "implement-oauth", Identity{
		AuthorName: "Alice", AuthorEmail: "a@example.com",
	})
	joined := strings.Join(env, "\n")

	for _, want := range []string{
		"AURIUM_GUARD=1",
		"AURIUM_BRANCH=implement-oauth",
		"core.hooksPath",
		"/Users/a/app/.aurium/hooks",
		"safe.directory",
		"gc.auto",
		"commit.gpgsign",
		"GIT_AUTHOR_NAME=Alice",
		"GIT_COMMITTER_EMAIL=a@example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ContainerEnv missing %q\ngot:\n%s", want, joined)
		}
	}

	// GIT_CONFIG_COUNT must equal the number of KEY/VALUE pairs, or git
	// silently ignores the tail of the list — including the hooks path, which
	// would disable the guard entirely.
	count := ""
	keys := 0
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_CONFIG_COUNT=") {
			count = strings.TrimPrefix(e, "GIT_CONFIG_COUNT=")
		}
		if strings.HasPrefix(e, "GIT_CONFIG_KEY_") {
			keys++
		}
	}
	if count == "" {
		t.Fatal("GIT_CONFIG_COUNT must be set")
	}
	if count != itoa(keys) {
		t.Fatalf("GIT_CONFIG_COUNT=%s but %d keys are defined", count, keys)
	}
}

// The load-bearing test for Invariant 1 (§5.5): a container may move its own
// branch and nothing else. If this regresses, agents can destroy each other's
// work, which is the exact failure Aurium exists to prevent.
//
// Note the target branch: it is deliberately NOT checked out in any worktree.
// git independently refuses to force-update a branch that is checked out
// somewhere, so testing against `main` would pass even with the hook removed.
func TestGuardHookBlocksWritingAnotherBranch(t *testing.T) {
	root := initRepo(t)
	if err := InstallHooks(root); err != nil {
		t.Fatal(err)
	}
	if err := EnsureExcluded(root); err != nil {
		t.Fatal(err)
	}
	// A branch git is perfectly willing to move: no worktree holds it.
	if err := New(root).RunOK(ctx(), "branch", "other"); err != nil {
		t.Fatal(err)
	}
	otherBefore := New(root).mustRev(t, "other")

	wt, err := AddWorktree(ctx(), root, "feature", "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	g := New(wt).WithEnv(append(os.Environ(),
		ContainerEnv(root, "feature", Identity{AuthorName: "A", AuthorEmail: "a@e"})...))

	// 1. Committing on its own branch is allowed.
	writeFile(t, filepath.Join(wt, "work.txt"), "agent work\n")
	if err := g.RunOK(ctx(), "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := g.RunOK(ctx(), "commit", "-qm", "own branch"); err != nil {
		t.Fatalf("a container must be able to commit on its own branch: %v", err)
	}

	// 2. Moving somebody else's branch is refused by the hook.
	err = g.RunOK(ctx(), "branch", "-f", "other", "HEAD")
	if err == nil {
		t.Fatal("guard hook must refuse an update to refs/heads/other from the feature worktree")
	}
	if !strings.Contains(err.Error(), "Invariant 1") {
		t.Fatalf("the refusal must come from the aurium guard hook, not incidentally from git; got: %v", err)
	}
	if after := New(root).mustRev(t, "other"); after != otherBefore {
		t.Fatalf("branch 'other' moved from %s to %s despite the guard", otherBefore, after)
	}

	// 3. Deleting somebody else's branch is refused.
	if err := g.RunOK(ctx(), "branch", "-D", "other"); err == nil {
		t.Fatal("guard hook must refuse deleting another branch")
	}
	if !New(root).RefExists(ctx(), "refs/heads/other") {
		t.Fatal("branch 'other' was deleted despite the guard")
	}

	// 4. The low-level plumbing path is guarded too — an agent that reaches
	//    for update-ref must not get further than one using porcelain.
	if err := g.RunOK(ctx(), "update-ref", "refs/heads/other", "HEAD"); err == nil {
		t.Fatal("guard hook must refuse a raw update-ref against another branch")
	}
}

func TestGuardHookIsInertOnTheHost(t *testing.T) {
	root := initRepo(t)
	if err := InstallHooks(root); err != nil {
		t.Fatal(err)
	}
	// The host does not set AURIUM_GUARD, so it retains full control — this is
	// how `aurium sync` rebases a child and `aurium snapshot` writes refs.
	g := New(root).WithEnv(append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0="+filepath.Join(root, ".aurium", "hooks"),
	))
	if err := g.RunOK(ctx(), "branch", "host-made-this"); err != nil {
		t.Fatalf("the host must not be restricted by the guard hook: %v", err)
	}
}

func TestPrePushHookBlocksPushFromContainer(t *testing.T) {
	root := initRepo(t)
	if err := InstallHooks(root); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, ".aurium", "hooks", "pre-push")
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("hooks must be executable or git silently ignores them")
	}
	body, _ := os.ReadFile(hook)
	if !strings.Contains(string(body), "AURIUM_GUARD") {
		t.Fatal("pre-push must gate on AURIUM_GUARD so host pushes still work (D10)")
	}
}

func TestRemoveWorktree(t *testing.T) {
	root := initRepo(t)
	wt, err := AddWorktree(ctx(), root, "gone", "gone", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveWorktree(ctx(), root, "gone", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree directory must be gone")
	}
	// The branch survives: destroying a container must not destroy its work
	// unless the user asked for --delete-branch.
	if !New(root).RefExists(ctx(), "refs/heads/gone") {
		t.Fatal("removing a worktree must not delete the branch")
	}
}

// ---- helpers ----

func (g *Git) mustRev(t *testing.T, rev string) string {
	t.Helper()
	sha, err := g.RevParse(ctx(), rev)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
