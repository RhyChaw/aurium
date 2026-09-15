package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryHasTheDocumentedAdapters(t *testing.T) {
	r := DefaultRegistry()
	for _, name := range []string{"claude", "codex", "shell"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("adapter %q must be registered", name)
		}
	}
	if _, ok := r.Get("nonexistent"); ok {
		t.Error("unknown adapters must not resolve")
	}
}

func TestCapabilitiesMatchTheSpecTable(t *testing.T) {
	// §9.1's table. If an adapter's real capabilities change, this is the
	// place that should fail.
	want := map[string]Caps{
		"claude": {MCP: true, Headless: true, Resume: true, Usage: true, Interactive: true},
		"codex":  {MCP: true, Headless: true, Resume: true, Usage: false, Interactive: true},
		"shell":  {MCP: false, Headless: false, Resume: false, Usage: false, Interactive: true},
	}
	r := DefaultRegistry()
	for name, w := range want {
		a, _ := r.Get(name)
		if got := a.Capabilities(); got != w {
			t.Errorf("%s capabilities = %+v, want %+v", name, got, w)
		}
	}
}

func TestClaudePrepareWritesInstructionImportAndMCPConfig(t *testing.T) {
	home := t.TempDir()
	a, _ := DefaultRegistry().Get("claude")

	p := Projection{
		Home:        home,
		ContextPath: "/Users/alice/app/.aurium/wt/implement-oauth/.aurium/CONTEXT.md",
		MCPCommand:  "aurium-mcp",
		AuriumURL:   "http://host.docker.internal:7770",
		Token:       "tok_secret",
	}
	if err := a.Prepare(p); err != nil {
		t.Fatal(err)
	}

	// Appendix B: CLAUDE.md imports the projection with Claude Code's @ syntax
	// so the agent re-reads it whenever the daemon regenerates it.
	md := readFile(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if !strings.Contains(md, "@"+p.ContextPath) {
		t.Fatalf("CLAUDE.md must import the projection with @<path>; got:\n%s", md)
	}

	cfg := readFile(t, filepath.Join(home, ".claude.json"))
	for _, want := range []string{`"mcpServers"`, `"aurium"`, `"aurium-mcp"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".claude.json missing %q; got:\n%s", want, cfg)
		}
	}

	// D16/§10.1: exactly one MCP server. If Aurium ever registered a second,
	// agents could reach an upstream directly and bypass the gateway's grants.
	if strings.Count(cfg, `"command"`) != 1 {
		t.Errorf("an agent must see exactly one MCP server; got:\n%s", cfg)
	}
}

// §10.6: the credential stays on the host. It reaches the container as a
// scoped bearer token in a file, never baked into agent config on disk.
func TestPrepareNeverWritesTheTokenIntoAgentConfig(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		home := t.TempDir()
		a, _ := DefaultRegistry().Get(name)
		if err := a.Prepare(Projection{
			Home:        home,
			ContextPath: "/wt/.aurium/CONTEXT.md",
			MCPCommand:  "aurium-mcp",
			AuriumURL:   "http://host.docker.internal:7770",
			Token:       "tok_supersecret",
		}); err != nil {
			t.Fatal(err)
		}

		filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.Contains(string(mustRead(t, path)), "tok_supersecret") {
				t.Errorf("%s adapter wrote the bearer token into %s", name, path)
			}
			return nil
		})
	}
}

func TestCodexPrepareWritesAgentsMdAndTomlServer(t *testing.T) {
	home := t.TempDir()
	a, _ := DefaultRegistry().Get("codex")
	if err := a.Prepare(Projection{
		Home:        home,
		ContextPath: "/wt/.aurium/CONTEXT.md",
		MCPCommand:  "aurium-mcp",
		AuriumURL:   "http://host.docker.internal:7770",
	}); err != nil {
		t.Fatal(err)
	}

	md := readFile(t, filepath.Join(home, ".codex", "AGENTS.md"))
	if !strings.Contains(md, "/wt/.aurium/CONTEXT.md") {
		t.Fatalf("AGENTS.md must reference the projection; got:\n%s", md)
	}

	toml := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(toml, "[mcp_servers.aurium]") {
		t.Fatalf("config.toml needs an [mcp_servers.aurium] table; got:\n%s", toml)
	}
	if !strings.Contains(toml, `command = "aurium-mcp"`) {
		t.Fatalf("config.toml must point at the shim; got:\n%s", toml)
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	home := t.TempDir()
	a, _ := DefaultRegistry().Get("claude")
	p := Projection{Home: home, ContextPath: "/wt/.aurium/CONTEXT.md", MCPCommand: "aurium-mcp"}

	for i := 0; i < 3; i++ {
		if err := a.Prepare(p); err != nil {
			t.Fatal(err)
		}
	}
	md := readFile(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if n := strings.Count(md, "@/wt/.aurium/CONTEXT.md"); n != 1 {
		t.Fatalf("re-running Prepare duplicated the import %d times:\n%s", n, md)
	}
}

// Prepare must not destroy instructions the user put in CLAUDE.md themselves.
func TestPreparePreservesExistingUserInstructions(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"),
		[]byte("# My own notes\nAlways use tabs.\n"), 0o644)

	a, _ := DefaultRegistry().Get("claude")
	if err := a.Prepare(Projection{Home: home, ContextPath: "/wt/CONTEXT.md", MCPCommand: "aurium-mcp"}); err != nil {
		t.Fatal(err)
	}

	md := readFile(t, filepath.Join(dir, "CLAUDE.md"))
	if !strings.Contains(md, "Always use tabs.") {
		t.Fatalf("Prepare destroyed the user's own instructions:\n%s", md)
	}
	if !strings.Contains(md, "@/wt/CONTEXT.md") {
		t.Fatalf("Prepare did not add the import:\n%s", md)
	}
}

func TestImageLayersInstallTheAgentCLI(t *testing.T) {
	r := DefaultRegistry()
	claude, _ := r.Get("claude")
	if !strings.Contains(claude.ImageLayer(), "@anthropic-ai/claude-code") {
		t.Errorf("claude image layer must install the CLI: %q", claude.ImageLayer())
	}
	codex, _ := r.Get("codex")
	if !strings.Contains(codex.ImageLayer(), "@openai/codex") {
		t.Errorf("codex image layer must install the CLI: %q", codex.ImageLayer())
	}
	shell, _ := r.Get("shell")
	if strings.TrimSpace(shell.ImageLayer()) != "" {
		t.Errorf("the shell adapter needs no image layer, got %q", shell.ImageLayer())
	}
}

func TestAuthEnvNamesMatchTheSpec(t *testing.T) {
	r := DefaultRegistry()
	claude, _ := r.Get("claude")
	got := strings.Join(claude.AuthEnv(), ",")
	if !strings.Contains(got, "ANTHROPIC_API_KEY") || !strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("claude AuthEnv = %v", claude.AuthEnv())
	}
	codex, _ := r.Get("codex")
	if !strings.Contains(strings.Join(codex.AuthEnv(), ","), "OPENAI_API_KEY") {
		t.Errorf("codex AuthEnv = %v", codex.AuthEnv())
	}
}

func TestLaunchCommandsAreTmuxSafe(t *testing.T) {
	r := DefaultRegistry()
	for _, name := range []string{"claude", "codex", "shell"} {
		a, _ := r.Get(name)
		cmd := a.LaunchCommand(StartOpts{})
		if len(cmd) == 0 {
			t.Fatalf("%s: empty launch command", name)
		}
		// The session must survive the agent exiting, or a crashed agent
		// silently takes the tmux window with it and the user sees nothing.
		if !strings.Contains(strings.Join(cmd, " "), "exec bash") {
			t.Errorf("%s launch command should fall back to a shell so a crash is visible: %v", name, cmd)
		}
	}
}

func TestResumeChangesTheClaudeCommand(t *testing.T) {
	a, _ := DefaultRegistry().Get("claude")
	fresh := strings.Join(a.LaunchCommand(StartOpts{}), " ")
	resumed := strings.Join(a.LaunchCommand(StartOpts{Resume: true}), " ")
	if fresh == resumed {
		t.Fatal("resume must change the command; restore depends on it (§6.3 step 6)")
	}
	if !strings.Contains(resumed, "--continue") {
		t.Fatalf("resume should use --continue, got %q", resumed)
	}
}

// MCPConfigPath is the one place that composes where a host-sandboxed turn's
// MCP config lives. Both the runtime, which points --mcp-config at it, and
// Prepare, which writes the file there, must call this rather than build the
// path themselves — two independent compositions is how they silently
// disagree and an agent starts with no tools and no error.
func TestMCPConfigPathIsUnderAgentsAgentID(t *testing.T) {
	got := MCPConfigPath("/home/user/.aurium", "ag_123")
	want := filepath.Join("/home/user/.aurium", "agents", "ag_123", "mcp.json")
	if got != want {
		t.Errorf("MCPConfigPath = %q, want %q", got, want)
	}
}

// The daemon serves MCP only at POST /mcp (internal/api/server.go); a bare
// base URL 404s/405s and a host-sandboxed turn gets no tools with no error
// naming why. cmd/aurium-mcp/main.go's forward already appends this same
// path to reach the daemon from inside a container; MCPEndpoint is the same
// computation for a client outside one.
func TestMCPEndpointAppendsThePathTheDaemonServesMCPOn(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"http://127.0.0.1:7770", "http://127.0.0.1:7770/mcp"},
		{"http://127.0.0.1:7770/", "http://127.0.0.1:7770/mcp"},
	} {
		if got := MCPEndpoint(tc.base); got != tc.want {
			t.Errorf("MCPEndpoint(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	return string(mustRead(t, p))
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return b
}
