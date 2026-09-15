package agent

import (
	"encoding/json"
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
	if strings.Contains(cfg, `"command"`) {
		t.Errorf("a host-sandboxed turn must spawn no binary at all; got:\n%s", cfg)
	}

	// Decoded rather than substring-matched: a round-1 fix once wrote the
	// bare base URL with no /mcp path, and a test asserting
	// `"url": "http://127.0.0.1:7770"` (the same constant the code produces)
	// passed anyway, because it echoed the wrong value back at itself. The
	// daemon serves MCP only at POST /mcp (internal/api/server.go); root
	// answers GET alone. So this checks the actual served route, not a
	// literal typed twice, and would have caught that.
	server := decodeAuriumServer(t, cfg)
	if server.Type != "http" {
		t.Errorf("host mcp config must use HTTP transport; got %+v", server)
	}
	if !strings.HasSuffix(server.URL, "/mcp") {
		t.Errorf("host mcp config's url must end in /mcp (the only route the daemon serves MCP on); got %q", server.URL)
	}
	if server.URL != MCPEndpoint("http://127.0.0.1:7770") {
		t.Errorf("host mcp config's url must be MCPEndpoint(HostAuriumURL), got %q", server.URL)
	}
	if server.Headers["Authorization"] != "Bearer tok_secret" {
		t.Errorf("host mcp config must carry the bearer token in its headers, got %+v", server.Headers)
	}
}

// decodeAuriumServer reads the one "aurium" server entry out of a written
// mcp.json, so assertions check what a real MCP client would parse rather
// than a hand-typed substring of it.
func decodeAuriumServer(t *testing.T, raw string) struct {
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
