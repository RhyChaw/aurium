// Package stack implements Aurium's stacking model: containers whose git
// parent is another container's branch, kept up to date by rebasing onto a
// recorded base.
//
// The recorded base (D6) is the whole trick. A container remembers the exact
// parent commit it was last built on. When the parent moves, the child's own
// commits are precisely those between the recorded base and the child's tip,
// so `git rebase --onto <newParentTip> <recordedBase>` replays exactly the
// child's work and nothing else. Without it, a naive rebase would try to
// replay commits the child merely inherited from the parent, producing
// duplicates or spurious conflicts.
package stack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RhyChaw/aurium/internal/gitx"
)

// Eligibility is the outcome of examining a container before syncing it.
type Eligibility string

const (
	// Eligible means a sync would run (reported by --dry-run).
	Eligible Eligibility = "eligible"
	// Synced means a sync ran and succeeded.
	Synced Eligibility = "synced"
	// UpToDate means the parent has not moved since the recorded base.
	UpToDate Eligibility = "up_to_date"
	// Dirty means the worktree has uncommitted changes (D8).
	Dirty Eligibility = "dirty"
	// Drifted means the recorded base is no longer an ancestor of the branch,
	// so history was rewritten and a rebase would be meaningless.
	Drifted Eligibility = "drifted"
	// Conflict means the rebase stopped for a human or agent to resolve.
	Conflict Eligibility = "conflict"
	// InProgress means a rebase is already running in this worktree.
	InProgress Eligibility = "in_progress"
	// MissingParent means the parent branch no longer exists.
	MissingParent Eligibility = "missing_parent"
)

// Target identifies what to sync.
type Target struct {
	// Branch is the container's own branch.
	Branch string
	// ParentBranch is the branch it stacks on; may be the repository base.
	ParentBranch string
	// BaseSHA is the recorded base (D6).
	BaseSHA string
}

// Options controls a sync.
type Options struct {
	// DryRun reports what would happen and changes nothing.
	DryRun bool
	// Autostash carries uncommitted work across the rebase instead of
	// refusing. Off by default: it is the user's work, not Aurium's, and a
	// stash that fails to reapply is a bad surprise.
	Autostash bool
	// AbortOnConflict rewinds a conflicted rebase instead of leaving it for
	// an agent to resolve.
	AbortOnConflict bool
	// Force skips the clean-worktree precondition. Still refuses on drift.
	Force bool
}

// Result is the outcome of a sync.
type Result struct {
	Eligibility Eligibility
	// NewBaseSHA is the advanced recorded base, set only on Synced.
	NewBaseSHA string
	// ParentTip is where the parent branch points now.
	ParentTip string
	// Replayed is how many of the child's own commits were rebased.
	Replayed int
	// Behind is how many commits the parent has gained since the recorded base.
	Behind int
	// ConflictedFiles tells an agent exactly where to look.
	ConflictedFiles []string
	// Plan is the command a dry run would have executed.
	Plan string
	// Reason explains a refusal in human terms.
	Reason string
}

// CheckEligibility examines a container without changing anything.
func CheckEligibility(ctx context.Context, g *gitx.Git, t Target) (Result, error) {
	res := Result{}

	if RebaseInProgress(g.Dir) {
		res.Eligibility = InProgress
		res.Reason = "a rebase is already in progress in this worktree; resolve or abort it first"
		return res, nil
	}

	parentTip, err := g.RevParse(ctx, t.ParentBranch)
	if err != nil {
		res.Eligibility = MissingParent
		res.Reason = fmt.Sprintf("parent branch %q does not exist", t.ParentBranch)
		return res, nil
	}
	res.ParentTip = parentTip

	// Drift check before anything else: if the recorded base is not an
	// ancestor of the branch, the history this container was built on was
	// rewritten and every subsequent calculation would be garbage.
	isAncestor, err := g.IsAncestor(ctx, t.BaseSHA, t.Branch)
	if err != nil || !isAncestor {
		res.Eligibility = Drifted
		res.Reason = fmt.Sprintf(
			"recorded base %s is not an ancestor of %s; history was rewritten",
			short(t.BaseSHA), t.Branch)
		return res, nil
	}

	if parentTip == t.BaseSHA {
		res.Eligibility = UpToDate
		res.NewBaseSHA = t.BaseSHA
		return res, nil
	}

	clean, err := g.IsClean(ctx)
	if err != nil {
		return res, err
	}
	if !clean {
		res.Eligibility = Dirty
		res.Reason = "worktree has uncommitted changes; commit them, or sync with --autostash"
		return res, nil
	}

	res.Eligibility = Eligible
	// How many commits the parent has gained since this child's recorded base.
	// The watcher puts this in its notification and the dashboard shows it, so
	// it must be the real number rather than a placeholder.
	if out, err := g.Run(ctx, "rev-list", "--count", t.BaseSHA+".."+parentTip); err == nil {
		fmt.Sscanf(out, "%d", &res.Behind)
	}
	res.Plan = fmt.Sprintf("git rebase --onto %s %s %s", short(parentTip), short(t.BaseSHA), t.Branch)
	return res, nil
}

