package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

// mcpServerEntry decodes the one "aurium" server entry out of a written
// mcp.json, so assertions check what a real MCP client would parse rather
// than a hand-typed substring of it — a round-1 fix once wrote the bare
// daemon base URL with no /mcp path, and a substring test asserting the same
// constant the code produced passed anyway.
func mcpServerEntry(t *testing.T, raw string) struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
} {
	t.Helper()
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("host mcp config is not valid JSON: %v\n%s", err, raw)
	}
	server, ok := doc.MCPServers["aurium"]
	if !ok {
		t.Fatalf("host mcp config has no \"aurium\" server:\n%s", raw)
	}
	return server
}

// tmuxCapableDriver wraps the local driver but claims tmux support, so a test
// can tell whether Create actually tried to start a session without needing a
// real container runtime.
type tmuxCapableDriver struct {
	*driver.Local
	tmuxCalls int
}

func (d *tmuxCapableDriver) Capabilities() driver.Caps {
	caps := d.Local.Capabilities()
	caps.Tmux = true
	return caps
}

func (d *tmuxCapableDriver) Exec(ctx context.Context, id string, cmd []string, o driver.ExecOpts) (driver.ExecResult, error) {
	if len(cmd) > 0 && cmd[0] == "tmux" {
		d.tmuxCalls++
	}
	return d.Local.Exec(ctx, id, cmd, o)
}

// hostAgent is what every host-placement Create needs: the claude adapter and
// a gateway token.
//
// The token is not decoration. Prepare refuses to write a host MCP config
// without one (agent.ErrNoHostToken), because a config carrying
// "Authorization: Bearer " is 401'd by the API middleware — and a host turn
// whose only MCP server is refused answers "is_error": false with no tools at
// all, which nothing downstream can tell from a real reply.
func hostAgent(o *CreateOpts) {
	o.Adapter = "claude"
	o.Token = "tok_test"
}

func hostPlacementFixture(t *testing.T) (*fixture, *tmuxCapableDriver) {
	t.Helper()
	f := newFixture(t)
	drv := &tmuxCapableDriver{Local: driver.NewLocal()}
	f.mgr.Drivers = driver.Registry{"local": drv}
	f.mgr.HostAuriumURL = "http://127.0.0.1:7770"
	// Real usage always has SnapshotHome set (it is ~/.aurium); a fixture
	// that left it empty would resolve agent.MCPConfigPath's relative
	// "agents/<id>/mcp.json" against the test binary's working directory —
	// writing into the repo itself rather than proving anything.
	f.mgr.SnapshotHome = t.TempDir()

	cfg, err := config.Parse([]byte(`
version: 1
project: {name: app, base_branch: main}
sandbox:
  driver: local
  agent: claude
  agent_placement: host
`))
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	return f, drv
}

// Host placement has nothing to attach to, and spawning a REPL on the user's
// own machine is the surprise Caps.Tmux already warns about — this is the
// manager-level version of TestHostPlacementStartsNoTmuxSession, checking
// that Create actually wires wantsTmuxSession in rather than just defining it.
func TestCreateWithHostPlacementStartsNoTmuxSessionEvenOnATmuxCapableDriver(t *testing.T) {
	f, drv := hostPlacementFixture(t)
	f.create("feature", hostAgent)

	if drv.tmuxCalls != 0 {
		t.Errorf("host placement must not start a tmux session, got %d tmux calls", drv.tmuxCalls)
	}
}

// HeadlessCommand points --mcp-config at agent.MCPConfigPath(auriumHome,
// agentID); nothing else creates that file, so a host-sandboxed turn's first
// message depends on Create having written it here.
func TestCreateWithHostPlacementWritesTheHostMCPConfig(t *testing.T) {
	f, _ := hostPlacementFixture(t)
	c := f.create("feature", hostAgent)

	agents, err := f.store.ListAgents(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want exactly one agent, got %d", len(agents))
	}

	path := agent.MCPConfigPath(f.mgr.SnapshotHome, agents[0].ID)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("host mcp config not written at the path HeadlessCommand will read: %v", err)
	}
	got := string(b)
	if strings.Contains(got, `"command"`) {
		t.Errorf("a host-sandboxed turn must spawn no binary; got:\n%s", got)
	}

	server := mcpServerEntry(t, got)
	if server.Type != "http" {
		t.Errorf("host mcp config must use HTTP transport; got %+v", server)
	}
	// The daemon serves MCP only at POST /mcp (internal/api/server.go); a
	// bare base URL 404s/405s. HasSuffix, not equality against a literal
	// this test would also have to keep in sync with HostAuriumURL by hand.
	if !strings.HasSuffix(server.URL, "/mcp") {
		t.Errorf("host mcp config's url must end in /mcp; got %q", server.URL)
	}
	if !strings.HasPrefix(server.URL, f.mgr.HostAuriumURL) {
		t.Errorf("host mcp config's url must be built from Manager.HostAuriumURL (%q); got %q",
			f.mgr.HostAuriumURL, server.URL)
	}
}

