package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Docker drives Docker Engine, Docker Desktop, OrbStack and (with the same
// argv, per D2) Podman through the CLI.
//
// The CLI rather than the engine API because it is the surface every
// installation has and every user can reproduce by hand from the -v output,
// which matters a great deal when debugging why a container did not come up.
type Docker struct {
	bin     string
	podman  bool
	Verbose bool
}

// NewDocker returns a driver shelling out to bin ("docker" or "podman").
func NewDocker(bin string) *Docker {
	if bin == "" {
		bin = "docker"
	}
	return &Docker{bin: bin, podman: strings.Contains(bin, "podman")}
}

func (d *Docker) Name() string {
	if d.podman {
		return "podman"
	}
	return "docker"
}

func (d *Docker) Capabilities() Caps {
	return Caps{
		Filesystem: FSShared,
		Pause:      true,
		// CRIU is Linux-only, experimental, and absent from Docker Desktop and
		// OrbStack. This is why D14 defines a snapshot without process memory.
		Checkpoint: false,
		Sidecars:   true,
		Snapshot:   true,
	}
}

// createArgs builds the `docker create` argv. It is a pure function so the
// §5.3 wiring — identical bind paths, loopback-only ports, labels, resource
// caps — is testable on a machine with no Docker.
func createArgs(s Spec) []string {
	args := []string{"create", "--init", "--name", s.Name}

	for _, kv := range sortedLabels(s.Labels) {
		args = append(args, "--label", kv)
	}
	// Always stamp the managed label so `docker ps` and the watcher can find
	// Aurium's containers and nothing else.
	args = append(args, "--label", LabelManaged+"=true")

	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.Workdir != "" {
		args = append(args, "--workdir", s.Workdir)
	}

	for _, b := range s.Binds {
		mode := "ro"
		if b.RW {
			mode = "rw"
		}
		// D3: identical paths on both sides.
		args = append(args, "-v", fmt.Sprintf("%s:%s:%s", b.Host, b.Container, mode))
	}
	for _, v := range s.Volumes {
		args = append(args, "-v", fmt.Sprintf("%s:%s", v.Name, v.Path))
	}

	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	for _, p := range s.Ports {
		// D11: bind loopback explicitly. A bare -p would publish on every
		// interface, putting an agent's dev server on the local network.
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p.Host, p.Internal))
		if p.Env != "" {
			args = append(args, "-e", fmt.Sprintf("%s=%d", p.Env, p.Internal))
		}
	}

	if s.Resources.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.Resources.CPUs, 'g', -1, 64))
	}
	if s.Resources.Memory != "" {
		args = append(args, "--memory", s.Resources.Memory)
	}
	if s.Resources.PIDs > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(s.Resources.PIDs))
	}

	args = append(args, s.Image)
	// D5: PID 1 does nothing. Agents are started later in tmux, so the
	// container's lifetime is not tied to any one agent process.
	args = append(args, "sleep", "infinity")
	return args
}

func execArgs(id string, cmd []string, o ExecOpts) []string {
	args := []string{"exec"}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.Stdin != "" {
		args = append(args, "-i")
	}
	if o.Workdir != "" {
		args = append(args, "--workdir", o.Workdir)
	}
	if o.User != "" {
		args = append(args, "--user", o.User)
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	args = append(args, id)
	return append(args, cmd...)
}

func commitArgs(id, imageRef string) []string {
	// §6.2 pauses the container first and keeps it paused while volumes are
	// archived. `docker commit` pauses and unpauses by default, which would
	// let the container run again mid-snapshot and make the volume archives
	// inconsistent with the rootfs.
	return []string{"commit", "--pause=false", id, imageRef}
}

// VolumeName builds a deterministic, per-container volume name. Determinism
// matters because fork/stack must be able to name the volumes of a container
// it is copying from.
var volumeUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func VolumeName(project, slug, declared string) string {
	clean := func(s string) string {
		return strings.Trim(volumeUnsafe.ReplaceAllString(s, "-"), "-")
	}
	return fmt.Sprintf("aurium-%s-%s-%s", clean(project), clean(slug), clean(declared))
}

// parsePorts reads `docker inspect --format {{json .NetworkSettings.Ports}}`.
// Ports with no published binding come back as null and are skipped.
func parsePorts(raw string) (map[int]int, error) {
	var m map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("driver/docker: parse ports %q: %w", raw, err)
	}

	out := make(map[int]int, len(m))
	for spec, bindings := range m {
		if len(bindings) == 0 {
			continue
		}
		internal, err := strconv.Atoi(strings.SplitN(spec, "/", 2)[0])
		if err != nil {
			continue
		}
		host, err := strconv.Atoi(bindings[0].HostPort)
		if err != nil {
			continue
		}
		out[internal] = host
	}
	return out, nil
}

// ---- Driver implementation ----

