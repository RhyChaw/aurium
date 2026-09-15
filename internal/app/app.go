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

	goruntime "runtime"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/providers"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/runtime/image"
	"github.com/RhyChaw/aurium/internal/secrets"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/RhyChaw/aurium/internal/usage"
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
	// Providers connects the model accounts agents run on (§D24).
	Providers *providers.Manager
	// Usage meters what those accounts spend (§D26).
	Usage *usage.Recorder
	Home  string
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
		Adapters: agent.DefaultRegistry(),
		Images: &image.Builder{
			Bin: "docker", Verbose: verbose,
			MCPBinary:   filepath.Join(home, "bin", "aurium-mcp"),
			SearchPaths: shimSearchPaths(home),
		},
		HomeRoot:     filepath.Join(home, "homes"),
		SnapshotHome: home,
		AuriumURL:    "http://host.docker.internal:7770",
		DockerBin:    "docker",
	}

	cx := contextengine.New(st, bus)
	msgs := ipc.New(st, bus, &tmuxNudger{mgr: mgr, store: st})
	gw := gateway.New(st, bus, cx, msgs)
	gw.Snapshots = &managerSnapshots{mgr: mgr, app: nil}

	sec := secrets.New(&secrets.FileFallback{Path: filepath.Join(home, "secrets")})
	a := &App{
		Store: st, Events: bus, Manager: mgr,
		Context: cx, IPC: msgs, Gateway: gw,
		Secrets:   sec,
		Providers: providers.New(st, sec, bus),
		Usage:     usage.New(st, bus),
		Home:      home,
	}
	// The runtime asks for credentials and reports usage through narrow
	// interfaces rather than importing these packages, so the dependency runs
	// one way: the thing that holds secrets knows about containers, not the
	// reverse.
	mgr.Credentials = &credentials{providers: a.Providers}
	// A conversation turn records the agent's reply and meters what it cost,
	// so the manager needs both. They are set after the App exists because the
	// recorder needs the bus and the bus needs the store.
	mgr.IPC = msgs
	mgr.Usage = &meter{recorder: a.Usage}
	// A turn reads the project's placement (in-container or host) fresh on
	// every call, the same way Delegator does, rather than trusting whatever
	// was true when the container was created.
	mgr.Config = a.ConfigForProject
	// The snapshot and delegation adapters need the app to resolve a
	// project's config, so they are attached once it exists.
	gw.Snapshots = &managerSnapshots{mgr: mgr, app: a}
	gw.Delegator = &runtime.Delegation{
		Manager: mgr, IPC: msgs, MaxDepth: 1,
		Config: a.ConfigForProject,
		Usage:  &meter{recorder: a.Usage},
	}
	return a, nil
}

// credentials adapts the provider manager to what the runtime needs.
type credentials struct{ providers *providers.Manager }

// AccountFor picks the default connected account for an adapter's provider.
func (c *credentials) AccountFor(ctx context.Context, adapter string) (string, bool) {
	provider := usage.ProviderFor(adapter)
	if provider == "" {
		return "", false
	}
	acct, err := c.providers.Store.DefaultProviderAccount(ctx, provider)
	if err != nil {
		return "", false
	}
	return acct.ID, true
}

func (c *credentials) Resolve(ctx context.Context, accountID string) (string, string, error) {
	return c.providers.Resolve(ctx, accountID)
}

// meter adapts the usage recorder to what the runtime needs.
type meter struct{ recorder *usage.Recorder }

func (m *meter) Meter(ctx context.Context, x runtime.Metered) error {
	_, err := m.recorder.Record(ctx, usage.Call{
		ProjectID: x.ProjectID, ContainerID: x.ContainerID, AgentID: x.AgentID,
		Adapter: x.Adapter, Model: x.Model, Kind: x.Kind, AccountID: x.AccountID,
		InputTokens: x.InputTokens, OutputTokens: x.OutputTokens,
		CostUSD: x.CostUSD, HasCost: x.HasCost,
	})
	return err
}

// shimSearchPaths lists where the linux aurium-mcp shim might be.
//
// More than one place because a daemon can be run three ways — installed on
// PATH, from a checkout's bin/, or from `go run` — and only the first of those
// has anything to do with ~/.aurium. A binary run out of a clone that has built
// its shims should just work.
func shimSearchPaths(home string) []string {
	arch := "amd64"
	if goruntime.GOARCH == "arm64" {
		arch = "arm64"
	}

	paths := []string{
		filepath.Join(home, "bin", "aurium-mcp-linux-"+arch),
		filepath.Join(home, "bin", "aurium-mcp"),
	}
	// Next to the running binary, which is bin/ in a checkout.
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths,
			filepath.Join(dir, "linux-"+arch, "aurium-mcp"),
			filepath.Join(dir, "..", "bin", "linux-"+arch, "aurium-mcp"),
		)
	}
	return paths
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
//
// D23: the repository, not the root path, is what resolves the project. A
// project rooted at its own repo (the standalone case) still resolves, because
// `aurium init` registers a repository row at that path too. Looking the
// project up by root instead would tie every project to exactly one repository
// forever, which is the limitation multi-repo projects exist to remove.
func (a *App) Project(ctx context.Context, dir string) (store.Project, store.Repository, *config.Config, error) {
	cfgPath, err := config.Find(CanonicalPath(dir))
	if err != nil {
		return store.Project{}, store.Repository{}, nil, err
	}
	root := CanonicalPath(filepath.Dir(cfgPath))

	// A worktree sits at <root>/.aurium/wt/<slug>, so if we landed inside one,
	// climb back out to the real repository root.
	if realRoot, ok := projectRootOfWorktree(root); ok {
		root = realRoot
		cfgPath = filepath.Join(root, config.Filename)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return store.Project{}, store.Repository{}, nil, err
	}

	repo, err := a.Store.RepositoryByPathAny(ctx, root)
	if err != nil {
		return store.Project{}, store.Repository{}, nil,
			fmt.Errorf("app: %s is not registered with any project; run `aurium init`: %w", root, err)
	}
	p, err := a.Store.GetProject(ctx, repo.ProjectID)
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
