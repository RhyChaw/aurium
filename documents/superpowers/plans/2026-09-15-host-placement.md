# Host Agent Placement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an agent's process run on the host at ~450 MB instead of inside a 2 GB container, with its shell commands routed back into the container it already has.

**Architecture:** Placement is an axis orthogonal to the driver. The container stays Docker; only the agent process moves. A headless turn runs in the container today purely because `converse.go` calls `drv.Exec` — placement chooses between that and a host process. The agent's built-in shell is denied by argv flags, and a new `aurium_exec` gateway tool runs commands back inside the container.

**Tech Stack:** Go 1.25+ (pinned go1.27.1), stdlib only, `CGO_ENABLED=0`.

**Spec:** `documents/superpowers/specs/2026-09-15-host-placement-design.md`

## Global Constraints

- Module path `github.com/RhyChaw/aurium`. **No new third-party dependencies** — stdlib only.
- Go floor 1.25; pinned toolchain go1.27.1; all builds `CGO_ENABLED=0`.
- **Adds no driver and changes no schema.** `Driver` is container lifecycle; this touches none of it.
- `in-container` remains the default. An existing project's behaviour must not change unless its `aurium.yaml` opts in.
- The worktree is bind-mounted at **identical paths on both sides** (D3), so `ExecOpts.Workdir` means the same thing on the host and in the container. Never translate paths.
- Both placements must produce the same `driver.ExecResult` shape, so nothing downstream — token counts, transcript rows, error text — can tell them apart except where the spec says it should.

## Deviation from the spec, decided while planning

The spec says `aurium_exec` is "subject to the existing risk classification, so a destructive verb in a command is held for approval". **Not in this plan.** `ClassifyRisk` splits *identifiers* (`getPullRequest` → get, pull, request) and matches whole words against verb lists. Fed a shell string, `rm -rf /` yields the words `rm`, `rf` — neither is in `highVerbs`, which contains `remove`, not `rm`. It would return low risk for the most destructive command a user can type.

Shipping that would be worse than shipping nothing, because it would look like a gate. So in this plan every `aurium_exec` call is **recorded and visible** — which is already more than `Bash` inside a container ever was — and verb-gating of free-form shell is deferred to its own change with a heuristic designed for shell, not for tool names.

---

### Task 1: Placement in config

**Files:**
- Modify: `internal/config/config.go` (the `Sandbox` struct and its defaulting)
- Modify: `internal/config/template.go` (document the new key)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.PlacementInContainer` and `config.PlacementHost` constants; `Sandbox.AgentPlacement string` with yaml tag `agent_placement`.

- [ ] **Step 1: Write the failing test**

```go
func TestAgentPlacementDefaultsToInContainer(t *testing.T) {
	var c Config
	c.Normalize()
	if c.Sandbox.AgentPlacement != PlacementInContainer {
		t.Fatalf("an unset placement must default to %q, got %q",
			PlacementInContainer, c.Sandbox.AgentPlacement)
	}
}

func TestAgentPlacementRejectsAnUnknownValue(t *testing.T) {
	c := Config{Sandbox: Sandbox{AgentPlacement: "somewhere-else"}}
	c.Normalize()
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown placement must be rejected, not silently accepted")
	}
}

func TestAgentPlacementAcceptsHost(t *testing.T) {
	c := Config{Sandbox: Sandbox{AgentPlacement: PlacementHost}}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatalf("host is a valid placement: %v", err)
	}
}
```

Match `Normalize`/`Validate` to whatever the file actually calls them; read it before writing. If validation lives elsewhere, put the check where the other `Sandbox` fields are checked and adjust the test to match.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestAgentPlacement -v`
Expected: FAIL — `undefined: PlacementInContainer`

- [ ] **Step 3: Write minimal implementation**

