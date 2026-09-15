package preflight

import (
	"net"
	"os"
	"path/filepath"
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

	err = PortFree(ln.Addr().String())
	if err == nil {
		t.Fatal("a port with a live listener must not report free")
	}
	// The listener is owned by this process, so the message must identify it as such
	if !strings.Contains(err.Error(), "aurium up --restart") {
		t.Fatalf("error for own process must suggest --restart, got: %v", err)
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

// goCheckRemedy isolates the "go" check's remedy under a fake PATH and HOME,
// the way writeFakeBin isolates a fake binary elsewhere in this package.
func goCheckRemedy(t *testing.T) string {
	t.Helper()
	for _, c := range Checks(t.TempDir(), "") {
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
