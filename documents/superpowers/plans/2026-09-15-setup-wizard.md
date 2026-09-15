# Aurium Setup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take a fresh contributor from `git clone` to a running Aurium dashboard with one command, on a machine that has no Go toolchain.

**Architecture:** Two stages split at the only real seam — a Go binary cannot install Go. `setup.sh` (POSIX sh) does the pre-Go minimum: detect platform, install a checksum-verified Go into `~/.local/go`, build, hand off. Everything after that lives in a new `internal/preflight` package as a table of checks, which `aurium doctor`, `GET /v1/preflight`, and the dashboard's first-run wizard all render. One table, three renderers, no drift.

**Tech Stack:** Go 1.25+ (pinned toolchain go1.27.1), stdlib only, `net/http.ServeMux` routing, vanilla ES modules with no build step, POSIX `sh`.

**Spec:** `docs/superpowers/specs/2026-09-15-setup-wizard-design.md`

## Global Constraints

- Module path is `github.com/RhyChaw/aurium`.
- **No new third-party dependencies.** The module list is deliberately small; every check uses the standard library.
- Go floor is **1.25**; pinned toolchain is **go1.27.1** (`go.mod`).
- All Go builds use `CGO_ENABLED=0`. Aurium's SQLite is `modernc.org/sqlite` (pure Go), so no C compiler is required — this is what lets the build survive an unusable `clang`.
- Platform matrix is `darwin|linux` × `arm64|amd64`. No Windows, no PowerShell.
- **`setup.sh` never invokes `sudo`** and never writes outside `$HOME`. It must remain safe to pipe from `curl`.
- `setup.sh` must not require `make`, `git`, or `python3` — all three are disabled by an unaccepted Xcode licence, which is the exact condition it exists to survive.
- Probes judge a binary by **running** it, never by `exec.LookPath` alone.
- Every new route must be added to `api/openapi.yaml` or `TestOpenAPIMatchesRegisteredRoutes` fails.
- Every new dashboard module must be added to the `want` list in `TestEveryDashboardModuleIsEmbedded` or it ships as a blank page.

---

### Task 1: The preflight probe primitive

The regression test for the bug that motivated this whole plan: a binary that exists and fails.

**Files:**
- Create: `internal/preflight/preflight.go`
- Create: `internal/preflight/preflight_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `preflight.BinaryWorks(ctx context.Context, name string, args ...string) error`; types `Check`, `Result`, `Remedy`, `Severity`, `RemedyKind`; `preflight.Run(ctx context.Context, checks []Check) []Result`.

- [ ] **Step 1: Write the failing test**

```go
package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBin puts an executable shell script named name on a fresh PATH.
func writeFakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// The bug this package exists to fix. macOS ships /usr/bin/git as a shim that
// exists, is executable, and fails on every invocation until the Xcode licence
// is accepted. exec.LookPath calls that a pass.
func TestBinaryWorksRejectsAPresentButFailingBinary(t *testing.T) {
	writeFakeBin(t, "git", `echo "xcrun: error: You have not agreed to the Xcode license" >&2; exit 69`)

	err := BinaryWorks(context.Background(), "git", "--version")
	if err == nil {
		t.Fatal("a binary that exits 69 must not report ok")
	}
	if !strings.Contains(err.Error(), "Xcode license") {
		t.Errorf("error must carry the binary's own stderr, got: %v", err)
	}
}

func TestBinaryWorksAcceptsAWorkingBinary(t *testing.T) {
	writeFakeBin(t, "git", `echo "git version 2.54.0"; exit 0`)
	if err := BinaryWorks(context.Background(), "git", "--version"); err != nil {
		t.Fatalf("a binary that exits 0 must report ok, got: %v", err)
	}
}

func TestBinaryWorksReportsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := BinaryWorks(context.Background(), "definitely-not-here")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf(`missing binary must say "not installed", got: %v`, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/preflight/ -run TestBinaryWorks -v`
Expected: FAIL — `undefined: BinaryWorks`

- [ ] **Step 3: Write minimal implementation**

```go
// Package preflight answers one question — can this machine run Aurium — as
// data rather than as printed output, so the CLI, the API and the dashboard
// all render the same answer instead of keeping three opinions.
package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Severity string

const (
	Required Severity = "required"
	Optional Severity = "optional"
)

type RemedyKind string

const (
	NoRemedy RemedyKind = ""
	// Auto is unprivileged and idempotent. Anything needing sudo, a GUI
	// installer or a TTY is Manual, permanently.
	Auto   RemedyKind = "auto"
	Manual RemedyKind = "manual"
)

type Remedy struct {
	Kind    RemedyKind
	Command string                            // exact copy-paste command, Manual only
	Note    string                            // why it can't be automated
	Fix     func(context.Context) error       // non-nil only when Kind == Auto
}

type Check struct {
	Name     string
	Severity Severity
	Probe    func(context.Context) error
	Remedy   Remedy
}

type Result struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Severity   string `json:"severity"`
	Detail     string `json:"detail,omitempty"`
	Error      string `json:"error,omitempty"`
	Remedy     string `json:"remedy,omitempty"`
	RemedyKind string `json:"remedy_kind,omitempty"`
}

