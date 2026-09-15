package preflight

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// namedCheck pulls one check out of the table so a test can drive its probe
// and its remedy directly.
func namedCheck(t *testing.T, name string) Check {
	t.Helper()
	for _, c := range Checks("") {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q is missing from the table", name)
	return Check{}
}

// The regression: this check used to stat the path app.Home() returned, and
// app.Home() *creates* the directory as a side effect of being asked. Both
// callers called it before building the table, so the stat could never fail,
// and the Auto remedy below it was unreachable — which on Linux left
// `doctor --fix` with nothing at all to apply.
func TestAuriumHomeCheckFailsWhenAbsentAndItsRemedyCreatesIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "aurium-home")
	t.Setenv("AURIUM_HOME", dir)

	c := namedCheck(t, "~/.aurium")
	ctx := context.Background()

	if err := c.Probe(ctx); err == nil {
		t.Fatal("a missing ~/.aurium must fail the check, not pass it into existence")
	}
	if c.Remedy.Kind != Auto || c.Remedy.Fix == nil {
		t.Fatalf("~/.aurium must carry an Auto remedy, got %+v", c.Remedy)
	}
	if err := c.Remedy.Fix(ctx); err != nil {
		t.Fatalf("the remedy must create the directory: %v", err)
	}
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("the check must pass once the remedy has run: %v", err)
	}

	// And end to end through Fix, which is what `doctor --fix` calls.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	res := Fix(ctx, []Check{c})
	if !res[0].OK {
		t.Fatalf("Fix must apply the Auto remedy and leave the check passing: %+v", res[0])
	}
}

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

	err = PortFree(ln.Addr().String())
	if err == nil {
		t.Fatal("a port with a live listener must not report free")
	}
	// The listener is owned by this process, so the message must identify it as such
	if !strings.Contains(err.Error(), "aurium up --restart") {
		t.Fatalf("error for own process must suggest --restart, got: %v", err)
	}
}

// The motivating failure the owner lookup exists for, and the one it silently
// stopped covering: on macOS an unprivileged lsof does not list another
// user's sockets, so lsof answers "nobody" and the code fell through to the
// raw `bind: address already in use` — the exact opaque message the whole
// function was written to replace. The fake lsof here reproduces that answer
// (no output) without needing a second user account.
func TestPortFreeExplainsAHolderThisUserCannotSee(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	writeFakeBin(t, "lsof", `exit 1`) // ran, matched nothing — lsof's real answer here

	err = PortFree(ln.Addr().String())
	if err == nil {
		t.Fatal("a port with a live listener must not report free")
	}
	if strings.Contains(err.Error(), "address already in use") {
		t.Errorf("must not degrade to the raw bind error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot see") || !strings.Contains(err.Error(), "--addr") {
		t.Errorf("an unseeable holder must be named as such and point at --addr, got: %v", err)
	}
}

// With no lsof at all nothing was learned, and the message must not pretend
// otherwise — "another user's daemon" would be a guess dressed as a finding.
func TestPortFreeStaysVagueWhenNothingCanBeLearned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("PATH", t.TempDir()) // no lsof anywhere

	err = PortFree(ln.Addr().String())
	if err == nil {
		t.Fatal("a port with a live listener must not report free")
	}
	if strings.Contains(err.Error(), "another user") {
		t.Errorf("must not claim an owner it never looked up, got: %v", err)
	}
}

