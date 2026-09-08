package stack

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

func c(id, branch, parentBranch, parentID string) store.Container {
	return store.Container{
		ID: id, Branch: branch, ParentBranch: parentBranch,
		ParentContainerID: parentID, BaseSHA: "base-" + id,
	}
}

func TestBuildForestNestsChildrenUnderParents(t *testing.T) {
	roots := BuildForest([]store.Container{
		c("c_A", "A", "main", ""),
		c("c_B", "B", "A", "c_A"),
		c("c_C", "C", "main", ""),
		c("c_D", "D", "B", "c_B"),
	}, "main")

	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2 (A and C hang off main)", len(roots))
	}

	byBranch := map[string]*Node{}
	var walk func([]*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			byBranch[n.Branch] = n
			walk(n.Children)
		}
	}
	walk(roots)

	if len(byBranch) != 4 {
		t.Fatalf("forest holds %d nodes, want 4", len(byBranch))
	}
	if len(byBranch["A"].Children) != 1 || byBranch["A"].Children[0].Branch != "B" {
		t.Error("B must hang off A")
	}
	if len(byBranch["B"].Children) != 1 || byBranch["B"].Children[0].Branch != "D" {
		t.Error("D must hang off B")
	}
	if byBranch["D"].Depth != 2 {
		t.Errorf("D depth = %d, want 2", byBranch["D"].Depth)
	}
}

// D7: parents must sync before children, or a child rebases onto a tip that
// is about to be rewritten and immediately drifts.
func TestTopoOrderPutsParentsBeforeChildren(t *testing.T) {
	roots := BuildForest([]store.Container{
		c("c_D", "D", "B", "c_B"),
		c("c_B", "B", "A", "c_A"),
		c("c_A", "A", "main", ""),
	}, "main")

	order := TopoOrder(roots)
	pos := map[string]int{}
	for i, n := range order {
		pos[n.Branch] = i
	}
	if len(order) != 3 {
		t.Fatalf("TopoOrder returned %d nodes, want 3", len(order))
	}
	if !(pos["A"] < pos["B"] && pos["B"] < pos["D"]) {
		t.Fatalf("order must be A, B, D; got %v", order)
	}
}

func TestForestToleratesAMissingParentRow(t *testing.T) {
	// A container whose parent was destroyed must still appear, or it becomes
	// invisible in `aurium tree` and unrecoverable.
	roots := BuildForest([]store.Container{
		c("c_orphan", "orphan", "vanished", "c_gone"),
	}, "main")
	if len(roots) != 1 || roots[0].Branch != "orphan" {
		t.Fatalf("an orphaned container must surface as a root, got %+v", roots)
	}
	if !roots[0].Orphaned {
		t.Error("the node should be marked orphaned so the UI can say why")
	}
}

func TestForestDetectsCycles(t *testing.T) {
	// Corrupt data must not hang the tree walk.
	roots := BuildForest([]store.Container{
		c("c_X", "X", "Y", "c_Y"),
		c("c_Y", "Y", "X", "c_X"),
	}, "main")
	order := TopoOrder(roots)
	if len(order) > 2 {
		t.Fatalf("a cycle must not produce infinite output, got %d nodes", len(order))
	}
}
