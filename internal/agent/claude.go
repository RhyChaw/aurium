package agent

import "fmt"

// Claude adapts Anthropic's Claude Code CLI.
//
// File locations and flags verified against
// https://docs.claude.com/en/docs/claude-code/settings and
// https://docs.claude.com/en/docs/claude-code/mcp on 2026-09-08.
// The nightly "adapter smoke" CI job asserts the MCP server is still listed
// by the agent, so a change upstream fails loudly rather than silently
// disconnecting every container from Aurium (§15).
type Claude struct{}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) ImageLayer() string {
	return "RUN npm install -g @anthropic-ai/claude-code"
}

// AuthEnv: either an API key or an OAuth token from `claude setup-token`.
// Whichever is present on the host is injected into the container only.
func (c *Claude) AuthEnv() []string {
	return []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}
}

func (c *Claude) Capabilities() Caps {
	return Caps{MCP: true, Headless: true, Resume: true, Usage: true, Interactive: true}
}

// Prepare writes Appendix B's two files.
func (c *Claude) Prepare(p Projection) error {
	// Claude Code's @-import syntax: the agent re-reads the target whenever it
	// changes, so the daemon regenerating CONTEXT.md is enough to update the
	// agent's instructions without restarting it (§8.5).
	if err := ensureImport(p.Home, ".claude/CLAUDE.md",
		"@"+p.ContextPath, projectionHeader); err != nil {
		return fmt.Errorf("agent/claude: write CLAUDE.md: %w", err)
	}
	// The token is deliberately absent here; it arrives as /run/aurium/token
	// and $AURIUM_TOKEN so revoking it does not mean rewriting agent config.
	if err := writeFileIn(p.Home, ".claude.json",
		mcpServerJSON(p.MCPCommand, p.AuriumURL)); err != nil {
		return fmt.Errorf("agent/claude: write .claude.json: %w", err)
	}
	return nil
}

func (c *Claude) LaunchCommand(o StartOpts) []string {
	cmd := "claude"
	if o.Resume {
		// §6.3 step 6: the transcript lives in $HOME, which docker commit
		// captured, so --continue picks up exactly where the snapshot was taken.
		cmd += " --continue"
	}
	if o.Model != "" {
		cmd += " --model " + shellQuote(o.Model)
	}
	return interactiveWrap(cmd)
}

func (c *Claude) HeadlessCommand(prompt string, o ExecOpts) []string {
	args := []string{"claude", "-p", prompt, "--output-format", "json"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args
}
