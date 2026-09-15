package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeadlessCommandDeniesTheShellUnderHostPlacement(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json",
	})
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--disallowedTools Bash",
		"--allowedTools mcp__aurium__exec",
		"--mcp-config /tmp/mcp.json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q: %v", want, argv)
		}
	}
}

// In-container placement is the default and must be untouched: the container
// is the wall there, and denying the shell inside it would cripple the agent
// for no gain.
func TestHeadlessCommandLeavesTheShellAloneInAContainer(t *testing.T) {
	c := &Claude{}
	joined := strings.Join(c.HeadlessCommand("do the thing", ExecOpts{}), " ")
	if strings.Contains(joined, "disallowedTools") {
		t.Errorf("in-container placement must not deny Bash: %s", joined)
	}
}

// HeadlessCommand points --mcp-config at agent.MCPConfigPath(auriumHome,
// agentID); Prepare is the only thing that can create the file there, and
// nothing else does. Without this, aurium_exec is unreachable and a
// host-sandboxed turn cannot run a single command.
func TestPrepareWritesTheHostMCPConfigAtMCPConfigPath(t *testing.T) {
	home := t.TempDir()
	auriumHome := t.TempDir()
	c := &Claude{}

	err := c.Prepare(Projection{
		Home:          home,
		ContextPath:   "/wt/.aurium/CONTEXT.md",
		MCPCommand:    "aurium-mcp",
		AuriumURL:     "http://host.docker.internal:7770",
		Token:         "tok_secret",
		HostSandboxed: true,
		AgentID:       "ag_123",
		AuriumHome:    auriumHome,
		HostAuriumURL: "http://127.0.0.1:7770",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := MCPConfigPath(auriumHome, "ag_123")
	cfg := readFile(t, want)
	// Shape confirmed against `claude mcp add --transport http --scope
	// project` (see the task-6 report): HTTP transport, no command to spawn.
	for _, sub := range []string{
		`"mcpServers"`, `"aurium"`, `"type": "http"`,
		`"url": "http://127.0.0.1:7770"`, `"Authorization": "Bearer tok_secret"`,
	} {
		if !strings.Contains(cfg, sub) {
			t.Errorf("host mcp config missing %q; got:\n%s", sub, cfg)
		}
	}
	if strings.Contains(cfg, `"command"`) {
		t.Errorf("a host-sandboxed turn must spawn no binary at all; got:\n%s", cfg)
	}
}

// Only a host-sandboxed turn gets this file; an in-container turn's
// HeadlessCommand never reads --mcp-config, so writing one would be a file
// nobody consults and a place for the two to quietly drift apart.
func TestPrepareWritesNoHostMCPConfigInContainerPlacement(t *testing.T) {
	home := t.TempDir()
	auriumHome := t.TempDir()
	c := &Claude{}

	if err := c.Prepare(Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		AgentID: "ag_123", AuriumHome: auriumHome,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(MCPConfigPath(auriumHome, "ag_123")); !os.IsNotExist(err) {
		t.Errorf("in-container placement must not get a host mcp config, err=%v", err)
	}
}

// Prepare must stay idempotent under host placement too: a restart or a
// second turn re-running it must not duplicate the notice or corrupt the file.
func TestPrepareIsIdempotentUnderHostPlacement(t *testing.T) {
	home := t.TempDir()
	auriumHome := t.TempDir()
	c := &Claude{}
	p := Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		HostSandboxed: true, AgentID: "ag_123", AuriumHome: auriumHome,
		HostAuriumURL: "http://127.0.0.1:7770",
	}
	for i := 0; i < 3; i++ {
		if err := c.Prepare(p); err != nil {
			t.Fatal(err)
		}
	}
	md := readFile(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if n := strings.Count(md, "Use the aurium_exec tool for every command."); n != 1 {
		t.Fatalf("re-running Prepare duplicated the host placement notice %d times:\n%s", n, md)
	}
	cfg := readFile(t, MCPConfigPath(auriumHome, "ag_123"))
	if strings.Count(cfg, `"mcpServers"`) != 1 {
		t.Fatalf("host mcp config should still be exactly one server:\n%s", cfg)
	}
}

// The paragraph exists because the spike showed an agent meeting `go:
// command not found` will otherwise spend turns searching
// /usr/local/go/bin, /opt/homebrew/bin/go and ~/go/bin before concluding.
func TestPrepareTellsAHostSandboxedAgentWhereItsShellIs(t *testing.T) {
	home := t.TempDir()
	c := &Claude{}
	if err := c.Prepare(Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		HostSandboxed: true, AgentID: "ag_1", AuriumHome: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	md := readFile(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if !strings.Contains(md, "Use the aurium_exec tool for every command.") {
		t.Fatalf("CLAUDE.md must tell a host-sandboxed agent where its shell is:\n%s", md)
	}
	if !strings.Contains(md, "do not go looking for it elsewhere on the machine.") {
		t.Fatalf("CLAUDE.md must carry the full notice:\n%s", md)
	}
}
