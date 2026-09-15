// Package agent adapts coding agents to Aurium.
//
// D16: an agent integrates through exactly two channels — a generated
// instruction file and one MCP server. Nothing here is provider-specific
// beyond file locations and a launch command, which is what keeps a new
// adapter to roughly two hundred lines.
package agent

import (
	"errors"
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
	//
	// It names a path INSIDE the container, so it is not a path this process
	// may hand to os.WriteFile. Write through FS below, never through Home.
	Home string
	// FS is the filesystem Prepare's files are written to. The runtime sets
	// it: a container-backed driver gets one that writes through the driver,
	// because Home is a path in the container's rootfs and not on this
	// machine. Nil falls back to the host's filesystem rooted at Home, which
	// is right only where Home really is a host directory — the local driver,
	// and host placement.
	FS HomeFS
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

	// HostSandboxed says this agent's turns run on the host rather than
	// inside its container (agent_placement: host). Home and ContextPath
	// above describe the container, which such an agent never even starts
	// in; only when this is true does Prepare also write the host-side MCP
	// config named by AgentID and AuriumHome, below. A host process has no
	// /run/aurium/token to read Token from instead, so here Prepare writes
	// it into that config's HTTP headers (Authorization: Bearer ...) — the
	// one place this design puts the bearer token on disk, and only on the
	// host's own machine.
	HostSandboxed bool
	// AgentID and AuriumHome combine, through MCPConfigPath, to say where a
	// host-sandboxed turn's MCP config belongs — the same computation the
	// runtime uses for ExecOpts.MCPConfigPath, so the two cannot disagree.
	// Only read when HostSandboxed.
	AgentID    string
	AuriumHome string
	// HostAuriumURL is the daemon's BASE address as reached from the host
	// itself — no /mcp suffix, the same convention cmd/aurium-mcp/main.go's
	// cfg.URL follows. AuriumURL's host.docker.internal resolves only
	// inside a container, so a host-sandboxed turn needs its own base
	// address for the same daemon; MCPEndpoint appends the actual route
	// when the client-facing config is built. Only read when HostSandboxed.
	HostAuriumURL string
}

