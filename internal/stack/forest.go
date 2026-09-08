package stack

import (
	"sort"

	"github.com/RhyChaw/aurium/internal/store"
)

// Node is one container in the stack forest.
type Node struct {
	Container store.Container
	Branch    string
	Depth     int
	Children  []*Node
	// Orphaned marks a container whose parent container row is gone. It is
	// surfaced rather than hidden: an invisible container is an unrecoverable
	// one, and the user needs to be told to reparent or destroy it.
	Orphaned bool
}

// BuildForest arranges containers into trees rooted at the base branch.
//
// Containers whose parent row is missing become roots and are flagged, so a
// destroyed parent never makes a child disappear from `aurium tree`.
func BuildForest(cs []store.Container, baseBranch string) []*Node {
	nodes := make(map[string]*Node, len(cs))
	for _, c := range cs {
		nodes[c.ID] = &Node{Container: c, Branch: c.Branch}
	}

	var roots []*Node
	for _, c := range cs {
		n := nodes[c.ID]
		parent, hasParent := nodes[c.ParentContainerID]

		switch {
		case c.ParentContainerID == "":
			// Stacked directly on the repository base branch.
			roots = append(roots, n)
		case !hasParent:
			n.Orphaned = true
			roots = append(roots, n)
		default:
			parent.Children = append(parent.Children, n)
		}
	}

	// Deterministic output: the tree is user-facing and must not reshuffle
	// between invocations.
	sortNodes(roots)
	for _, n := range nodes {
		sortNodes(n.Children)
	}

	assignDepth(roots, 0, map[*Node]bool{})
	return roots
}

func sortNodes(ns []*Node) {
	sort.Slice(ns, func(i, j int) bool { return ns[i].Branch < ns[j].Branch })
}

// assignDepth walks the forest, refusing to revisit a node so that corrupt
// data describing a cycle cannot hang the walk.
func assignDepth(ns []*Node, depth int, seen map[*Node]bool) {
	for _, n := range ns {
		if seen[n] {
			continue
		}
		seen[n] = true
		n.Depth = depth
		assignDepth(n.Children, depth+1, seen)
	}
}

// TopoOrder flattens the forest parents-first (D7).
//
// Syncing in this order matters: a child rebased before its parent lands on a
// tip that the parent's own sync is about to rewrite, so the child would drift
// immediately and need a second pass.
func TopoOrder(roots []*Node) []*Node {
	var out []*Node
	seen := map[*Node]bool{}

	var visit func(*Node)
	visit = func(n *Node) {
		if seen[n] {
			return // cycle guard
		}
		seen[n] = true
		out = append(out, n)
		for _, c := range n.Children {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return out
}

// Descendants returns every node beneath n, parents first. Used by
// `aurium destroy --cascade` and `aurium sync --cascade`.
func Descendants(n *Node) []*Node {
	all := TopoOrder([]*Node{n})
	if len(all) == 0 {
		return nil
	}
	return all[1:] // drop n itself
}