// Run executes every check in order and never stops early: a contributor
// wants the whole list of what is wrong, not the first thing that failed.
func Run(ctx context.Context, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := Result{Name: c.Name, Severity: string(c.Severity), OK: true}
		if err := c.Probe(ctx); err != nil {
			r.OK = false
			r.Error = err.Error()
			r.Remedy = c.Remedy.Command
			r.RemedyKind = string(c.Remedy.Kind)
		}
		out = append(out, r)
	}
	return out
}

// BinaryWorks reports whether name is on PATH *and* actually runs.
//
// exec.LookPath answers "is there a file with this name", which is a different
// question. macOS answers yes for /usr/bin/git on a machine where the Xcode
// licence has never been accepted and no git command can run; doctor printed
// `ok git` for exactly that machine while every git invocation failed.
func BinaryWorks(ctx context.Context, name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return errors.New("not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The binary's own first line of stderr is almost always the real
		// explanation, and is far more useful than "exit status 69".
		if line := firstLine(stderr.String()); line != "" {
			return errors.New(line)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/preflight/ -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/preflight/
git commit -m "feat(preflight): judge a binary by running it, not by finding it"
```

---

### Task 2: The check table

**Files:**
- Create: `internal/preflight/checks.go`
- Create: `internal/preflight/checks_test.go`

**Interfaces:**
- Consumes: `Check`, `Remedy`, `BinaryWorks`, `Severity` from Task 1.
- Produces: `preflight.Checks(home string, addr string) []Check`; `preflight.GoVersionAtLeast(out string, major, minor int) error`; `preflight.PortFree(addr string) error`.

- [ ] **Step 1: Write the failing test**

```go
package preflight

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestGoVersionAtLeastRejectsOldToolchain(t *testing.T) {
	if err := GoVersionAtLeast("go version go1.21.0 darwin/arm64", 1, 25); err == nil {
		t.Fatal("go1.21 must fail a 1.25 floor")
	}
}

func TestGoVersionAtLeastAcceptsPinnedToolchain(t *testing.T) {
	if err := GoVersionAtLeast("go version go1.27.1 darwin/arm64", 1, 25); err != nil {
		t.Fatalf("go1.27.1 must satisfy a 1.25 floor: %v", err)
	}
}

func TestGoVersionAtLeastRejectsUnparseable(t *testing.T) {
	if err := GoVersionAtLeast("something else entirely", 1, 25); err == nil {
		t.Fatal("unparseable output must fail rather than silently pass")
	}
}

// A port held by another process must be reported as held. The daemon
// suggested `aurium up --restart` for a listener owned by a different user,
// which cannot work: you cannot signal someone else's process.
func TestPortFreeDetectsAHeldPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := PortFree(ln.Addr().String()); err == nil {
		t.Fatal("a port with a live listener must not report free")
	}
}

func TestPortFreeAcceptsAFreePort(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	if err := PortFree(addr); err != nil {
		t.Fatalf("a closed port must report free: %v", err)
	}
}

func TestChecksCoverTheRequiredGround(t *testing.T) {
	names := map[string]bool{}
	for _, c := range Checks(t.TempDir(), "127.0.0.1:7770") {
		names[c.Name] = true
	}
	for _, want := range []string{"go", "git", "docker", "docker daemon", "tmux", "~/.aurium", "port"} {
		if !names[want] {
			t.Errorf("check %q is missing from the table", want)
		}
	}
}

// The API passes no address, because over HTTP "the port is in use" is not a
// finding — it is the daemon answering the request.
func TestChecksOmitsThePortCheckWithoutAnAddress(t *testing.T) {
	for _, c := range Checks(t.TempDir(), "") {
		if c.Name == "port" {
			t.Fatal("an empty addr must omit the port check")
		}
	}
}

func TestEveryAutoRemedyHasAFixAndEveryManualHasACommand(t *testing.T) {
	for _, c := range Checks(t.TempDir(), "127.0.0.1:7770") {
		switch c.Remedy.Kind {
		case Auto:
			if c.Remedy.Fix == nil {
				t.Errorf("%s: Auto remedy with no Fix function", c.Name)
			}
		case Manual:
			if strings.TrimSpace(c.Remedy.Command) == "" {
				t.Errorf("%s: Manual remedy with no command to copy", c.Name)
			}
		}
	}
}

var _ = context.Background
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/preflight/ -run 'TestGoVersion|TestPortFree|TestChecks|TestEveryAuto' -v`
Expected: FAIL — `undefined: GoVersionAtLeast`, `undefined: PortFree`, `undefined: Checks`

- [ ] **Step 3: Write minimal implementation**

```go
package preflight

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var goVersionRe = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

// GoVersionAtLeast parses `go version` output against a floor. Unparseable
// output is a failure: a toolchain we cannot identify is one we cannot vouch
// for, and guessing "probably fine" is how a broken build reaches a build log.
func GoVersionAtLeast(out string, major, minor int) error {
	m := goVersionRe.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("could not read a version from %q", strings.TrimSpace(out))
	}
	haveMajor, _ := strconv.Atoi(m[1])
	haveMinor, _ := strconv.Atoi(m[2])
	if haveMajor > major || (haveMajor == major && haveMinor >= minor) {
		return nil
	}
	return fmt.Errorf("go%d.%d is older than the required go%d.%d", haveMajor, haveMinor, major, minor)
}

// PortFree reports whether addr can be listened on, and when it cannot, who
// is holding it. The owner matters: `aurium up --restart` is good advice for
// your own stale daemon and useless for another user's live one.
func PortFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return nil
	}
	if owner := portOwner(addr); owner != "" {
		return fmt.Errorf("in use by %s", owner)
	}
	return fmt.Errorf("in use: %w", err)
}

