package stack

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/gitx"
)

// End-to-end cascade: main -> A -> B, where syncing A rewrites A's commits and
// B must then follow.
//
// Note this passes with a naive `git rebase A` too, because git drops
// already-upstream commits by patch-id. It is kept as an integration test of
// the ordinary path. The case that actually requires the recorded base — where
// patch-id matching fails — is in recordedbase_test.go.
func TestCascadeSyncAfterParentHistoryRewriteDoesNotDuplicateCommits(t *testing.T) {
	r := newRepo(t)
	mainWT := r.worktree("mainwork", "main")

	// A stacks on main.
	aBase := r.branchSHA("main")
	aWT := r.worktree("A", "main")
	r.commit(aWT, "a1.txt", "a1", "A: first")
	r.commit(aWT, "a2.txt", "a2", "A: second")

	// B stacks on A, recording A's current tip as its base.
	bBase := r.branchSHA("A")
	bWT := r.worktree("B", "A")
	r.commit(bWT, "b1.txt", "b1", "B: first")

	// main moves, so A must be rebased. This rewrites A's two commits.
	r.commit(mainWT, "m1.txt", "m1", "main: moved")

	aRes, err := Sync(ctx(), gitx.New(aWT), Target{
		Branch: "A", ParentBranch: "mainwork", BaseSHA: aBase,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if aRes.Eligibility != Synced {
		t.Fatalf("syncing A: %q (%s)", aRes.Eligibility, aRes.Reason)
	}

	// A's commits now have different shas than the ones B inherited.
	if r.branchSHA("A") == bBase {
		t.Fatal("precondition: rebasing A should have rewritten its tip")
	}

	// Now cascade to B, using B's RECORDED base (the old A tip), not merge-base.
	bRes, err := Sync(ctx(), gitx.New(bWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: bBase,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bRes.Eligibility != Synced {
		t.Fatalf("syncing B: %q (%s)", bRes.Eligibility, bRes.Reason)
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

	// The whole point: A's commits appear ONCE, not twice.
	if c := count("A: first"); c != 1 {
		t.Errorf(`"A: first" appears %d times, want 1 — inherited commits were replayed`+"\nlog: %v", c, subjects)
	}
	if c := count("A: second"); c != 1 {
		t.Errorf(`"A: second" appears %d times, want 1`+"\nlog: %v", c, subjects)
	}
	if c := count("B: first"); c != 1 {
		t.Errorf(`"B: first" appears %d times, want 1`+"\nlog: %v", c, subjects)
	}
	if c := count("main: moved"); c != 1 {
		t.Errorf(`main's commit did not reach B`+"\nlog: %v", subjects)
	}

	// B must end up with everything.
	for _, f := range []string{"a1.txt", "a2.txt", "b1.txt", "m1.txt"} {
		if !r.exists(bWT, f) {
			t.Errorf("B is missing %s after the cascade", f)
		}
	}

	// And B's new recorded base is A's new tip, ready for the next round.
	if bRes.NewBaseSHA != r.branchSHA("A") {
		t.Errorf("B's new base %s should be A's tip %s", bRes.NewBaseSHA, r.branchSHA("A"))
	}
	r.fsck()
}

// Three deep: main -> A -> B -> C. Syncing in topological order (D7) must
// leave every branch holding exactly its own work plus its ancestors'.
func TestThreeDeepCascadeInTopologicalOrder(t *testing.T) {
	r := newRepo(t)
	mainWT := r.worktree("mainwork", "main")

	aBase := r.branchSHA("main")
	aWT := r.worktree("A", "main")
	r.commit(aWT, "a.txt", "a", "A work")

	bBase := r.branchSHA("A")
	bWT := r.worktree("B", "A")
	r.commit(bWT, "b.txt", "b", "B work")

	cBase := r.branchSHA("B")
	cWT := r.worktree("C", "B")
	r.commit(cWT, "c.txt", "c", "C work")

	r.commit(mainWT, "m.txt", "m", "main moves")

	// Parents before children, or a child rebases onto a tip that is about to
	// be rewritten and immediately drifts.
	steps := []struct {
		branch, parent, base, wt string
	}{
		{"A", "mainwork", aBase, aWT},
		{"B", "A", bBase, bWT},
		{"C", "B", cBase, cWT},
	}
	newBase := map[string]string{}
	for _, s := range steps {
		res, err := Sync(ctx(), gitx.New(s.wt), Target{
			Branch: s.branch, ParentBranch: s.parent, BaseSHA: s.base,
		}, Options{})
		if err != nil {
			t.Fatalf("syncing %s: %v", s.branch, err)
		}
		if res.Eligibility != Synced {
			t.Fatalf("syncing %s: %q (%s)", s.branch, res.Eligibility, res.Reason)
		}
		newBase[s.branch] = res.NewBaseSHA
	}

	// C has everything, exactly once each.
	subjects := r.log("C")
	for _, want := range []string{"A work", "B work", "C work", "main moves"} {
		n := 0
		for _, s := range subjects {
			if s == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q appears %d times in C, want 1\nlog: %v", want, n, subjects)
		}
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt", "m.txt"} {
		if !r.exists(cWT, f) {
			t.Errorf("C is missing %s", f)
		}
	}

	// Each recorded base points at its parent's post-sync tip.
	if newBase["B"] != r.branchSHA("A") {
		t.Error("B's recorded base is not A's tip")
	}
	if newBase["C"] != r.branchSHA("B") {
		t.Error("C's recorded base is not B's tip")
	}
	r.fsck()
}
