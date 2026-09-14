package agent

import "fmt"

// Claude adapts Anthropic's Claude Code CLI.
//
// UNVERIFIED. The file locations and flags below are written from memory and
// have NOT been checked against the published documentation, and no agent has
// ever been observed loading them. §15 of the ERD requires a doc link with a
// verification date and a nightly smoke job asserting the agent actually lists
// the aurium MCP server; neither exists yet. Until they do, treat every path
// here as a guess:
//
//	~/.claude/CLAUDE.md   import syntax and location unconfirmed
//	~/.claude.json        mcpServers schema and location unconfirmed
//	claude --continue     resume flag unconfirmed
//
// Tracked in docs/superpowers/plans/2026-09-08-aurium-phases-abc.md.
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
	args := []string{"claude"}
	if o.Continue {
		// UNVERIFIED, like everything else here: --continue in -p mode is
		// documented as resuming the most recent conversation in the working
		// directory, which is per-container because the worktree is.
		args = append(args, "--continue")
	}
	args = append(args, "-p", prompt, "--output-format", "json")
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args
}
