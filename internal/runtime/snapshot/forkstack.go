package snapshot

import (
	"context"
	"fmt"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/store"
)

// Lineage describes the git bookkeeping a fork or a stack produces.
//
// This little struct is the entire difference between the two operations
// (§6.4), and getting it wrong is silent: both produce a working container,
// but one of them syncs incorrectly forever after.
//
//	fork  — an independent copy. Its git parent is whatever the source's
//	        parent was, and its recorded base is the source's recorded base.
//	        The source's commits become the fork's OWN commits, because the
//	        fork is a peer, not a descendant. Syncing it replays them.
//
//	stack — a child that tracks the source. Its git parent is the source's
//	        BRANCH, and its recorded base is the source's head at the chosen
//	        snapshot. The source's commits are inherited, not owned, so
//	        syncing replays only what the child adds afterwards.
type Lineage struct {
	ParentBranch      string
	ParentContainerID string
	BaseSHA           string
	StartRef          string
	Kind              string
}

// ForkLineage computes the bookkeeping for a fork of a container at a snapshot.
func ForkLineage(source store.Container, snap store.Snapshot) Lineage {
	return Lineage{
		// A peer of the source: same git parent as the source has.
		ParentBranch:      source.ParentBranch,
		ParentContainerID: source.ParentContainerID,
		// The source's own commits become the fork's own commits.
		BaseSHA:  snap.BaseSHA,
		StartRef: snap.HeadSHA,
		Kind:     store.OriginFork,
	}
}

// StackLineage computes the bookkeeping for stacking a new container on one.
func StackLineage(source store.Container, snap store.Snapshot) Lineage {
	return Lineage{
		// A child of the source: it tracks the source's branch.
		ParentBranch:      source.Branch,
		ParentContainerID: source.ID,
		// Everything up to the source's head is inherited, not owned.
		BaseSHA:  snap.HeadSHA,
		StartRef: snap.HeadSHA,
		Kind:     store.OriginStack,
	}
}

// EnsureSnapshot returns the snapshot to fork or stack from, taking one first
// when the caller did not name a sequence (§6.4: "if --at is omitted, a
// snapshot is taken first").
func EnsureSnapshot(ctx context.Context, d Deps, containerID string, at int, o TakeOpts) (store.Snapshot, error) {
	if at > 0 {
		return d.Store.GetSnapshotBySeq(ctx, containerID, at)
	}
	if o.Trigger == "" {
		o.Trigger = TriggerAuto
	}
	return Take(ctx, d, containerID, o)
}

// EmitForked records a completed fork.
func EmitForked(ctx context.Context, d Deps, source, created store.Container, snap store.Snapshot) error {
	return d.Events.Emit(ctx, events.Event{
		Type: events.ContainerForked, Actor: events.ActorHuman,
		ProjectID: created.ProjectID, ContainerID: created.ID,
		Payload: map[string]any{
			"from": source.ID, "from_branch": source.Branch,
			"snapshot": snap.ID, "seq": snap.Seq, "branch": created.Branch,
		},
	})
}

// EmitStacked records a completed stack.
func EmitStacked(ctx context.Context, d Deps, parent, created store.Container, snap store.Snapshot) error {
	return d.Events.Emit(ctx, events.Event{
		Type: events.ContainerStacked, Actor: events.ActorHuman,
		ProjectID: created.ProjectID, ContainerID: created.ID,
		Payload: map[string]any{
			"on": parent.ID, "on_branch": parent.Branch,
			"snapshot": snap.ID, "seq": snap.Seq, "branch": created.Branch,
		},
	})
}

// CloneVolumeSpecs maps a source container's per-container volumes onto names
// for a new container, so fork and stack get copies rather than sharing.
// Sharing them would break Invariant 1: the child could corrupt the parent's
// node_modules.
func CloneVolumeSpecs(project, fromSlug, toSlug string, declared []string,
	nameFn func(project, slug, declared string) string) (from, to []string) {

	for _, decl := range declared {
		from = append(from, nameFn(project, fromSlug, decl))
		to = append(to, nameFn(project, toSlug, decl))
	}
	if len(from) != len(to) {
		panic(fmt.Sprintf("snapshot: volume mapping mismatch %d/%d", len(from), len(to)))
	}
	return from, to
}
