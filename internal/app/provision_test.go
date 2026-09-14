package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/project"
	"github.com/RhyChaw/aurium/internal/providers"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/secrets"
	"github.com/RhyChaw/aurium/internal/store"
)

// newApp opens an App against a throwaway home, so no test reads or writes the
// developer's real ~/.aurium.
func newApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("AURIUM_HOME", t.TempDir())
	a, err := Open(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// gitRepo makes a real repository, because attaching one runs git.
func gitRepo(t *testing.T, dir, branch string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-qm", "initial")
	return CanonicalPath(dir)
}

func TestCreateProjectSpansSeveralRepositories(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()

	root := t.TempDir()
	api := gitRepo(t, filepath.Join(root, "api"), "main")
	web := gitRepo(t, filepath.Join(root, "web"), "develop")

	p, repos, err := a.CreateProject(ctx, NewProject{
		Name: "My Product", Description: "api and web", Root: root,
		Repos:  []RepoSpec{{Path: "./api"}, {Path: web}},
		Driver: "local", Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("want 2 repositories, got %d", len(repos))
	}
	// The base branch comes from each repository, not from the project: an
	// API repo on main beside an infra repo on master is ordinary.
	byPath := map[string]string{}
	for _, r := range repos {
		byPath[r.Path] = r.BaseBranch
	}
	if byPath[api] != "main" || byPath[web] != "develop" {
		t.Fatalf("base branches wrong: %v", byPath)
	}

	// Every member repo gets the machinery `aurium init` would have given it.
	for _, r := range repos {
		if _, err := os.Stat(filepath.Join(r.Path, config.Filename)); err != nil {
			t.Errorf("%s has no aurium.yaml: %v", r.Path, err)
		}
	}

	d, err := project.Load(p.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if d.Project.Name != "My Product" || len(d.Repos) != 2 {
		t.Fatalf("descriptor wrong: %+v", d)
	}
	// Inside the project root, paths stay relative so the descriptor travels.
	if d.Repos[0].Path != "./api" {
		t.Fatalf("a repo under the root must be recorded relatively, got %q", d.Repos[0].Path)
	}

	// D23: a directory resolves to its project through its repository row, so
	// a repo is findable even though the project root is not a repo at all.
	gotP, gotRepo, cfg, err := a.Project(ctx, filepath.Join(api))
	if err != nil {
		t.Fatal(err)
	}
	if gotP.ID != p.ID || gotRepo.Path != api {
		t.Fatalf("resolved to %s/%s", gotP.ID, gotRepo.Path)
	}
	if cfg.Sandbox.Driver != "local" || cfg.Sandbox.Agent != "shell" {
		t.Fatalf("project defaults must seed a member's aurium.yaml: %+v", cfg.Sandbox)
	}
}

// A repository outside the project root cannot be written as a readable
// relative path, so it is recorded absolutely.
func TestRepoOutsideTheRootIsRecordedAbsolutely(t *testing.T) {
	a := newApp(t)
	outside := gitRepo(t, filepath.Join(t.TempDir(), "infra"), "main")

	p, _, err := a.CreateProject(context.Background(), NewProject{
		Name: "x", Root: t.TempDir(), Repos: []RepoSpec{{Path: outside}},
		Driver: "local", Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := project.Load(p.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(d.Repos[0].Path) {
		t.Fatalf("want an absolute path, got %q", d.Repos[0].Path)
	}
}

// Half a project on disk is worse than a clean failure, because nothing
// cleans it up.
func TestCreateProjectFailsWholeWhenARepoIsBad(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, filepath.Join(root, "good"), "main")
	if err := os.MkdirAll(filepath.Join(root, "notgit"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := a.CreateProject(ctx, NewProject{
		Name: "x", Root: root, Repos: []RepoSpec{{Path: "./good"}, {Path: "./notgit"}},
		Driver: "local", Agent: "shell",
	}); err == nil {
		t.Fatal("a non-repository member must fail the call")
	}

	if _, err := os.Stat(filepath.Join(root, project.Filename)); err == nil {
		t.Fatal("a failed create must not leave a descriptor behind")
	}
	projects, err := a.Store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("a failed create must leave no project row, got %d", len(projects))
	}
}

// "Add these three repos" where one is already a member is an ordinary ask.
func TestAttachIsIdempotentWithinAProjectAndRefusedAcross(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	repo := gitRepo(t, filepath.Join(t.TempDir(), "api"), "main")

	p, repos, err := a.CreateProject(ctx, NewProject{
		Name: "one", Root: t.TempDir(), Repos: []RepoSpec{{Path: repo}},
		Driver: "local", Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}

	again, err := a.AttachRepository(ctx, p, RepoSpec{Path: repo}, "local", "", "shell")
	if err != nil {
		t.Fatalf("re-attaching a member must be a no-op, got %v", err)
	}
	if again.ID != repos[0].ID {
		t.Fatal("re-attaching must return the existing row, not a second one")
	}

	other, _, err := a.CreateProject(ctx, NewProject{
		Name: "two", Root: t.TempDir(), Driver: "local", Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AttachRepository(ctx, other, RepoSpec{Path: repo}, "local", "", "shell"); err == nil {
		t.Fatal("a repository belongs to one project at a time")
	}
}

// A descriptor a human will edit must match what the database believes.
func TestSyncDescriptorRewritesMembership(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, filepath.Join(root, "api"), "main")
	extra := gitRepo(t, filepath.Join(root, "web"), "main")

	p, _, err := a.CreateProject(ctx, NewProject{
		Name: "x", Root: root, Repos: []RepoSpec{{Path: "./api"}},
		Driver: "local", Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AttachRepository(ctx, p, RepoSpec{Path: extra}, "local", "", "shell"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncDescriptor(ctx, p); err != nil {
		t.Fatal(err)
	}

	d, err := project.Load(p.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Repos) != 2 {
		t.Fatalf("the descriptor must carry both repos, got %+v", d.Repos)
	}
}

// With no root given the descriptor goes under ~/.aurium/projects rather than
// into one member repo, which would make that repo quietly special.
func TestDefaultRootIsOutOfTheWay(t *testing.T) {
	a := newApp(t)
	p, _, err := a.CreateProject(context.Background(), NewProject{Name: "My Thing"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(a.Home, "projects", "my-thing")
	if CanonicalPath(want) != p.Root {
		t.Fatalf("root = %q, want %q", p.Root, want)
	}
}

// An agent must be recorded against the account that pays for it, and the
// credential must reach the container's environment. Without the first,
// "which company, which agent" is unanswerable; without the second, connecting
// an account in the dashboard changes nothing about what actually runs.
func TestAgentsRunOnTheConnectedAccount(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()

	// A fake keyring: no test may reach the developer's login keychain.
	ring := &memKeyring{m: map[string]string{}}
	a.Secrets.Keyring = ring

	acct, err := a.Providers.Connect(ctx, providers.ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey,
		Label: "work", Secret: "sk-ant-SECRETVALUE",
	})
	if err != nil {
		t.Fatal(err)
	}

	repo := gitRepo(t, filepath.Join(t.TempDir(), "api"), "main")
	p, repos, err := a.CreateProject(ctx, NewProject{
		Name: "x", Root: t.TempDir(), Repos: []RepoSpec{{Path: repo}},
		Driver: "local", Agent: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(repos[0].Path, config.Filename))
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Manager.Create(ctx, runtime.CreateOpts{
		ProjectID: p.ID, RepoID: repos[0].ID, RepoRoot: repos[0].Path,
		Branch: "feature", ParentBranch: "main", Config: cfg,
		Adapter: "claude", Role: store.RolePrimary, OriginKind: store.OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}

	agents, err := a.Store.ListAgents(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want one agent, got %d", len(agents))
	}
	if agents[0].ProviderAccountID != acct.ID {
		t.Fatalf("the agent must record the account paying for it, got %q", agents[0].ProviderAccountID)
	}

	// And the credential must have reached the container. The local driver
	// keeps the spec's environment, which is where it would have been injected.
	name, value, err := a.Providers.Resolve(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ANTHROPIC_API_KEY" || value != "sk-ant-SECRETVALUE" {
		t.Fatalf("Resolve = %q=%q", name, value)
	}
}

// A `shell` agent spends nothing Aurium can attribute, so it must not be
// recorded against an account — a billing view that attributes shell sessions
// to Anthropic is worse than one that leaves them blank.
func TestShellAgentsGetNoAccount(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	a.Secrets.Keyring = &memKeyring{m: map[string]string{}}

	if _, err := a.Providers.Connect(ctx, providers.ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey, Secret: "sk-ant-x",
	}); err != nil {
		t.Fatal(err)
	}

	repo := gitRepo(t, filepath.Join(t.TempDir(), "api"), "main")
	p, repos, err := a.CreateProject(ctx, NewProject{
		Name: "x", Root: t.TempDir(), Repos: []RepoSpec{{Path: repo}},
		Driver: "local", Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(repos[0].Path, config.Filename))
	c, err := a.Manager.Create(ctx, runtime.CreateOpts{
		ProjectID: p.ID, RepoID: repos[0].ID, RepoRoot: repos[0].Path,
		Branch: "feature", ParentBranch: "main", Config: cfg,
		Adapter: "shell", Role: store.RolePrimary, OriginKind: store.OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := a.Store.ListAgents(ctx, c.ID)
	if len(agents) != 1 || agents[0].ProviderAccountID != "" {
		t.Fatalf("a shell agent must have no account: %+v", agents)
	}
}

// memKeyring stands in for the OS keyring.
type memKeyring struct{ m map[string]string }

func (k *memKeyring) Set(service, account, secret string) error {
	k.m[service+"/"+account] = secret
	return nil
}

func (k *memKeyring) Get(service, account string) (string, error) {
	v, ok := k.m[service+"/"+account]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

func (k *memKeyring) Delete(service, account string) error {
	delete(k.m, service+"/"+account)
	return nil
}
