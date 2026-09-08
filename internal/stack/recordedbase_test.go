package stack

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/gitx"
)

// The case that genuinely separates a recorded base from a naive
// `git rebase <parent>`.
//
// git rebase normally drops commits already upstream by comparing patch-ids,
// which hides the difference in easy cases. Patch-id matching fails the moment
// the parent AMENDS a commit's content — a conflict resolution, a review fix,
// a squash. Then:
//
//	B holds the OLD version of the parent's commit, inherited at fork time.
//	A holds an AMENDED version with different content and a different patch-id.
//
// A naive `git rebase A B` computes merge-base(B, A), finds B's stale copy of
// the parent's commit is "not upstream" (the patch-id differs), and replays it
// on top of the amended one — conflicting on the very file the parent just fixed.
//
// The recorded base states B's own work exactly: commits after the old A tip.
// The stale copy is never considered.
func TestRecordedBaseIgnoresParentCommitsThatWereAmended(t *testing.T) {
	r := newRepo(t)
	aWT := r.worktree("A", "main")

	// A does some work.
	r.commit(aWT, "shared.go", "func Auth() { return TODO }\n", "A: add auth")

	// B stacks on A, inheriting that commit and recording A's tip.
	bBase := r.branchSHA("A")
	bWT := r.worktree("B", "A")
	r.commit(bWT, "b.go", "package b\n", "B: own work")

	// A amends its commit — same file, different content, new patch-id.
	// This is an everyday event: fixing a review comment before merge.
	r.write(aWT, "shared.go", "func Auth() { return realImplementation() }\n")
	r.git(aWT, "add", "-A")
	r.git(aWT, "commit", "-q", "--amend", "-m", "A: add auth (fixed)")

	res, err := Sync(ctx(), gitx.New(bWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: bBase,
	}, Options{})
	if err != nil {
		t.Fatalf("sync returned an error: %v", err)
	}
	if res.Eligibility != Synced {
		t.Fatalf("sync should succeed cleanly using the recorded base; got %q (%s) conflicts=%v",
			res.Eligibility, res.Reason, res.ConflictedFiles)
	}

	// B must end up with the parent's AMENDED content, not its own stale copy.
	content := r.git(bWT, "show", "HEAD:shared.go")
	if content != "func Auth() { return realImplementation() }" {
		t.Fatalf("B has the wrong version of the parent's file:\n%s", content)
	}

	// B's own commit survived, and the parent's commit appears once.
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
	if count("B: own work") != 1 {
		t.Errorf("B's own commit missing or duplicated: %v", subjects)
	}
	if count("A: add auth (fixed)") != 1 {
		t.Errorf("amended parent commit should appear once: %v", subjects)
	}
	if count("A: add auth") != 0 {
		t.Errorf("the stale pre-amend commit must not be replayed: %v", subjects)
	}
	r.fsck()
}
