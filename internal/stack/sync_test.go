package stack

import (
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/gitx"
)

// The core of Phase A. B is stacked on A; A moves; syncing B must replay B's
// own commits on top of A's new tip and advance B's recorded base — without
// touching A.
func TestSyncRebasesChildOntoMovedParentAndAdvancesBase(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	recordedBase := r.branchSHA("A") // what B was created from
	childWT := r.worktree("B", "A")

	r.commit(childWT, "b1.txt", "child work", "child work")
	r.commit(parentWT, "a1.txt", "parent moves", "parent moves")
	parentTip := r.branchSHA("A")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: recordedBase,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Synced {
		t.Fatalf("Eligibility = %q, want synced", res.Eligibility)
	}
	if res.NewBaseSHA != parentTip {
		t.Fatalf("base_sha must advance to the parent tip: got %s want %s", res.NewBaseSHA, parentTip)
	}

	// B kept its own work and gained the parent's.
	if !r.exists(childWT, "b1.txt") {
		t.Error("the child's own commit was lost in the rebase")
	}
	if !r.exists(childWT, "a1.txt") {
		t.Error("the parent's new commit did not reach the child")
	}

	// Invariant 1: syncing a child must not move the parent.
	if after := r.branchSHA("A"); after != parentTip {
		t.Fatalf("sync moved the parent branch from %s to %s", parentTip, after)
	}
	r.fsck()
}

// The recorded base is what makes this correct. A naive `rebase A` would try
// to replay every commit reachable from B that A lacks — including commits B
// inherited from A's own history — producing duplicates or conflicts.
func TestSyncReplaysOnlyTheChildsOwnCommits(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	r.commit(parentWT, "a0.txt", "a0", "parent commit before B exists")

	recordedBase := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	r.commit(childWT, "b1.txt", "b1", "child commit one")
	r.commit(childWT, "b2.txt", "b2", "child commit two")

	r.commit(parentWT, "a1.txt", "a1", "parent commit after B exists")

	if _, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: recordedBase,
	}, Options{}); err != nil {
		t.Fatal(err)
	}

	subjects := r.log("B")
	count := func(want string) int {
		n := 0
		for _, s := range subjects {
			if s == want {
				n++
			}
		}
		return n
	}
	if c := count("parent commit before B exists"); c != 1 {
		t.Errorf("inherited parent commit appears %d times, want exactly 1 — the recorded base was ignored\nlog: %v", c, subjects)
	}
	if count("child commit one") != 1 || count("child commit two") != 1 {
		t.Errorf("child commits not replayed exactly once: %v", subjects)
	}
	if count("parent commit after B exists") != 1 {
		t.Errorf("new parent commit missing: %v", subjects)
	}
	r.fsck()
}

func TestSyncOnAnUnmovedParentIsUpToDate(t *testing.T) {
	r := newRepo(t)
	r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	r.commit(childWT, "b.txt", "b", "child work")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != UpToDate {
		t.Fatalf("Eligibility = %q, want up_to_date", res.Eligibility)
	}
	if res.NewBaseSHA != base {
		t.Fatal("an up-to-date sync must not change the recorded base")
	}
}

// D8: a dirty worktree is a hard precondition. A rebase would clobber
// uncommitted work, and an agent frequently has some.
func TestSyncRefusesDirtyWorktree(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	r.commit(parentWT, "a.txt", "a", "parent moves")

	r.write(childWT, "uncommitted.txt", "work in progress")
	headBefore := r.rev(childWT, "HEAD")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Dirty {
		t.Fatalf("Eligibility = %q, want dirty", res.Eligibility)
	}
	if r.rev(childWT, "HEAD") != headBefore {
		t.Fatal("a refused sync must not touch the branch")
	}
	if !r.exists(childWT, "uncommitted.txt") {
		t.Fatal("uncommitted work was destroyed by a sync that should have refused")
	}
}

