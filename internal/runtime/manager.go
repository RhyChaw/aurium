// Package runtime owns the lifecycle of a container: the sequence in §5.3 that
// turns a branch name into a running worktree with an agent in it.
//
// Everything here is ordered so that a failure at any step leaves nothing
// behind. A half-created container — a worktree with no row, a row with no
// container, a branch nobody owns — is worse than a clean failure, because the
// user cannot see it and Aurium will not clean it up.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/runtime/image"
	"github.com/RhyChaw/aurium/internal/store"
)

// Manager creates, starts and destroys containers.
type Manager struct {
	Store    *store.Store
	Events   *events.Bus
	Drivers  driver.Registry
	Adapters agent.Registry
	Images   *image.Builder

	// HomeRoot is where per-container $HOME directories live for drivers that
	// have no image of their own (the local driver). Real containers keep
	// $HOME inside their rootfs, which is what makes agent transcripts part of
	// a snapshot.
	HomeRoot string

	// SnapshotHome is ~/.aurium, under which snapshot archives are stored.
	SnapshotHome string

	// AuriumURL is what containers use to reach the daemon.
	AuriumURL string
	// DockerBin is echoed in attach commands shown to the user.
	DockerBin string

	// IPC records what an agent says back, which is what puts a reply in the
	// dashboard's chat pane. Optional: without it a turn still runs and is
	// still metered, it just leaves no transcript.
	IPC *ipc.Bus
	// Usage meters conversation turns (§D26). Optional.
	Usage UsageMeter

	// Credentials resolves a connected provider account into the single
	// environment variable an agent needs (§D24). Optional: with no resolver,
	// or no connected account, Aurium falls back to aurium.yaml's
	// env_passthrough, which is how it worked before accounts existed.
	Credentials Credentials

	// Config resolves a project's aurium.yaml. Optional: a Converse call with
	// none set — every caller before this field existed — resolves placement
	// as config.PlacementInContainer, which is the behaviour those callers
	// already had. When it IS set, a failed lookup fails the turn rather than
	// silently falling back to in-container: a project on host placement
	// because the container does not fit on the machine must not silently
	// get a container anyway.
	Config func(ctx context.Context, projectID string) (*config.Config, error)
}

// Credentials is the runtime's view of provider accounts. The interface exists
// so internal/runtime does not import internal/providers, which would make the
// dependency run the wrong way: the thing that holds secrets should depend on
// the thing that runs containers, not the reverse.
type Credentials interface {
	// AccountFor picks the account an agent on this adapter should use, or
	// returns false when none is connected.
	AccountFor(ctx context.Context, adapter string) (accountID string, ok bool)
	// Resolve returns the environment variable name and value for an account.
	// An empty name means the account holds no credential of its own — the
	// agent's CLI has its own login — and nothing should be injected.
	Resolve(ctx context.Context, accountID string) (name, value string, err error)
}

// CreateOpts describes a container to create.
type CreateOpts struct {
	ProjectID string
	RepoID    string
	RepoRoot  string
	TaskID    string

	Branch string
	Slug   string
	// ParentBranch is the git parent; usually the repository base branch.
	ParentBranch string
	// ParentContainerID is set when this container stacks on another.
	ParentContainerID string

	Config  *config.Config
	Adapter string
	Role    string
	Model   string

	// NoAgent creates the container without starting an agent in it.
	NoAgent bool
	// OriginKind and OriginSnapshotID record where this came from (§6.4).
	OriginKind       string
	OriginSnapshotID string
	// StartRef overrides where the new branch begins; defaults to ParentBranch.
	StartRef string
	// Token is the container's scoped bearer token, if the daemon issued one.
	Token string
	// ProviderAccountID pins this container's agent to one connected account.
	// Empty means "whichever account is the default for the adapter".
	ProviderAccountID string
}

// DestroyOpts controls teardown.
type DestroyOpts struct {
	KeepVolumes  bool
	DeleteBranch bool
	Archive      bool
}

var slugUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Slugify turns a branch name into a filesystem- and docker-safe slug.
func Slugify(branch string) string {
	s := slugUnsafe.ReplaceAllString(branch, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "container"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// Create runs the §5.3 sequence.
func (m *Manager) Create(ctx context.Context, o CreateOpts) (store.Container, error) {
	if o.Slug == "" {
		o.Slug = Slugify(o.Branch)
	}
	if o.StartRef == "" {
		o.StartRef = o.ParentBranch
	}
	if o.OriginKind == "" {
		o.OriginKind = store.OriginFresh
	}
	if o.Role == "" {
		o.Role = store.RolePrimary
	}

	drv, err := m.Drivers.Get(o.Config.Sandbox.Driver)
	if err != nil {
		return store.Container{}, err
	}
	// Before anything is created. A driver that cannot run fails several steps
	// later, on whatever that step happened to need — a user with Docker
	// stopped got told a file was missing, which was true, useless, and about
	// the wrong thing entirely.
	if err := drv.Available(ctx); err != nil {
		return store.Container{}, fmt.Errorf(
			"runtime: %w.\n\nStart it, or set `sandbox.driver: local` in %s to run "+
				"agents as host processes with no isolation",
			err, filepath.Join(o.RepoRoot, "aurium.yaml"))
	}
	adapter, ok := m.Adapters.Get(o.Adapter)
	if !ok {
		return store.Container{}, fmt.Errorf("runtime: unknown agent adapter %q", o.Adapter)
	}

	// undo accumulates cleanup for everything created so far, run in reverse
	// on any failure. This is what keeps a failed create from leaving debris.
	var undo []func()
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}

	git := gitx.New(o.RepoRoot)
	baseSHA, err := git.RevParse(ctx, o.StartRef)
	if err != nil {
		return store.Container{}, fmt.Errorf("runtime: resolve start ref %q: %w", o.StartRef, err)
	}

	// Hooks and the exclude entry are idempotent and repo-wide, so they are
	// set up before anything that can fail per-container.
	if err := gitx.InstallHooks(o.RepoRoot); err != nil {
		return store.Container{}, err
	}
	if err := gitx.EnsureExcluded(o.RepoRoot); err != nil {
		return store.Container{}, err
	}

	worktree, err := gitx.AddWorktree(ctx, o.RepoRoot, o.Slug, o.Branch, o.StartRef)
	if err != nil {
		return store.Container{}, err
	}
	undo = append(undo, func() {
		_ = gitx.RemoveWorktree(context.WithoutCancel(ctx), o.RepoRoot, o.Slug, true)
		_ = gitx.New(o.RepoRoot).DeleteRef(context.WithoutCancel(ctx), "refs/heads/"+o.Branch)
	})

	c := store.Container{
		ProjectID:         o.ProjectID,
		TaskID:            o.TaskID,
		RepoID:            o.RepoID,
		Branch:            o.Branch,
		Slug:              o.Slug,
		ParentContainerID: o.ParentContainerID,
		ParentBranch:      o.ParentBranch,
		OriginKind:        o.OriginKind,
		OriginSnapshotID:  o.OriginSnapshotID,
		BaseSHA:           baseSHA,
		HeadSHA:           baseSHA,
		Driver:            drv.Name(),
		Worktree:          worktree,
		Status:            store.ContainerCreating,
	}
	c, err = m.Store.CreateContainer(ctx, c)
	if err != nil {
		rollback()
		return store.Container{}, err
	}
	undo = append(undo, func() {
		_ = m.Store.DeleteContainer(context.WithoutCancel(ctx), c.ID)
	})

	spec, home, err := m.buildSpec(ctx, o, c, adapter, drv)
	if err != nil {
		rollback()
		return store.Container{}, err
	}

	if spec.Network != "" {
		if err := drv.EnsureNetwork(ctx, spec.Network); err != nil {
			rollback()
			return store.Container{}, err
		}
	}

	runtimeID, err := drv.Create(ctx, spec)
	if err != nil {
		rollback()
		return store.Container{}, fmt.Errorf("runtime: create container for %s: %w", o.Branch, err)
	}
	undo = append(undo, func() {
		_ = drv.Destroy(context.WithoutCancel(ctx), runtimeID, false)
	})

	if err := drv.Start(ctx, runtimeID); err != nil {
		rollback()
		return store.Container{}, fmt.Errorf("runtime: start container for %s: %w", o.Branch, err)
	}

	ports, err := drv.Ports(ctx, runtimeID)
	if err != nil {
		rollback()
		return store.Container{}, err
	}
	if err := m.Store.SetContainerRuntime(ctx, c.ID, runtimeID, spec.Image, spec.Network, ports); err != nil {
		rollback()
		return store.Container{}, err
	}
	c.RuntimeID, c.Image, c.Network, c.Ports = runtimeID, spec.Image, spec.Network, ports

	// The projection file and agent config land before any hook runs, so a
	// post_create hook can already read CONTEXT.md.
	projection := agent.Projection{
		Home:        home,
		ContextPath: filepath.Join(worktree, gitx.AuriumDir, "CONTEXT.md"),
		MCPCommand:  "aurium-mcp",
		AuriumURL:   m.AuriumURL,
		Token:       o.Token,
	}
	if err := adapter.Prepare(projection); err != nil {
		rollback()
		return store.Container{}, err
	}

	if err := m.runHooks(ctx, drv, runtimeID, worktree, spec.Env, o.Config.Hooks.PostCreate); err != nil {
		rollback()
		return store.Container{}, fmt.Errorf("runtime: post_create hook: %w", err)
	}

	if err := m.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerRunning, ""); err != nil {
		rollback()
		return store.Container{}, err
	}
	c.Status = store.ContainerRunning

	if err := m.Events.Emit(ctx, events.Event{
		Type: events.ContainerCreated, Actor: events.ActorHuman,
		ProjectID: o.ProjectID, ContainerID: c.ID, TaskID: o.TaskID,
		Payload: map[string]any{
			"branch": o.Branch, "parent": o.ParentBranch,
			"origin": o.OriginKind, "driver": drv.Name(), "ports": ports,
		},
	}); err != nil {
		rollback()
		return store.Container{}, err
	}

	if !o.NoAgent {
		if _, err := m.startAgent(ctx, c, o.Adapter, o.Role, o.Model, o.ProviderAccountID,
			agent.StartOpts{}); err != nil {
			rollback()
			return store.Container{}, err
		}
	}
	return c, nil
}

