package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RhyChaw/aurium/internal/gitx"
)

// CaptureTree records the worktree's exact state as a git commit (§6.2 step 2).
//
// It cannot use HEAD: an agent's valuable state is usually uncommitted — edits
// in progress and untracked scratch files. So it stages everything into a
// TEMPORARY index and writes a tree from that. Using a temporary index is what
// makes the operation invisible to the container: the agent's own index and
// HEAD are untouched, so a snapshot never surprises it with staged files.
//
// The resulting commit's parent is the branch head, which keeps the snapshot
// reachable from a ref rather than dangling where `git gc` would collect it.
func CaptureTree(ctx context.Context, repoRoot, worktree, containerID string, seq int, includeIgnored bool) (string, error) {
	g := gitx.New(worktree)

	head, err := g.RevParse(ctx, "HEAD")
	if err != nil {
		return "", fmt.Errorf("snapshot: resolve HEAD in %s: %w", worktree, err)
	}

	tmpIndex, err := os.CreateTemp("", "aurium-index-")
	if err != nil {
		return "", err
	}
	tmpIndex.Close()
	indexPath := tmpIndex.Name()
	defer os.Remove(indexPath)

	// Start the temporary index from HEAD so unchanged files are already
	// staged and `add` only has to walk what differs.
	staged := g.WithEnv(append(os.Environ(), "GIT_INDEX_FILE="+indexPath))
	if err := staged.RunOK(ctx, "read-tree", head); err != nil {
		return "", fmt.Errorf("snapshot: seed temporary index: %w", err)
	}

	addArgs := []string{"add", "-A"}
	if includeIgnored {
		addArgs = append(addArgs, "--force")
	}
	addArgs = append(addArgs, ".")
	if err := staged.RunOK(ctx, addArgs...); err != nil {
		return "", fmt.Errorf("snapshot: stage worktree: %w", err)
	}

	tree, err := staged.Run(ctx, "write-tree")
	if err != nil {
		return "", fmt.Errorf("snapshot: write tree: %w", err)
	}

	commit, err := g.Run(ctx, "commit-tree", tree, "-p", head,
		"-m", fmt.Sprintf("aurium snapshot %s/%d", containerID, seq))
	if err != nil {
		return "", fmt.Errorf("snapshot: commit tree: %w", err)
	}

	if err := gitx.New(repoRoot).UpdateRef(ctx, TreeRef(containerID, seq), commit); err != nil {
		return "", fmt.Errorf("snapshot: write snapshot ref: %w", err)
	}
	return commit, nil
}

// RestoreTree resets a worktree to a snapshot commit's tree (§6.3 step 3).
//
// `read-tree -u --reset` reverts tracked content and removes files the
// snapshot does not have. It does not remove files that were untracked at
// snapshot time and are untracked now, so a separate clean pass handles
// those — but only ones git can see, which is what leaves gitignored
// directories like node_modules alone. Wiping those on every restore would
// make restore too expensive to use.
func RestoreTree(ctx context.Context, repoRoot, worktree, commit string) error {
	g := gitx.New(worktree)

	// Remove untracked-but-not-ignored files first. Anything the snapshot
	// contains is written back immediately afterwards.
	if err := g.RunOK(ctx, "clean", "-fd"); err != nil {
		return fmt.Errorf("snapshot: clean worktree: %w", err)
	}

	if err := g.RunOK(ctx, "read-tree", "-u", "--reset", commit); err != nil {
		return fmt.Errorf("snapshot: restore tree: %w", err)
	}

	// The snapshot's tree includes files that were untracked when it was
	// taken. read-tree has just written them to disk and staged them; resetting
	// the index against HEAD puts them back to "untracked / modified", which is
	// how they looked to the agent originally.
	head, err := g.RevParse(ctx, "HEAD")
	if err != nil {
		return err
	}
	if err := g.RunOK(ctx, "reset", "-q", "--mixed", head); err != nil {
		return fmt.Errorf("snapshot: reset index after restore: %w", err)
	}
	return nil
}

// EnsureDir creates a snapshot's storage directory.
func EnsureDir(home, projectID, containerID string, seq int) (string, error) {
	dir := Dir(home, projectID, containerID, seq)
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
