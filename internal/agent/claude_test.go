package agent

import (
	"encoding/json"
	"errors"
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

// --mcp-config alone is ADDITIVE: the host process would also load the
// developer's user-scope MCP servers and any .mcp.json in the worktree, which
// is exactly the D16 invariant mcpServerJSON documents — one server, so an
// agent cannot reach an upstream without passing the gateway's grant checks.
func TestHeadlessCommandRegistersOnlyAuriumsMCPServerUnderHostPlacement(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json",
	})
	if !argvHas(argv, "--strict-mcp-config") {
		t.Errorf("host placement must not inherit other MCP configurations: %v", argv)
	}
}

// The notice and the context projection are delivered through argv because
// nothing else reaches a host turn: it runs with the developer's environment,
// so it reads the developer's ~/.claude/CLAUDE.md, never the file Prepare
// wrote under the CONTAINER's $HOME.
//
// This asserts what the launched process receives, not what was written to a
// file — a file under the container's $HOME can be written perfectly and
// still reach nobody.
func TestHeadlessCommandDeliversTheNoticeAndContextInArgv(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json",
		ProjectContext: "# Objective\nShip the thing.\n",
	})

	prompt, ok := argvValue(argv, "--append-system-prompt")
	if !ok {
		t.Fatalf("host placement must append a system prompt; argv = %v", argv)
	}
	if !strings.Contains(prompt, "Use the aurium_exec tool for every command.") {
		t.Errorf("the launched process never receives the placement notice:\n%s", prompt)
	}
	if !strings.Contains(prompt, "do not go looking for it elsewhere on the machine.") {
		t.Errorf("the notice reaches the process truncated:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Ship the thing.") {
		t.Errorf("the context projection never reaches the process:\n%s", prompt)
	}
}

// The in-container path's @-import is re-read whenever CONTEXT.md changes
// (§8.5). Delivered in argv, that only holds if the prompt is rendered fresh
// each turn: the CLI records the system prompt on a conversation's first
// request by default and resends the record on every --continue, which would
// freeze the projection at whatever it said when the conversation began.
func TestHeadlessCommandKeepsTheProjectionFreshAcrossAConversation(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json", Continue: true,
		ProjectContext: "# Objective\nShip the thing.\n",
	})
	if got, ok := argvValue(argv, "--system-prompt-snapshot"); !ok || got != "off" {
		t.Errorf("a continued host turn must re-render its system prompt, got %q (present=%v): %v",
			got, ok, argv)
	}
}

// With no projection to carry, the prompt is the notice alone — not a dangling
// heading over nothing.
func TestHeadlessCommandOmitsAnEmptyContextProjection(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json", ProjectContext: "  \n ",
	})
	prompt, ok := argvValue(argv, "--append-system-prompt")
	if !ok {
		t.Fatalf("host placement must append a system prompt; argv = %v", argv)
	}
	if strings.Contains(prompt, contextHeading) {
		t.Errorf("an empty projection must not produce an empty heading:\n%s", prompt)
	}
}

// In-container placement is untouched: it receives its instructions through
// the container's ~/.claude/CLAUDE.md, which its process genuinely reads.
func TestHeadlessCommandAppendsNoSystemPromptInAContainer(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{ProjectContext: "# Objective\n"})
	if _, ok := argvValue(argv, "--append-system-prompt"); ok {
		t.Errorf("in-container placement must not append a system prompt: %v", argv)
	}
	if argvHas(argv, "--strict-mcp-config") {
		t.Errorf("in-container placement must not change MCP resolution: %v", argv)
	}
	if argvHas(argv, "--system-prompt-snapshot") {
		t.Errorf("in-container placement must not change prompt handling: %v", argv)
	}
}