// Sync brings a child up to date with its parent (§6.5).
//
// D7: this runs on the host, in the child's worktree. Running it inside the
// container would need git credentials there (D10 forbids) and would make a
// conflicted rebase invisible to the daemon.
func Sync(ctx context.Context, g *gitx.Git, t Target, o Options) (Result, error) {
	res, err := CheckEligibility(ctx, g, t)
	if err != nil {
		return res, err
	}

	switch res.Eligibility {
	case UpToDate, Drifted, MissingParent, InProgress:
		return res, nil
	case Dirty:
		// Autostash and Force both proceed; the eligibility check has already
		// established there is nothing worse wrong.
		if !o.Autostash && !o.Force {
			return res, nil
		}
		res.Plan = fmt.Sprintf("git rebase --onto %s %s %s", short(res.ParentTip), short(t.BaseSHA), t.Branch)
	}

	if o.DryRun {
		res.Eligibility = Eligible
		return res, nil
	}

	// Count what we are about to replay, for the event payload and the CLI.
	if out, err := g.Run(ctx, "rev-list", "--count", t.BaseSHA+".."+t.Branch); err == nil {
		fmt.Sscanf(out, "%d", &res.Replayed)
	}

	args := []string{"rebase"}
	if o.Autostash {
		args = append(args, "--autostash")
	}
	args = append(args, "--onto", res.ParentTip, t.BaseSHA, t.Branch)

	if err := g.RunOK(ctx, args...); err != nil {
		if !gitx.IsConflict(err) {
			return res, fmt.Errorf("stack: rebase %s onto %s: %w", t.Branch, t.ParentBranch, err)
		}
		res.Eligibility = Conflict
		res.ConflictedFiles = conflictedFiles(ctx, g)
		res.Reason = fmt.Sprintf("rebase of %s onto %s conflicts in %s",
			t.Branch, t.ParentBranch, strings.Join(res.ConflictedFiles, ", "))

		if o.AbortOnConflict {
			// Best effort: if the abort itself fails the worktree is already
			// broken and the caller needs the conflict result either way.
			_ = g.RunOK(ctx, "rebase", "--abort")
		}
		return res, nil
	}

	// Success: the child now sits on the parent's tip, so that becomes the new
	// recorded base. This single assignment is what makes the next sync
	// compute the right range.
	res.Eligibility = Synced
	res.NewBaseSHA = res.ParentTip
	return res, nil
}

// RebaseInProgress reports whether a rebase is half-finished in a worktree.
// git leaves one of two state directories behind depending on the backend.
func RebaseInProgress(worktree string) bool {
	gitPath := filepath.Join(worktree, ".git")
	// In a linked worktree, .git is a file pointing at the real directory.
	if info, err := os.Stat(gitPath); err == nil && !info.IsDir() {
		if body, err := os.ReadFile(gitPath); err == nil {
			if dir, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir: "); ok {
				gitPath = dir
			}
		}
	}
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitPath, name)); err == nil {
			return true
		}
	}
	return false
}

// conflictedFiles lists the paths git could not merge, so an agent is told
// exactly where to work rather than having to go looking.
func conflictedFiles(ctx context.Context, g *gitx.Git) []string {
	out, err := g.Run(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
