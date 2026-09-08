// Package driver abstracts the container runtime (D2, §5.1).
//
// Aurium talks to Docker/Podman through their CLIs rather than an engine API,
// because the CLI is the stable, documented surface that every installation
// has, including Docker Desktop and OrbStack. The interface is deliberately
// narrow so that Apple's `container`, a microVM runtime, or a remote driver
// can implement it later without the layers above changing.
package driver

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnsupported is returned by drivers that cannot perform an operation, for
// example the local driver asked for a rootfs snapshot. Callers check it with
// errors.Is and degrade rather than failing the whole command.
var ErrUnsupported = errors.New("driver: operation not supported")

// ErrNotFound is returned when a runtime object no longer exists. The watcher
// treats it as "reconcile this row", not as a fatal error.
var ErrNotFound = errors.New("driver: runtime object not found")

// Filesystem describes how the driver sees the host filesystem. `shared` means
// bind mounts at identical paths work (D3); `remote` means the far side owns
// its own clone and the source must be pushed rather than mounted (§11).
type Filesystem string

const (
	FSShared Filesystem = "shared"
	FSRemote Filesystem = "remote"
)

// Caps advertises what a driver can do, so callers degrade deliberately
// instead of discovering gaps through errors.
type Caps struct {
	Filesystem Filesystem
	// Pause quiesces a container so a snapshot is consistent (§6.2 step 1).
	Pause bool
	// Checkpoint is CRIU. No supported driver has it: process memory is not
	// portably capturable, which is why a snapshot is defined without it (D14).
	Checkpoint bool
	Sidecars   bool
	// Snapshot is the ability to commit the rootfs to an image.
	Snapshot bool
	// Tmux means the runtime supervises long-lived interactive agents in a
	// tmux session a human can attach to (D5). Drivers without it can still
	// run headless agents through Exec; they simply have nothing to attach to,
	// and Aurium must not try to spawn a REPL on the user's own machine.
	Tmux bool
}

// Bind is a host directory mounted into the container. Aurium always mounts at
// the identical path (D3) so that absolute paths in build output, editor
// state, language servers and error messages mean the same thing inside and
// outside the container.
type Bind struct {
	Host      string
	Container string
	RW        bool
}

// VolumeMount is a named volume. Per-container volumes are cloned on
// fork/stack; shared volumes (caches) are not (§5.2).
type VolumeMount struct {
	Name   string
	Path   string
	Shared bool
}

// PortSpec asks the driver to publish an internal port. Host is 0 to let the
// runtime choose, which is the normal case: Aurium records what it got.
type PortSpec struct {
	Internal int
	Host     int
	// Env, when set, receives the internal port number inside the container.
	Env string
}

// Resources caps a container. Admission control (Phase B) uses Memory.
type Resources struct {
	CPUs   float64
	Memory string
	PIDs   int
}

// Spec is everything needed to create a container.
type Spec struct {
	Name      string
	Image     string
	Network   string
	Workdir   string
	Env       []string
	Binds     []Bind
	Volumes   []VolumeMount
	Ports     []PortSpec
	Labels    map[string]string
	Resources Resources
	// User is the uid:gid the container runs as; Aurium bakes the host uid
	// into the image so files written to a bind mount are owned correctly.
	User string
}

// ExecOpts controls a command run inside a container.
type ExecOpts struct {
	Workdir string
	Env     []string
	User    string
	Stdin   string
	// TTY is needed for interactive programs; tmux does not want one here.
	TTY bool
}

// ExecResult is the outcome of an Exec.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RootfsRef identifies a committed rootfs, i.e. an image tag plus digest.
type RootfsRef struct {
	Image  string
	Digest string
}

// State is the driver's view of a container.
type State struct {
	ID      string
	Running bool
	Paused  bool
	// ExitCode is meaningful only when the container has stopped.
	ExitCode int
	Image    string
	Labels   map[string]string
}

// Handle is a container discovered by List, used by the watcher to reconcile
// the database against reality (§6.6).
type Handle struct {
	ID     string
	Name   string
	Labels map[string]string
	State  State
}

// Driver is the container runtime abstraction (§5.1).
type Driver interface {
	Name() string
	Capabilities() Caps

	Create(ctx context.Context, s Spec) (runtimeID string, err error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Unpause(ctx context.Context, id string) error
	Destroy(ctx context.Context, id string, keepVolumes bool) error

	Exec(ctx context.Context, id string, cmd []string, o ExecOpts) (ExecResult, error)
	Ports(ctx context.Context, id string) (map[int]int, error)
	Inspect(ctx context.Context, id string) (State, error)
	List(ctx context.Context, labels map[string]string) ([]Handle, error)

	// Snapshot commits the container's rootfs to an image (§6.2 step 3).
	Snapshot(ctx context.Context, id, imageRef string) (RootfsRef, error)
	// Restore recreates a container whose L2 layer is the given rootfs.
	Restore(ctx context.Context, s Spec, from RootfsRef) (string, error)
	// CloneVolumes copies per-container volume contents. Volumes are not
	// copy-on-write on macOS, so this is a real copy and is documented as such
	// (§1.1 finding 2).
	CloneVolumes(ctx context.Context, from, to []VolumeMount) error

	// EnsureNetwork creates the per-container bridge network if absent.
	EnsureNetwork(ctx context.Context, name string) error
	RemoveNetwork(ctx context.Context, name string) error
}

// Labels Aurium stamps on every container it creates, so the watcher can
// reconcile runtime state against the database (§5.3, §6.6).
const (
	LabelProject   = "aurium.project"
	LabelContainer = "aurium.container"
	LabelTask      = "aurium.task"
	LabelManaged   = "aurium.managed"
)

// Registry maps driver names to constructors so config can select one.
type Registry map[string]Driver

// Get returns the named driver.
func (r Registry) Get(name string) (Driver, error) {
	d, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("driver: unknown driver %q", name)
	}
	return d, nil
}
