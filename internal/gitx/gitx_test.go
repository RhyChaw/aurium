package gitx

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevParseAndCurrentBranch(t *testing.T) {
	root := initRepo(t)
	g := New(root)

	sha, err := g.RevParse(ctx(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 {
		t.Fatalf("RevParse returned %q, want a 40-char sha", sha)
	}

	branch, err := g.CurrentBranch(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if branch != "main" {
		t.Fatalf("CurrentBranch = %q, want main", branch)
	}
}

func TestIsCleanDetectsTrackedAndUntrackedChanges(t *testing.T) {
	root := initRepo(t)
	g := New(root)

	clean, err := g.IsClean(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Fatal("a fresh repo must be clean")
	}

	// An untracked file makes the worktree dirty: sync preconditions (D8)
	// depend on this, because an untracked file can be clobbered by a rebase.
	writeFile(t, filepath.Join(root, "scratch.txt"), "wip")
	clean, _ = g.IsClean(ctx())
	if clean {
		t.Fatal("an untracked file must make the worktree dirty")
	}

	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "scratch")
	clean, _ = g.IsClean(ctx())
	if !clean {
		t.Fatal("committing must make the worktree clean again")
	}
}

func TestAheadBehind(t *testing.T) {
	root := initRepo(t)
	g := New(root)

	run(t, root, "git", "checkout", "-qb", "feature")
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "feature work")

	run(t, root, "git", "checkout", "-q", "main")
	writeFile(t, filepath.Join(root, "b.txt"), "b")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "main work")

	ahead, behind, err := g.AheadBehind(ctx(), "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if ahead != 1 || behind != 1 {
		t.Fatalf("AheadBehind(feature, main) = %d/%d, want 1/1", ahead, behind)
	}
}

func TestMergeBase(t *testing.T) {
	root := initRepo(t)
	g := New(root)
	base, _ := g.RevParse(ctx(), "HEAD")

	run(t, root, "git", "checkout", "-qb", "feature")
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "work")

	got, err := g.MergeBase(ctx(), "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("MergeBase = %s, want %s", got, base)
	}
}

func TestRefExists(t *testing.T) {
	root := initRepo(t)
	g := New(root)
	if !g.RefExists(ctx(), "refs/heads/main") {
		t.Fatal("refs/heads/main must exist")
	}
	if g.RefExists(ctx(), "refs/heads/nope") {
		t.Fatal("a missing ref must not report as existing")
	}
}

func TestUpdateRef(t *testing.T) {
	root := initRepo(t)
	g := New(root)
	sha, _ := g.RevParse(ctx(), "HEAD")

	if err := g.UpdateRef(ctx(), "refs/aurium/snap/c_1/1", sha); err != nil {
		t.Fatal(err)
	}
	got, err := g.RevParse(ctx(), "refs/aurium/snap/c_1/1")
	if err != nil {
		t.Fatal(err)
	}
	if got != sha {
		t.Fatalf("snapshot ref = %s, want %s", got, sha)
	}
}

func TestIsAncestor(t *testing.T) {
	root := initRepo(t)
	g := New(root)
	base, _ := g.RevParse(ctx(), "HEAD")

	writeFile(t, filepath.Join(root, "a.txt"), "a")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "later")
	tip, _ := g.RevParse(ctx(), "HEAD")

	if ok, _ := g.IsAncestor(ctx(), base, tip); !ok {
		t.Fatal("base must be an ancestor of tip")
	}
	if ok, _ := g.IsAncestor(ctx(), tip, base); ok {
		t.Fatal("tip must not be an ancestor of base")
	}
}

func TestErrorCarriesStderrAndExitCode(t *testing.T) {
	root := initRepo(t)
	g := New(root)

	_, err := g.Run(ctx(), "rev-parse", "refs/heads/does-not-exist")
	if err == nil {
		t.Fatal("expected an error")
	}
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("want *gitx.Error, got %T", err)
	}
	if ge.Code == 0 {
		t.Fatal("Error must carry the exit code")
	}
	if ge.Stderr == "" {
		t.Fatal("Error must carry stderr — it is what the user sees")
	}
	if !strings.Contains(err.Error(), "rev-parse") {
		t.Fatalf("error text must name the failing command, got %q", err.Error())
	}
}

func TestIsConflictRecognisesRebaseConflicts(t *testing.T) {
	root := initRepo(t)
	g := New(root)

	// Two branches editing the same line: rebasing one onto the other conflicts.
	writeFile(t, filepath.Join(root, "shared.txt"), "original\n")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "shared")

	run(t, root, "git", "checkout", "-qb", "feature")
	writeFile(t, filepath.Join(root, "shared.txt"), "feature version\n")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "feature edit")

	run(t, root, "git", "checkout", "-q", "main")
	writeFile(t, filepath.Join(root, "shared.txt"), "main version\n")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "main edit")

	run(t, root, "git", "checkout", "-q", "feature")
	err := g.RunOK(ctx(), "rebase", "main")
	if err == nil {
		t.Fatal("expected the rebase to conflict")
	}
	if !IsConflict(err) {
		t.Fatalf("IsConflict must recognise a rebase conflict; got %v", err)
	}

	// A plain missing-ref error is not a conflict.
	_, other := g.Run(ctx(), "rev-parse", "refs/heads/nope")
	if IsConflict(other) {
		t.Fatal("a missing ref must not be classified as a conflict")
	}
}
