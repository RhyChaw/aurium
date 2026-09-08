package driver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/RhyChaw/aurium/internal/ids"
)

// Local runs "containers" as plain host processes in the worktree, with no
// isolation at all.
//
// It exists for two reasons. First, it makes the daemon, context engine, IPC
// and gateway testable on a machine with no container runtime, which keeps
// `go test ./...` honest in CI and on a laptop with Docker stopped. Second, it
// is a usable degraded mode for a single agent on a trusted repository.
//
// It deliberately reports Snapshot:false and Pause:false rather than faking
// them: a snapshot that silently omitted the rootfs would break the promise in
// D14 that a snapshot is restorable.
type Local struct {
	mu         sync.Mutex
	containers map[string]*localContainer
}

type localContainer struct {
	spec    Spec
	running bool
}

// NewLocal returns a host-process driver.
func NewLocal() *Local {
	return &Local{containers: map[string]*localContainer{}}
}

func (l *Local) Name() string { return "local" }

func (l *Local) Capabilities() Caps {
	return Caps{
		Filesystem: FSShared,
		Pause:      false,
		Checkpoint: false,
		Sidecars:   false,
		Snapshot:   false,
		// No supervision: the local driver has no container to run tmux in,
		// and spawning an interactive REPL on the user's own machine would be
		// a surprise, not a feature.
		Tmux: false,
	}
}

func (l *Local) Create(ctx context.Context, s Spec) (string, error) {
	if s.Workdir == "" {
		return "", fmt.Errorf("driver/local: Spec.Workdir is required")
	}
	id := "local_" + ids.New("rt")
	l.mu.Lock()
	defer l.mu.Unlock()
	l.containers[id] = &localContainer{spec: s}
	return id, nil
}

func (l *Local) Start(ctx context.Context, id string) error {
	return l.withContainer(id, func(c *localContainer) error {
		c.running = true
		return nil
	})
}

func (l *Local) Stop(ctx context.Context, id string) error {
	return l.withContainer(id, func(c *localContainer) error {
		c.running = false
		return nil
	})
}

// Pause is unsupported: there is no process tree to freeze, and pretending
// otherwise would let a snapshot be taken of a moving target.
func (l *Local) Pause(ctx context.Context, id string) error {
	return fmt.Errorf("driver/local: pause: %w", ErrUnsupported)
}

func (l *Local) Unpause(ctx context.Context, id string) error {
	return fmt.Errorf("driver/local: unpause: %w", ErrUnsupported)
}

// Destroy forgets the container. It is idempotent because cleanup paths run
// after crashes, where the container may already be gone.
func (l *Local) Destroy(ctx context.Context, id string, keepVolumes bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.containers, id)
	return nil
}

func (l *Local) Exec(ctx context.Context, id string, cmd []string, o ExecOpts) (ExecResult, error) {
	l.mu.Lock()
	c, ok := l.containers[id]
	l.mu.Unlock()
	if !ok {
		return ExecResult{}, fmt.Errorf("driver/local: %s: %w", id, ErrNotFound)
	}
	if len(cmd) == 0 {
		return ExecResult{}, fmt.Errorf("driver/local: empty command")
	}

	workdir := o.Workdir
	if workdir == "" {
		workdir = c.spec.Workdir
	}

	proc := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	proc.Dir = workdir
	proc.Env = append(os.Environ(), c.spec.Env...)
	proc.Env = append(proc.Env, o.Env...)
	if o.Stdin != "" {
		proc.Stdin = bytes.NewBufferString(o.Stdin)
	}

	var stdout, stderr bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stderr

	err := proc.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}

	var ee *exec.ExitError
	if err != nil {
		if asExit(err, &ee) {
			// A non-zero exit is a result the caller inspects, not a failure
			// of the driver.
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("driver/local: exec %v: %w", cmd, err)
	}
	return res, nil
}

// Ports returns nothing: a host process binds whatever it binds, and Aurium
// does not remap it.
func (l *Local) Ports(ctx context.Context, id string) (map[int]int, error) {
	return map[int]int{}, nil
}

func (l *Local) Inspect(ctx context.Context, id string) (State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.containers[id]
	if !ok {
		return State{}, fmt.Errorf("driver/local: %s: %w", id, ErrNotFound)
	}
	return State{
		ID:      id,
		Running: c.running,
		Image:   c.spec.Image,
		Labels:  c.spec.Labels,
	}, nil
}

func (l *Local) List(ctx context.Context, labels map[string]string) ([]Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Handle
	for id, c := range l.containers {
		if !matchLabels(c.spec.Labels, labels) {
			continue
		}
		out = append(out, Handle{
			ID:     id,
			Name:   c.spec.Name,
			Labels: c.spec.Labels,
			State:  State{ID: id, Running: c.running, Image: c.spec.Image, Labels: c.spec.Labels},
		})
	}
	return out, nil
}

func (l *Local) Snapshot(ctx context.Context, id, imageRef string) (RootfsRef, error) {
	return RootfsRef{}, fmt.Errorf("driver/local: snapshot rootfs: %w", ErrUnsupported)
}

func (l *Local) Restore(ctx context.Context, s Spec, from RootfsRef) (string, error) {
	// Restoring source and volumes still works; only the rootfs layer is
	// missing, so a plain Create is the honest behaviour here.
	return l.Create(ctx, s)
}

func (l *Local) CloneVolumes(ctx context.Context, from, to []VolumeMount) error {
	if len(from) == 0 {
		return nil
	}
	return fmt.Errorf("driver/local: clone volumes: %w", ErrUnsupported)
}

func (l *Local) EnsureNetwork(ctx context.Context, name string) error { return nil }
func (l *Local) RemoveNetwork(ctx context.Context, name string) error { return nil }

func (l *Local) withContainer(id string, fn func(*localContainer) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.containers[id]
	if !ok {
		return fmt.Errorf("driver/local: %s: %w", id, ErrNotFound)
	}
	return fn(c)
}

// matchLabels reports whether have contains every key/value in want.
func matchLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