func (d *Docker) Create(ctx context.Context, s Spec) (string, error) {
	out, err := d.run(ctx, createArgs(s)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (d *Docker) Start(ctx context.Context, id string) error {
	_, err := d.run(ctx, "start", id)
	return err
}

func (d *Docker) Stop(ctx context.Context, id string) error {
	_, err := d.run(ctx, "stop", id)
	return err
}

func (d *Docker) Pause(ctx context.Context, id string) error {
	_, err := d.run(ctx, "pause", id)
	return err
}

func (d *Docker) Unpause(ctx context.Context, id string) error {
	_, err := d.run(ctx, "unpause", id)
	return err
}

func (d *Docker) Destroy(ctx context.Context, id string, keepVolumes bool) error {
	args := []string{"rm", "--force"}
	if !keepVolumes {
		// -v removes anonymous volumes only; named per-container volumes are
		// removed explicitly by the caller, which knows which are shared.
		args = append(args, "--volumes")
	}
	args = append(args, id)

	_, err := d.run(ctx, args...)
	if err != nil && isNoSuchContainer(err) {
		return nil // idempotent: cleanup runs after crashes
	}
	return err
}

func (d *Docker) Exec(ctx context.Context, id string, cmd []string, o ExecOpts) (ExecResult, error) {
	proc := exec.CommandContext(ctx, d.bin, execArgs(id, cmd, o)...)
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
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("driver/docker: exec %v: %w", cmd, err)
	}
	return res, nil
}

func (d *Docker) Ports(ctx context.Context, id string) (map[int]int, error) {
	out, err := d.run(ctx, "inspect", "--format", "{{json .NetworkSettings.Ports}}", id)
	if err != nil {
		return nil, err
	}
	return parsePorts(strings.TrimSpace(out))
}

func (d *Docker) Inspect(ctx context.Context, id string) (State, error) {
	out, err := d.run(ctx, "inspect", "--format",
		"{{.State.Running}}|{{.State.Paused}}|{{.State.ExitCode}}|{{.Config.Image}}", id)
	if err != nil {
		if isNoSuchContainer(err) {
			return State{}, fmt.Errorf("driver/docker: %s: %w", id, ErrNotFound)
		}
		return State{}, err
	}

	parts := strings.SplitN(strings.TrimSpace(out), "|", 4)
	if len(parts) != 4 {
		return State{}, fmt.Errorf("driver/docker: unexpected inspect output %q", out)
	}
	code, _ := strconv.Atoi(parts[2])
	return State{
		ID:       id,
		Running:  parts[0] == "true",
		Paused:   parts[1] == "true",
		ExitCode: code,
		Image:    parts[3],
	}, nil
}

func (d *Docker) List(ctx context.Context, labels map[string]string) ([]Handle, error) {
	args := []string{"ps", "--all", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}\t{{.Labels}}"}
	for k, v := range labels {
		args = append(args, "--filter", "label="+k+"="+v)
	}
	out, err := d.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var handles []Handle
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 {
			continue
		}
		h := Handle{ID: fields[0], Name: fields[1], Labels: map[string]string{}}
		if len(fields) == 3 {
			for _, kv := range strings.Split(fields[2], ",") {
				if k, v, ok := strings.Cut(kv, "="); ok {
					h.Labels[k] = v
				}
			}
		}
		handles = append(handles, h)
	}
	return handles, nil
}

func (d *Docker) Snapshot(ctx context.Context, id, imageRef string) (RootfsRef, error) {
	if _, err := d.run(ctx, commitArgs(id, imageRef)...); err != nil {
		return RootfsRef{}, err
	}
	digest, err := d.run(ctx, "image", "inspect", "--format", "{{.Id}}", imageRef)
	if err != nil {
		return RootfsRef{}, err
	}
	return RootfsRef{Image: imageRef, Digest: strings.TrimSpace(digest)}, nil
}

func (d *Docker) Restore(ctx context.Context, s Spec, from RootfsRef) (string, error) {
	s.Image = from.Image // L2 becomes the snapshot; the container gets a fresh L3
	return d.Create(ctx, s)
}

// CloneVolumes copies per-container volume contents through a throwaway
// container. Volumes are not copy-on-write on macOS (§1.1 finding 2), so this
// is a real byte copy and its cost is proportional to volume size.
func (d *Docker) CloneVolumes(ctx context.Context, from, to []VolumeMount) error {
	if len(from) != len(to) {
		return fmt.Errorf("driver/docker: clone volumes: %d sources but %d destinations", len(from), len(to))
	}
	for i, src := range from {
		if src.Shared {
			continue // shared caches are mounted, never copied
		}
		dst := to[i]
		if _, err := d.run(ctx, "volume", "create", dst.Name); err != nil {
			return err
		}
		_, err := d.run(ctx, "run", "--rm",
			"-v", src.Name+":/from:ro",
			"-v", dst.Name+":/to",
			"alpine", "sh", "-c", "cd /from && cp -a . /to/")
		if err != nil {
			return fmt.Errorf("driver/docker: clone volume %s -> %s: %w", src.Name, dst.Name, err)
		}
	}
	return nil
}

func (d *Docker) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := d.run(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	_, err := d.run(ctx, "network", "create", name)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	_, err := d.run(ctx, "network", "rm", name)
	if err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "No such")) {
		return nil
	}
	return err
}

func (d *Docker) run(ctx context.Context, args ...string) (string, error) {
	if d.Verbose {
		fmt.Fprintf(os.Stderr, "+ %s %s\n", d.bin, strings.Join(args, " "))
	}
	cmd := exec.CommandContext(ctx, d.bin, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("driver/docker: %s %s: %s: %w",
			d.bin, strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

func isNoSuchContainer(err error) bool {
	s := err.Error()
	return strings.Contains(s, "No such container") ||
		strings.Contains(s, "no such container") ||
		strings.Contains(s, "is not running")
}

// sortedLabels renders labels as "k=v" strings in key order. Go map iteration
// is deliberately randomised, so without sorting the argv would differ between
// runs, making the -v output undiffable and any argv assertion flaky.
func sortedLabels(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