// argvHas reports whether argv carries a flag as its own element — a
// substring match over a joined command line would also match a flag that
// merely appeared inside a prompt.
func argvHas(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// argvValue returns the element following flag, which is what the process
// actually receives as that flag's value.
func argvValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
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

// Verified against the real CLI: with --mcp-config naming an unauthorized
// server, `claude -p --output-format json` returns "is_error": false and an
// empty permission_denials, which Aurium records as an ordinary reply. With
// Bash denied as well the agent has no tools and no shell, and nothing
// downstream can notice. So a config that would carry "Authorization: Bearer "
// is refused here rather than written.
func TestPrepareRefusesToWriteAHostMCPConfigWithNoToken(t *testing.T) {
	home := t.TempDir()
	auriumHome := t.TempDir()
	c := &Claude{}

	err := c.Prepare(Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		HostSandboxed: true, AgentID: "ag_123", AuriumHome: auriumHome,
		HostAuriumURL: "http://127.0.0.1:7770",
		// Token deliberately empty: production mints none yet.
	})
	if err == nil {
		t.Fatal("a host-sandboxed agent with no token must fail, not get an unauthorized config")
	}
	if !errors.Is(err, ErrNoHostToken) {
		t.Errorf("the failure must be nameable by callers: %v", err)
	}
	if _, statErr := os.Stat(MCPConfigPath(auriumHome, "ag_123")); !os.IsNotExist(statErr) {
		t.Errorf("no config may be written when it could only 401, err=%v", statErr)
	}
}

// In-container placement never writes that config, so a missing token must
// stay exactly as harmless as it was before host placement existed: the agent
// loses the gateway tools and keeps its own Bash.
func TestPrepareWithNoTokenIsStillFineInContainer(t *testing.T) {
	home := t.TempDir()
	c := &Claude{}
	if err := c.Prepare(Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		AuriumURL: "http://host.docker.internal:7770",
	}); err != nil {
		t.Fatalf("in-container placement must not require a token: %v", err)
	}
}

// Prepare must stay idempotent under host placement too: a restart or a
// second turn re-running it must not duplicate anything or corrupt a file.
func TestPrepareIsIdempotentUnderHostPlacement(t *testing.T) {
	home := t.TempDir()
	auriumHome := t.TempDir()
	c := &Claude{}
	p := Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		HostSandboxed: true, AgentID: "ag_123", AuriumHome: auriumHome,
		HostAuriumURL: "http://127.0.0.1:7770", Token: "tok_secret",
	}
	for i := 0; i < 3; i++ {
		if err := c.Prepare(p); err != nil {
			t.Fatal(err)
		}
	}
	md := readFile(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if n := strings.Count(md, "@/wt/.aurium/CONTEXT.md"); n != 1 {
		t.Fatalf("re-running Prepare duplicated the context import %d times:\n%s", n, md)
	}
	cfg := readFile(t, MCPConfigPath(auriumHome, "ag_123"))
	if strings.Count(cfg, `"mcpServers"`) != 1 {
		t.Fatalf("host mcp config should still be exactly one server:\n%s", cfg)
	}
}

// The notice must NOT go into the container's ~/.claude/CLAUDE.md under host
// placement. It used to, and reached nobody: the host process runs with the
// developer's environment, so it reads the developer's ~/.claude/CLAUDE.md.
// Writing it there makes a file that looks delivered and is not.
func TestPrepareDoesNotHideTheNoticeInAFileTheHostProcessNeverReads(t *testing.T) {
	home := t.TempDir()
	c := &Claude{}
	if err := c.Prepare(Projection{
		Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp",
		HostSandboxed: true, AgentID: "ag_1", AuriumHome: t.TempDir(),
		HostAuriumURL: "http://127.0.0.1:7770", Token: "tok_secret",
	}); err != nil {
		t.Fatal(err)
	}
	md := readFile(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if strings.Contains(md, "Use the aurium_exec tool for every command.") {
		t.Fatalf("the notice must travel in argv, not in a container file:\n%s", md)
	}
	// The container's own instructions are untouched: this file is still what
	// an in-container turn in this same container would read.
	if !strings.Contains(md, "@/wt/.aurium/CONTEXT.md") {
		t.Fatalf("the container's context import must survive host placement:\n%s", md)
	}
}
