package stack

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/gitx"
)

// The watcher publishes this number and the dashboard shows it, so a
// placeholder here would be a wrong figure in the audit log.
func TestCheckEligibilityCountsCommitsBehind(t *testing.T) {
	r := newRepo(t)
	parentWT := r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")

	r.commit(parentWT, "a1.txt", "1", "parent 1")
	r.commit(parentWT, "a2.txt", "2", "parent 2")
	r.commit(parentWT, "a3.txt", "3", "parent 3")

	res, err := CheckEligibility(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligibility != Eligible {
		t.Fatalf("Eligibility = %q", res.Eligibility)
	}
	if res.Behind != 3 {
		t.Fatalf("Behind = %d, want 3", res.Behind)
	}
}

func TestUpToDateReportsZeroBehind(t *testing.T) {
	r := newRepo(t)
	r.worktree("A", "main")
	base := r.branchSHA("A")
	childWT := r.worktree("B", "A")

	res, _ := CheckEligibility(ctx(), gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: base,
	})
	if res.Behind != 0 {
		t.Fatalf("Behind = %d, want 0 when up to date", res.Behind)
	}
}
