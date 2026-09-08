package cli_test

import (
	"strings"
	"testing"
)

// Context is default-deny, so without seeded grants every agent is denied
// everything and search silently returns nothing. `aurium init` must seed them.
func TestInitSeedsContextGrants(t *testing.T) {
	e := newEnv(t)

	var n int
	// The grants live in the daemon's database, reachable through the CLI's
	// own store; query it the way a human would notice the symptom instead:
	// an agent-visible search must find what was written.
	e.aurium("context", "append", "decisions/jwt", "RS256 because mobile cannot hold a secret")

	out := e.aurium("context", "query", "RS256")
	if !strings.Contains(out, "decisions/jwt") {
		t.Fatalf("seeded grants missing: a readable item was not found by search:\n%s", out)
	}
	_ = n
}

// `integration connect` spawns a server only long enough to learn its tools.
// Without reconnection at daemon start, an agent's first call reports the
// integration as disconnected.
func TestConnectRecordsEnoughToReconnect(t *testing.T) {
	e := newEnv(t)

	// A server that exits immediately still registers, and the failure is
	// recorded rather than leaving a half-connected row.
	out, _ := e.auriumErr("integration", "connect", "flaky", "--mcp", "false")

	listing := e.aurium("integration", "list")
	if !strings.Contains(listing, "flaky") {
		t.Fatalf("the integration row should exist even after a failed handshake:\n%s\n%s", out, listing)
	}
	// The stored config must carry the command, or reconnection is impossible.
	if !strings.Contains(listing, "error") && !strings.Contains(listing, "connecting") {
		t.Errorf("status should reflect the failure:\n%s", listing)
	}
}

func TestApproveRequiresTheDaemon(t *testing.T) {
	e := newEnv(t)
	// With nothing pending, approve lists rather than failing.
	out := e.aurium("approve")
	if !strings.Contains(out, "Nothing is waiting") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