// auriumHealthServer stands in for a running daemon: /v1/health is
// unauthenticated, which is what makes it probeable from a preflight check.
func auriumHealthServer(t *testing.T, body string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// doctor's --addr defaults to the address `aurium up` binds, so on a healthy
// machine with the daemon running the port check was a *required* failure and
// doctor exited 1 — and setup.sh, which ends in `doctor --fix`, closed a
// perfectly successful second run on a red FAIL.
func TestPortFreeTreatsOurOwnRunningDaemonAsOK(t *testing.T) {
	addr := auriumHealthServer(t,
		`{"status":"ok","version":"0.1.0-dev","pid":4242,"started_at":"2026-09-15T00:00:00Z","uptime":"1m0s"}`)

	err := PortFree(addr)
	if err == nil {
		t.Fatal("the probe must still report what it found")
	}
	var info *InfoError
	if !errors.As(err, &info) {
		t.Fatalf("our own Aurium daemon holding the port is not a failure, got: %v", err)
	}
	if !strings.Contains(info.Msg, "Aurium") {
		t.Errorf("the detail must say what is holding the port, got: %q", info.Msg)
	}
	// And it must survive the round trip through Run as a passing row.
	res := Run(context.Background(), []Check{{
		Name: "port", Severity: Required,
		Probe: func(ctx context.Context) error { return PortFree(addr) },
	}})
	if !res[0].OK || res[0].Detail == "" {
		t.Fatalf("Run must grade an InfoError as a pass carrying Detail, got %+v", res[0])
	}
}

// Confirmed, not assumed: some other server of this user's on that port is a
// real failure, and calling it Aurium would be a confident lie.
func TestPortFreeDoesNotMistakeAnotherServerForAurium(t *testing.T) {
	addr := auriumHealthServer(t, `{"status":"ok"}`) // half the world's health endpoints

	err := PortFree(addr)
	if err == nil {
		t.Fatal("a held port must not report free")
	}
	var info *InfoError
	if errors.As(err, &info) {
		t.Fatalf("a non-Aurium server must not be reported as ok, got info: %q", info.Msg)
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
	for _, c := range Checks("127.0.0.1:7770") {
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
	for _, c := range Checks("") {
		if c.Name == "port" {
			t.Fatal("an empty addr must omit the port check")
		}
	}
}

// goCheckRemedy isolates the "go" check's remedy under a fake PATH and HOME,
// the way writeFakeBin isolates a fake binary elsewhere in this package.
func goCheckRemedy(t *testing.T) string {
	t.Helper()
	for _, c := range Checks("") {
		if c.Name == "go" {
			return c.Remedy.Command
		}
	}
	t.Fatal("go check missing from the table")
	return ""
}

// The regression this guards against: a contributor who already ran
// setup.sh, and for whom it already worked, must never be told to run it
// again — that advice is circular and can never succeed. Nothing installed
// anywhere gets the ./setup.sh advice; a toolchain setup.sh already placed
// at ~/.local/go/bin, just not on this shell's PATH, must get different
// advice instead. If the remedy ever collapses back to a single message,
// one of these two must fail.
func TestGoRemedyRecommendsSetupWhenNothingIsInstalledAnywhere(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	if rem := goCheckRemedy(t); !strings.Contains(rem, "setup.sh") {
		t.Errorf("go missing everywhere must recommend setup.sh, got %q", rem)
	}
}

func TestGoRemedyRecommendsPATHWhenSetupAlreadyInstalledIt(t *testing.T) {
	home := t.TempDir()
	goBin := filepath.Join(home, ".local", "go", "bin")
	if err := os.MkdirAll(goBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGo := "#!/bin/sh\necho go version go1.27.1 darwin/arm64\n"
	if err := os.WriteFile(filepath.Join(goBin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // deliberately does not include goBin

	rem := goCheckRemedy(t)
	if strings.Contains(rem, "setup.sh") {
		t.Fatalf("go already installed by setup.sh must not be told to rerun it, got %q", rem)
	}
	if !strings.Contains(rem, "PATH") || !strings.Contains(rem, ".local/go/bin") {
		t.Errorf("go present but off PATH must recommend adding ~/.local/go/bin to PATH, got %q", rem)
	}
}

func TestEveryAutoRemedyHasAFixAndEveryManualHasACommand(t *testing.T) {
	for _, c := range Checks("127.0.0.1:7770") {
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

// The daemon does not inherit a login shell's environment: launched from the
// app bundle, or from a terminal opened before the profile line existed, PATH
// lacks ~/.local/go/bin while the toolchain sits there perfectly usable. The
// dashboard reported "go not installed" for a Go that setup.sh had installed —
// a check answering a different question than the one it printed.
func TestGoIsFoundWhereSetupInstallsItEvenWhenNotOnPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // deliberately empty of any go

	bin := filepath.Join(home, ".local", "go", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "go")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'go version go1.27.1 darwin/arm64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := goWorks(context.Background()); err != nil {
		t.Fatalf("a usable go at %s must satisfy the check even off PATH: %v", fake, err)
	}
}

// And when it genuinely is nowhere, the check must still fail — otherwise the
// fix above would have turned the probe into one that cannot fail.
func TestGoStillFailsWhenItIsNowhere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if err := goWorks(context.Background()); err == nil {
		t.Fatal("with no go on PATH and none installed, the check must fail")
	}
}
