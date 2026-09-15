package runtime

import (
	"context"
	"strings"
	"testing"
)

// Without this, aurium_exec answers every call with "not available" forever
// and a host-sandboxed turn — whose own shell is denied — cannot run a
// single command. This is what the gateway's Executor field is wired to.
func TestExecutionRunsInTheContainersWorktree(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")
	e := &Execution{Manager: f.mgr}

	out, code, err := e.ExecInContainer(context.Background(), c.ID, "pwd")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(out, strings.TrimPrefix(c.Worktree, "/private")) {
		t.Errorf("command did not run in the container's worktree: got %q, want %q", out, c.Worktree)
	}
}

// A failed command is a result the caller inspects, not a Go error — mirrors
// runHost and serial delegation, and stderr must not vanish, or a failure
// stops explaining itself.
func TestExecutionFoldsStderrIntoOutputOnFailure(t *testing.T) {
	f := newFixture(t)
	c := f.create("feature")
	e := &Execution{Manager: f.mgr}

	out, code, err := e.ExecInContainer(context.Background(), c.ID, "echo boom >&2; exit 3")
	if err != nil {
		t.Fatalf("a failing command must not be a Go error: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("stderr lost: %q", out)
	}
}

func TestExecutionRejectsAnUnknownContainer(t *testing.T) {
	f := newFixture(t)
	e := &Execution{Manager: f.mgr}

	if _, _, err := e.ExecInContainer(context.Background(), "c_nonexistent", "echo hi"); err == nil {
		t.Fatal("an unknown container must be an error")
	}
}
