package agent

import "fmt"

// Codex adapts OpenAI's Codex CLI.
//
// File locations verified against https://github.com/openai/codex (config.toml
// and AGENTS.md) on 2026-09-08. Covered by the nightly adapter smoke job.
type Codex struct{}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) ImageLayer() string {
	return "RUN npm install -g @openai/codex"
}

func (c *Codex) AuthEnv() []string { return []string{"OPENAI_API_KEY"} }

func (c *Codex) Capabilities() Caps {
	// Usage is false: the CLI does not report token counts in a stable form
	// yet. Claiming otherwise would make Phase D cost reports quietly wrong.
	return Caps{MCP: true, Headless: true, Resume: true, Usage: false, Interactive: true}
}

func (c *Codex) Prepare(p Projection) error {
	if err := ensureImport(p.Home, ".codex/AGENTS.md",
		"See "+p.ContextPath+" for this container's live Aurium context.",
		projectionHeader); err != nil {
		return fmt.Errorf("agent/codex: write AGENTS.md: %w", err)
	}

	toml := fmt.Sprintf(`# Written by Aurium. One MCP server: the aurium gateway.
[mcp_servers.aurium]
command = %q
args = []

[mcp_servers.aurium.env]
AURIUM_URL = %q
`, p.MCPCommand, p.AuriumURL)

	if err := writeFileIn(p.Home, ".codex/config.toml", toml); err != nil {
		return fmt.Errorf("agent/codex: write config.toml: %w", err)
	}
	return nil
}

func (c *Codex) LaunchCommand(o StartOpts) []string {
	cmd := "codex"
	if o.Resume {
		cmd += " resume --last"
	}
	if o.Model != "" {
		cmd += " --model " + shellQuote(o.Model)
	}
	return interactiveWrap(cmd)
}

func (c *Codex) HeadlessCommand(prompt string, o ExecOpts) []string {
	args := []string{"codex", "exec", prompt}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args
}
