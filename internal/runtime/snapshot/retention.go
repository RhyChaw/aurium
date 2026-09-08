package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/store"
)

// Policy is the retention rule (§6.7).
type Policy struct {
	// KeepLast is how many recent snapshots to keep per container.
	KeepLast int
	DryRun   bool
}

// Kept records a snapshot that survived gc and why.
type Kept struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"`
	Reason string `json:"reason"`
}

// Report is the outcome of a gc run.
type Report struct {
	Deleted    []string `json:"deleted"`
	Kept       []Kept   `json:"kept"`
	BytesFreed int64    `json:"bytes_freed"`
}

// Reasons a snapshot is kept.
const (
	KeepRecent     = "within keep_last"
	KeepLabelled   = "labelled"
	KeepReferenced = "a container was forked or stacked from it"
)

// GC deletes snapshots outside the policy (§6.7).
//
// Three things are always kept: the most recent N, anything the user labelled
// (labelling is how a user says "this one matters"), and anything a container
// was forked or stacked from — deleting those would make a live container's
// lineage unexplainable and its environment unreproducible.
func GC(ctx context.Context, d Deps, projectID string, p Policy) (Report, error) {
	if p.KeepLast <= 0 {
		p.KeepLast = 10
	}
	var report Report

	containers, err := d.Store.ListContainers(ctx, projectID)
	if err != nil {
		return report, err
	}
	referenced, err := d.Store.ReferencedSnapshotIDs(ctx, projectID)
	if err != nil {
		return report, err
	}

	for _, c := range containers {
		snaps, err := d.Store.ListSnapshots(ctx, c.ID) // newest first
		if err != nil {
			return report, err
		}
		for i, s := range snaps {
			switch {
			case i < p.KeepLast:
				report.Kept = append(report.Kept, Kept{s.ID, s.Seq, KeepRecent})
			case s.Label != "":
				report.Kept = append(report.Kept, Kept{s.ID, s.Seq, KeepLabelled})
			case referenced[s.ID]:
				report.Kept = append(report.Kept, Kept{s.ID, s.Seq, KeepReferenced})
			default:
				if p.DryRun {
					report.Deleted = append(report.Deleted, s.ID)
					report.BytesFreed += s.Bytes
					continue
				}
				if err := deleteSnapshot(ctx, d, c, s); err != nil {
					return report, err
				}
				report.Deleted = append(report.Deleted, s.ID)
				report.BytesFreed += s.Bytes
			}
		}
	}
	return report, nil
}

// deleteSnapshot removes a snapshot's git ref, archives and row. The image is
// left to the driver's own pruning: an image layer may be shared with a
// container that is still running.
func deleteSnapshot(ctx context.Context, d Deps, c store.Container, s store.Snapshot) error {
	repo, err := d.Store.GetRepository(ctx, c.RepoID)
	if err != nil {
		return err
	}
	if err := gitx.New(repo.Path).DeleteRef(ctx, s.TreeRef); err != nil {
		// A missing ref is fine — the point is that it is gone.
		if gitx.New(repo.Path).RefExists(ctx, s.TreeRef) {
			return fmt.Errorf("snapshot: delete ref %s: %w", s.TreeRef, err)
		}
	}
	dir := filepath.Dir(s.ManifestPath)
	if dir != "" && dir != "." && dir != "/" {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("snapshot: remove %s: %w", dir, err)
		}
	}
	return d.Store.DeleteSnapshot(ctx, s.ID)
}