// buildSpec assembles the driver Spec: identical-path binds, declared volumes,
// ports, env and resources.
func (m *Manager) buildSpec(ctx context.Context, o CreateOpts, c store.Container,
	adapter agent.Adapter, drv driver.Driver) (driver.Spec, string, error) {

	sb := o.Config.Sandbox
	name := fmt.Sprintf("aurium-%s-%s", Slugify(o.Config.Project.Name), o.Slug)

	// $HOME lives inside the container's rootfs for real drivers; the local
	// driver has no rootfs, so it gets a directory on the host.
	home := "/home/aurium"
	if !drv.Capabilities().Snapshot {
		home = filepath.Join(m.HomeRoot, c.ID)
		if err := os.MkdirAll(home, 0o755); err != nil {
			return driver.Spec{}, "", err
		}
	}

	env := []string{
		"AURIUM_CONTAINER=" + c.ID,
		"AURIUM_URL=" + m.AuriumURL,
		"HOME=" + home,
	}
	if o.Token != "" {
		env = append(env, "AURIUM_TOKEN="+o.Token)
	}
	for k, v := range sb.Env {
		env = append(env, k+"="+v)
	}
	// The agent's own provider credential, and only names the project declared.
	for _, name := range sb.EnvPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	// A connected account wins over env_passthrough. The passthrough takes
	// whatever this terminal happened to export, which is not a choice anybody
	// made; an account is. Appending after the loop is what makes it win —
	// later entries override earlier ones in both docker and exec.
	if name, value, err := m.credentialFor(ctx, o.Adapter, o.ProviderAccountID); err != nil {
		return driver.Spec{}, "", err
	} else if name != "" {
		env = append(env, name+"="+value)
	}
	// D3/§5.4: git inside the container must use the guard hooks and the
	// host's identity.
	env = append(env, gitx.ContainerEnv(o.RepoRoot, o.Branch,
		gitx.HostIdentity(ctx, o.RepoRoot))...)

	gitDir, err := gitx.New(o.RepoRoot).Run(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return driver.Spec{}, "", err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(o.RepoRoot, gitDir)
	}

	binds := []driver.Bind{
		// Shared and writable, guarded by hooks: this is what makes worktrees
		// cheap and lets the host rebase without entering the container.
		{Host: gitDir, Container: gitDir, RW: true},
		{Host: c.Worktree, Container: c.Worktree, RW: true},
		{Host: gitx.HooksPath(o.RepoRoot), Container: gitx.HooksPath(o.RepoRoot), RW: false},
	}

	var volumes []driver.VolumeMount
	for _, decl := range sb.Volumes.PerSandbox {
		volumes = append(volumes, driver.VolumeMount{
			Name: driver.VolumeName(o.Config.Project.Name, o.Slug, decl),
			Path: filepath.Join(c.Worktree, decl),
		})
	}
	for _, sv := range sb.Volumes.Shared {
		volumes = append(volumes, driver.VolumeMount{
			Name:   driver.VolumeName(o.Config.Project.Name, "shared", sv.Name),
			Path:   sv.Path,
			Shared: true,
		})
	}

	var ports []driver.PortSpec
	for _, p := range sb.Ports {
		ports = append(ports, driver.PortSpec{Internal: p.Internal, Env: p.Env})
	}

	img := sb.Image
	if m.Images != nil && drv.Capabilities().Snapshot {
		built, err := m.Images.Ensure(ctx, image.Spec{
			BaseImage:  sb.Image,
			AgentLayer: adapter.ImageLayer(),
			UID:        os.Getuid(),
			GID:        os.Getgid(),
		})
		if err != nil {
			return driver.Spec{}, "", err
		}
		img = built
	}

	spec := driver.Spec{
		Name:    name,
		Image:   img,
		Workdir: c.Worktree,
		Env:     env,
		Binds:   binds,
		Volumes: volumes,
		Ports:   ports,
		Labels: map[string]string{
			driver.LabelProject:   o.ProjectID,
			driver.LabelContainer: c.ID,
			driver.LabelTask:      o.TaskID,
		},
		Resources: driver.Resources{
			CPUs:   sb.Resources.CPUs,
			Memory: sb.Resources.Memory,
			PIDs:   sb.Resources.PIDs,
		},
	}
	if drv.Capabilities().Snapshot {
		spec.Network = name
		spec.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	return spec, home, nil
}

// credentialFor resolves the provider credential an adapter should run on.
//
// A missing or broken account is not fatal here: the container still starts,
// the agent still runs, and it fails with its own provider's error message,
// which is more informative than anything Aurium could invent. The account is
// marked broken by Resolve, so the dashboard says so.
func (m *Manager) credentialFor(ctx context.Context, adapter, accountID string) (string, string, error) {
	if m.Credentials == nil {
		return "", "", nil
	}
	if accountID == "" {
		id, ok := m.Credentials.AccountFor(ctx, adapter)
		if !ok {
			return "", "", nil
		}
		accountID = id
	}
	name, value, err := m.Credentials.Resolve(ctx, accountID)
	if err != nil {
		return "", "", nil
	}
	return name, value, nil
}

// accountIDFor is credentialFor's first half: which account an agent is
// recorded as running on, whether or not a credential could be resolved.
func (m *Manager) accountIDFor(ctx context.Context, adapter, accountID string) string {
	if accountID != "" {
		return accountID
	}
	if m.Credentials == nil {
		return ""
	}
	if id, ok := m.Credentials.AccountFor(ctx, adapter); ok {
		return id
	}
	return ""
}

// StartAgent starts an agent in an existing container.
func (m *Manager) StartAgent(ctx context.Context, containerID, adapterName, role, model string) (store.Agent, error) {
	c, err := m.Store.GetContainer(ctx, containerID)
	if err != nil {
		return store.Agent{}, err
	}
	return m.startAgent(ctx, c, adapterName, role, model, "", agent.StartOpts{})
}

// startAgent launches one agent. accountID pins it to a connected provider
// account; empty means "the default for this adapter".
func (m *Manager) startAgent(ctx context.Context, c store.Container,
	adapterName, role, model, accountID string, o agent.StartOpts) (store.Agent, error) {

	adapter, ok := m.Adapters.Get(adapterName)
	if !ok {
		return store.Agent{}, fmt.Errorf("runtime: unknown agent adapter %q", adapterName)
	}

	// D15: one interactive agent per container. Two agents sharing a worktree
	// and a git index is exactly the collision Aurium exists to remove, so it
	// is refused rather than merely discouraged.
	if adapter.Capabilities().Interactive {
		live, err := m.Store.ListLiveAgents(ctx, c.ID)
		if err != nil {
			return store.Agent{}, err
		}
		for _, a := range live {
			if existing, ok := m.Adapters.Get(a.Adapter); ok && existing.Capabilities().Interactive {
				return store.Agent{}, fmt.Errorf(
					"runtime: container %s already runs interactive agent %s (%s); "+
						"one interactive agent per container (D15) — fork the container to run another",
					c.ID, a.ID, a.Adapter)
			}
		}
	}

	// Which account this agent is billed against (§D24). The credential
	// itself was injected when the container was created — an agent started
	// into an existing container inherits that environment — so this records
	// the attribution rather than re-delivering a secret.
	a, err := m.Store.CreateAgent(ctx, store.Agent{
		ContainerID:       c.ID,
		Adapter:           adapterName,
		Role:              role,
		Model:             model,
		TmuxSession:       "agent",
		Status:            store.AgentStarting,
		ProviderAccountID: m.accountIDFor(ctx, adapterName, accountID),
	})
	if err != nil {
		return store.Agent{}, err
	}

	drv, err := m.Drivers.Get(c.Driver)
	if err != nil {
		return store.Agent{}, err
	}
	o.Model = model

	// Only drivers that supervise sessions get an interactive agent launched.
	// On a driver without tmux there is nothing to attach to, and spawning a
	// REPL on the user's own machine would be a surprise rather than a
	// feature; the agent row still exists so headless Exec and IPC work.
	if drv.Capabilities().Tmux {
		session := &agent.Session{Driver: drv, ContainerID: c.RuntimeID, Name: a.TmuxSession}
		if err := session.Start(ctx, adapter.LaunchCommand(o), nil, c.Worktree); err != nil {
			// Record why, then surface it: a container with a dead agent is a
			// state the user needs to see, not a silent half-success.
			_ = m.Store.UpdateAgentStatus(ctx, a.ID, store.AgentError)
			return store.Agent{}, fmt.Errorf("runtime: start agent %s: %w", adapterName, err)
		}
	}

	if err := m.Store.UpdateAgentStatus(ctx, a.ID, store.AgentRunning); err != nil {
		return store.Agent{}, err
	}
	a.Status = store.AgentRunning

	err = m.Events.Emit(ctx, events.Event{
		Type: events.AgentStarted, Actor: events.ActorHuman,
		ProjectID: c.ProjectID, ContainerID: c.ID, AgentID: a.ID, TaskID: c.TaskID,
		Payload: map[string]any{"adapter": adapterName, "role": role},
	})
	return a, err
}

// runHooks executes configured shell hooks inside the container.
func (m *Manager) runHooks(ctx context.Context, drv driver.Driver,
	runtimeID, workdir string, env []string, hooks []string) error {

	for _, h := range hooks {
		res, err := drv.Exec(ctx, runtimeID, []string{"sh", "-lc", h},
			driver.ExecOpts{Workdir: workdir, Env: env})
		if err != nil {
			return fmt.Errorf("%q: %w", h, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%q exited %d: %s", h, res.ExitCode,
				strings.TrimSpace(res.Stderr+res.Stdout))
		}
	}
	return nil
}

// Destroy tears a container down. The branch survives unless asked otherwise:
// destroying a container must not destroy the work in it.
func (m *Manager) Destroy(ctx context.Context, containerID string, o DestroyOpts) error {
	c, err := m.Store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	repo, err := m.Store.GetRepository(ctx, c.RepoID)
	if err != nil {
		return err
	}

	if drv, err := m.Drivers.Get(c.Driver); err == nil && c.RuntimeID != "" {
		if err := drv.Destroy(ctx, c.RuntimeID, o.KeepVolumes); err != nil {
			return fmt.Errorf("runtime: destroy container %s: %w", containerID, err)
		}
		if c.Network != "" {
			_ = drv.RemoveNetwork(ctx, c.Network)
		}
	}

	if err := gitx.RemoveWorktree(ctx, repo.Path, c.Slug, true); err != nil {
		return err
	}
	if o.DeleteBranch {
		if err := gitx.New(repo.Path).DeleteRef(ctx, "refs/heads/"+c.Branch); err != nil {
			return err
		}
	}

	if o.Archive {
		if err := m.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerArchived, ""); err != nil {
			return err
		}
	} else if err := m.Store.DeleteContainer(ctx, c.ID); err != nil {
		return err
	}

	return m.Events.Emit(ctx, events.Event{
		Type: events.ContainerDestroyed, Actor: events.ActorHuman,
		ProjectID: c.ProjectID, ContainerID: c.ID,
		Payload: map[string]any{
			"branch": c.Branch, "deleted_branch": o.DeleteBranch, "archived": o.Archive,
		},
	})
}

// agentProjection builds the Projection an adapter's Prepare receives.
func agentProjection(m *Manager, c store.Container, home string) agent.Projection {
	return agent.Projection{
		Home:        home,
		ContextPath: filepath.Join(c.Worktree, gitx.AuriumDir, "CONTEXT.md"),
		MCPCommand:  "aurium-mcp",
		AuriumURL:   m.AuriumURL,
	}
}

// startAgentResuming starts an agent, optionally continuing its previous
// conversation. Resume is what makes restore useful: the transcript lives in
// $HOME, which the captured rootfs restored.
func (m *Manager) startAgentResuming(ctx context.Context, c store.Container,
	adapterName, role string, resume bool) (store.Agent, error) {
	return m.startAgent(ctx, c, adapterName, role, "", "", agent.StartOpts{Resume: resume})
}

// isUnsupported reports whether err is a driver capability gap rather than a
// real failure, so callers can degrade instead of aborting.
func isUnsupported(err error) bool {
	return errors.Is(err, driver.ErrUnsupported)
}

// execOptsFor builds driver exec options for a container's worktree.
func execOptsFor(c store.Container) driver.ExecOpts {
	return driver.ExecOpts{Workdir: c.Worktree}
}
