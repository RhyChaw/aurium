package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reference recorded in the database must name where a secret lives, never
// contain it (§10.6).
func TestRefContainsNoSecret(t *testing.T) {
	ref := Ref("my-app", "github")
	if !strings.HasPrefix(ref, "keyring:") {
		t.Fatalf("ref = %q", ref)
	}
	if strings.Contains(ref, "ghp_") {
		t.Fatal("a reference must not embed a credential")
	}
	if _, err := accountFor(ref); err != nil {
		t.Fatalf("a reference must round-trip to an account: %v", err)
	}
	if _, err := accountFor("not-a-reference"); err == nil {
		t.Fatal("a malformed reference must be rejected")
	}
}

func TestFileFallbackRoundTrip(t *testing.T) {
	f := &FileFallback{Path: filepath.Join(t.TempDir(), "secrets")}

	if err := f.Set("app/github", "ghp_SECRET"); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get("app/github")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ghp_SECRET" {
		t.Fatalf("got %q", got)
	}

	if _, err := f.Get("app/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	if err := f.Delete("app/github"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("app/github"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a deleted secret must be gone")
	}
}

func TestFileFallbackIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets")
	f := &FileFallback{Path: path}
	if err := f.Set("app/github", "ghp_SECRET"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("secrets file is mode %o; it must not be readable by anyone else", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file used for the atomic write was left behind")
	}
}

// One credential's write must not lose the others.
func TestFileFallbackKeepsOtherSecrets(t *testing.T) {
	f := &FileFallback{Path: filepath.Join(t.TempDir(), "secrets")}
	f.Set("app/github", "one")
	f.Set("app/sentry", "two")
	f.Set("app/github", "one-updated")

	if v, _ := f.Get("app/sentry"); v != "two" {
		t.Fatalf("an unrelated secret was lost: %q", v)
	}
	if v, _ := f.Get("app/github"); v != "one-updated" {
		t.Fatalf("the update did not land: %q", v)
	}
}
