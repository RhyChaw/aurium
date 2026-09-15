package preflight

import (
	"context"
	"errors"
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

func TestBinaryWorksSilentFailure(t *testing.T) {
	writeFakeBin(t, "silentfail", `exit 42`)
	err := BinaryWorks(context.Background(), "silentfail")
	if err == nil {
		t.Fatal("a binary that exits non-zero must report an error")
	}
	if !strings.Contains(err.Error(), "silentfail") {
		t.Errorf("error must mention the binary path, got: %v", err)
	}
}

func TestRun(t *testing.T) {
	checks := []Check{
		{
			Name:     "passing",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				return nil
			},
			Remedy: Remedy{
				Kind:    NoRemedy,
				Command: "",
			},
		},
		{
			Name:     "failing",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				return errors.New("something went wrong")
			},
			Remedy: Remedy{
				Kind:    Manual,
				Command: "run this command",
			},
		},
	}

	results := Run(context.Background(), checks)

	if len(results) != 2 {
		t.Fatalf("Run should return 2 results, got %d", len(results))
	}

	// Check passing result
	if !results[0].OK {
		t.Errorf("passing check should have OK=true")
	}
	if results[0].Error != "" {
		t.Errorf("passing check should have empty Error, got: %q", results[0].Error)
	}
	if results[0].Remedy != "" {
		t.Errorf("passing check should have empty Remedy, got: %q", results[0].Remedy)
	}
	if results[0].RemedyKind != "" {
		t.Errorf("passing check should have empty RemedyKind, got: %q", results[0].RemedyKind)
	}

	// Check failing result
	if results[1].OK {
		t.Errorf("failing check should have OK=false")
	}
	if results[1].Error != "something went wrong" {
		t.Errorf("failing check should have Error='something went wrong', got: %q", results[1].Error)
	}
	if results[1].Remedy != "run this command" {
		t.Errorf("failing check should have Remedy='run this command', got: %q", results[1].Remedy)
	}
	if results[1].RemedyKind != string(Manual) {
		t.Errorf("failing check should have RemedyKind='manual', got: %q", results[1].RemedyKind)
	}
}

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
