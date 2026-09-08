// Package gitx is Aurium's git layer.
//
// D1: Aurium shells out to the git binary rather than linking a git library.
// Aurium's core operation is a rebase with a recorded base, and the best
// implementation of that in existence is git's own. Shelling out also means
// the worktrees Aurium creates are ordinary worktrees a human can inspect,
// repair and use with any other tool.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Git runs git commands in a directory.
type Git struct {
	// Dir is the working directory: a repository root or a worktree.
	Dir string
	// Env replaces the child process environment when non-nil. Container
	// worktrees use this to carry GIT_CONFIG_COUNT and the guard-hook
	// variables (§5.4).
	Env []string
	// Verbose echoes each command to stderr, backing the CLI's -v flag.
	Verbose bool
}

// New returns a Git rooted at dir.
func New(dir string) *Git { return &Git{Dir: dir} }

// WithEnv returns a copy of g that runs commands with the given environment.
func (g *Git) WithEnv(env []string) *Git {
	c := *g
	c.Env = env
	return &c
}

// Error is a failed git invocation. It keeps stderr because that is the text
// worth showing a user, and the exit code because callers branch on it.
type Error struct {
	Args   []string
	Stderr string
	Code   int
	err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() error { return e.err }

// Run executes git with args and returns trimmed stdout.
func (g *Git) Run(ctx context.Context, args ...string) (string, error) {
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "+ git -C %s %s\n", g.Dir, strings.Join(args, " "))
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	if g.Env != nil {
		cmd.Env = g.Env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		code := 0
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		}
		return strings.TrimSpace(stdout.String()), &Error{
			Args:   args,
			Stderr: stderr.String() + stdout.String(),
			Code:   code,
			err:    err,
		}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// RunOK runs git and discards stdout.
func (g *Git) RunOK(ctx context.Context, args ...string) error {
	_, err := g.Run(ctx, args...)
	return err
}

// RevParse resolves a revision to a full 40-character sha.
func (g *Git) RevParse(ctx context.Context, rev string) (string, error) {
	return g.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// CurrentBranch returns the checked-out branch name, or an error on a detached
// HEAD (which Aurium treats as a broken container, never a normal state).
func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("gitx: no branch checked out in %s (detached HEAD?): %w", g.Dir, err)
	}
	return out, nil
}

// IsClean reports whether the worktree has no changes at all — including
// untracked files, which a rebase can silently clobber. D8 makes this a hard
// precondition of sync.
func (g *Git) IsClean(ctx context.Context) (bool, error) {
	out, err := g.Run(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// MergeBase returns the best common ancestor of two revisions.
func (g *Git) MergeBase(ctx context.Context, a, b string) (string, error) {
	return g.Run(ctx, "merge-base", a, b)
}

// AheadBehind returns how many commits a has that b does not, and vice versa.
func (g *Git) AheadBehind(ctx context.Context, a, b string) (ahead, behind int, err error) {
	out, err := g.Run(ctx, "rev-list", "--left-right", "--count", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("gitx: unexpected rev-list output %q", out)
	}
	// --left-right prints "<left> <right>" where left is commits only in a.
	if ahead, err = strconv.Atoi(fields[0]); err != nil {
		return 0, 0, err
	}
	if behind, err = strconv.Atoi(fields[1]); err != nil {
		return 0, 0, err
	}
	return ahead, behind, nil
}

// RefExists reports whether a fully-qualified ref resolves.
func (g *Git) RefExists(ctx context.Context, ref string) bool {
	_, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// IsAncestor reports whether maybeAncestor is reachable from rev. The sync
// engine uses it to tell a normal parent advance from a rewritten history:
// if the recorded base is no longer an ancestor of the branch, the container
// has drifted and a rebase would produce nonsense (D8).
func (g *Git) IsAncestor(ctx context.Context, maybeAncestor, rev string) (bool, error) {
	err := g.RunOK(ctx, "merge-base", "--is-ancestor", maybeAncestor, rev)
	if err == nil {
		return true, nil
	}
	var ge *Error
	if asGitError(err, &ge) && ge.Code == 1 {
		return false, nil // exit 1 means "no", not "broken"
	}
	return false, err
}

// UpdateRef points ref at sha, creating it if needed.
func (g *Git) UpdateRef(ctx context.Context, ref, sha string) error {
	return g.RunOK(ctx, "update-ref", ref, sha)
}

// DeleteRef removes ref if it exists.
func (g *Git) DeleteRef(ctx context.Context, ref string) error {
	return g.RunOK(ctx, "update-ref", "-d", ref)
}

// conflictMarkers are the phrases git uses when an operation stops for the
// user to resolve overlapping changes. Matching text is unpleasant, but git
// gives no distinct exit code for "conflicted" versus "failed".
var conflictMarkers = []string{
	"CONFLICT",
	"could not apply",
	"needs merge",
	"Automatic merge failed",
	"fix conflicts",
	"Resolve all conflicts",
}

// IsConflict reports whether err is git stopping on a conflict rather than
// failing outright. The sync engine maps this to the `conflict` status and
// hands the half-finished rebase to the agent (§6.5).
func IsConflict(err error) bool {
	var ge *Error
	if !asGitError(err, &ge) {
		return false
	}
	for _, marker := range conflictMarkers {
		if strings.Contains(ge.Stderr, marker) {
			return true
		}
	}
	return false
}
