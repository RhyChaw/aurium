package runtime

import (
	"context"
	"strings"
	"testing"

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
