package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

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

	// Resolved up front so a missing binary is reported by name, rather than
	// as an opaque failure from the run itself.
	path, err := exec.LookPath(cmd[0])
	if err != nil {
		return driver.ExecResult{}, fmt.Errorf(
			"runtime: %s is not installed on this host, which agent_placement %q requires: %w",
			cmd[0], "host", err)
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