```go
// Agent placement decides where the agent's own process runs. It is not the
// driver: the container is unchanged either way, and still runs the agent's
// commands. Only the process that talks to the model moves.
const (
	// PlacementInContainer runs the agent inside its container, as Aurium
	// always has. The container must then be sized for a working agent.
	PlacementInContainer = "in-container"
	// PlacementHost runs the agent on this machine, at roughly a fifth of the
	// memory, with its shell denied and its commands routed back into the
	// container through aurium_exec.
	PlacementHost = "host"
)
```

Add to `Sandbox`: `AgentPlacement string \`yaml:"agent_placement"\`` — placed directly after `Agent`, since they are read together.

In the defaulting function, beside the other defaults:

```go
if c.Sandbox.AgentPlacement == "" {
	c.Sandbox.AgentPlacement = PlacementInContainer
}
```

In validation:

```go
switch c.Sandbox.AgentPlacement {
case PlacementInContainer, PlacementHost:
default:
	return fmt.Errorf("config: sandbox.agent_placement is %q; it must be %q or %q",
		c.Sandbox.AgentPlacement, PlacementInContainer, PlacementHost)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Add the key to the template**

In `internal/config/template.go`, after the `agent:` line:

```
  # Where the agent's own process runs. "in-container" is the default and
  # sizes the container for a working agent. "host" runs it on your machine
  # at roughly a fifth of the memory, denies its built-in shell, and routes
  # its commands back into this container.
  agent_placement: in-container
```

- [ ] **Step 6: Commit**

```bash
git add internal/config/
git commit -m "feat(config): agent placement, in-container by default"
```

---

### Task 2: Running a turn on the host

**Files:**
- Create: `internal/runtime/hostexec.go`
- Test: `internal/runtime/hostexec_test.go`

**Interfaces:**
- Consumes: `driver.ExecOpts{Workdir, Env, User, Stdin, TTY}`, `driver.ExecResult{Stdout, Stderr, ExitCode}`.
- Produces: `runHost(ctx context.Context, cmd []string, o driver.ExecOpts) (driver.ExecResult, error)`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestRunHost -v`
Expected: FAIL — `undefined: runHost`

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runtime/ -run TestRunHost -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/hostexec.go internal/runtime/hostexec_test.go
git commit -m "feat(runtime): run an agent turn on the host, same result shape"
```

---

### Task 3: The turn chooses its placement

**Files:**
- Modify: `internal/runtime/converse.go` (the `drv.Exec` call around line 147)
- Test: `internal/runtime/hostexec_test.go` (append)

**Interfaces:**
- Consumes: `runHost` from Task 2; `config.PlacementHost` from Task 1.
- Produces: `execTurn(ctx context.Context, placement string, drv driver.Driver, runtimeID string, cmd []string, o driver.ExecOpts) (driver.ExecResult, error)`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestExecTurn -v`
Expected: FAIL — `undefined: execTurn`

- [ ] **Step 3: Write minimal implementation**

In `internal/runtime/hostexec.go`:

```go
// execTurn runs a turn where the project's placement says it should run.
//
// This one function is the whole of host placement. A turn runs inside the
// container today only because converse.go called drv.Exec directly; putting
// the choice here keeps it a setting rather than a second code path.
func execTurn(ctx context.Context, placement string, drv driver.Driver, runtimeID string,
	cmd []string, o driver.ExecOpts) (driver.ExecResult, error) {
	if placement == config.PlacementHost {
		return runHost(ctx, cmd, o)
	}
	return drv.Exec(ctx, runtimeID, cmd, o)
}
```

In `converse.go`, replace `res, err := drv.Exec(runCtx, c.RuntimeID, cmd, runOpts)` with:

```go
res, err := execTurn(runCtx, cfg.Sandbox.AgentPlacement, drv, c.RuntimeID, cmd, runOpts)
```