// portOwner is best-effort and deliberately quiet on failure: a missing lsof
// must degrade to a vaguer message, never to an error of its own.
func portOwner(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-F", "un").Output()
	if err != nil {
		return ""
	}
	var user, pid string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid = line[1:]
		case 'u':
			user = line[1:]
		}
	}
	if user == "" {
		return ""
	}
	if u := os.Getenv("USER"); u != "" && u == user {
		return fmt.Sprintf("your own process (pid %s) — replace it with: aurium up --restart", pid)
	}
	return fmt.Sprintf("another user %q (pid %s) — that process is not yours to restart; use --addr to pick a free port", user, pid)
}

// Checks is the single list of what Aurium needs. doctor, GET /v1/preflight
// and the dashboard wizard all render this and nothing else.
//
// An empty addr omits the port check. Over HTTP it is meaningless — if a
// caller reached /v1/preflight then the port is held, by the daemon answering
// them — so the API passes "" rather than reporting a failure that is really
// proof of success.
func Checks(home, addr string) []Check {
	checks := []Check{
		{
			Name:     "go",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				out, err := exec.CommandContext(ctx, "go", "version").Output()
				if err != nil {
					return BinaryWorks(ctx, "go", "version")
				}
				return GoVersionAtLeast(string(out), 1, 25)
			},
			Remedy: Remedy{
				Kind:    Manual,
				Command: "./setup.sh",
				Note:    "installs a checksum-verified go1.27.1 into ~/.local/go without sudo",
			},
		},
		{
			Name:     "git",
			Severity: Required,
			Probe:    func(ctx context.Context) error { return BinaryWorks(ctx, "git", "--version") },
			Remedy:   gitRemedy(),
		},
		{
			Name:     "docker",
			Severity: Required,
			Probe:    func(ctx context.Context) error { return BinaryWorks(ctx, "docker", "--version") },
			Remedy: Remedy{
				Kind:    Manual,
				Command: "install Docker Desktop, OrbStack or podman",
				Note:    "a container runtime cannot be installed unprivileged",
			},
		},
		{
			Name:     "docker daemon",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				cmd := exec.CommandContext(ctx, "docker", "info")
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil {
					if line := firstLine(stderr.String()); line != "" {
						return fmt.Errorf("%s", line)
					}
					return err
				}
				return nil
			},
			Remedy: dockerDaemonRemedy(),
		},
		{
			Name:     "tmux",
			Severity: Optional,
			Probe:    func(ctx context.Context) error { return BinaryWorks(ctx, "tmux", "-V") },
			Remedy: Remedy{
				Kind:    Manual,
				Command: "brew install tmux   # or: apt-get install tmux",
				Note:    "host-side convenience only; containers get their own from the image",
			},
		},
		{
			Name:     "~/.aurium",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				_, err := os.Stat(home)
				return err
			},
			Remedy: Remedy{
				Kind: Auto,
				Fix:  func(ctx context.Context) error { return os.MkdirAll(home, 0o755) },
			},
		},
	}

	if addr != "" {
		checks = append(checks, Check{
			Name:     "port",
			Severity: Required,
			Probe:    func(ctx context.Context) error { return PortFree(addr) },
			Remedy: Remedy{
				Kind:    Manual,
				Command: "aurium up --addr 127.0.0.1:7771",
				Note:    "or stop whatever holds the address; the probe names the owner",
			},
		})
	}
	return checks
}

