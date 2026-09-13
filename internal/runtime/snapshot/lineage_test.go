package snapshot

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

// The §6.4 table, as an executable statement. Fork and stack differ in exactly
// three fields, and getting them wrong is silent: both produce a working
// container, but one of them syncs incorrectly forever after.
func TestForkAndStackLineagesDifferExactlyAsSpecified(t *testing.T) {
	source := store.Container{
		ID: "c_A", Branch: "A", ParentBranch: "main", ParentContainerID: "",
		BaseSHA: "base_of_A",
	}
	snap := store.Snapshot{
		ID: "s_1", Seq: 3, HeadSHA: "head_of_A_at_3", BaseSHA: "base_of_A",
	}

	fork := ForkLineage(source, snap)
	stack := StackLineage(source, snap)

	// fork: a peer. Same git parent as the source, and the source's own
	// commits become the fork's own — so its recorded base is the source's.
	if fork.ParentBranch != "main" {
		t.Errorf("fork parent = %q, want main (the source's own parent)", fork.ParentBranch)
	}
	if fork.ParentContainerID != "" {
		t.Errorf("fork must not become a child of the source, got parent %q", fork.ParentContainerID)
	}
	if fork.BaseSHA != "base_of_A" {
		t.Errorf("fork base = %q, want the source's recorded base", fork.BaseSHA)
	}
	if fork.Kind != store.OriginFork {
		t.Errorf("fork kind = %q", fork.Kind)
	}

	// stack: a child. Tracks the source's branch, and everything up to the
	// source's head is inherited rather than owned.
	if stack.ParentBranch != "A" {
		t.Errorf("stack parent = %q, want the source's branch A", stack.ParentBranch)
	}
	if stack.ParentContainerID != "c_A" {
		t.Errorf("stack must record the source as its parent container, got %q", stack.ParentContainerID)
	}
	if stack.BaseSHA != "head_of_A_at_3" {
		t.Errorf("stack base = %q, want the source's head at the snapshot", stack.BaseSHA)
	}
	if stack.Kind != store.OriginStack {
		t.Errorf("stack kind = %q", stack.Kind)
	}

	// Both start from the same commit; only the bookkeeping differs.
	if fork.StartRef != stack.StartRef || fork.StartRef != "head_of_A_at_3" {
		t.Errorf("both must start at the snapshot head: fork %q stack %q", fork.StartRef, stack.StartRef)
	}
}

// A fork of a stacked container stays at the same depth rather than becoming
// a grandchild — it is a copy of its source, including the source's position.
func TestForkOfAStackedContainerKeepsTheSourcesParent(t *testing.T) {
	source := store.Container{
		ID: "c_B", Branch: "B", ParentBranch: "A", ParentContainerID: "c_A",
		BaseSHA: "head_of_A",
	}
	snap := store.Snapshot{Seq: 1, HeadSHA: "head_of_B", BaseSHA: "head_of_A"}

	fork := ForkLineage(source, snap)
	if fork.ParentBranch != "A" || fork.ParentContainerID != "c_A" {
		t.Fatalf("a fork of B should be a sibling of B under A, got parent %q/%q",
			fork.ParentBranch, fork.ParentContainerID)
	}
	if fork.BaseSHA != "head_of_A" {
		t.Fatalf("fork base = %q, want B's recorded base", fork.BaseSHA)
	}
}

func TestCloneVolumeSpecsPairsSourcesToDestinations(t *testing.T) {
	name := func(project, slug, declared string) string {
		return project + "-" + slug + "-" + declared
	}
	from, to := CloneVolumeSpecs("app", "A", "B", []string{"node_modules", "target"}, name)

	if len(from) != 2 || len(to) != 2 {
		t.Fatalf("got %d sources and %d destinations", len(from), len(to))
	}
	if from[0] != "app-A-node_modules" || to[0] != "app-B-node_modules" {
		t.Fatalf("mapping wrong: %v -> %v", from, to)
	}
	// Sharing would let a child corrupt its parent's node_modules, breaking
	// Invariant 1 through a path git cannot guard.
	for i := range from {
		if from[i] == to[i] {
			t.Fatalf("volume %q would be shared rather than copied", from[i])
		}
	}
}
