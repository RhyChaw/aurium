package agent

import (
	"fmt"

	"github.com/RhyChaw/aurium/internal/gateway"
)

// hostAllowedTool is the one MCP tool a host-sandboxed turn may call. Claude
// Code namespaces an MCP tool as mcp__<server key>__<the server's own tool
// name, verbatim> — confirmed empirically against a real `claude -p` run
// (see documents/superpowers/plans/2026-09-08-aurium-phases-abc.md's task-6
// report): the server key is "aurium" (mcpServerJSON's and
// writeHostMCPConfig's key, below), and the tool name is gateway's own
// AuriumExecTool, not a shortened or prefix-stripped form of it. Built from
// that constant, rather than typed out a second time, so this cannot drift
// from the name the gateway actually dispatches on the way "mcp__aurium__exec"
// once did.
const hostAllowedTool = "mcp__aurium__" + gateway.AuriumExecTool

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
// Tracked in documents/superpowers/plans/2026-09-08-aurium-phases-abc.md.
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
	if err := ensureImport(p, ".claude/CLAUDE.md",
		"@"+p.ContextPath, projectionHeader); err != nil {
		return fmt.Errorf("agent/claude: write CLAUDE.md: %w", err)
	}
	// The token is deliberately absent here; it arrives as /run/aurium/token
	// and $AURIUM_TOKEN so revoking it does not mean rewriting agent config.
	if err := writeFileIn(p, ".claude.json",
		mcpServerJSON(p.MCPCommand, p.AuriumURL)); err != nil {
		return fmt.Errorf("agent/claude: write .claude.json: %w", err)
	}

	if p.HostSandboxed {
		// The host placement notice is deliberately NOT written here. It used
		// to be appended to the file above, under the CONTAINER's $HOME — a
		// path a host process never reads, since runHost launches it with the
		// developer's environment and nothing sets HOME. It travels in argv
		// now instead (HeadlessCommand's --append-system-prompt), which is the
		// only channel a host turn actually has.
		//
		// HeadlessCommand points --mcp-config at exactly this file
		// (agent.MCPConfigPath); without writing it, a host-sandboxed turn's
		// only tool is missing and it has no shell to fall back to either.
		if err := writeHostMCPConfig(p); err != nil {
			return fmt.Errorf("agent/claude: write host mcp config: %w", err)
		}
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
	if o.HostSandboxed {
		// Verified by spike before this was designed: the agent uses the MCP
		// tool unprompted once its own shell is gone, and degrades gracefully
		// rather than failing when no alternative exists at all.
		args = append(args,
			"--disallowedTools", "Bash",
			"--allowedTools", hostAllowedTool,
			"--mcp-config", o.MCPConfigPath,
			// Without this --mcp-config is ADDITIVE: the host process would
			// also load the developer's user-scope MCP servers and any
			// .mcp.json sitting in the worktree. mcpServerJSON states the
			// invariant it would break — exactly one server is registered
			// (D16), because otherwise an agent reaches upstreams without
			// passing the gateway's grant checks (§10.1). Verified against
			// the CLI: "--strict-mcp-config  Only use MCP servers from
			// --mcp-config, ignoring all other MCP configurations".
			"--strict-mcp-config",
			// The container's ~/.claude/CLAUDE.md reaches this process
			// nowhere (see Prepare), so the notice and the context
			// projection are delivered here, where the process cannot miss
			// them.
			"--append-system-prompt", hostSystemPrompt(o.ProjectContext),
			// And rendered fresh on every turn, not once per conversation.
			// The in-container path's @-import gives §8.5 its defining
			// property: the daemon regenerating CONTEXT.md updates the
			// agent's instructions without restarting it. Delivered in argv
			// that property is not free — the CLI documents
			// --system-prompt-snapshot's default as recording the prompt on
			// a conversation's FIRST request and resending that record "as
			// is, even when a later launch passes different text". Every
			// turn after the first is a --continue, so without this the
			// projection would freeze at whatever it said when the
			// conversation began.
			"--system-prompt-snapshot", "off")
	}
	args = append(args, "-p", prompt, "--output-format", "json")
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args
}
