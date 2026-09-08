package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Phase A gate, automated: N containers on one repository with no state
// leakage, stacking, stale detection, sync, and an intact object database.

var auriumBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aurium-e2e-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	auriumBin = filepath.Join(dir, "aurium")
	build := exec.Command("go", "build", "-o", auriumBin, "github.com/RhyChaw/aurium/cmd/aurium")
	if out, err := build.CombinedOutput(); err != nil {
		panic("building aurium: " + err.Error() + "\n" + string(out))
	}
	os.Exit(m.Run())
}

type env struct {
	t    *testing.T
	root string
	home string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, root: filepath.Join(t.TempDir(), "app"), home: t.TempDir()}
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		t.Fatal(err)
	}
	e.git("init", "-q", "-b", "main")
	e.git("config", "user.email", "e2e@aurium.dev")
	e.git("config", "user.name", "E2E")
	e.git("config", "commit.gpgsign", "false")
	e.writeIn(e.root, "README.md", "# app\n")
	e.git("add", "-A")
	e.git("commit", "-qm", "initial")
	e.aurium("init", "--driver", "local", "--agent", "shell")
	return e
}

func (e *env) aurium(args ...string) string {
	e.t.Helper()
	out, err := e.auriumErr(args...)
	if err != nil {
		e.t.Fatalf("aurium %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) auriumErr(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(auriumBin, args...)
	cmd.Dir = e.root
	cmd.Env = append(os.Environ(), "AURIUM_HOME="+e.home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	return e.gitIn(e.root, args...)
}

func (e *env) gitIn(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) writeIn(dir, name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) wt(branch string) string {
	return filepath.Join(e.root, ".aurium", "wt", branch)
}

// commitIn makes a commit inside a container's worktree, the way an agent would.
func (e *env) commitIn(branch, file, body, msg string) {
	e.t.Helper()
	wt := e.wt(branch)
	e.writeIn(wt, file, body)
	e.gitIn(wt, "add", "-A")
	e.gitIn(wt, "commit", "-qm", msg)
}

func (e *env) exists(branch, file string) bool {
	_, err := os.Stat(filepath.Join(e.wt(branch), file))
	return err == nil
}

func TestPhaseAGate(t *testing.T) {
	e := newEnv(t)

	// Three containers: B stacks on A; C is an independent lineage off main.
	e.aurium("container", "create", "A")
	e.aurium("container", "create", "B", "--parent", "A")
	e.aurium("container", "create", "C")

	// Each agent works only in its own worktree.
	e.commitIn("A", "a.txt", "a", "A: work")
	e.commitIn("B", "b.txt", "b", "B: work")
	e.commitIn("C", "c.txt", "c", "C: work")

	// No leakage: nobody sees anybody else's files.
	if e.exists("A", "b.txt") || e.exists("A", "c.txt") {
		t.Error("A can see another container's work")
	}
	if e.exists("C", "a.txt") || e.exists("C", "b.txt") {
		t.Error("C can see another container's work")
	}
	// B only has what it inherited at creation, not A's later commit.
	if e.exists("B", "a.txt") {
		t.Error("B received A's commit without a sync")
	}

	// The parent moves; B must be reported stale and C must not be.
	e.commitIn("A", "a2.txt", "a2", "A: parent moves")
	tree := e.aurium("tree")
	if !strings.Contains(tree, "stale") {
		t.Errorf("B should be stale after A moved:\n%s", tree)
	}

	// Sync brings A's work into B while keeping B's own.
	e.aurium("sync", "B")
	if !e.exists("B", "b.txt") {
		t.Error("sync lost B's own work")
	}
	if !e.exists("B", "a2.txt") {
		t.Error("sync did not bring the parent's commit into B")
	}

	// C is untouched throughout — the isolation the product exists for.
	if e.exists("C", "a.txt") || e.exists("C", "a2.txt") || e.exists("C", "b.txt") {
		t.Error("an unrelated container was affected by the sync")
	}

	// And the repository is intact.
	e.git("fsck", "--no-progress")
}

func TestGuardHookStopsACrossBranchWriteThroughTheCLI(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.aurium("container", "create", "B")

	before := e.git("rev-parse", "refs/heads/A")

	// Simulate an agent inside B reaching for A's branch. The container env is
	// what the guard hook keys on.
	cmd := exec.Command("git", "update-ref", "refs/heads/A", "HEAD")
	cmd.Dir = e.wt("B")
	cmd.Env = append(os.Environ(),
		"AURIUM_GUARD=1",
		"AURIUM_BRANCH=B",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0="+filepath.Join(e.root, ".aurium", "hooks"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("a container must not be able to move another container's branch")
	}
	if !strings.Contains(string(out), "Invariant 1") {
		t.Fatalf("the refusal should come from the aurium guard hook, got:\n%s", out)
	}
	if after := e.git("rev-parse", "refs/heads/A"); after != before {
		t.Fatalf("branch A moved from %s to %s", before, after)
	}
}

func TestDestroyKeepsTheBranchAndLeavesSiblingsAlone(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.aurium("container", "create", "B")
	e.commitIn("A", "a.txt", "a", "A: work")

	e.aurium("destroy", "B")

	if _, err := os.Stat(e.wt("B")); !os.IsNotExist(err) {
		t.Error("destroy should remove the worktree")
	}
	// Work is never destroyed implicitly.
	e.git("rev-parse", "refs/heads/B")
	// The sibling is untouched.
	if !e.exists("A", "a.txt") {
		t.Error("destroying one container disturbed another")
	}
}

func TestDestroyRefusesToOrphanStackedChildren(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.aurium("container", "create", "B", "--parent", "A")

	out, err := e.auriumErr("destroy", "A")
	if err == nil {
		t.Fatal("destroying a container with stacked children must be refused without --cascade")
	}
	if !strings.Contains(out, "cascade") {
		t.Fatalf("the error should point at --cascade, got:\n%s", out)
	}
}

func TestEventsRecordTheWholeTrail(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")

	out := e.aurium("events")
	for _, want := range []string{"project.created", "container.created", "agent.started"} {
		if !strings.Contains(out, want) {
			t.Errorf("event %q missing from the audit trail:\n%s", want, out)
		}
	}
}

func TestInitIsNotSilentlyRepeatable(t *testing.T) {
	e := newEnv(t)
	if out, err := e.auriumErr("init"); err == nil {
		t.Fatalf("a second init must fail rather than overwrite the config:\n%s", out)
	}
}

func TestCommandsWorkFromInsideAContainerWorktree(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")

	// Running from inside a worktree must resolve back to the project, not
	// treat the worktree as its own repository.
	cmd := exec.Command(auriumBin, "status")
	cmd.Dir = e.wt("A")
	cmd.Env = append(os.Environ(), "AURIUM_HOME="+e.home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status from inside a worktree failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "containers") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
