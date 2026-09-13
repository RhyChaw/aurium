package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

func gcFixture(t *testing.T) (Deps, store.Container) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	os.MkdirAll(root, 0o755)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "T"},
		{"commit", "-qm", "initial", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "aurium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	p, _ := st.CreateProject(ctx, "app", root)
	repo, _ := st.CreateRepository(ctx, p.ID, root, "main", "")
	c, _ := st.CreateContainer(ctx, store.Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "feature", Slug: "feature",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: root, Status: store.ContainerRunning, OriginKind: store.OriginFresh,
	})

	return Deps{
		Store: st, Events: events.New(st),
		Drivers: driver.Registry{"local": driver.NewLocal()},
		Home:    t.TempDir(),
	}, c
}

// seedSnapshots creates n snapshot rows with real archive directories.
func seedSnapshots(t *testing.T, d Deps, c store.Container, n int, label func(int) string) []store.Snapshot {
	t.Helper()
	ctx := context.Background()
	var out []store.Snapshot
	for i := 1; i <= n; i++ {
		dir, err := EnsureDir(d.Home, c.ProjectID, c.ID, i)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema":1}`), 0o644)

		s, err := d.Store.CreateSnapshot(ctx, store.Snapshot{
			ContainerID: c.ID, Seq: i, Label: label(i), Trigger: TriggerManual,
			HeadSHA: "head", TreeRef: TreeRef(c.ID, i), BaseSHA: "base",
			ImageRef: ImageRef(c.ID, i), ManifestPath: filepath.Join(dir, "manifest.json"),
			ContextVersion: 1, Bytes: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestGCKeepsRecentLabelledAndReferencedSnapshots(t *testing.T) {
	d, c := gcFixture(t)
	ctx := context.Background()

	// 15 snapshots; #3 is labelled, #5 will be referenced by a fork.
	snaps := seedSnapshots(t, d, c, 15, func(i int) string {
		if i == 3 {
			return "before-oauth-refactor"
		}
		return ""
	})

	// A container forked from snapshot 5 — its lineage must stay explainable.
	repo, _ := d.Store.RepositoryByPath(ctx, c.ProjectID, c.Worktree)
	forked, err := d.Store.CreateContainer(ctx, store.Container{
		ProjectID: c.ProjectID, RepoID: repo.ID, Branch: "forked", Slug: "forked",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: c.Worktree + "-forked", Status: store.ContainerRunning,
		OriginKind: store.OriginFork, OriginSnapshotID: snaps[4].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = forked

	report, err := GC(ctx, d, c.ProjectID, Policy{KeepLast: 10})
	if err != nil {
		t.Fatal(err)
	}

	kept := map[string]string{}
	for _, k := range report.Kept {
		kept[k.ID] = k.Reason
	}

	// Seqs 6..15 are the ten most recent.
	for _, s := range snaps[5:] {
		if _, ok := kept[s.ID]; !ok {
			t.Errorf("snapshot %d should be kept as recent", s.Seq)
		}
	}
	if kept[snaps[2].ID] != KeepLabelled {
		t.Errorf("labelled snapshot 3 was not kept: %q", kept[snaps[2].ID])
	}
	if kept[snaps[4].ID] != KeepReferenced {
		t.Errorf("snapshot 5 is a fork origin and must be kept: %q", kept[snaps[4].ID])
	}
	// Only 1, 2 and 4 are eligible for deletion.
	if len(report.Deleted) != 3 {
		t.Fatalf("deleted %d snapshots, want 3 (seqs 1, 2, 4): %v", len(report.Deleted), report.Deleted)
	}

	// Deletion is real: rows and archive directories are gone.
	for _, id := range report.Deleted {
		if _, err := d.Store.GetSnapshot(ctx, id); err == nil {
			t.Errorf("snapshot row %s survived gc", id)
		}
	}
	if _, err := os.Stat(Dir(d.Home, c.ProjectID, c.ID, 1)); !os.IsNotExist(err) {
		t.Error("archive directory of a deleted snapshot survived")
	}
	// And the kept ones are untouched.
	if _, err := os.Stat(Dir(d.Home, c.ProjectID, c.ID, 3)); err != nil {
		t.Error("archive directory of a labelled snapshot was deleted")
	}
}

func TestGCDryRunChangesNothing(t *testing.T) {
	d, c := gcFixture(t)
	ctx := context.Background()
	seedSnapshots(t, d, c, 15, func(int) string { return "" })

	report, err := GC(ctx, d, c.ProjectID, Policy{KeepLast: 10, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Deleted) != 5 {
		t.Fatalf("dry run should report 5 deletions, got %d", len(report.Deleted))
	}
	snaps, _ := d.Store.ListSnapshots(ctx, c.ID)
	if len(snaps) != 15 {
		t.Fatalf("dry run deleted %d snapshots for real", 15-len(snaps))
	}
	if _, err := os.Stat(Dir(d.Home, c.ProjectID, c.ID, 1)); err != nil {
		t.Error("dry run removed an archive directory")
	}
}

func TestGCReportsBytesFreed(t *testing.T) {
	d, c := gcFixture(t)
	seedSnapshots(t, d, c, 12, func(int) string { return "" })

	report, err := GC(context.Background(), d, c.ProjectID, Policy{KeepLast: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.BytesFreed != 2000 {
		t.Fatalf("BytesFreed = %d, want 2000 (two 1000-byte snapshots)", report.BytesFreed)
	}
}
