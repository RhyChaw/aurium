package runtime

import (
	"context"
	"fmt"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
)

// hostHome reports whether a driver's $HOME is a directory on this machine
// rather than a path inside the container's rootfs.
//
// buildSpec and the projection must agree about this. They used to decide it
// separately, and the projection's copy was simply missing — which is how the
// daemon came to write a container path with os.WriteFile. One function, read
// by both, is what stops that drifting apart again.
func hostHome(drv driver.Driver) bool { return !drv.Capabilities().Snapshot }

// projectionFS is the filesystem an adapter's Prepare should write into.
func projectionFS(ctx context.Context, drv driver.Driver, runtimeID, home string) agent.HomeFS {
	if hostHome(drv) {
		return agent.OSHome{Root: home}
	}
	return containerHome{ctx: ctx, drv: drv, id: runtimeID, root: home}
}

// containerHome writes into a running container's own filesystem, through the
// driver, because that is where the container's $HOME actually is.
//
// It uses Exec rather than a bind mount deliberately. $HOME in the rootfs is
// what makes `docker commit` capture an agent's transcript (§1.1 finding 1),
// and so what makes fork and restore resume a real conversation; a bind would
// move that state onto the host and share it between an agent and its forks.
type containerHome struct {
	ctx  context.Context
	drv  driver.Driver
	id   string
	root string
}

// sh runs a script with the target path as $1. Passing the path as an argument
// rather than interpolating it into the script is what keeps a directory name
// from being read as shell syntax.
func (c containerHome) sh(script, arg string, stdin string) (driver.ExecResult, error) {
	return c.drv.Exec(c.ctx, c.id, []string{"sh", "-c", script, "sh", arg},
		driver.ExecOpts{Stdin: stdin})
}

func (c containerHome) path(rel string) string { return c.root + "/" + rel }

func (c containerHome) ReadFile(rel string) ([]byte, error) {
	// `|| true` so a missing file is empty rather than an error: HomeFS
	// promises absence reads as nil, and a fresh home has none of these files.
	res, err := c.sh(`cat -- "$1" 2>/dev/null || true`, c.path(rel), "")
	if err != nil {
		return nil, fmt.Errorf("read %s in container: %w", c.path(rel), err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read %s in container: exit %d: %s",
			c.path(rel), res.ExitCode, res.Stderr)
	}
	return []byte(res.Stdout), nil
}

func (c containerHome) WriteFile(rel, content string) error {
	res, err := c.sh(`mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`, c.path(rel), content)
	if err != nil {
		return fmt.Errorf("write %s in container: %w", c.path(rel), err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s in container: exit %d: %s",
			c.path(rel), res.ExitCode, res.Stderr)
	}
	return nil
}
