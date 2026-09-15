package runtime

import (
	"context"
	"strings"

	"github.com/RhyChaw/aurium/internal/runtime/driver"
)

// Execution adapts the runtime to gateway.Executor, the same way Delegation
// adapts it to gateway.Delegator: the gateway declares the interface so
// internal/gateway need not import internal/runtime (which would otherwise
// import the gateway right back), and this is the concrete thing wired onto
// the other end of it.
//
// It is what makes aurium_exec (§10, host placement) do anything at all. With
// no Executor set, the gateway answers every call with "not available" and a
// host-sandboxed turn — whose own shell is denied — cannot run a single
// command.
type Execution struct {
	Manager *Manager
}

// ExecInContainer runs command inside containerID's container, in its
// worktree, regardless of where the caller's own process happens to be
// running. This is the one place aurium_exec reaches: a host-sandboxed
// agent's commands are routed back through here so the container remains the
// only place project commands run.
func (e *Execution) ExecInContainer(ctx context.Context, containerID, command string) (string, int, error) {
	c, err := e.Manager.Store.GetContainer(ctx, containerID)
	if err != nil {
		return "", 0, err
	}
	drv, err := e.Manager.Drivers.Get(c.Driver)
	if err != nil {
		return "", 0, err
	}

	res, err := drv.Exec(ctx, c.RuntimeID, []string{"sh", "-c", command},
		driver.ExecOpts{Workdir: c.Worktree})
	if err != nil {
		return "", 0, err
	}

	// A failed command is a result, not an error (mirrors runHost and
	// serial delegation): the caller sees the exit code, and stderr is
	// folded in so a failure explains itself instead of vanishing.
	out := res.Stdout
	if res.ExitCode != 0 {
		out = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	}
	return out, res.ExitCode, nil
}
