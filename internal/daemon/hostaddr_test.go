package daemon

import (
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
)

// HostAuriumURL used to be the constant "http://127.0.0.1:7770" while the
// daemon's address is a --addr flag. On any other port, every host-placed
// agent got an MCP config pointing at nothing — and a host turn whose only
// MCP server is unreachable reports success with no tools, so nothing said so.
//
// This asserts the daemon actually wires its OWN address in, not that a
// helper can format one.
func TestNewDerivesTheHostAuriumURLFromItsListenAddress(t *testing.T) {
	t.Setenv("AURIUM_HOME", t.TempDir())

	d, err := New(Options{Addr: "127.0.0.1:7999"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.App.Close() })

	got := d.App.Manager.HostAuriumURL
	if !strings.Contains(got, "7999") {
		t.Fatalf("HostAuriumURL = %q, but this daemon listens on 127.0.0.1:7999", got)
	}
	// And the endpoint a host turn's config will name has to be reachable on
	// that same address.
	if want := "http://127.0.0.1:7999/mcp"; agent.MCPEndpoint(got) != want {
		t.Fatalf("MCPEndpoint(HostAuriumURL) = %q, want %q", agent.MCPEndpoint(got), want)
	}
}

// A daemon started with no --addr keeps the default, and a wildcard bind
// resolves to loopback: "listen everywhere" is not an address a client can
// dial.
func TestNewNormalisesTheAddressAHostTurnDials(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"", "http://127.0.0.1:7770"},
		{":7771", "http://127.0.0.1:7771"},
		{"0.0.0.0:7772", "http://127.0.0.1:7772"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			t.Setenv("AURIUM_HOME", t.TempDir())
			d, err := New(Options{Addr: tc.addr})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.App.Close() })
			if got := d.App.Manager.HostAuriumURL; got != tc.want {
				t.Errorf("addr %q -> HostAuriumURL %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}