The config is already resolved in that function; read the surrounding code and use the variable that holds it rather than fetching it again.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runtime/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/
git commit -m "feat(runtime): a turn runs where its placement says"
```

---

### Task 4: The agent's shell is denied, and given somewhere else to go

**Files:**
- Modify: `internal/agent/adapter.go` (`ExecOpts`)
- Modify: `internal/agent/claude.go` (`HeadlessCommand`)
- Test: `internal/agent/claude_test.go`

**Interfaces:**
- Produces: `agent.ExecOpts` gains `HostSandboxed bool` and `MCPConfigPath string`; `HeadlessCommand` appends the denial flags when `HostSandboxed`.

- [ ] **Step 1: Write the failing test**

```go
func TestHeadlessCommandDeniesTheShellUnderHostPlacement(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json",
	})
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--disallowedTools Bash",
		"--allowedTools mcp__aurium__exec",
		"--mcp-config /tmp/mcp.json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q: %v", want, argv)
		}
	}
}

// In-container placement is the default and must be untouched: the container
// is the wall there, and denying the shell inside it would cripple the agent
// for no gain.
func TestHeadlessCommandLeavesTheShellAloneInAContainer(t *testing.T) {
	c := &Claude{}
	joined := strings.Join(c.HeadlessCommand("do the thing", ExecOpts{}), " ")
	if strings.Contains(joined, "disallowedTools") {
		t.Errorf("in-container placement must not deny Bash: %s", joined)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestHeadlessCommand -v`
Expected: FAIL — `unknown field HostSandboxed`

- [ ] **Step 3: Write minimal implementation**

Add to `agent.ExecOpts`:

```go
// HostSandboxed says this turn runs on the host rather than in its
// container. The agent's own shell is denied and its commands go back into
// the container through aurium_exec, so the container remains the only place
// project commands run.
HostSandboxed bool
// MCPConfigPath points at the JSON naming the daemon's MCP endpoint. Only
// read when HostSandboxed.
MCPConfigPath string
```

In `HeadlessCommand`, after the existing args are assembled and before the prompt is appended — read the function and keep the prompt last:

```go
if o.HostSandboxed {
	// Verified by spike before this was designed: the agent uses the MCP
	// tool unprompted once its own shell is gone, and degrades gracefully
	// rather than failing when no alternative exists at all.
	args = append(args,
		"--disallowedTools", "Bash",
		"--allowedTools", "mcp__aurium__exec",
		"--mcp-config", o.MCPConfigPath)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/
git commit -m "feat(agent): deny the shell under host placement, point it at aurium_exec"
```

---

### Task 5: `aurium_exec`

**Files:**
- Modify: `internal/gateway/gateway.go` (an `Executor` interface beside `Delegator`)
- Modify: `internal/gateway/native.go` (declare the tool near `aurium_merge` at :180, dispatch near :412)
- Test: `internal/gateway/native_test.go`

**Interfaces:**
- Produces: `gateway.Executor` with `ExecInContainer(ctx context.Context, containerID string, command string) (stdout string, exitCode int, err error)`; the `aurium_exec` tool.

- [ ] **Step 1: Write the failing test**

```go
type fakeExecutor struct{ got string }

func (f *fakeExecutor) ExecInContainer(ctx context.Context, containerID, command string) (string, int, error) {
	f.got = command
	return "ok\n", 0, nil
}

func TestAuriumExecRunsInTheCallersContainer(t *testing.T) {
	f := &fakeExecutor{}
	g := newTestGateway(t)
	g.Executor = f

	res, err := g.callTool(t, "aurium_exec", map[string]any{"command": "go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if f.got != "go test ./..." {
		t.Errorf("command not passed through: %q", f.got)
	}
	if !strings.Contains(resultText(res), "ok") {
		t.Errorf("output not returned: %v", res)
	}
}

// Without an executor the tool must say so, not pretend to have run.
func TestAuriumExecWithoutAnExecutorSaysSo(t *testing.T) {
	g := newTestGateway(t)
	g.Executor = nil
	res, err := g.callTool(t, "aurium_exec", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "not available") {
		t.Errorf("want an explicit unavailable result, got %v", res)
	}
}
```

Use whatever harness `internal/gateway/native_test.go` already provides for building a gateway and calling a tool; do not invent a second one. Adjust `newTestGateway`/`callTool`/`resultText` to the real helper names.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestAuriumExec -v`
Expected: FAIL — `g.Executor undefined`

- [ ] **Step 3: Write minimal implementation**

In `gateway.go`, beside `Delegator`:

```go
// Executor runs a command inside a container. It is an interface for the same
// reason Delegator is: internal/gateway must not import internal/runtime,
// which imports the gateway back.
type Executor interface {
	ExecInContainer(ctx context.Context, containerID string, command string) (stdout string, exitCode int, err error)
}
```

Add an `Executor Executor` field to the `Gateway` struct.

Declare the tool in `native.go` beside `aurium_merge`:

```go
{
	Name: "aurium_exec",
	Description: "Run a shell command inside this agent's container. Under host " +
		"placement the agent's own shell is unavailable, and this is where all " +
		"project commands run — the container has the project's toolchain, the " +
		"host does not.",
	InputSchema: mcp.Schema{
		Type: "object",
		Properties: map[string]mcp.Schema{
			"command": {Type: "string", Description: "The shell command to run."},
		},
		Required: []string{"command"},
	},
},
```

Match the exact schema-literal shape the neighbouring tools use; copy their structure rather than this sketch if it differs.

Dispatch, beside `aurium_merge`:

```go
case "aurium_exec":
	if g.Executor == nil {
		return mcp.ErrorResult("aurium_exec is not available in this configuration"), nil
	}
	command := argStr(args, "command")
	if strings.TrimSpace(command) == "" {
		return mcp.ErrorResult("command is required"), nil
	}
	out, code, err := g.Executor.ExecInContainer(ctx, caller.ContainerID, command)
	if err != nil {
		return mcp.ErrorResult("exec failed: " + err.Error()), nil
	}
	// Every command is recorded. This is already more than Bash inside a
	// container ever was, where Aurium saw nothing at all.
	return mcp.JSONResult(map[string]any{"stdout": out, "exit_code": code}), nil
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gateway/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/
git commit -m "feat(gateway): aurium_exec runs a command in the caller's container"
```

---

### Task 6: Wiring, honesty, and the paragraph that saves turns

**Files:**
- Modify: `internal/runtime/manager.go:536-546` (no tmux session under host placement)
- Modify: `internal/runtime/manager.go` or wherever the `Gateway` is constructed (set `Executor`)
- Modify: `internal/agent/claude.go` `Prepare` (the instruction file)
- Test: `internal/runtime/hostexec_test.go` (append)

**Interfaces:**
- Consumes: everything from Tasks 1-5.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestHostPlacement -v`
Expected: FAIL — `undefined: wantsTmuxSession`

- [ ] **Step 3: Write minimal implementation**

In `internal/runtime/hostexec.go`:

```go
// wantsTmuxSession reports whether to launch an interactive session.
//
// Two independent reasons not to, and both must hold to proceed: a driver
// without tmux has no container to run it in, and host placement has no
// container involved in the agent at all.
func wantsTmuxSession(placement string, caps driver.Caps) bool {
	return caps.Tmux && placement != config.PlacementHost
}
```

In `manager.go`, change `if drv.Capabilities().Tmux {` to `if wantsTmuxSession(cfg.Sandbox.AgentPlacement, drv.Capabilities()) {`, using the config variable already in scope.

Where the `Gateway` is constructed, set `Executor` to an adapter that calls `drv.Exec(ctx, runtimeID, []string{"sh", "-c", command}, driver.ExecOpts{Workdir: worktree})` for the given container. Follow how `Delegator` is wired and mirror it.

In `claude.go`'s `Prepare`, add to the instruction file, only under host placement:

```
Your shell runs inside this project's container, not on the host machine.
Use the aurium_exec tool for every command. The container has the project's
toolchain; the host may not have it at all. If a command reports that a tool
is missing, that is the container's environment telling you something true —
do not go looking for it elsewhere on the machine.
```

This paragraph exists because the spike showed an agent meeting `go: command not found` will otherwise spend turns searching `/usr/local/go/bin`, `/opt/homebrew/bin/go` and `~/go/bin` before concluding.

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, everything green

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/ internal/agent/
git commit -m "feat(runtime): wire host placement, and tell the agent where its shell is"
```

---

## Verification

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
```

Then a real turn end to end, on a project whose `aurium.yaml` sets
`agent_placement: host`: create a container, send the agent a message, and
confirm from `aurium events` that the turn ran and the transcript recorded it.
Confirm with `ps` that a `claude` process appeared on the host rather than in
the container, and that its RSS is in the hundreds of megabytes rather than
gigabytes — that number is the entire point of the change.

---

### Task 7: Snapshots stop promising what they no longer capture

**Files:**
- Modify: `internal/runtime/snapshot/engine.go` (the snapshot entry point)
- Test: `internal/runtime/snapshot/engine_test.go`

**Interfaces:**
- Consumes: `config.PlacementHost` from Task 1.

**Why this task exists.** The spec requires that host placement not claim a capability it half has, and nothing else in this plan delivers it. `--continue` resumes a conversation today because the transcript lives in `$HOME` inside the rootfs `docker commit` captures. Under host placement it lives in `~/.claude` on the machine, outside the rootfs — so a snapshot still captures source, rootfs and volumes correctly, and silently no longer captures the conversation.

`Caps` cannot express this: it is per-driver, and placement is not a driver. The same Docker driver captures the conversation under one placement and not under the other. So the honesty has to be attached to the snapshot itself.

A restore that silently starts a fresh conversation, when the user believes they restored one, is exactly the class of failure this project keeps finding: `doctor` saying `ok git`, a copy button that copies nothing, an `idle_pause_minutes` nothing implements.

- [ ] **Step 1: Write the failing test**

```go
func TestSnapshotRecordsThatHostPlacementOmitsTheConversation(t *testing.T) {
	snap := newTestSnapshot(t, config.PlacementHost)
	if snap.IncludesConversation {
		t.Error("under host placement the transcript is outside the rootfs and is not captured")
	}
	if snap.Note == "" {
		t.Error("a snapshot that omits the conversation must say so, not omit it silently")
	}
}

func TestSnapshotUnderInContainerStillCapturesTheConversation(t *testing.T) {
	snap := newTestSnapshot(t, config.PlacementInContainer)
	if !snap.IncludesConversation {
		t.Error("in-container placement keeps the transcript in the captured rootfs")
	}
}
```

Adapt `newTestSnapshot` to the package's existing harness and the real snapshot struct; read `engine_test.go` first. If snapshots have no note field, add `IncludesConversation bool` and `Note string` to the snapshot record and its store row, and assert on those.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/snapshot/ -run TestSnapshot -v`
Expected: FAIL — `undefined: IncludesConversation`

- [ ] **Step 3: Write minimal implementation**

Set the field where the snapshot record is built:

```go
// The transcript lives in $HOME inside the rootfs under in-container
// placement, so docker commit captures it and --continue resumes. Under host
// placement it lives in ~/.claude on the machine, outside the rootfs: source,
// rootfs and volumes are still captured exactly, and the conversation is not.
// Recording that is the difference between a restore that surprises someone
// and one that tells them what they are getting.
snap.IncludesConversation = placement != config.PlacementHost
if !snap.IncludesConversation {
	snap.Note = "conversation not captured: the agent runs on the host under " +
		"agent_placement: host, so its transcript is outside the container rootfs"
}
```

Surface `Note` wherever snapshots are listed — `aurium snapshot` output and the dashboard's snapshot list — so it is visible before a restore rather than after.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runtime/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/snapshot/
git commit -m "fix(snapshot): say when the conversation is not in the snapshot"
```
