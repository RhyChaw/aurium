package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

func watcherFixture(t *testing.T) (*Watcher, *app.App, store.Container, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	os.MkdirAll(root, 0o755)
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "-q", "-b", "main")
	git(root, "config", "user.email", "t@t")
	git(root, "config", "user.name", "T")
	git(root, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(root, "README.md"), []byte("x\n"), 0o644)
	git(root, "add", "-A")
	git(root, "commit", "-qm", "initial")
	gitx.EnsureExcluded(root)

	parentWT, err := gitx.AddWorktree(context.Background(), root, "A", "A", "main")
	if err != nil {
		t.Fatal(err)
	}
	childWT, err := gitx.AddWorktree(context.Background(), root, "B", "B", "A")
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "aurium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	bus := events.New(st)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, "app", root)
	repo, _ := st.CreateRepository(ctx, p.ID, root, "main", "")

	aTip := strings.TrimSpace(runGit(t, root, "rev-parse", "A"))
	child, err := st.CreateContainer(ctx, store.Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "B", Slug: "B",
		ParentBranch: "A", BaseSHA: aTip, Driver: "local",
		Worktree: childWT, Status: store.ContainerRunning, OriginKind: store.OriginStack,
	})
	if err != nil {
		t.Fatal(err)
	}

	a := &app.App{
		Store: st, Events: bus, Home: t.TempDir(),
		Manager: &runtime.Manager{
			Store: st, Events: bus,
			Drivers:  driver.Registry{"local": driver.NewLocal()},
			Adapters: agent.DefaultRegistry(),
		},
	}
	return NewWatcher(a, nil), a, child, parentWT
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestWatcherMarksChildrenStaleWhenTheParentMoves(t *testing.T) {
	w, a, child, parentWT := watcherFixture(t)
	ctx := context.Background()

	// Nothing has moved yet.
	w.CheckParents(ctx)
	got, _ := a.Store.GetContainer(ctx, child.ID)
	if got.Status == store.ContainerStale {
		t.Fatal("a child must not be stale before its parent moves")
	}

	os.WriteFile(filepath.Join(parentWT, "a.txt"), []byte("a\n"), 0o644)
	runGit(t, parentWT, "add", "-A")
	runGit(t, parentWT, "commit", "-qm", "parent moves")

	w.CheckParents(ctx)

	got, _ = a.Store.GetContainer(ctx, child.ID)
	if got.Status != store.ContainerStale {
		t.Fatalf("child status = %q, want stale", got.Status)
	}

	evs, _ := a.Events.Replay(ctx, 0, events.Filter{
		Types: []string{events.ContainerParentChanged},
	}, 10)
	if len(evs) != 1 {
		t.Fatalf("want exactly one parent_changed event, got %d", len(evs))
	}
}

// The watcher polls every two seconds. Without deduplication a single parent
// commit would produce an endless stream of identical notifications.
func TestWatcherReportsEachParentMoveOnce(t *testing.T) {
	w, a, _, parentWT := watcherFixture(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(parentWT, "a.txt"), []byte("a\n"), 0o644)
	runGit(t, parentWT, "add", "-A")
	runGit(t, parentWT, "commit", "-qm", "parent moves")

	for i := 0; i < 5; i++ {
		w.CheckParents(ctx)
	}

	evs, _ := a.Events.Replay(ctx, 0, events.Filter{
		Types: []string{events.ContainerParentChanged},
	}, 50)
	if len(evs) != 1 {
		t.Fatalf("five polls of one parent move produced %d events, want 1", len(evs))
	}

	// A second, distinct move is a new fact and must be reported again.
	os.WriteFile(filepath.Join(parentWT, "b.txt"), []byte("b\n"), 0o644)
	runGit(t, parentWT, "add", "-A")
	runGit(t, parentWT, "commit", "-qm", "parent moves again")
	w.CheckParents(ctx)

	evs, _ = a.Events.Replay(ctx, 0, events.Filter{
		Types: []string{events.ContainerParentChanged},
	}, 50)
	if len(evs) != 2 {
		t.Fatalf("a second parent move should be reported, got %d events total", len(evs))
	}
}

func TestReconcileMarksVanishedContainersStopped(t *testing.T) {
	w, a, child, _ := watcherFixture(t)
	ctx := context.Background()

	// Claim a runtime id the driver has never heard of, as if `docker rm` had
	// been run behind the daemon's back.
	if err := a.Store.SetContainerRuntime(ctx, child.ID, "vanished", "img", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.UpdateContainerStatus(ctx, child.ID, store.ContainerRunning, ""); err != nil {
		t.Fatal(err)
	}

	w.Reconcile(ctx)

	got, _ := a.Store.GetContainer(ctx, child.ID)
	if got.Status != store.ContainerStopped {
		t.Fatalf("status = %q, want stopped — the row must not keep claiming it runs", got.Status)
	}
	if got.LastError == "" {
		t.Error("reconcile should record why the status changed")
	}
}