// The default is in-container, and it must see no change: no host mcp
// config file, ever, for a project that never asked for host placement.
func TestCreateInContainerPlacementWritesNoHostMCPConfig(t *testing.T) {
	f := newFixture(t)
	f.mgr.SnapshotHome = t.TempDir()
	c := f.create("feature", func(o *CreateOpts) { o.Adapter = "claude" })

	agents, err := f.store.ListAgents(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := agent.MCPConfigPath(f.mgr.SnapshotHome, agents[0].ID)
	if _, err := os.ReadFile(path); !os.IsNotExist(err) {
		t.Errorf("in-container placement must not write a host mcp config, err=%v", err)
	}
	if agents[0].Role != store.RolePrimary {
		t.Fatalf("agent = %+v", agents[0])
	}
}

// Under host placement the CONTAINER still gets its usual CLAUDE.md
// instructions — this project's context, D16's one MCP server — because the
// container is unchanged by placement and an in-container turn in it would
// read exactly this file.
//
// What it must NOT contain is the host placement notice. That used to be
// written here, where a host process cannot read it: runHost launches with
// the developer's environment, nothing sets HOME, so the process reads the
// DEVELOPER's ~/.claude/CLAUDE.md. The notice and the projection travel in
// argv now; TestConverseHostPlacementPutsTheNoticeAndContextInTheArgv is what
// proves they arrive.
func TestCreateWithHostPlacementStillWritesTheContainerInstructions(t *testing.T) {
	f, _ := hostPlacementFixture(t)
	c := f.create("feature", hostAgent)

	home := filepath.Join(f.mgr.HomeRoot, c.ID)
	md, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(md), "Use the aurium_exec tool for every command.") {
		t.Errorf("the notice must not be parked in a file no host process reads:\n%s", md)
	}
	if !strings.Contains(string(md), "@") {
		t.Errorf("CLAUDE.md must still import the context projection:\n%s", md)
	}
}

// recreate (used by Restore) is Prepare's only other call site, and it
// chooses its own new agent id independently of Create's.
//
// Nothing in production mints container tokens yet (see the spec's "Nothing
// mints container tokens"), and recreate has no way to recover the one the
// container was created with: the database stores only a digest. So a restore
// under host placement cannot write a usable MCP config, and must say so by
// name rather than rebuild a container whose agent has no shell and no tools.
func TestRecreateWithHostPlacementRefusesWithoutAToken(t *testing.T) {
	f, _ := hostPlacementFixture(t)
	c := f.create("feature", hostAgent)
	ctx := context.Background()

	err := f.mgr.recreate(ctx, c.ID, f.cfg, false)
	if err == nil {
		t.Fatal("recreate under host placement with no token must fail, not produce a toolless agent")
	}
	if !errors.Is(err, agent.ErrNoHostToken) {
		t.Errorf("the failure must be nameable: %v", err)
	}
}

// Restore of an in-container project must be untouched by any of that: it
// never wanted a host MCP config and must not now need a token to be rebuilt.
func TestRecreateInContainerPlacementStillWorks(t *testing.T) {
	f := newFixture(t)
	f.mgr.SnapshotHome = t.TempDir()
	c := f.create("feature", func(o *CreateOpts) { o.Adapter = "claude" })
	ctx := context.Background()

	before, err := f.store.ListAgents(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("want exactly one agent before recreate, got %d", len(before))
	}
	if err := f.mgr.recreate(ctx, c.ID, f.cfg, false); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.ListAgents(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("want exactly one agent after recreate, got %d", len(after))
	}
	if after[0].ID == before[0].ID {
		t.Fatal("recreate must replace the old agent row with a new one, not keep the old id")
	}
	if _, err := os.Stat(agent.MCPConfigPath(f.mgr.SnapshotHome, after[0].ID)); !os.IsNotExist(err) {
		t.Errorf("in-container recreate must not write a host mcp config, err=%v", err)
	}
}
