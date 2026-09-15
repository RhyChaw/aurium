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
