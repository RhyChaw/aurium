package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
)

// runHost runs one agent turn on this machine rather than inside its
// container.
//
// It returns the same driver.ExecResult the container path returns, because
// everything downstream — token accounting, transcript rows, error text —
// must not be able to tell the two apart. The worktree is bind-mounted at an
// identical path (D3), so o.Workdir means the same thing here as it does in
// the container and needs no translation.
func runHost(ctx context.Context, cmd []string, o driver.ExecOpts) (driver.ExecResult, error) {
	if len(cmd) == 0 {
		return driver.ExecResult{}, errors.New("runtime: empty command")
	}

	// Host placement cannot honour User (would require privilege escalation,
	// which this design forbids) or TTY (meaningless for a headless turn).
	// A zero value is silent for compatibility with existing callers; any
	// explicit request is an error.
	if o.User != "" {
		return driver.ExecResult{}, fmt.Errorf(
			"runtime: agent_placement %q cannot run commands as a different user", config.PlacementHost)
	}
	if o.TTY {
		return driver.ExecResult{}, fmt.Errorf(
			"runtime: agent_placement %q cannot allocate a TTY", config.PlacementHost)
	}

	// Resolved up front so a missing binary is reported by name, rather than
	// as an opaque failure from the run itself.
	path, err := exec.LookPath(cmd[0])
	if err != nil {
		return driver.ExecResult{}, fmt.Errorf(
			"runtime: %s is not installed on this host, which agent_placement %q requires: %w",
			cmd[0], config.PlacementHost, err)
	}

	c := exec.CommandContext(ctx, path, cmd[1:]...)
	c.Dir = o.Workdir
	c.Env = append(os.Environ(), o.Env...)
	if o.Stdin != "" {
		c.Stdin = strings.NewReader(o.Stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr

	runErr := c.Run()
	res := driver.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}

	// Check if the context was cancelled or timed out. The process was SIGKILLed
	// by CommandContext, so the exit code is meaningless. Return an error that
	// distinguishes a cancelled turn from a failed command, but include the
	// partial output for diagnosis.
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("runtime: turn on host was cancelled: %w", err)
	}

	// A command that ran and failed is a result. Only a command that could not
	// be run at all is an error.
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if runErr != nil {
		return res, fmt.Errorf("runtime: run %s on host: %w", cmd[0], runErr)
	}
	return res, nil
}

// wantsTmuxSession reports whether to launch an interactive session.
//
// Two independent reasons not to, and both must hold to proceed: a driver
// without tmux has no container to run it in, and host placement has no
// container involved in the agent at all.
func wantsTmuxSession(placement string, caps driver.Caps) bool {
	return caps.Tmux && placement != config.PlacementHost
}

// execTurn runs a turn where the project's placement says it should run.
//
// This one function is the whole of host placement. A turn ran inside the
// container before only because converse.go called drv.Exec directly; putting
// the choice here keeps it a setting rather than a second code path.
func execTurn(ctx context.Context, placement string, drv driver.Driver, runtimeID string,
	cmd []string, o driver.ExecOpts) (driver.ExecResult, error) {
	if placement == config.PlacementHost {
		return runHost(ctx, cmd, o)
	}
	return drv.Exec(ctx, runtimeID, cmd, o)
}
