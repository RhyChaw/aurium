package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/store"
)

// A host-sandboxed turn's MCP config names agent.MCPEndpoint(HostAuriumURL)
// as its url (internal/agent's writeHostMCPConfig). Round 2 of task 6 shipped
// that computation but with a bare base URL — no /mcp — because the test
// asserting the shape compared it against the same constant the code
// produced, so nothing could tell the two apart from a config the daemon
// would actually reject with a 404/405.
//
// This test closes that gap from the other side: instead of asserting a
// string shape, it POSTs to exactly the path agent.MCPEndpoint derives from
// this daemon's own base URL, against the real mux this package registers,
// and requires the request to be routed at all. If MCPEndpoint ever stopped
// matching the route internal/api/server.go's "POST /mcp" registers, this
// fails — not because a literal changed, but because the request would 404.
func TestHostMCPEndpointMatchesTheRouteTheDaemonRegisters(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)
	ctx := context.Background()

	if _, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary,
		TmuxSession: "agent", Status: store.AgentRunning,
	}); err != nil {
		t.Fatal(err)
	}

	endpoint := agent.MCPEndpoint(h.srv.URL)
	path := strings.TrimPrefix(endpoint, h.srv.URL)
	if path == endpoint || !strings.HasPrefix(path, "/") {
		t.Fatalf("could not derive a path from MCPEndpoint(%q) = %q", h.srv.URL, endpoint)
	}

	res := h.do("POST", path,
		h.cToken, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200 — the route a host-sandboxed turn's "+
			"config names must be the one this daemon actually serves MCP on",
			path, res.StatusCode)
	}
}
