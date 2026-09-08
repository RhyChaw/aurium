package gitx

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RhyChaw/aurium/assets"
)

// AuriumDir is the per-repository directory Aurium owns (D4). It holds
// worktrees, guard hooks and the context projection. It lives inside the
// repository so worktree paths are stable and relative, and it is added to
// .git/info/exclude so it never appears as untracked noise.
const AuriumDir = ".aurium"

// WorktreePath returns where the worktree for slug lives.
func WorktreePath(repoRoot, slug string) string {
	return filepath.Join(repoRoot, AuriumDir, "wt", slug)
}

// HooksPath returns the guard hooks directory for a repository.
func HooksPath(repoRoot string) string {
	return filepath.Join(repoRoot, AuriumDir, "hooks")
}

// AddWorktree creates a new branch at startRef and checks it out into
// <repoRoot>/.aurium/wt/<slug>.
func AddWorktree(ctx context.Context, repoRoot, slug, branch, startRef string) (string, error) {
	path := WorktreePath(repoRoot, slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("gitx: create worktree parent: %w", err)
	}

	g := New(repoRoot)
	args := []string{"worktree", "add", "-b", branch, path, startRef}
	if err := g.RunOK(ctx, args...); err != nil {
		return "", fmt.Errorf("gitx: add worktree %s: %w", slug, err)
	}
	return path, nil
}

// AddWorktreeExisting checks out an existing branch into a new worktree. Used
// by restore, which recreates a container around a branch that already exists.
func AddWorktreeExisting(ctx context.Context, repoRoot, slug, branch string) (string, error) {
	path := WorktreePath(repoRoot, slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := New(repoRoot).RunOK(ctx, "worktree", "add", path, branch); err != nil {
		return "", fmt.Errorf("gitx: add worktree %s for existing branch %s: %w", slug, branch, err)
	}
	return path, nil
}

// RemoveWorktree detaches a worktree. The branch is deliberately left alone:
// destroying a container must not destroy the work unless the user asked for
// --delete-branch.
func RemoveWorktree(ctx context.Context, repoRoot, slug string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, WorktreePath(repoRoot, slug))

	g := New(repoRoot)
	if err := g.RunOK(ctx, args...); err != nil {
		// A worktree whose directory was deleted out from under git needs a
		// prune before git will forget it.
		if pruneErr := g.RunOK(ctx, "worktree", "prune"); pruneErr != nil {
			return fmt.Errorf("gitx: remove worktree %s: %w", slug, err)
		}
		if _, statErr := os.Stat(WorktreePath(repoRoot, slug)); statErr == nil {
			return fmt.Errorf("gitx: remove worktree %s: %w", slug, err)
		}
	}
	return nil
}

// EnsureExcluded adds .aurium/ to .git/info/exclude exactly once, so Aurium's
// own directory never shows up as untracked in the user's repository. It uses
// info/exclude rather than .gitignore because it is a local concern and must
// not appear in a diff or a PR.
func EnsureExcluded(repoRoot string) error {
	gitDir, err := resolveGitDir(repoRoot)
	if err != nil {
		return err
	}
	excludePath := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}

	body, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	entry := AuriumDir + "/"
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil // already excluded
		}
	}

	updated := string(body)
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += "# Aurium worktrees, hooks and context projection\n" + entry + "\n"
	return os.WriteFile(excludePath, []byte(updated), 0o644)
}

// InstallHooks writes the guard hooks into <repoRoot>/.aurium/hooks. They are
// activated by core.hooksPath in ContainerEnv rather than by copying into
// .git/hooks, so Aurium never disturbs hooks the user already has (D9).
func InstallHooks(repoRoot string) error {
	dir := HooksPath(repoRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	entries, err := assets.Hooks.ReadDir("hooks")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := fs.ReadFile(assets.Hooks, "hooks/"+e.Name())
		if err != nil {
			return err
		}
		// 0755: git silently ignores a hook that is not executable, which
		// would disable Invariant 1 with no error anywhere.
		if err := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Identity is the git author/committer a container commits as. Aurium copies
// it from the host so commits made by an agent are attributed to the human who
// started it, not to root or to a container-local default.
type Identity struct {
	AuthorName  string
	AuthorEmail string
}

// HostIdentity reads user.name and user.email from the host git config.
func HostIdentity(ctx context.Context, repoRoot string) Identity {
	g := New(repoRoot)
	name, _ := g.Run(ctx, "config", "--get", "user.name")
	email, _ := g.Run(ctx, "config", "--get", "user.email")
	return Identity{AuthorName: name, AuthorEmail: email}
}

// ContainerEnv builds the environment a container's git runs with (§5.4).
//
// It uses GIT_CONFIG_COUNT rather than writing a config file because the
// settings must apply to the container and never leak into the host
// repository's own configuration.
func ContainerEnv(repoRoot, branch string, id Identity) []string {
	cfg := [][2]string{
		// The worktree is owned by the host uid; without this git refuses to
		// operate on it as "dubious ownership".
		{"safe.directory", "*"},
		// Background gc inside a container would repack the shared object
		// database under other containers' feet.
		{"gc.auto", "0"},
		// No signing keys exist in a container; without this every commit
		// fails once a user has commit.gpgsign=true globally.
		{"commit.gpgsign", "false"},
		// D9: activate the guard hooks by path rather than by copying them
		// into .git/hooks, so the user's own hooks are untouched.
		{"core.hooksPath", HooksPath(repoRoot)},
	}

	env := make([]string, 0, len(cfg)*2+8)
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		n := strconv.Itoa(i)
		env = append(env,
			"GIT_CONFIG_KEY_"+n+"="+kv[0],
			"GIT_CONFIG_VALUE_"+n+"="+kv[1],
		)
	}

	if id.AuthorName != "" {
		env = append(env, "GIT_AUTHOR_NAME="+id.AuthorName, "GIT_COMMITTER_NAME="+id.AuthorName)
	}
	if id.AuthorEmail != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+id.AuthorEmail, "GIT_COMMITTER_EMAIL="+id.AuthorEmail)
	}

	// Read by the guard hooks. Together these say: this process is a
	// container, and it owns exactly this branch.
	env = append(env, "AURIUM_GUARD=1", "AURIUM_BRANCH="+branch)
	return env
}

// resolveGitDir returns the repository's .git directory, following the file
// indirection git uses inside a worktree.
func resolveGitDir(repoRoot string) (string, error) {
	out, err := New(repoRoot).Run(context.Background(), "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("gitx: %s is not a git repository: %w", repoRoot, err)
	}
	if filepath.IsAbs(out) {
		return out, nil
	}
	return filepath.Join(repoRoot, out), nil
}