func gitRemedy() Remedy {
	if runtime.GOOS == "darwin" {
		// The overwhelmingly common macOS cause: git is present and refuses to
		// run. Needs a TTY for the password, so setup.sh cannot do it.
		return Remedy{
			Kind:    Manual,
			Command: "sudo xcodebuild -license accept",
			Note:    "run this in a real terminal — sudo needs a TTY to read a password",
		}
	}
	return Remedy{Kind: Manual, Command: "apt-get install git   # or your distro's equivalent"}
}

func dockerDaemonRemedy() Remedy {
	if runtime.GOOS == "darwin" {
		return Remedy{
			Kind: Auto,
			Fix: func(ctx context.Context) error {
				return exec.CommandContext(ctx, "open", "-a", "Docker").Run()
			},
		}
	}
	return Remedy{
		Kind:    Manual,
		Command: "sudo systemctl start docker",
		Note:    "starting a system service requires privileges setup will not take",
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/preflight/ -v`
Expected: PASS (all tests)

- [ ] **Step 5: Commit**

```bash
git add internal/preflight/
git commit -m "feat(preflight): one table of what Aurium needs, with owners for held ports"
```

---

### Task 3: doctor renders the table

**Files:**
- Modify: `internal/cli/misc.go:306-393` — replace the body of `newDoctorCmd`, delete `binaryExists` and `dockerRunning`
- Create: `internal/cli/doctor_test.go`

**Interfaces:**
- Consumes: `preflight.Checks`, `preflight.Run`, `preflight.Result` from Tasks 1-2.
- Produces: `aurium doctor` and `aurium doctor --json`.

- [ ] **Step 1: Write the failing test**

```go
package cli

import (
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/preflight"
)

func TestRenderDoctorMarksFailuresAndPrintsTheRemedy(t *testing.T) {
	var sb strings.Builder
	results := []preflight.Result{
		{Name: "go", OK: true, Severity: "required"},
		{Name: "git", OK: false, Severity: "required",
			Error: "xcrun: error: You have not agreed to the Xcode license",
			Remedy: "sudo xcodebuild -license accept", RemedyKind: "manual"},
		{Name: "tmux", OK: false, Severity: "optional", Error: "not installed"},
	}

	ok := renderDoctor(&sb, results)
	out := sb.String()

	if ok {
		t.Error("a failed required check must make the run not-ok")
	}
	if !strings.Contains(out, "ok    go") {
		t.Errorf("passing check missing from output:\n%s", out)
	}
	if !strings.Contains(out, "Xcode license") {
		t.Errorf("the probe's own error must be shown:\n%s", out)
	}
	if !strings.Contains(out, "sudo xcodebuild -license accept") {
		t.Errorf("the remedy must be printed verbatim:\n%s", out)
	}
}

// An optional check failing is not a reason to fail the command.
func TestRenderDoctorOptionalFailureStaysOK(t *testing.T) {
	var sb strings.Builder
	ok := renderDoctor(&sb, []preflight.Result{
		{Name: "tmux", OK: false, Severity: "optional", Error: "not installed"},
	})
	if !ok {
		t.Error("an optional failure must not fail the command")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRenderDoctor -v`
Expected: FAIL — `undefined: renderDoctor`

- [ ] **Step 3: Write minimal implementation**

Add to `internal/cli/misc.go`, and rewrite `newDoctorCmd` to call it:

```go
// renderDoctor prints results and reports whether the machine is usable.
// Optional failures are printed but do not fail the command.
func renderDoctor(w io.Writer, results []preflight.Result) bool {
	ok := true
	for _, r := range results {
		switch {
		case r.OK:
			fmt.Fprintf(w, "  ok    %s\n", r.Name)
		case r.Severity == string(preflight.Optional):
			fmt.Fprintf(w, "  warn  %s: %s\n", r.Name, r.Error)
		default:
			ok = false
			fmt.Fprintf(w, "  FAIL  %s: %s\n", r.Name, r.Error)
		}
		if !r.OK && r.Remedy != "" {
			fmt.Fprintf(w, "        fix: %s\n", r.Remedy)
		}
	}
	return ok
}
```

Replace the check-running half of `newDoctorCmd` with:

```go
home, _ := app.Home()
results := preflight.Run(cmd.Context(), preflight.Checks(home, addr))

if jsonOut {
    return json.NewEncoder(cmd.OutOrStdout()).Encode(results)
}
fmt.Println("Aurium doctor")
ok := renderDoctor(cmd.OutOrStdout(), results)
```

Keep the existing project/database/adapter reporting below it unchanged. Delete `binaryExists` and `dockerRunning` — `preflight` owns both now. Add both flags, and the `io` and `encoding/json` imports:

```go
var (
    jsonOut bool
    addr    string
)
cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
// doctor checks the address the daemon would bind, so it has to know it. The
// default matches `aurium up`.
cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7770", "loopback address to check")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -v && go build ./... && ./bin/aurium doctor`
Expected: PASS; `doctor` now reports `go` and `port`, and reports git accurately.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "fix(doctor): report what actually runs, and stop lying about git"
```

---

### Task 4: `doctor --fix`

**Files:**
- Modify: `internal/cli/misc.go` — add the `--fix` flag and the remediation loop
- Modify: `internal/preflight/preflight.go` — add `Fix`
- Modify: `internal/preflight/preflight_test.go` — add the fix tests

**Interfaces:**
- Consumes: `Check`, `Result`, `Run` from Task 1.
- Produces: `preflight.Fix(ctx context.Context, checks []Check) []Result` — applies every `Auto` remedy whose probe failed, then re-probes.

- [ ] **Step 1: Write the failing test**

```go
func TestFixAppliesAutoRemediesAndRerunsTheProbe(t *testing.T) {
	fixed := false
	checks := []Check{{
		Name:     "creatable",
		Severity: Required,
		Probe: func(ctx context.Context) error {
			if fixed {
				return nil
			}
			return errors.New("missing")
		},
		Remedy: Remedy{Kind: Auto, Fix: func(ctx context.Context) error { fixed = true; return nil }},
	}}

	results := Fix(context.Background(), checks)
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("an applied Auto remedy must leave the check passing: %+v", results)
	}
}

// A Manual remedy must never be executed on the user's behalf.
func TestFixNeverRunsManualRemedies(t *testing.T) {
	ran := false
	checks := []Check{{
		Name:     "privileged",
		Severity: Required,
		Probe:    func(ctx context.Context) error { return errors.New("blocked") },
		Remedy: Remedy{
			Kind:    Manual,
			Command: "sudo xcodebuild -license accept",
			Fix:     func(ctx context.Context) error { ran = true; return nil },
		},
	}}

	results := Fix(context.Background(), checks)
	if ran {
		t.Error("Fix must never execute a Manual remedy")
	}
	if results[0].OK {
		t.Error("a Manual-remedy failure must stay failed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/preflight/ -run TestFix -v`
Expected: FAIL — `undefined: Fix`

- [ ] **Step 3: Write minimal implementation**

```go
// Fix applies every Auto remedy whose check is failing, then re-probes. Manual
// remedies are never executed: they are the ones that need sudo, a GUI or a
// TTY, and a setup tool that escalates on your behalf is not one you should
// pipe from curl.
func Fix(ctx context.Context, checks []Check) []Result {
	for _, c := range checks {
		if c.Remedy.Kind != Auto || c.Remedy.Fix == nil {
			continue
		}
		if c.Probe(ctx) == nil {
			continue
		}
		_ = c.Remedy.Fix(ctx) // a failed fix simply leaves the re-probe failing
	}
	return Run(ctx, checks)
}
```

Wire `--fix` in `newDoctorCmd`: when set, call `preflight.Fix` instead of `preflight.Run`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/preflight/ ./internal/cli/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/preflight/ internal/cli/
git commit -m "feat(doctor): --fix applies what it can and never escalates"
```

---

### Task 5: `GET /v1/preflight`

**Files:**
- Modify: `internal/api/server.go:82-137` — register the route
- Create: `internal/api/preflight.go`
- Modify: `api/openapi.yaml` — document it or `TestOpenAPIMatchesRegisteredRoutes` fails
- Create: `internal/api/preflight_test.go`

**Interfaces:**
- Consumes: `preflight.Checks`, `preflight.Run`.
- Produces: `GET /v1/preflight` → `{"checks": [Result...]}`.

- [ ] **Step 1: Write the failing test**

```go
package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPreflightReturnsTheCheckTable(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/v1/preflight")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		Checks []struct {
			Name     string `json:"name"`
			OK       bool   `json:"ok"`
			Severity string `json:"severity"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Checks) == 0 {
		t.Fatal("preflight must return checks")
	}
	for _, c := range body.Checks {
		if c.Name == "" || c.Severity == "" {
			t.Errorf("every check needs a name and a severity: %+v", c)
		}
	}
}
```

Match `newHarness` and its request helper to whatever `internal/api/server_test.go` already defines; do not invent a second harness.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestPreflight -v`
Expected: FAIL — 404, because the route is not registered

- [ ] **Step 3: Write minimal implementation**

`internal/api/preflight.go`:

```go
package api

import (
	"net/http"

	"github.com/RhyChaw/aurium/internal/preflight"
)

// The dashboard's first-run wizard renders exactly what `aurium doctor`
// renders, because both read this one table.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	home, _ := app.Home()
	// No address: see the note on Checks. Server has no Addr field, and a port
	// check here would report the caller's own daemon as a failure.
	results := preflight.Run(r.Context(), preflight.Checks(home, ""))
	writeJSON(w, http.StatusOK, map[string]any{"checks": results})
}
```

Register in `routes()` beside the other host-only routes:

```go
s.handle("GET /v1/preflight", s.preflight, "")
```

Then add to `api/openapi.yaml`, following the existing entries' shape:

```yaml
  /v1/preflight:
    get:
      summary: Whether this machine can run Aurium
      responses:
        "200":
          description: The preflight check table
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -v`
Expected: PASS, including `TestOpenAPIMatchesRegisteredRoutes`

- [ ] **Step 5: Commit**

```bash
git add internal/api/ api/openapi.yaml
git commit -m "feat(api): serve the preflight table the CLI already renders"
```

---

### Task 6: `setup.sh`

**Files:**
- Create: `setup.sh` (repo root, mode 0755)
- Create: `go-checksums.txt` (repo root)
- Modify: `Makefile` — add a `setup` target that delegates to the script
- Modify: `.github/workflows/ci.yml` — add shellcheck and a bare-container run
- Modify: `README.md` — make `./setup.sh` the first instruction under "Run it"

**Interfaces:**
- Consumes: `aurium doctor --fix` from Task 4.
- Produces: `./setup.sh`, exit 0 on a ready machine.

- [ ] **Step 1: Write the failing test**

`.github/workflows/ci.yml` gains a job that is the real test — a machine with no Go:

```yaml
  setup-from-bare:
    runs-on: ubuntu-latest
    container: ubuntu:24.04
    steps:
      - run: apt-get update && apt-get install -y curl ca-certificates
      - uses: actions/checkout@v4
      - run: sh -n setup.sh
      - run: ./setup.sh
      - run: ./bin/aurium doctor --json
```

- [ ] **Step 2: Run to verify it fails**

Run: `shellcheck setup.sh`
Expected: FAIL — `setup.sh: No such file or directory`

- [ ] **Step 3: Write minimal implementation**

`go-checksums.txt` — real SHA256s for the pinned toolchain, verified against go.dev:

```
go1.27.1.darwin-amd64.tar.gz 8f8f52c6649542cf027bbc9b9c68d1ec042f9f34808a40413f0b8b3f66f3caa4
go1.27.1.darwin-arm64.tar.gz ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12
go1.27.1.linux-amd64.tar.gz 63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
go1.27.1.linux-arm64.tar.gz 3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec
```

Regenerate these when the pin moves:

```bash
curl -s 'https://go.dev/dl/?mode=json&include=all' | \
  python3 -c 'import json,sys;[print(f["filename"],f["sha256"]) for r in json.load(sys.stdin) if r["version"]=="go1.27.1" for f in r["files"] if f["kind"]=="archive" and (f["os"],f["arch"]) in {("darwin","arm64"),("darwin","amd64"),("linux","arm64"),("linux","amd64")}]'
```

`setup.sh`:

```sh
#!/bin/sh
# Aurium setup — clone to running dashboard, one command.
#
# Deliberately POSIX sh with no make, no git and no python: on macOS all three
# are disabled until the Xcode licence is accepted, which is one of the exact
# conditions this script exists to survive. It never uses sudo and never writes
# outside $HOME, so piping it from curl is a defensible thing to ask of anyone.
set -eu

GO_VERSION=go1.27.1
GO_FLOOR_MINOR=25
PREFIX="${HOME}/.local"
REPO_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

detect_platform() {
	case $(uname -s) in
		Darwin) OS=darwin ;;
		Linux)  OS=linux ;;
		*) die "unsupported OS $(uname -s); this script covers macOS and Linux" ;;
	esac
	case $(uname -m) in
		arm64|aarch64) ARCH=arm64 ;;
		x86_64|amd64)  ARCH=amd64 ;;
		*) die "unsupported architecture $(uname -m)" ;;
	esac
}

