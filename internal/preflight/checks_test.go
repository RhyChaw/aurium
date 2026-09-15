package preflight

import (
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
