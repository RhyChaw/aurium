// Package app wires Aurium's pieces together for a single process.
//
// In Phase A the CLI runs everything in-process against the same schema the
// daemon will own from Phase B (§13). Keeping the wiring here rather than in
// cmd/ means the daemon reuses it unchanged.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/runtime/image"
	"github.com/RhyChaw/aurium/internal/secrets"
	"github.com/RhyChaw/aurium/internal/store"
)

// App holds every long-lived component.
type App struct {
	Store   *store.Store
	Events  *events.Bus
	Manager *runtime.Manager
	Context *contextengine.Engine
	IPC     *ipc.Bus
	Gateway *gateway.Gateway
	Secrets *secrets.Store
	Home    string
}

// Home returns ~/.aurium, creating it if needed.
func Home() (string, error) {
	if custom := os.Getenv("AURIUM_HOME"); custom != "" {
		return custom, os.MkdirAll(custom, 0o755)
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(h, ".aurium")
	return dir, os.MkdirAll(dir, 0o755)
}

// Open builds an App backed by ~/.aurium/aurium.db.
func Open(verbose bool) (*App, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}

	st, err := store.Open(filepath.Join(home, "aurium.db"))
	if err != nil {
		return nil, err
	}
	bus := events.New(st)

	docker := driver.NewDocker("docker")
	docker.Verbose = verbose
	podman := driver.NewDocker("podman")
	podman.Verbose = verbose

	mgr := &runtime.Manager{
		Store:  st,
		Events: bus,
		Drivers: driver.Registry{
			"docker": docker,
			"podman": podman,
			"local":  driver.NewLocal(),
		},
		Adapters:     agent.DefaultRegistry(),
		Images:       &image.Builder{Bin: "docker", Verbose: verbose, MCPBinary: filepath.Join(home, "bin", "aurium-mcp")},
		HomeRoot:     filepath.Join(home, "homes"),
		SnapshotHome: home,
		AuriumURL:    "http://host.docker.internal:7770",
		DockerBin:    "docker",
	}

	cx := contextengine.New(st, bus)
	msgs := ipc.New(st, bus, &tmuxNudger{mgr: mgr, store: st})
	gw := gateway.New(st, bus, cx, msgs)
	gw.Snapshots = &managerSnapshots{mgr: mgr, app: nil}

	a := &App{
		Store: st, Events: bus, Manager: mgr,
		Context: cx, IPC: msgs, Gateway: gw,
		Secrets: secrets.New(&secrets.FileFallback{Path: filepath.Join(home, "secrets")}),
		Home:    home,
	}
	// The snapshot and delegation adapters need the app to resolve a
	// project's config, so they are attached once it exists.
	gw.Snapshots = &managerSnapshots{mgr: mgr, app: a}
	gw.Delegator = &runtime.Delegation{
		Manager: mgr, IPC: msgs, MaxDepth: 1,
		Config: a.ConfigForProject,
	}
	return a, nil
}

// ConfigForProject loads a project's aurium.yaml.
func (a *App) ConfigForProject(ctx context.Context, projectID string) (*config.Config, error) {
	p, err := a.Store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return config.Load(filepath.Join(p.Root, config.Filename))
}

// managerSnapshots lets an agent snapshot its own container through the
// gateway, resolving the project config the manager needs.
type managerSnapshots struct {
	mgr *runtime.Manager
	app *App
}

func (m *managerSnapshots) SnapshotContainer(ctx context.Context, containerID, label string) (store.Snapshot, error) {
	c, err := m.mgr.Store.GetContainer(ctx, containerID)
	if err != nil {
		return store.Snapshot{}, err
	}
	cfg, err := m.app.ConfigForProject(ctx, c.ProjectID)
	if err != nil {
		return store.Snapshot{}, err
	}
	return m.mgr.Snapshot(ctx, containerID, cfg, label, "manual")
}

// tmuxNudger delivers IPC nudges into a container's agent session.
type tmuxNudger struct {
	mgr   *runtime.Manager
	store *store.Store
}

func (t *tmuxNudger) Nudge(ctx context.Context, containerID, text string) error {
	c, err := t.store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	drv, err := t.mgr.Drivers.Get(c.Driver)
	if err != nil {
		return err
	}
	// A driver without session supervision has nothing to nudge. That is not
	// an error: the message is still in the inbox and will be read.
	if !drv.Capabilities().Tmux || c.RuntimeID == "" {
		return nil
	}
	agents, err := t.store.ListLiveAgents(ctx, containerID)
	if err != nil || len(agents) == 0 {
		return nil
	}
	session := &agent.Session{Driver: drv, ContainerID: c.RuntimeID, Name: agents[0].TmuxSession}
	return session.Nudge(ctx, text)
}

func (a *App) Close() error { return a.Store.Close() }

// Project resolves the project containing dir, along with its config and repo
// row. It walks up from the working directory, so commands work from anywhere
// inside a project — including inside a container worktree.
func (a *App) Project(ctx context.Context, dir string) (store.Project, store.Repository, *config.Config, error) {
	cfgPath, err := config.Find(CanonicalPath(dir))
	if err != nil {
		return store.Project{}, store.Repository{}, nil, err
	}
	root := CanonicalPath(filepath.Dir(cfgPath))

	// A worktree sits at <root>/.aurium/wt/<slug>, so if we landed inside one,
	// climb back out to the real project root.
	if realRoot, ok := projectRootOfWorktree(root); ok {
		root = realRoot
		cfgPath = filepath.Join(root, config.Filename)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return store.Project{}, store.Repository{}, nil, err
	}

	p, err := a.Store.ProjectByRoot(ctx, root)
	if err != nil {
		return store.Project{}, store.Repository{}, nil,
			fmt.Errorf("app: %s is not initialised; run `aurium init`: %w", root, err)
	}
	repo, err := a.Store.RepositoryByPath(ctx, p.ID, root)
	if err != nil {
		return store.Project{}, store.Repository{}, nil, err
	}
	return p, repo, cfg, nil
}

// projectRootOfWorktree detects <root>/.aurium/wt/<slug> and returns <root>.
func projectRootOfWorktree(dir string) (string, bool) {
	wtParent := filepath.Dir(dir)       // .../.aurium/wt
	auriumDir := filepath.Dir(wtParent) // .../.aurium
	root := filepath.Dir(auriumDir)     // ...
	if filepath.Base(wtParent) == "wt" && filepath.Base(auriumDir) == gitx.AuriumDir {
		if _, err := os.Stat(filepath.Join(root, config.Filename)); err == nil {
			return root, true
		}
	}
	return "", false
}
