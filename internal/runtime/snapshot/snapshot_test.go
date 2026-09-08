package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/gitx"
)

func ctx() context.Context { return context.Background() }

type fixture struct {
	t    *testing.T
	root string
	wt   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, root: filepath.Join(t.TempDir(), "app")}
	os.MkdirAll(f.root, 0o755)
	f.git(f.root, "init", "-q", "-b", "main")
	f.git(f.root, "config", "user.email", "t@aurium.dev")
	f.git(f.root, "config", "user.name", "T")
	f.git(f.root, "config", "commit.gpgsign", "false")

	f.write(f.root, "committed.txt", "original\n")
	f.write(f.root, ".gitignore", "node_modules/\ndist/\n")
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-qm", "initial")

	gitx.EnsureExcluded(f.root)
	wt, err := gitx.AddWorktree(ctx(), f.root, "feature", "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	f.wt = wt
	return f
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(dir, name, body string) {
	f.t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(dir, name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		f.t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

func (f *fixture) exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// treeFiles lists the paths captured in a snapshot's git tree.
func (f *fixture) treeFiles(ref string) []string {
	out := f.git(f.root, "ls-tree", "-r", "--name-only", ref)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// A snapshot must capture what the agent actually has: committed work,
// uncommitted edits, and untracked scratch files. HEAD captures none of the
// last two, which is why a snapshot writes its own tree.
func TestCaptureTreeIncludesTrackedUntrackedAndHonoursGitignore(t *testing.T) {
	f := newFixture(t)

	f.write(f.wt, "committed.txt", "edited but not committed\n")
	f.write(f.wt, "scratch.txt", "untracked work in progress\n")
	f.write(f.wt, "node_modules/lib.js", "derived, gitignored\n")

	commit, err := CaptureTree(ctx(), f.root, f.wt, "c_1", 1, false)
	if err != nil {
		t.Fatal(err)
	}

	files := f.treeFiles(commit)
	has := func(name string) bool {
		for _, x := range files {
			if x == name {
				return true
			}
		}
		return false
	}

	if !has("committed.txt") {
		t.Error("tracked file missing from the snapshot tree")
	}
	if !has("scratch.txt") {
		t.Error("untracked file missing — an agent's work in progress would be lost")
	}
	// §5.2: gitignored build output is derivable and excluded by default.
	if has("node_modules/lib.js") {
		t.Error("gitignored file captured despite include_ignored:false")
	}

	// The uncommitted edit, not the committed content, is what was captured.
	blob := f.git(f.root, "show", commit+":committed.txt")
	if !strings.Contains(blob, "edited but not committed") {
		t.Fatalf("snapshot captured HEAD instead of the worktree: %q", blob)
	}
}

func TestCaptureTreeWithIncludeIgnored(t *testing.T) {
	f := newFixture(t)
	f.write(f.wt, "node_modules/lib.js", "derived\n")

	commit, err := CaptureTree(ctx(), f.root, f.wt, "c_1", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range f.treeFiles(commit) {
		if x == "node_modules/lib.js" {
			found = true
		}
	}
	if !found {
		t.Error("include_ignored:true should capture gitignored files")
	}
}

// Snapshotting must not disturb the container it is snapshotting. The agent
// may be mid-edit; a staged file appearing in its index would be a real
// surprise.
func TestCaptureTreeDoesNotTouchTheWorktreeIndexOrHead(t *testing.T) {
	f := newFixture(t)
	f.write(f.wt, "scratch.txt", "wip\n")
	f.git(f.wt, "add", "scratch.txt") // deliberately staged

	headBefore := f.git(f.wt, "rev-parse", "HEAD")
	statusBefore := f.git(f.wt, "status", "--porcelain")

	if _, err := CaptureTree(ctx(), f.root, f.wt, "c_1", 1, false); err != nil {
		t.Fatal(err)
	}

	if after := f.git(f.wt, "rev-parse", "HEAD"); after != headBefore {
		t.Error("snapshotting moved HEAD")
	}
	if after := f.git(f.wt, "status", "--porcelain"); after != statusBefore {
		t.Errorf("snapshotting changed the index/worktree state:\nbefore %q\nafter  %q",
			statusBefore, after)
	}
}

func TestCaptureTreeWritesTheSnapshotRef(t *testing.T) {
	f := newFixture(t)
	commit, err := CaptureTree(ctx(), f.root, f.wt, "c_1", 7, false)
	if err != nil {
		t.Fatal(err)
	}
	ref := TreeRef("c_1", 7)
	if got := f.git(f.root, "rev-parse", ref); got != commit {
		t.Fatalf("%s = %s, want %s", ref, got, commit)
	}
	// The snapshot commit's parent is the branch head, so the snapshot is
	// reachable history rather than a dangling object git gc would collect.
	parent := f.git(f.root, "rev-parse", commit+"^")
	head := f.git(f.wt, "rev-parse", "HEAD")
	if parent != head {
		t.Errorf("snapshot commit parent = %s, want the branch head %s", parent, head)
	}
}

// The other half: restoring must undo every kind of change, including the
// untracked and deleted ones a plain `git checkout` would leave alone.
func TestRestoreTreeRevertsEditsDeletionsAndAdditions(t *testing.T) {
	f := newFixture(t)

	f.write(f.wt, "committed.txt", "snapshot state\n")
	f.write(f.wt, "scratch.txt", "snapshot scratch\n")
	commit, err := CaptureTree(ctx(), f.root, f.wt, "c_1", 1, false)
	if err != nil {
		t.Fatal(err)
	}

	// Now wreck the worktree the way a bad agent run would.
	f.write(f.wt, "committed.txt", "corrupted\n")
	os.Remove(filepath.Join(f.wt, "scratch.txt"))
	f.write(f.wt, "junk.txt", "should not survive\n")

	if err := RestoreTree(ctx(), f.root, f.wt, commit); err != nil {
		t.Fatal(err)
	}

	if got := f.read(f.wt, "committed.txt"); got != "snapshot state\n" {
		t.Errorf("edited file not reverted: %q", got)
	}
	if !f.exists(f.wt, "scratch.txt") {
		t.Error("deleted untracked file not restored")
	}
	if f.exists(f.wt, "junk.txt") {
		t.Error("file added after the snapshot survived the restore")
	}
}

// Restore must not wipe a gitignored directory the snapshot never captured:
// blowing away node_modules on every restore would make restore unusable.
func TestRestoreLeavesGitignoredContentAlone(t *testing.T) {
	f := newFixture(t)
	commit, err := CaptureTree(ctx(), f.root, f.wt, "c_1", 1, false)
	if err != nil {
		t.Fatal(err)
	}

	f.write(f.wt, "node_modules/lib.js", "installed dependency\n")
	if err := RestoreTree(ctx(), f.root, f.wt, commit); err != nil {
		t.Fatal(err)
	}
	if !f.exists(f.wt, "node_modules/lib.js") {
		t.Error("restore deleted gitignored content it never captured")
	}
}

func TestManifestRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := &Manifest{
		Schema: SchemaVersion, ID: "s_01J", Container: "c_01J", Seq: 17,
		Label: "before-oauth-refactor", CreatedAt: "2026-09-07T18:02:11Z",
		Trigger: TriggerManual,
		Git: GitPart{
			Branch: "implement-oauth", HeadSHA: "a1b2", TreeRef: TreeRef("c_01J", 17),
			BaseSHA: "9f8e", Parent: "main",
		},
		Rootfs: RootfsPart{Image: ImageRef("c_01J", 17), Digest: "sha256:abc", Captured: true},
		Volumes: []VolumePart{{
			Name: "aurium-app-oauth-node_modules", Mount: "/wt/node_modules",
			Archive: "volumes/node_modules.tar.zst", Bytes: 412331020,
		}},
		ContextVersion: 43,
		Agents:         []AgentPart{{Adapter: "claude", Session: "agent", ResumeHint: "claude --continue", CanResume: true}},
		EnvHash:        "sha256:def",
		Ports:          map[string]int{"3000": 53012},
	}
	if err := want.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 17 || got.Git.TreeRef != want.Git.TreeRef || got.ContextVersion != 43 {
		t.Fatalf("manifest did not round-trip: %+v", got)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Bytes != 412331020 {
		t.Fatalf("volumes did not round-trip: %+v", got.Volumes)
	}
}

func TestReadManifestRejectsAFutureSchema(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"),
		[]byte(`{"schema": 99, "id": "s_1"}`), 0o644)
	if _, err := ReadManifest(dir); err == nil {
		t.Fatal("a newer manifest schema must be refused, not silently misread")
	}
}

func TestRefAndPathHelpersAreStable(t *testing.T) {
	if got := TreeRef("c_1", 3); got != "refs/aurium/snap/c_1/3" {
		t.Errorf("TreeRef = %q", got)
	}
	if got := ImageRef("c_1", 3); got != "aurium-snap/c_1:3" {
		t.Errorf("ImageRef = %q", got)
	}
	if got := Dir("/home", "p_1", "c_1", 3); got != "/home/snapshots/p_1/c_1/3" {
		t.Errorf("Dir = %q", got)
	}
}
