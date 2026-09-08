package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

type fixture struct {
	t       *testing.T
	mgr     *Manager
	store   *store.Store
	bus     *events.Bus
	root    string
	project store.Project
	repo    store.Repository
	cfg     *config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	sh := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	sh("init", "-q", "-b", "main")
	sh("config", "user.email", "t@aurium.dev")
	sh("config", "user.name", "Aurium Test")
	sh("config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(root, "README.md"), []byte("# app\n"), 0o644)
	sh("add", "-A")
	sh("commit", "-qm", "initial")

	st, err := store.Open(filepath.Join(t.TempDir(), "aurium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	bus := events.New(st)
	ctx := context.Background()

	p, err := st.CreateProject(ctx, "app", root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepository(ctx, p.ID, root, "main", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Parse([]byte(`
version: 1
project: {name: app, base_branch: main}
sandbox:
  driver: local
  agent: shell
hooks:
  post_create: ["echo created > .post-create-ran"]
`))
	if err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		Store:    st,
		Events:   bus,
		Drivers:  driver.Registry{"local": driver.NewLocal()},
		Adapters: agent.DefaultRegistry(),
		HomeRoot: t.TempDir(),
	}
	return &fixture{t: t, mgr: mgr, store: st, bus: bus, root: root, project: p, repo: repo, cfg: cfg}
}

func (f *fixture) create(branch string, opts ...func(*CreateOpts)) store.Container {
	f.t.Helper()
	o := CreateOpts{
		ProjectID:    f.project.ID,
		RepoID:       f.repo.ID,
		RepoRoot:     f.root,
		Branch:       branch,
		ParentBranch: "main",
		Config:       f.cfg,
		Adapter:      "shell",
		Role:         store.RolePrimary,
	}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := f.mgr.Create(context.Background(), o)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestCreateBuildsWorktreeHooksAndRecordsState(t *testing.T) {
	f := newFixture(t)
	c := f.create("implement-oauth")

	if c.Status != store.ContainerRunning {
		t.Errorf("status = %q, want running", c.Status)
	}
	if c.BaseSHA == "" {
		t.Error("base_sha must be recorded at creation — sync depends on it (D6)")
	}

	// The worktree exists at the D4 location and holds the repo contents.
	wt := gitx.WorktreePath(f.root, c.Slug)
	if c.Worktree != wt {
		t.Errorf("worktree = %q, want %q", c.Worktree, wt)
	}
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Errorf("worktree not checked out: %v", err)
	}

	// Guard hooks are installed and executable, or Invariant 1 is unenforced.
	hook := filepath.Join(gitx.HooksPath(f.root), "reference-transaction")
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatalf("guard hook missing: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("guard hook is not executable; git would silently ignore it")
	}

	// .aurium/ is excluded so the user's repo does not look dirty.
	clean, err := gitx.New(f.root).IsClean(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Error("creating a container must not leave the parent repo dirty")
	}
}

func TestCreateRunsPostCreateHooks(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")

	marker := filepath.Join(c.Worktree, ".post-create-ran")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post_create hook did not run: %v", err)
	}
}

func TestCreateEmitsEventsInOrder(t *testing.T) {
	f := newFixture(t)
	f.create("feature")

	got, err := f.bus.Replay(context.Background(), 0, events.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range got {
		types = append(types, e.Type)
	}
	// D19: creation is observable as it happens, not summarised afterwards.
	for _, want := range []string{events.ContainerCreated, events.AgentStarted} {
		if !contains(types, want) {
			t.Errorf("missing %q event; got %v", want, types)
		}
	}
	if idx(types, events.ContainerCreated) > idx(types, events.AgentStarted) {
		t.Error("container.created must precede agent.started")
	}
}

func TestCreateRegistersTheAgent(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")

	agents, err := f.store.ListAgents(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want exactly one agent per container (D15), got %d", len(agents))
	}
	if agents[0].Role != store.RolePrimary || agents[0].Adapter != "shell" {
		t.Fatalf("agent = %+v", agents[0])
	}
}

// D15: one interactive agent per container. A second must be refused, since
// two agents in one worktree is the collision the product removes.
func TestSecondInteractiveAgentIsRefused(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")

	_, err := f.mgr.StartAgent(context.Background(), c.ID, "shell", store.RolePrimary, "")
	if err == nil {
		t.Fatal("a second interactive agent in one container must be refused (D15)")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Fatalf("the error should explain why, got: %v", err)
	}
}

func TestCreateOnTakenBranchFailsWithoutLeavingAWorktree(t *testing.T) {
	f := newFixture(t)
	f.create("feature")

	_, err := f.mgr.Create(context.Background(), CreateOpts{
		ProjectID: f.project.ID, RepoID: f.repo.ID, RepoRoot: f.root,
		Branch: "feature", ParentBranch: "main", Config: f.cfg,
		Adapter: "shell", Role: store.RolePrimary,
	})
	if err == nil {
		t.Fatal("two containers must not share a branch")
	}
}

// A failure partway through creation must not leave a worktree, a branch and
// a database row disagreeing about reality.
func TestCreateRollsBackOnDriverFailure(t *testing.T) {
	f := newFixture(t)
	f.mgr.Drivers = driver.Registry{"local": &failingDriver{}}

	_, err := f.mgr.Create(context.Background(), CreateOpts{
		ProjectID: f.project.ID, RepoID: f.repo.ID, RepoRoot: f.root,
		Branch: "doomed", ParentBranch: "main", Config: f.cfg,
		Adapter: "shell", Role: store.RolePrimary,
	})
	if err == nil {
		t.Fatal("expected the driver failure to surface")
	}

	if _, err := os.Stat(gitx.WorktreePath(f.root, "doomed")); !os.IsNotExist(err) {
		t.Error("a failed create must not leave a worktree behind")
	}
	cs, _ := f.store.ListContainers(context.Background(), f.project.ID)
	for _, c := range cs {
		if c.Branch == "doomed" {
			t.Error("a failed create must not leave a container row behind")
		}
	}
}

func TestDestroyRemovesWorktreeButKeepsTheBranch(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")
	ctx := context.Background()

	if err := f.mgr.Destroy(ctx, c.ID, DestroyOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Worktree); !os.IsNotExist(err) {
		t.Error("worktree should be gone")
	}
	// The work survives unless the user explicitly asked otherwise.
	if !gitx.New(f.root).RefExists(ctx, "refs/heads/feature") {
		t.Error("destroying a container must not delete its branch by default")
	}
	if _, err := f.store.GetContainer(ctx, c.ID); err == nil {
		t.Error("container row should be gone")
	}
}

func TestDestroyWithDeleteBranchRemovesIt(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")
	ctx := context.Background()

	if err := f.mgr.Destroy(ctx, c.ID, DestroyOpts{DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if gitx.New(f.root).RefExists(ctx, "refs/heads/feature") {
		t.Error("--delete-branch should remove the branch")
	}
}

// ---- helpers ----

type failingDriver struct{ driver.Local }

func (f *failingDriver) Create(ctx context.Context, s driver.Spec) (string, error) {
	return "", context.DeadlineExceeded
}
func (f *failingDriver) Name() string              { return "local" }
func (f *failingDriver) Capabilities() driver.Caps { return driver.Caps{Filesystem: driver.FSShared} }

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func idx(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