# Judge go by running it. A binary that exists and fails is the failure mode
# this whole script is built around.
go_is_usable() {
	command -v go >/dev/null 2>&1 || return 1
	v=$(go version 2>/dev/null) || return 1
	minor=$(printf '%s' "$v" | sed -n 's/.*go1\.\([0-9][0-9]*\).*/\1/p')
	[ -n "$minor" ] && [ "$minor" -ge "$GO_FLOOR_MINOR" ]
}

install_go() {
	tarball="${GO_VERSION}.${OS}-${ARCH}.tar.gz"
	want=$(awk -v f="$tarball" '$1==f {print $2}' "${REPO_DIR}/go-checksums.txt")
	[ -n "$want" ] || die "no checksum recorded for ${tarball}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	say "installing ${GO_VERSION} for ${OS}/${ARCH} into ${PREFIX}/go"
	curl -fL --retry 1 -o "${tmp}/go.tar.gz" "https://go.dev/dl/${tarball}" ||
		die "download failed; fetch https://go.dev/dl/${tarball} by hand and extract it to ${PREFIX}/go"

	if command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "${tmp}/go.tar.gz" | awk '{print $1}')
	else
		got=$(sha256sum "${tmp}/go.tar.gz" | awk '{print $1}')
	fi
	# Never retried: a mismatch means the bytes are not the bytes we pinned.
	[ "$got" = "$want" ] || die "checksum mismatch for ${tarball}: got ${got}, want ${want}"

	mkdir -p "$PREFIX"
	rm -rf "${PREFIX}/go"
	tar -C "$PREFIX" -xzf "${tmp}/go.tar.gz"
}

