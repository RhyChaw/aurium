package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

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
	f.create("feature", func(o *CreateOpts) { o.Adapter = "claude" })

	if drv.tmuxCalls != 0 {
		t.Errorf("host placement must not start a tmux session, got %d tmux calls", drv.tmuxCalls)
	}
}

// HeadlessCommand points --mcp-config at agent.MCPConfigPath(auriumHome,
// agentID); nothing else creates that file, so a host-sandboxed turn's first
// message depends on Create having written it here.
func TestCreateWithHostPlacementWritesTheHostMCPConfig(t *testing.T) {
	f, _ := hostPlacementFixture(t)
	c := f.create("feature", func(o *CreateOpts) { o.Adapter = "claude" })

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
	for _, want := range []string{`"aurium"`, `"AURIUM_URL": "http://127.0.0.1:7770"`} {
		if !strings.Contains(got, want) {
			t.Errorf("host mcp config missing %q; got:\n%s", want, got)
		}
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

// Under host placement the agent still gets its usual CLAUDE.md instructions
// (this project's context, D16's one MCP server) plus the placement notice —
// Prepare adds to the file, it does not replace it.
func TestCreateWithHostPlacementStillWritesTheContainerInstructions(t *testing.T) {
	f, _ := hostPlacementFixture(t)
	c := f.create("feature", func(o *CreateOpts) { o.Adapter = "claude" })

	home := filepath.Join(f.mgr.HomeRoot, c.ID)
	md, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "Use the aurium_exec tool for every command.") {
		t.Errorf("CLAUDE.md missing the host placement notice:\n%s", md)
	}
	if !strings.Contains(string(md), "@") {
		t.Errorf("CLAUDE.md must still import the context projection:\n%s", md)
	}
}
