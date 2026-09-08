package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalCapabilitiesAreHonest(t *testing.T) {
	c := NewLocal().Capabilities()
	if c.Filesystem != FSShared {
		t.Fatal("local runs on the host filesystem")
	}
	if c.Snapshot {
		t.Fatal("local cannot commit a rootfs; claiming otherwise makes snapshot silently lossy")
	}
	if c.Pause {
		t.Fatal("local cannot quiesce a process tree")
	}
}

func TestLocalCreateStartExec(t *testing.T) {
	d := NewLocal()
	ctx := context.Background()
	dir := t.TempDir()

	id, err := d.Create(ctx, Spec{
		Name:    "aurium-test-feature",
		Workdir: dir,
		Env:     []string{"AURIUM_TOKEN=secret"},
		Labels:  map[string]string{LabelContainer: "c_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}

	res, err := d.Exec(ctx, id, []string{"sh", "-c", "pwd && echo $AURIUM_TOKEN"}, ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	// The local driver must run in the worktree and carry the spec env,
	// because the whole daemon is exercised through it in tests.
	if !strings.Contains(res.Stdout, "secret") {
		t.Fatalf("spec env not passed through: %q", res.Stdout)
	}
}

func TestLocalExecReportsExitCodeWithoutError(t *testing.T) {
	d := NewLocal()
	ctx := context.Background()
	id, _ := d.Create(ctx, Spec{Name: "x", Workdir: t.TempDir()})

	res, err := d.Exec(ctx, id, []string{"sh", "-c", "exit 3"}, ExecOpts{})
	if err != nil {
		t.Fatalf("a non-zero exit is a result, not an error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestLocalExecWritesIntoTheWorktree(t *testing.T) {
	d := NewLocal()
	ctx := context.Background()
	dir := t.TempDir()
	id, _ := d.Create(ctx, Spec{Name: "x", Workdir: dir})

	if _, err := d.Exec(ctx, id, []string{"sh", "-c", "echo hi > made.txt"}, ExecOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); err != nil {
		t.Fatalf("exec must run in the container workdir: %v", err)
	}
}

func TestLocalSnapshotIsUnsupported(t *testing.T) {
	d := NewLocal()
	_, err := d.Snapshot(context.Background(), "x", "img")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported so callers degrade deliberately, got %v", err)
	}
}

func TestLocalInspectAndListTrackLabels(t *testing.T) {
	d := NewLocal()
	ctx := context.Background()
	id, _ := d.Create(ctx, Spec{
		Name:    "aurium-app-feature",
		Workdir: t.TempDir(),
		Labels:  map[string]string{LabelContainer: "c_9", LabelProject: "p_1"},
	})
	d.Start(ctx, id)

	st, err := d.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatal("started container must report Running")
	}

	got, err := d.List(ctx, map[string]string{LabelProject: "p_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("List by label returned %+v", got)
	}
	if none, _ := d.List(ctx, map[string]string{LabelProject: "p_other"}); len(none) != 0 {
		t.Fatal("List must filter by label")
	}
}

func TestLocalDestroyIsIdempotent(t *testing.T) {
	d := NewLocal()
	ctx := context.Background()
	id, _ := d.Create(ctx, Spec{Name: "x", Workdir: t.TempDir()})

	if err := d.Destroy(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	// Destroying twice must not error: cleanup paths run after crashes.
	if err := d.Destroy(ctx, id, false); err != nil {
		t.Fatalf("Destroy must be idempotent, got %v", err)
	}
	if _, err := d.Inspect(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after destroy, got %v", err)
	}
}