detect_platform

if go_is_usable; then
	GO=go
else
	if [ ! -x "${PREFIX}/go/bin/go" ]; then
		install_go
	fi
	GO="${PREFIX}/go/bin/go"
fi
say "using $("$GO" version)"

# CGO off because Aurium's SQLite (modernc.org/sqlite) is pure Go — so an
# unusable clang cannot stop the build. -buildvcs=false because git may be
# present and refuse to run, and VCS stamping reports that as "exit status 69".
export CGO_ENABLED=0
BUILD="$GO build -buildvcs=false"

cd "$REPO_DIR"
say "building"
for c in aurium auriumd aurium-mcp; do
	$BUILD -o "bin/${c}" "./cmd/${c}"
done
for a in amd64 arm64; do
	GOOS=linux GOARCH="$a" $BUILD -o "bin/linux-${a}/aurium-mcp" ./cmd/aurium-mcp
done
mkdir -p "${HOME}/.aurium/bin"
cp bin/linux-amd64/aurium-mcp "${HOME}/.aurium/bin/aurium-mcp-linux-amd64"
cp bin/linux-arm64/aurium-mcp "${HOME}/.aurium/bin/aurium-mcp-linux-arm64"

say ""
exec ./bin/aurium doctor --fix
```

`Makefile` — the script is the source of truth, so the target delegates to it:

```make
# One command from a clean clone. Delegates to setup.sh rather than the other
# way round: make is one of the tools an unaccepted Xcode licence disables, so
# the shell path has to work without it.
.PHONY: setup
setup:
	./setup.sh
