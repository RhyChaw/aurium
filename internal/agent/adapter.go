// Package agent adapts coding agents to Aurium.
//
// D16: an agent integrates through exactly two channels — a generated
// instruction file and one MCP server. Nothing here is provider-specific
// beyond file locations and a launch command, which is what keeps a new
// adapter to roughly two hundred lines.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Caps advertises what an adapter's underlying CLI can do (§9.1).
type Caps struct {
	// MCP means the agent can load an MCP server, which is how it reaches
	// every Aurium tool and every granted integration.
	MCP bool
	// Headless means it can run a single prompt non-interactively, which
	// serial delegation (§9.4) requires.
	Headless bool
	// Resume means it can pick up a previous conversation, which is what
	// makes restore useful (§6.3).
	Resume bool
	// Usage means it reports token usage, for Phase D cost tracking.
	Usage bool
	// Interactive means it runs as a REPL in tmux.
	Interactive bool
}

// Projection is everything an adapter needs to wire a container to Aurium.
type Projection struct {
	// Home is the container's $HOME. Everything written here is per-container
	// and is captured by `docker commit`, which is why an agent's transcript
	// survives a snapshot (§1.1 finding 1).
	Home string
	// ContextPath is the generated CONTEXT.md inside the worktree (§8.5).
	ContextPath string
	// MCPCommand is the in-container shim binary.
	MCPCommand string
	// AuriumURL is where the shim forwards JSON-RPC.
	AuriumURL string
	// Token is the container's scoped bearer token. Adapters must NOT write it
	// to disk: it is delivered as /run/aurium/token and $AURIUM_TOKEN so it can
	// be revoked without rewriting agent config.
	Token string
}

// StartOpts controls launching an agent.
type StartOpts struct {
	// Resume continues a previous conversation (used by restore).
	Resume bool
	Model  string
	Prompt string
}

// ExecOpts controls a headless one-shot run.
type ExecOpts struct {
	Model   string
	Timeout int
	// Continue resumes the agent's previous conversation rather than starting
	// a fresh one, which is what makes a sequence of headless turns a
	// conversation instead of a series of strangers. The first turn must not
	// set it: there is no session to continue, and asking to continue nothing
	// is an error rather than a fresh start.
	Continue bool
	// HostSandboxed says this turn runs on the host rather than in its
	// container. The agent's own shell is denied and its commands go back into
	// the container through aurium_exec, so the container remains the only place
	// project commands run.
	HostSandboxed bool
	// MCPConfigPath points at the JSON naming the daemon's MCP endpoint. Only
	// read when HostSandboxed.
	MCPConfigPath string
}

// ExecResult is the outcome of a headless run.
type ExecResult struct {
	Output   string
	ExitCode int
	// InputTokens and OutputTokens are zero when the CLI does not report them.
	InputTokens  int
	OutputTokens int
}

// Status is an agent's observed state.
type Status struct {
	State string // running | idle | exited | blocked
	Since string
	// ExitCode is meaningful only when State is "exited".
	ExitCode int
	// BlockedReason is set when the agent signalled it needs a human.
	BlockedReason string
}

// Adapter is the §9.1 interface.
//
// Note what is absent: nothing about context. Context is the runtime's job,
// and putting a getContext() here would re-couple context to providers, which
// §65 of the spec forbids (§1.1 finding 5).
type Adapter interface {
	Name() string
	// ImageLayer is a Dockerfile RUN fragment installing the agent CLI.
	ImageLayer() string
	// AuthEnv names host environment variables to pass through; at least one
	// must be set for the adapter to work.
	AuthEnv() []string
	// Prepare writes the instruction file and MCP config into the container
	// $HOME. It must be idempotent and must not clobber the user's own files.
	Prepare(p Projection) error
	// LaunchCommand is the argv run inside tmux.
	LaunchCommand(o StartOpts) []string
	// HeadlessCommand is the argv for a one-shot run; nil when unsupported.
	HeadlessCommand(prompt string, o ExecOpts) []string
	Capabilities() Caps
}

// MCPConfigPath is where a host-sandboxed turn's MCP config lives:
// ~/.aurium/agents/<agentID>/mcp.json.
//
// This is the ONE place that composes this path. The runtime points
// ExecOpts.MCPConfigPath at exactly what this returns, and Prepare writes the
// file there; both must call this rather than build the path themselves, or
// they can silently disagree and an agent starts with no tools and no error.
func MCPConfigPath(auriumHome, agentID string) string {
	return filepath.Join(auriumHome, "agents", agentID, "mcp.json")
}

// Registry maps adapter names to implementations.
type Registry map[string]Adapter

// Get returns the named adapter.
func (r Registry) Get(name string) (Adapter, bool) {
	a, ok := r[name]
	return a, ok
}

// Register adds an adapter.
func (r Registry) Register(a Adapter) { r[a.Name()] = a }

// Names lists registered adapters.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for n := range r {
		out = append(out, n)
	}
	return out
}

// DefaultRegistry returns the built-in adapters.
func DefaultRegistry() Registry {
	r := Registry{}
	r.Register(&Claude{})
	r.Register(&Codex{})
	r.Register(&Shell{})
	return r
}

// ---- shared helpers ----

// writeFileIn writes a file under home, creating parents.
func writeFileIn(home, rel, content string) error {
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// ensureImport appends a line to a file exactly once, preserving whatever the
// user already wrote there. Agents' instruction files are user-editable, so
// Prepare must be additive, not authoritative.
func ensureImport(home, rel, line, header string) error {
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	existing, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), line) {
		return nil // already imported
	}

	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" {
		body += "\n"
	}
	body += header + "\n" + line + "\n"
	return os.WriteFile(p, []byte(body), 0o644)
}

// interactiveWrap turns an agent command into a tmux-safe command line.
//
// The trailing `exec bash -l` matters: without it, an agent that crashes or
// exits takes the tmux window with it and the user attaches to nothing,
// with no output and no clue what happened.
func interactiveWrap(cmd string) []string {
	return []string{"sh", "-lc", cmd + "; exec bash -l"}
}

// mcpServerJSON renders the single-server MCP config shared by adapters that
// use JSON. Exactly one server is registered (D16): if Aurium listed upstream
// servers directly, an agent could call them without passing the gateway's
// grant checks (§10.1).
func mcpServerJSON(command, auriumURL string) string {
	return fmt.Sprintf(`{
  "mcpServers": {
    "aurium": {
      "command": %q,
      "args": [],
      "env": {
        "AURIUM_URL": %q
      }
    }
  }
}
`, command, auriumURL)
}

// projectionHeader explains the imported file to whoever opens it.
const projectionHeader = `<!-- Added by Aurium. The file below is regenerated by auriumd whenever
     this container's context changes; do not edit it directly. -->`
