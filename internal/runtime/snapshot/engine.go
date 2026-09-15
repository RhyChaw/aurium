package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

// Deps is what the snapshot engine needs from the rest of the system.
type Deps struct {
	Store   *store.Store
	Events  *events.Bus
	Drivers driver.Registry
	// Home is ~/.aurium, under which snapshot archives live.
	Home string
}

// TakeOpts controls a snapshot.
type TakeOpts struct {
	Label   string
	Trigger string
	// IncludeIgnored captures gitignored build output too (§5.2).
	IncludeIgnored bool
	// Volumes are the per-container volumes to archive.
	Volumes []driver.VolumeMount
	// ContextVersion is the current max context item version in scope.
	ContextVersion int
	// EnvHash digests the sandbox config so restore can warn on drift.
	EnvHash string
	// Placement is the sandbox's agent_placement at the moment of capture
	// (config.PlacementInContainer or config.PlacementHost). It decides
	// whether this snapshot's rootfs still holds the agent's conversation:
	// under host placement the transcript lives in ~/.claude on the host
	// machine, outside anything captured here. Empty is treated as
	// config.PlacementInContainer, which is every placement that existed
	// before this field did.
	Placement string
}

// Take captures a container's state (§6.2).
//
// The order matters and is the whole correctness argument: pause first so
// nothing writes to the rootfs, the volumes or the index while we work, then
// capture source, rootfs and volumes, then unpause. A snapshot taken on a
// running container could have a rootfs from one instant and volumes from
// another, which would restore to a state that never existed.
func Take(ctx context.Context, d Deps, containerID string, o TakeOpts) (store.Snapshot, error) {
	c, err := d.Store.GetContainer(ctx, containerID)
	if err != nil {
		return store.Snapshot{}, err
	}
	repo, err := d.Store.GetRepository(ctx, c.RepoID)
	if err != nil {
		return store.Snapshot{}, err
	}
	drv, err := d.Drivers.Get(c.Driver)
	if err != nil {
		return store.Snapshot{}, err
	}
	if o.Trigger == "" {
		o.Trigger = TriggerManual
	}

	seq, err := d.Store.NextSnapshotSeq(ctx, containerID)
	if err != nil {
		return store.Snapshot{}, err
	}

	// 1. Quiesce.
	paused := false
	if drv.Capabilities().Pause && c.RuntimeID != "" {
		if err := drv.Pause(ctx, c.RuntimeID); err != nil {
			return store.Snapshot{}, fmt.Errorf("snapshot: pause %s: %w", containerID, err)
		}
		paused = true
	}
	// Unpause even on a failure path: leaving a container frozen because a
	// snapshot failed would be a much worse outcome than the failed snapshot.
	defer func() {
		if paused {
			_ = drv.Unpause(context.WithoutCancel(ctx), c.RuntimeID)
		}
	}()

	dir, err := EnsureDir(d.Home, c.ProjectID, containerID, seq)
	if err != nil {
		return store.Snapshot{}, err
	}

	// 2. Source.
	if _, err := CaptureTree(ctx, repo.Path, c.Worktree, containerID, seq, o.IncludeIgnored); err != nil {
		return store.Snapshot{}, err
	}
	head, err := gitx.New(c.Worktree).RevParse(ctx, "HEAD")
	if err != nil {
		return store.Snapshot{}, err
	}

	// 3. Rootfs. Drivers without the capability record that plainly rather
	// than leaving a manifest that looks complete but is not.
	rootfs := RootfsPart{BaseImage: c.Image}
	if drv.Capabilities().Snapshot && c.RuntimeID != "" {
		ref, err := drv.Snapshot(ctx, c.RuntimeID, ImageRef(containerID, seq))
		if err != nil && !errors.Is(err, driver.ErrUnsupported) {
			return store.Snapshot{}, fmt.Errorf("snapshot: commit rootfs: %w", err)
		}
		if err == nil {
			rootfs.Image, rootfs.Digest, rootfs.Captured = ref.Image, ref.Digest, true
		}
	}

	// 4. Volumes.
	var volumes []VolumePart
	var totalBytes int64
	for _, v := range o.Volumes {
		if v.Shared {
			continue // caches are mounted, never captured
		}
		archive := filepath.Join("volumes", v.Name+".tar.zst")
		n, err := archiveVolume(ctx, drv, v, filepath.Join(dir, archive))
		if err != nil {
			if errors.Is(err, driver.ErrUnsupported) {
				continue
			}
			return store.Snapshot{}, err
		}
		volumes = append(volumes, VolumePart{Name: v.Name, Mount: v.Path, Archive: archive, Bytes: n})
		totalBytes += n
	}

	// 5. Manifest.
	agents, err := d.Store.ListAgents(ctx, containerID)
	if err != nil {
		return store.Snapshot{}, err
	}
	var agentParts []AgentPart
	for _, a := range agents {
		agentParts = append(agentParts, AgentPart{
			Adapter: a.Adapter, Role: a.Role, Session: a.TmuxSession, Model: a.Model,
			ResumeHint: resumeHint(a.Adapter), CanResume: resumeHint(a.Adapter) != "",
		})
	}

	ports := map[string]int{}
	for internal, host := range c.Ports {
		ports[strconv.Itoa(internal)] = host
	}

	snapID := ids.New(ids.Snapshot)
	m := &Manifest{
		Schema: SchemaVersion, ID: snapID, Container: containerID, Seq: seq,
		Label: o.Label, CreatedAt: ids.Now(), Trigger: o.Trigger,
		Git: GitPart{
			Branch: c.Branch, HeadSHA: head, TreeRef: TreeRef(containerID, seq),
			BaseSHA: c.BaseSHA, Parent: c.ParentBranch,
		},
		Rootfs: rootfs, Volumes: volumes,
		ContextVersion: o.ContextVersion, Agents: agentParts,
		EnvHash: o.EnvHash, Ports: ports,
	}
	if err := m.Write(dir); err != nil {
		return store.Snapshot{}, err
	}

	// The transcript lives in $HOME inside the rootfs under in-container
	// placement, so docker commit captures it and --continue resumes. Under
	// host placement it lives in ~/.claude on the machine, outside the
	// rootfs: source, rootfs and volumes are still captured exactly, and the
	// conversation is not. Recording that is the difference between a
	// restore that surprises someone and one that tells them what they are
	// getting.
	placement := o.Placement
	if placement == "" {
		placement = config.PlacementInContainer
	}
	includesConversation := placement != config.PlacementHost
	var note string
	if !includesConversation {
		note = "conversation not captured: the agent runs on the host under " +
			"agent_placement: host, so its transcript is outside the container rootfs"
	}

	rec, err := d.Store.CreateSnapshot(ctx, store.Snapshot{
		ID: snapID, ContainerID: containerID, Seq: seq, Label: o.Label, Trigger: o.Trigger,
		HeadSHA: head, TreeRef: TreeRef(containerID, seq), BaseSHA: c.BaseSHA,
		ImageRef: rootfs.Image, ManifestPath: filepath.Join(dir, "manifest.json"),
		ContextVersion: o.ContextVersion, Bytes: totalBytes,
		IncludesConversation: includesConversation, Note: note,
	})
	if err != nil {
		return store.Snapshot{}, err
	}

	err = d.Events.Emit(ctx, events.Event{
		Type: events.ContainerSnapshotTaken, Actor: events.ActorHuman,
		ProjectID: c.ProjectID, ContainerID: containerID,
		Payload: map[string]any{
			"snapshot": snapID, "seq": seq, "label": o.Label, "trigger": o.Trigger,
			"rootfs_captured": rootfs.Captured, "bytes": totalBytes,
			"includes_conversation": includesConversation,
		},
	})
	return rec, err
}

// resumeHint is the command that reattaches an agent to its previous
// conversation. The transcript lives in $HOME, which the rootfs commit
// captured, so this is what makes "show me what the agent saw" work.
func resumeHint(adapter string) string {
	switch adapter {
	case "claude":
		return "claude --continue"
	case "codex":
		return "codex resume --last"
	default:
		return ""
	}
}

// archiveVolume tars and zstd-compresses a volume through a throwaway
// container, returning the archive size.
func archiveVolume(ctx context.Context, drv driver.Driver, v driver.VolumeMount, dest string) (int64, error) {
	if !drv.Capabilities().Snapshot {
		return 0, driver.ErrUnsupported
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	if err := drv.ArchiveVolume(ctx, v.Name, dest); err != nil {
		return 0, fmt.Errorf("snapshot: archive volume %s: %w", v.Name, err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// EnvHash digests the environment-defining part of a project's config, so a
// restore into a changed environment can warn rather than silently differ.
func EnvHash(sandboxYAML []byte) string {
	sum := sha256.Sum256(sandboxYAML)
	return "sha256:" + hex.EncodeToString(sum[:])
}
