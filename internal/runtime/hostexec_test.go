package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
)

func TestRunHostCapturesOutputAndWorkdir(t *testing.T) {
	dir := t.TempDir()
	res, err := runHost(context.Background(), []string{"pwd"}, driver.ExecOpts{Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	// macOS resolves /var to /private/var, so compare on the suffix.
	if !strings.HasSuffix(strings.TrimSpace(res.Stdout), strings.TrimPrefix(dir, "/private")) {
		t.Errorf("command did not run in Workdir: got %q, want %q", res.Stdout, dir)
	}
}

// A non-zero exit is a result, not a Go error: the agent's own command failing
// is information for the agent, and turning it into an error loses the output
// that explains it.
func TestRunHostReturnsNonZeroExitAsAResult(t *testing.T) {
	res, err := runHost(context.Background(), []string{"sh", "-c", "echo boom >&2; exit 3"},
		driver.ExecOpts{})
	if err != nil {
		t.Fatalf("a failing command must not be a Go error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Errorf("stderr lost: %q", res.Stderr)
	}
}

// The failure the whole preflight table exists to prevent: a missing binary
// must say so at the point of use, by name.
func TestRunHostNamesAMissingBinary(t *testing.T) {
	_, err := runHost(context.Background(), []string{"definitely-not-installed-xyz"}, driver.ExecOpts{})
	if err == nil {
		t.Fatal("a missing binary must be an error")
	}
	if !strings.Contains(err.Error(), "definitely-not-installed-xyz") {
		t.Errorf("the error must name the binary, got: %v", err)
	}
}

func TestRunHostPassesEnvAndStdin(t *testing.T) {
	res, err := runHost(context.Background(), []string{"sh", "-c", "read x; echo \"$AURIUM_T-$x\""},
		driver.ExecOpts{Env: []string{"AURIUM_T=set"}, Stdin: "piped\n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "set-piped" {
		t.Errorf("env or stdin lost: %q", res.Stdout)
	}
}

// A cancelled or timed-out turn is an error, distinct from a failed command.
// The killed process has no meaningful exit code, so we return an error that
// names the cancellation and includes ctx.Err(), but preserve partial output.
func TestRunHostIdentifiesCancelledTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runHost(ctx, []string{"sh", "-c", "sleep 5"}, driver.ExecOpts{})
	if err == nil {
		t.Fatal("a cancelled turn must be an error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error must identify cancellation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error must include context error, got: %v", err)
	}
}

// Host placement cannot honour User requests, which would require
// privilege escalation forbidden by design.
func TestRunHostRejectsUserRequest(t *testing.T) {
	_, err := runHost(context.Background(), []string{"echo", "test"},
		driver.ExecOpts{User: "other"})
	if err == nil {
		t.Fatal("User request must be an error")
	}
	if !strings.Contains(err.Error(), "cannot run commands as a different user") {
		t.Errorf("error must name the constraint, got: %v", err)
	}
}

// Host placement cannot honour TTY requests, which are meaningless for headless turns.
func TestRunHostRejectsTTYRequest(t *testing.T) {
	_, err := runHost(context.Background(), []string{"echo", "test"},
		driver.ExecOpts{TTY: true})
	if err == nil {
		t.Fatal("TTY request must be an error")
	}
	if !strings.Contains(err.Error(), "cannot allocate a TTY") {
		t.Errorf("error must name the constraint, got: %v", err)
	}
}

type fakeDriver struct {
	driver.Driver // embedded: only Exec is called here
	called        bool
}

func (f *fakeDriver) Exec(ctx context.Context, id string, cmd []string, o driver.ExecOpts) (driver.ExecResult, error) {
	f.called = true
	return driver.ExecResult{Stdout: "from-container"}, nil
}

func TestExecTurnUsesTheContainerByDefault(t *testing.T) {
	f := &fakeDriver{}
	res, err := execTurn(context.Background(), config.PlacementInContainer, f, "rt_1",
		[]string{"echo", "hi"}, driver.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.called {
		t.Error("in-container placement must go through the driver")
	}
	if res.Stdout != "from-container" {
		t.Errorf("result not passed through: %q", res.Stdout)
	}
}

func TestExecTurnUsesTheHostWhenPlacementSaysSo(t *testing.T) {
	f := &fakeDriver{}
	res, err := execTurn(context.Background(), config.PlacementHost, f, "rt_1",
		[]string{"echo", "from-host"}, driver.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if f.called {
		t.Error("host placement must not touch the driver")
	}
	if !strings.Contains(res.Stdout, "from-host") {
		t.Errorf("host output lost: %q", res.Stdout)
	}
}
