package cli_test

import (
	"strings"
	"testing"
)

func TestContextRoundTripThroughTheCLI(t *testing.T) {
	e := newEnv(t)

	e.aurium("context", "set", "task/objective", "Add OAuth 2.0 with PKCE")
	out := e.aurium("context", "get", "task/objective")
	if !strings.Contains(out, "Add OAuth 2.0 with PKCE") {
		t.Fatalf("content did not round-trip:\n%s", out)
	}
	if !strings.Contains(out, "v1") {
		t.Errorf("the version should be shown, since a writer needs it:\n%s", out)
	}

	// A blind second create must be refused rather than clobbering.
	if out, err := e.auriumErr("context", "set", "task/objective", "clobber"); err == nil {
		t.Fatalf("a write with no base version over an existing item must be refused:\n%s", out)
	}

	// With the right version it succeeds.
	e.aurium("context", "set", "task/objective", "Add OAuth 2.0 with PKCE, behind a flag",
		"--base-version", "1")
	out = e.aurium("context", "get", "task/objective")
	if !strings.Contains(out, "behind a flag") || !strings.Contains(out, "v2") {
		t.Fatalf("the versioned write did not land:\n%s", out)
	}
}

func TestContextAppendAccumulates(t *testing.T) {
	e := newEnv(t)
	e.aurium("context", "append", "decisions/jwt", "RS256, because mobile cannot hold a secret.")
	e.aurium("context", "append", "decisions/jwt", "15 minute access tokens.")

	out := e.aurium("context", "get", "decisions/jwt")
	if !strings.Contains(out, "RS256") || !strings.Contains(out, "15 minute") {
		t.Fatalf("appends did not accumulate:\n%s", out)
	}
}

func TestContextSearchFindsWhatWasWritten(t *testing.T) {
	e := newEnv(t)
	e.aurium("context", "append", "discoveries/auth", "the legacy flow double-encodes state")

	out := e.aurium("context", "query", "double-encodes")
	if !strings.Contains(out, "discoveries/auth") {
		t.Fatalf("search did not find the item:\n%s", out)
	}
}

// The projection is what an agent reads on every turn, so it must appear in
// the worktree and describe the container.
func TestProjectionIsWrittenIntoTheWorktree(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.aurium("context", "set", "task/objective", "Ship the thing")

	// Regenerate explicitly, as the daemon does on a context change.
	out := e.aurium("context", "list")
	if !strings.Contains(out, "task/objective") {
		t.Fatalf("the item is missing:\n%s", out)
	}
}

func TestInboxIsEmptyByDefault(t *testing.T) {
	e := newEnv(t)
	out := e.aurium("inbox")
	if !strings.Contains(out, "empty") {
		t.Fatalf("a fresh project should have an empty inbox:\n%s", out)
	}
}

func TestApproveWithNothingPendingSaysSo(t *testing.T) {
	e := newEnv(t)
	out := e.aurium("approve")
	if !strings.Contains(out, "Nothing is waiting") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestIntegrationListStartsEmpty(t *testing.T) {
	e := newEnv(t)
	out := e.aurium("integration", "list")
	if !strings.Contains(out, "No integrations") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// An unknown MCP command must fail cleanly and leave the integration marked
// with the error, rather than a half-connected row.
func TestConnectingABrokenIntegrationRecordsTheError(t *testing.T) {
	e := newEnv(t)
	out, err := e.auriumErr("integration", "connect", "broken",
		"--mcp", "definitely-not-a-real-binary-xyz")
	if err == nil {
		t.Fatalf("connecting a nonexistent server should fail:\n%s", out)
	}

	listing := e.aurium("integration", "list")
	if !strings.Contains(listing, "broken") || !strings.Contains(listing, "error") {
		t.Fatalf("the failure should be visible in the listing:\n%s", listing)
	}
}
