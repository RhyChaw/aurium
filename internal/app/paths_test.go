package app

import (
	"os"
	"path/filepath"
	"testing"
)

// Project roots are looked up by exact string match, so a symlinked path must
// canonicalise to the same string from either direction. On macOS /var is a
// symlink to /private/var, which broke project lookup until this existed.
func TestCanonicalPathResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	viaReal := CanonicalPath(real)
	viaLink := CanonicalPath(link)
	if viaReal != viaLink {
		t.Fatalf("the same directory canonicalised two ways:\n  %s\n  %s", viaReal, viaLink)
	}
}

func TestCanonicalPathIsAbsoluteAndIdempotent(t *testing.T) {
	got := CanonicalPath(".")
	if !filepath.IsAbs(got) {
		t.Fatalf("CanonicalPath must return an absolute path, got %q", got)
	}
	if again := CanonicalPath(got); again != got {
		t.Fatalf("CanonicalPath is not idempotent: %q then %q", got, again)
	}
}

func TestCanonicalPathHandlesMissingPaths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist")
	if got := CanonicalPath(p); !filepath.IsAbs(got) {
		t.Fatalf("a missing path should still canonicalise to something absolute, got %q", got)
	}
}
