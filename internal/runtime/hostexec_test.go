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

// The daemon's environment is not the agent's. In-container placement passes
// only sandbox.env, the declared env_passthrough names and the resolved
// credential — env_passthrough exists precisely to name what crosses the
// boundary — so a host turn must not quietly inherit every secret the daemon
// was started with.
func TestRunHostDoesNotInheritTheDaemonsEnvironment(t *testing.T) {
	t.Setenv("AURIUM_TEST_DAEMON_SECRET", "leaked")

	res, err := runHost(context.Background(),
		[]string{"sh", "-c", "echo \"[$AURIUM_TEST_DAEMON_SECRET]\""}, driver.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "[]" {
		t.Errorf("an undeclared variable reached the agent: %q", res.Stdout)
	}
}

// Inheriting nothing at all would be its own bug: a process with no PATH
// cannot find its own toolchain, and a process with no HOME cannot read the
// agent CLI's login.
func TestRunHostInheritsTheFewVariablesAProcessNeeds(t *testing.T) {
	res, err := runHost(context.Background(),
		[]string{"sh", "-c", "test -n \"$PATH\" && test -n \"$HOME\" && echo have-both"},
		driver.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "have-both" {
		t.Errorf("PATH and HOME must survive: %q / %q", res.Stdout, res.Stderr)
	}
}

// Declared entries come last, so a project that sets HOME or PATH in
// sandbox.env means it — the same last-wins rule docker exec and the local
// driver follow.
func TestRunHostLetsADeclaredValueOverrideTheInheritedOne(t *testing.T) {
	res, err := runHost(context.Background(), []string{"sh", "-c", "echo \"$HOME\""},
		driver.ExecOpts{Env: []string{"HOME=/declared/home"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "/declared/home" {
		t.Errorf("declared env must win: %q", res.Stdout)
	}
}

// declaredSandboxEnv is what both placements read from aurium.yaml. If the two
// ever disagreed, env_passthrough would mean one thing in a container and
// another on the host.
func TestDeclaredSandboxEnvIsExactlyWhatTheProjectDeclared(t *testing.T) {
	t.Setenv("AURIUM_TEST_PASSED", "through")
	t.Setenv("AURIUM_TEST_NOT_DECLARED", "secret")

	got := declaredSandboxEnv(config.Sandbox{
		Env:            map[string]string{"GOFLAGS": "-mod=mod"},
		EnvPassthrough: []string{"AURIUM_TEST_PASSED", "AURIUM_TEST_ABSENT"},
	})
	joined := strings.Join(got, " ")
	for _, want := range []string{"GOFLAGS=-mod=mod", "AURIUM_TEST_PASSED=through"} {
		if !strings.Contains(joined, want) {
			t.Errorf("declared env is missing %q: %v", want, got)
		}
	}
	if strings.Contains(joined, "AURIUM_TEST_NOT_DECLARED") {
		t.Errorf("a variable the project did not name must not appear: %v", got)
	}
	// A declared name this process does not have is simply absent, not empty:
	// an empty value would shadow one the container might otherwise inherit.
	if strings.Contains(joined, "AURIUM_TEST_ABSENT") {
		t.Errorf("an unset passthrough name must not be exported empty: %v", got)
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

// Host placement has nothing to attach to, and spawning a REPL on the user's
// own machine is the surprise Caps.Tmux already warns about.
func TestHostPlacementStartsNoTmuxSession(t *testing.T) {
	if wantsTmuxSession(config.PlacementHost, driver.Caps{Tmux: true}) {
		t.Error("host placement must not start a tmux session even on a tmux-capable driver")
	}
	if !wantsTmuxSession(config.PlacementInContainer, driver.Caps{Tmux: true}) {
		t.Error("in-container placement on a tmux driver must still get a session")
	}
	if wantsTmuxSession(config.PlacementInContainer, driver.Caps{Tmux: false}) {
		t.Error("a driver without tmux never gets a session")
	}
}