// StartOpts controls launching an agent.
type StartOpts struct {
	// Resume continues a previous conversation (used by restore).
	Resume bool
	Model  string
	Prompt string
	// AgentID pins the created agent row to an id the caller already
	// computed, rather than letting the store generate one. Create needs
	// this: Prepare writes a host-sandboxed turn's MCP config at a path
	// keyed by agent id (agent.MCPConfigPath) before the agent row exists,
	// so the id must be chosen first and carried here to keep the two in
	// agreement. Empty generates one as before.
	AgentID string
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
	// ProjectContext is the rendered .aurium/CONTEXT.md — the agent's
	// objective, constraints and stack position.
	//
	// It is carried here, as text, because a host turn has no other way to
	// receive it. The in-container path delivers it as an @-import in the
	// container's ~/.claude/CLAUDE.md, which only works because $HOME there is
	// the one Prepare wrote into. A host process runs with the developer's own
	// environment and reads the developer's ~/.claude/CLAUDE.md, so anything
	// written under the container's $HOME reaches it nowhere. Only read when
	// HostSandboxed.
	ProjectContext string
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

// HomeFS is the filesystem an agent's $HOME lives on.
//
// It exists because the daemon and the agent do not share one. Projection.Home
// is a path inside the container ("/home/aurium"); calling os.WriteFile on it
// writes to a same-named path on the HOST, which is a different filesystem
// with different contents. On macOS that fails outright — /home is autofs, so
// the daemon reports `mkdir /home/aurium: operation not supported` and no
// agent can be created at all. On Linux it is worse, because it can succeed:
// the instructions land in the host's /home/aurium and the container, which
// looks in its own rootfs, finds nothing. A silent, correct-looking no-op.
//
// So the runtime says where the files go rather than the adapter assuming.
type HomeFS interface {
	// ReadFile returns the file's contents, or nil with a nil error when it
	// does not exist. Absence is the ordinary case on a fresh home and is not
	// worth making every caller restate.
	ReadFile(rel string) ([]byte, error)
	// WriteFile writes content at rel, creating parent directories.
	WriteFile(rel, content string) error
}

// OSHome is a HomeFS rooted at a real directory on this machine. It is correct
// wherever $HOME is genuinely host-side: the local driver, which has no
// rootfs, and host placement.
type OSHome struct{ Root string }

func (h OSHome) ReadFile(rel string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(h.Root, rel))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

func (h OSHome) WriteFile(rel, content string) error {
	p := filepath.Join(h.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// home is the filesystem this projection's files belong on. A nil FS means the
// host's own, rooted at Home — see the field comment for when that is right.
func (p Projection) home() HomeFS {
	if p.FS != nil {
		return p.FS
	}
	return OSHome{Root: p.Home}
}

// ---- shared helpers ----

// writeFileIn writes a file under the projection's home, creating parents.
func writeFileIn(p Projection, rel, content string) error {
	return p.home().WriteFile(rel, content)
}

// ensureImport appends a line to a file exactly once, preserving whatever the
// user already wrote there. Agents' instruction files are user-editable, so
// Prepare must be additive, not authoritative.
func ensureImport(p Projection, rel, line, header string) error {
	fsys := p.home()
	existing, err := fsys.ReadFile(rel)
	if err != nil {
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
	return fsys.WriteFile(rel, body)
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

// MCPEndpoint turns a daemon base URL into the one route it actually serves
// MCP on: POST /mcp (internal/api/server.go registers nothing at "/" but a
// GET). cmd/aurium-mcp/main.go's forward already does exactly this
// (strings.TrimRight(cfg.URL, "/")+"/mcp"); this mirrors it so the two
// clients of the daemon's base URL cannot compute two different endpoints.
// Exported so a test can check it against the route the daemon actually
// registers, not just against itself.
//
// A base URL (HostAuriumURL, AuriumURL, cmd/aurium-mcp's cfg.URL) stays a
// base — other readers may depend on that — so the path is appended only
// here, where a client-facing config is built.
func MCPEndpoint(base string) string {
	return strings.TrimRight(base, "/") + "/mcp"
}

// hostMCPServerJSON is mcpServerJSON's counterpart for a turn that runs on the
// host rather than in a container. It spawns no binary at all: the
// aurium-mcp shim exists only to bridge stdio to HTTP from inside a
// container, where nothing can reach the daemon's socket directly (§10's
// host placement design). A process already running on the host has no such
// problem — it names the daemon's /mcp endpoint over HTTP transport
// directly. Shape confirmed empirically against `claude mcp add
// --transport http --scope project aurium <url> --header "Authorization:
// Bearer <token>"` and reading the .mcp.json it wrote (see the task-6
// report): {"mcpServers": {"aurium": {"type": "http", "url": ..., "headers":
// {"Authorization": "Bearer ..."}}}}.
func hostMCPServerJSON(auriumBaseURL, token string) string {
	return fmt.Sprintf(`{
  "mcpServers": {
    "aurium": {
      "type": "http",
      "url": %q,
      "headers": {
        "Authorization": %q
      }
    }
  }
}
`, MCPEndpoint(auriumBaseURL), "Bearer "+token)
}

// ErrNoHostToken is returned when a host-sandboxed agent is prepared without a
// bearer token.
//
// An in-container agent with no token loses the gateway tools but keeps its own
// Bash and stays useful for project work. A host-sandboxed one has its shell
// denied as well, so a config carrying "Authorization: Bearer " — which the API
// middleware answers with a 401 — leaves it with no tools and no shell at all.
// The CLI reports that as a perfectly normal, empty reply: `claude -p
// --output-format json` returns "is_error": false with no permission_denials
// when its only MCP server is unauthorized. Nothing downstream can tell that
// apart from a working turn, so the only place it can be caught is here,
// before the config is written.
var ErrNoHostToken = errors.New(
	"agent: a host-sandboxed agent needs a gateway token, and none was issued; " +
		"without one its only MCP server is refused and, with its own shell denied, " +
		"it would run with no tools at all")

// writeHostMCPConfig writes the file agent.MCPConfigPath names — the ONE
// place that composes that path, called here exactly as the runtime calls it
// for ExecOpts.MCPConfigPath, so Prepare and HeadlessCommand cannot disagree
// about where it lives. Called only when p.HostSandboxed.
func writeHostMCPConfig(p Projection) error {
	if p.Token == "" {
		return ErrNoHostToken
	}
	path := MCPConfigPath(p.AuriumHome, p.AgentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hostMCPServerJSON(p.HostAuriumURL, p.Token)), 0o644)
}

// projectionHeader explains the imported file to whoever opens it.
const projectionHeader = `<!-- Added by Aurium. The file below is regenerated by auriumd whenever
     this container's context changes; do not edit it directly. -->`

// hostPlacementNotice tells a host-sandboxed agent why its shell is gone and
// where its commands actually run.
//
// The spike (2026-09-14) showed an agent meeting `go: command not found` will
// otherwise spend turns searching /usr/local/go/bin, /opt/homebrew/bin/go and
// ~/go/bin before concluding — the environment split is intended, but an
// agent not told about it treats it as a broken machine.
const hostPlacementNotice = `Your shell runs inside this project's container, not on the host machine.
Use the aurium_exec tool for every command. The container has the project's
toolchain; the host may not have it at all. If a command reports that a tool
is missing, that is the container's environment telling you something true —
do not go looking for it elsewhere on the machine.`

// contextHeading introduces the projection inlined into a host turn's system
// prompt, so the agent can tell Aurium's context from the rest of the prompt.
const contextHeading = "This container's Aurium context (.aurium/CONTEXT.md):"

// hostSystemPrompt is what a host-sandboxed turn receives through
// --append-system-prompt: the placement notice, plus the context projection
// the in-container path delivers as an @-import.
//
// Both travel in argv rather than on disk. A host turn is launched with the
// developer's own environment — nothing sets HOME — so it reads the
// developer's ~/.claude/CLAUDE.md, never the one Prepare wrote under the
// container's $HOME. Redirecting HOME instead would cohere only for the
// `local` driver, whose $HOME is a host directory; under Docker it is a path
// inside the rootfs that does not exist out here at all.
func hostSystemPrompt(projectContext string) string {
	prompt := hostPlacementNotice
	if body := strings.TrimSpace(projectContext); body != "" {
		prompt += "\n\n" + contextHeading + "\n\n" + body
	}
	return prompt
}