func TestSyncAutostashCarriesUncommittedWorkAcross(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	r.commit(parentWT, "a.txt", "a", "parent moves")

	r.write(childWT, "wip.txt", "work in progress")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	}, Options{Autostash: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Synced {
		t.Fatalf("Eligibility = %q, want synced with autostash", res.Eligibility)
	}
	if !r.exists(childWT, "wip.txt") {
		t.Fatal("autostash must restore the uncommitted work after rebasing")
	}
	if !r.exists(childWT, "a.txt") {
		t.Fatal("the parent's commit did not arrive")
	}
	r.fsck()
}

// A conflict is a normal outcome that gets handed to the agent (§6.5), not an
// error. The half-finished rebase is deliberately left in place so the agent
// can resolve it in its own worktree.
func TestSyncReportsConflictAndLeavesRebaseInProgress(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	r.commit(parentWT, "shared.txt", "original\n", "shared file")

	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	r.commit(childWT, "shared.txt", "child version\n", "child edits shared")
	r.commit(parentWT, "shared.txt", "parent version\n", "parent edits shared")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	}, Options{})
	if err != nil {
		t.Fatalf("a conflict is a result, not an error: %v", err)
	}
	if res.Eligibility != Conflict {
		t.Fatalf("Eligibility = %q, want conflict", res.Eligibility)
	}
	if res.NewBaseSHA != "" {
		t.Fatal("a conflicted sync must not advance the recorded base")
	}
	if !RebaseInProgress(childWT) {
		t.Fatal("the rebase must be left in progress for the agent to resolve")
	}
	if len(res.ConflictedFiles) == 0 || res.ConflictedFiles[0] != "shared.txt" {
		t.Fatalf("ConflictedFiles = %v, want [shared.txt] so the agent knows where to look", res.ConflictedFiles)
	}
	r.fsck()
}

func TestSyncAbortsConflictWhenAsked(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	r.commit(parentWT, "shared.txt", "original\n", "shared")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	childTip := r.commit(childWT, "shared.txt", "child\n", "child edit")
	r.commit(parentWT, "shared.txt", "parent\n", "parent edit")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	}, Options{AbortOnConflict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Conflict {
		t.Fatalf("Eligibility = %q, want conflict", res.Eligibility)
	}
	if RebaseInProgress(childWT) {
		t.Fatal("AbortOnConflict must leave no rebase in progress")
	}
	if r.rev(childWT, "HEAD") != childTip {
		t.Fatal("aborting must restore the branch to exactly where it was")
	}
	r.fsck()
}

// If the recorded base is no longer an ancestor of the branch, somebody
// rewrote history. Rebasing would produce nonsense, so sync refuses (D8).
func TestSyncRefusesDriftedHistory(t *testing.T) {
	r := newRepo(t)
	r.worktree("A", "main")
	childWT := r.worktree("B", "A")
	r.commit(childWT, "b.txt", "b", "child work")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A",
		BaseSHA: "0000000000000000000000000000000000000000",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Drifted {
		t.Fatalf("Eligibility = %q, want drifted", res.Eligibility)
	}
}

func TestSyncRefusesWhenParentBranchIsGone(t *testing.T) {
	r := newRepo(t)
	r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")

	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "deleted-branch", BaseSHA: base,
	}, Options{})
	if err == nil && res.Eligibility != MissingParent {
		t.Fatalf("Eligibility = %q, want missing_parent", res.Eligibility)
	}
}

func TestDryRunReportsWithoutChangingAnything(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")
	r.commit(childWT, "b.txt", "b", "child")
	r.commit(parentWT, "a.txt", "a", "parent moves")

	headBefore := r.rev(childWT, "HEAD")
	res, err := Sync(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	}, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Eligible {
		t.Fatalf("Eligibility = %q, want eligible", res.Eligibility)
	}
	if r.rev(childWT, "HEAD") != headBefore {
		t.Fatal("--dry-run must not move the branch")
	}
	if !strings.Contains(res.Plan, "rebase") {
		t.Fatalf("dry run should describe the command it would run, got %q", res.Plan)
	}
}
