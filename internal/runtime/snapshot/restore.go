package snapshot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

// RestoreOpts controls a restore.
type RestoreOpts struct {
	// Backup takes a pre_restore snapshot first. On by default at the CLI:
	// restore is the one operation that deliberately destroys current state,
	// so it must always be undoable.
	Backup bool
	// Volumes are the per-container volumes to recreate.
	Volumes []driver.VolumeMount
	// Placement is the current agent_placement, forwarded to the pre-restore
	// backup snapshot so it is exactly as honest about the conversation as
	// any other snapshot (see TakeOpts.Placement).
	Placement string
}

// Restore returns a container to a previous snapshot (§6.3).
//
// It restores source, rootfs and volumes — everything a snapshot captured.
// It does not restore processes, because a snapshot never captured them
// (D14); the agent is restarted with its resume hint instead. Under
// in-container placement that resumes the real conversation, because the
// transcript lives in $HOME inside the captured rootfs; under host
// placement the transcript was never in the rootfs, so the resume hint
// starts a fresh one — which is exactly what the restored snapshot's
// IncludesConversation/Note say in advance.
func Restore(ctx context.Context, d Deps, containerID string, seq int, o RestoreOpts) error {
	c, err := d.Store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	repo, err := d.Store.GetRepository(ctx, c.RepoID)
	if err != nil {
		return err
	}
	snap, err := d.Store.GetSnapshotBySeq(ctx, containerID, seq)
	if err != nil {
		return fmt.Errorf("snapshot: container %s has no snapshot %d: %w", containerID, seq, err)
	}
	m, err := ReadManifest(filepath.Dir(snap.ManifestPath))
	if err != nil {
		return err
	}
	drv, err := d.Drivers.Get(c.Driver)
	if err != nil {
		return err
	}

	// 1. Make the current state recoverable before destroying it.
	if o.Backup {
		if _, err := Take(ctx, d, containerID, TakeOpts{
			Trigger: TriggerPreRestore, Volumes: o.Volumes, Placement: o.Placement,
		}); err != nil {
			return fmt.Errorf("snapshot: pre-restore backup failed, refusing to restore: %w", err)
		}
	}

	// 2. Tear down the container, keeping the worktree — it is a bind mount
	//    on the host, and step 3 rewrites it in place.
	if c.RuntimeID != "" {
		if err := drv.Destroy(ctx, c.RuntimeID, false); err != nil {
			return fmt.Errorf("snapshot: destroy container before restore: %w", err)
		}
	}

	// 3. Source: put the branch back, then the worktree contents.
	g := gitx.New(repo.Path)
	if err := g.UpdateRef(ctx, "refs/heads/"+c.Branch, m.Git.HeadSHA); err != nil {
		return fmt.Errorf("snapshot: reset branch %s: %w", c.Branch, err)
	}
	if err := RestoreTree(ctx, repo.Path, c.Worktree, m.Git.TreeRef); err != nil {
		return err
	}

	// 4. Volumes.
	for _, v := range m.Volumes {
		archive := filepath.Join(filepath.Dir(snap.ManifestPath), v.Archive)
		if err := drv.RestoreVolume(ctx, v.Name, archive); err != nil {
			if errors.Is(err, driver.ErrUnsupported) {
				continue
			}
			return err
		}
	}

	// 5. The recorded base comes back too. Restoring the source without it
	//    would leave the container rebasing from the wrong point.
	if err := d.Store.UpdateContainerBaseSHA(ctx, containerID, m.Git.BaseSHA); err != nil {
		return err
	}
	if err := d.Store.UpdateContainerHeadSHA(ctx, containerID, m.Git.HeadSHA); err != nil {
		return err
	}
	if err := d.Store.SetContainerOrigin(ctx, containerID, snap.ID, store.OriginRestore); err != nil {
		return err
	}

	return d.Events.Emit(ctx, events.Event{
		Type: events.ContainerRestored, Actor: events.ActorHuman,
		ProjectID: c.ProjectID, ContainerID: containerID,
		Payload: map[string]any{
			"snapshot": snap.ID, "seq": seq,
			"head": m.Git.HeadSHA, "base": m.Git.BaseSHA,
			"rootfs_restored": m.Rootfs.Captured,
		},
	})
}