```

`.github/workflows/ci.yml` — add shellcheck to the existing `build` job and the `setup-from-bare` job from Step 1, plus `macos-latest` to the matrix.

- [ ] **Step 4: Run to verify it passes**

Run: `chmod +x setup.sh && shellcheck setup.sh && sh -n setup.sh && ./setup.sh`
Expected: shellcheck clean; the script builds and hands off to `doctor --fix`.

- [ ] **Step 5: Commit**

```bash
git add setup.sh go-checksums.txt Makefile .github/workflows/ci.yml README.md
git commit -m "feat(setup): one command from a clean clone, no sudo, no make"
```

---

### Task 7: The first-run wizard

**Files:**
- Create: `internal/api/web/views/setup.js`
- Modify: `internal/api/web/app.js:28-40` — add the panel and route to it on an empty daemon
- Modify: `internal/api/web/lib/api.js` — add the `preflight()` call
- Modify: `internal/api/dashboard_test.go:21-27` — add `views/setup.js` to `want`
- Modify: `internal/api/web/style.css` — styles for the step list

**Interfaces:**
- Consumes: `GET /v1/preflight` (Task 5); existing `renderProviders`, `reloadProviders`, `renderHome`.
- Produces: `renderSetup(host)`, `reloadPreflight()` exported from `views/setup.js`.

- [ ] **Step 1: Write the failing test**

```go
// A view missing from the embed list is a blank page with one console error,
// which is exactly what this test exists to make loud.
func TestSetupViewIsEmbedded(t *testing.T) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(sub, "views/setup.js"); err != nil {
		t.Errorf("views/setup.js is not embedded: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestSetupViewIsEmbedded -v`
Expected: FAIL — `views/setup.js is not embedded: file does not exist`

- [ ] **Step 3: Write minimal implementation**

`internal/api/web/views/setup.js`, following the existing view convention — render from the one store, no build step:

```js
import { el, mount } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";

export async function reloadPreflight() {
  try {
    const { checks } = await Aurium.preflight();
    update({ preflight: checks });
  } catch (err) {
    update({ preflight: [], preflightError: String(err) });
  }
}

function checkRow(c) {
  const cls = c.ok ? "is-ok" : c.severity === "optional" ? "is-warn" : "is-fail";
  return el(`li.check.${cls}`, {},
    el("span.check-name", {}, c.name),
    el("span.check-detail", {}, c.ok ? "ok" : c.error ?? "failed"),
    // The exact command, selectable: the whole point is that it can be copied.
    !c.ok && c.remedy ? el("code.check-fix", {}, c.remedy) : null);
}

export function renderSetup(host) {
  const checks = state.preflight ?? [];
  const blocking = checks.filter((c) => !c.ok && c.severity === "required");

  mount(host, [
    el("h2", {}, "Set up Aurium"),
    el("p.lede", {}, "Four steps. The first one is your machine."),

    el("section.step", {},
      el("h3", {}, "1 · Environment"),
      blocking.length
        ? el("p.notice", {}, `${blocking.length} thing(s) need fixing before agents can run.`)
        : el("p.notice.is-ok", {}, "This machine is ready."),
      el("ul.checks", {}, checks.map(checkRow)),
      el("button.mini", { onclick: reloadPreflight }, "Re-check")),

    el("section.step", {},
      el("h3", {}, "2 · Connect a provider"),
      el("p", {}, "Agents run on your accounts."),
      el("button", { onclick: () => update({ panel: "providers" }) }, "Open Providers")),

    el("section.step", {},
      el("h3", {}, "3 · Connect GitHub"),
      el("button", { onclick: () => update({ panel: "providers" }) }, "Open Providers")),

    el("section.step", {},
      el("h3", {}, "4 · Your first project"),
      el("button", { onclick: () => update({ panel: "home" }) }, "Create a project")),

    // A wizard you cannot leave is a trap, not a wizard.
    el("button.mini", { onclick: () => update({ panel: "workspace", setupDismissed: true }) },
      "Skip for now"),
  ]);
}
```

Add to `lib/api.js`, beside the other calls:

```js
preflight: () => get("/v1/preflight"),
```

In `app.js`, register the panel and route to it on a daemon that has nothing:

```js
import { renderSetup, reloadPreflight } from "./views/setup.js";

const PANELS = {
  setup: { label: "Setup", render: renderSetup },
  workspace: { label: "Workspace", render: renderWorkspace },
  // ...unchanged
};

const ORDER = ["workspace", "home", "approvals", "usage", "providers", "events"];

// The wizard is a destination, not a tab: it earns the screen only on a daemon
// with nothing in it, and only until the user skips it.
function shouldOpenSetup() {
  return !state.setupDismissed
    && (state.projects ?? []).length === 0
    && (state.providers ?? []).length === 0;
}
```

Call `reloadPreflight()` from `selectPanel` when `key === "setup"`, matching how `usage` and `providers` already load on arrival. Then add `"views/setup.js"` to the `want` list in `dashboard_test.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -v && ./bin/aurium up --addr 127.0.0.1:7771`
Expected: PASS; an empty daemon opens on the wizard, the environment step lists live checks, "Skip for now" reaches the dashboard.

- [ ] **Step 5: Commit**

```bash
git add internal/api/web/ internal/api/dashboard_test.go
git commit -m "feat(dashboard): a first-run wizard that renders the same checks as doctor"
```

---

## Verification

Run before opening the PR:

```bash
go build ./... && go vet ./... && go test ./...
shellcheck setup.sh && sh -n setup.sh
```

The honest end-to-end check is the bare container, because it is the only environment that genuinely lacks a toolchain:

```bash
docker run --rm -v "$PWD:/src" -w /src ubuntu:24.04 sh -c \
  'apt-get update -qq && apt-get install -y -qq curl ca-certificates && ./setup.sh'
```
