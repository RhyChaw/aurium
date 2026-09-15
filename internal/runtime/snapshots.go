package runtime

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/runtime/snapshot"
	"github.com/RhyChaw/aurium/internal/store"
)

// Home is where snapshot archives live; set by the app wiring.
func (m *Manager) SnapshotDeps() snapshot.Deps {
	return snapshot.Deps{
		Store:   m.Store,
		Events:  m.Events,
		Drivers: m.Drivers,
		Home:    m.SnapshotHome,
	}
}

// volumesFor returns the per-container volume mounts a container declares.
func volumesFor(cfg *config.Config, c store.Container) []driver.VolumeMount {
	var out []driver.VolumeMount
	for _, decl := range cfg.Sandbox.Volumes.PerSandbox {
		out = append(out, driver.VolumeMount{
			Name: driver.VolumeName(cfg.Project.Name, c.Slug, decl),
			Path: filepath.Join(c.Worktree, decl),
		})
	}
	return out
}

// Snapshot captures a container's state.
func (m *Manager) Snapshot(ctx context.Context, containerID string, cfg *config.Config,
	label, trigger string) (store.Snapshot, error) {

	c, err := m.Store.GetContainer(ctx, containerID)
	if err != nil {
		return store.Snapshot{}, err
	}
	return snapshot.Take(ctx, m.SnapshotDeps(), containerID, snapshot.TakeOpts{
		Label:          label,
		Trigger:        trigger,
		IncludeIgnored: cfg.Snapshot.IncludeIgnored,
		Volumes:        volumesFor(cfg, c),
	})
}

// Restore returns a container to a snapshot and restarts its agent.
func (m *Manager) Restore(ctx context.Context, containerID string, seq int,
	cfg *config.Config, backup bool) error {

	c, err := m.Store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	if err := snapshot.Restore(ctx, m.SnapshotDeps(), containerID, seq, snapshot.RestoreOpts{
		Backup:  backup,
		Volumes: volumesFor(cfg, c),
	}); err != nil {
		return err
	}
	// The container was destroyed by the restore; bring it back with the
	// snapshot's rootfs as its base layer and resume the agent.
	return m.recreate(ctx, containerID, cfg, true)
}

// ForkOpts controls a fork or a stack.
type ForkOpts struct {
	// At is a snapshot sequence; 0 means "snapshot now first" (§6.4).
	At int
	// Branch is the new container's branch.
	Branch string
	// Stack makes the new container a child of the source rather than a peer.
	Stack   bool
	Adapter string
	Role    string
	TaskID  string
	NoAgent bool
}

// Fork creates an independent copy of a container; with Stack set, a child
// that tracks it (§6.4).
func (m *Manager) Fork(ctx context.Context, sourceID string, cfg *config.Config,
	o ForkOpts) (store.Container, error) {

	source, err := m.Store.GetContainer(ctx, sourceID)
	if err != nil {
		return store.Container{}, err
	}
	repo, err := m.Store.GetRepository(ctx, source.RepoID)
	if err != nil {
		return store.Container{}, err
	}

	snap, err := snapshot.EnsureSnapshot(ctx, m.SnapshotDeps(), sourceID, o.At, snapshot.TakeOpts{
		Trigger:        snapshot.TriggerAuto,
		IncludeIgnored: cfg.Snapshot.IncludeIgnored,
		Volumes:        volumesFor(cfg, source),
	})
	if err != nil {
		return store.Container{}, err
	}

	lineage := snapshot.ForkLineage(source, snap)
	if o.Stack {
		lineage = snapshot.StackLineage(source, snap)
	}

	if o.Adapter == "" {
		o.Adapter = cfg.Sandbox.Agent
	}
	if o.Role == "" {
		o.Role = store.RolePrimary
	}

	created, err := m.Create(ctx, CreateOpts{
		ProjectID:         source.ProjectID,
		RepoID:            source.RepoID,
		RepoRoot:          repo.Path,
		TaskID:            o.TaskID,
		Branch:            o.Branch,
		ParentBranch:      lineage.ParentBranch,
		ParentContainerID: lineage.ParentContainerID,
		Config:            cfg,
		Adapter:           o.Adapter,
		Role:              o.Role,
		NoAgent:           o.NoAgent,
		OriginKind:        lineage.Kind,
		OriginSnapshotID:  snap.ID,
		StartRef:          lineage.StartRef,
	})
	if err != nil {
		return store.Container{}, err
	}

	// Create derives base_sha from the start ref; fork and stack override it
	// with the lineage's answer, which is the whole difference between them.
	if err := m.Store.UpdateContainerBaseSHA(ctx, created.ID, lineage.BaseSHA); err != nil {
		return store.Container{}, err
	}
	created.BaseSHA = lineage.BaseSHA

	// Volumes are copied, never shared: sharing would let the new container
	// corrupt the source's node_modules, which git cannot guard against.
	if err := m.cloneVolumes(ctx, cfg, source, created); err != nil {
		return store.Container{}, err
	}

	if o.Stack {
		err = snapshot.EmitStacked(ctx, m.SnapshotDeps(), source, created, snap)
	} else {
		err = snapshot.EmitForked(ctx, m.SnapshotDeps(), source, created, snap)
	}
	return created, err
}

func (m *Manager) cloneVolumes(ctx context.Context, cfg *config.Config, from, to store.Container) error {
	decls := cfg.Sandbox.Volumes.PerSandbox
	if len(decls) == 0 {
		return nil
	}
	drv, err := m.Drivers.Get(to.Driver)
	if err != nil {
		return err
	}
	src, dst := volumesFor(cfg, from), volumesFor(cfg, to)
	if err := drv.CloneVolumes(ctx, src, dst); err != nil {
		if isUnsupported(err) {
			return nil
		}
		return fmt.Errorf("runtime: clone volumes for %s: %w", to.ID, err)
	}
	return nil
}

// recreate rebuilds a container's runtime object in place, used after a
// restore or an environment sync destroyed it.
func (m *Manager) recreate(ctx context.Context, containerID string, cfg *config.Config, resume bool) error {
	c, err := m.Store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	repo, err := m.Store.GetRepository(ctx, c.RepoID)
	if err != nil {
		return err
	}
	drv, err := m.Drivers.Get(c.Driver)
	if err != nil {
		return err
	}

	agents, err := m.Store.ListAgents(ctx, c.ID)
	if err != nil {
		return err
	}
	adapterName := cfg.Sandbox.Agent
	role := store.RolePrimary
	if len(agents) > 0 {
		adapterName, role = agents[0].Adapter, agents[0].Role
		// The old agent rows describe a process that no longer exists.
		for _, a := range agents {
			if err := m.Store.DeleteAgent(ctx, a.ID); err != nil {
				return err
			}
		}
	}
	adapter, ok := m.Adapters.Get(adapterName)
	if !ok {
		return fmt.Errorf("runtime: unknown agent adapter %q", adapterName)
	}

	spec, home, err := m.buildSpec(ctx, CreateOpts{
		ProjectID: c.ProjectID, RepoID: c.RepoID, RepoRoot: repo.Path,
		Branch: c.Branch, Slug: c.Slug, Config: cfg, Adapter: adapterName,
	}, c, adapter, drv)
	if err != nil {
		return err
	}
	// L2 becomes the snapshot's rootfs where one was captured.
	if snap, err := m.Store.LatestSnapshot(ctx, c.ID); err == nil && snap.ImageRef != "" {
		spec.Image = snap.ImageRef
	}

	runtimeID, err := drv.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("runtime: recreate container %s: %w", containerID, err)
	}
	if err := drv.Start(ctx, runtimeID); err != nil {
		return err
	}
	ports, err := drv.Ports(ctx, runtimeID)
	if err != nil {
		return err
	}
	if err := m.Store.SetContainerRuntime(ctx, c.ID, runtimeID, spec.Image, spec.Network, ports); err != nil {
		return err
	}
	c.RuntimeID = runtimeID

	// Chosen here, before the agent row exists, for the same reason Create
	// does: a host-sandboxed turn's MCP config is written by Prepare below,
	// keyed by this id, and startAgentResuming must create the row with the
	// same one or the two silently disagree.
	agentID := ids.New(ids.Agent)
	hostSandboxed := cfg.Sandbox.AgentPlacement == config.PlacementHost

	if err := adapter.Prepare(agentProjection(m, c, home, hostSandboxed, agentID)); err != nil {
		return err
	}
	if err := m.runHooks(ctx, drv, runtimeID, c.Worktree, spec.Env, cfg.Hooks.PostCreate); err != nil {
		return fmt.Errorf("runtime: post_create hook after recreate: %w", err)
	}

	_, err = m.startAgentResuming(ctx, c, adapterName, role, resume, cfg, agentID)
	return err
}
