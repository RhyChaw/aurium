package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

// echoAdapter stands in for `claude -p --output-format json`.
//
// Its headless command is `printf` with a canned envelope, which makes the
// whole turn — driver exec, envelope parse, reply, usage, status transitions —
// testable without a model, a network or a credential. The envelope below is
// the shape of a REAL run, captured on 2026-09-14; if the CLI changes, the
// adapter's own parser tests are what should fail, not this one.
type echoAdapter struct {
	envelope string
	exitCode int
	// lastCmd records what was asked for, so continuation can be asserted.
	lastCmd []string
	// lastOpts records the ExecOpts a turn was built with, so placement can
	// be asserted without parsing argv.
	lastOpts agent.ExecOpts
}

func (e *echoAdapter) Name() string                   { return "echo" }
func (e *echoAdapter) ImageLayer() string             { return "" }
func (e *echoAdapter) AuthEnv() []string              { return nil }
func (e *echoAdapter) Prepare(agent.Projection) error { return nil }
func (e *echoAdapter) LaunchCommand(agent.StartOpts) []string {
	return []string{"sh", "-c", "true"}
}

func (e *echoAdapter) Capabilities() agent.Caps {
	return agent.Caps{Headless: true, Usage: true}
}

func (e *echoAdapter) HeadlessCommand(prompt string, o agent.ExecOpts) []string {
	e.lastOpts = o
	body := strings.ReplaceAll(e.envelope, "PROMPT", prompt)
	script := fmt.Sprintf("cat <<'EOF'\n%s\nEOF\n", body)
	if e.exitCode != 0 {
		script += fmt.Sprintf("exit %d\n", e.exitCode)
	}
	cmd := []string{"sh", "-c", script}
	if o.Continue {
		cmd = append(cmd, "--continued")
	}
	e.lastCmd = cmd
	return cmd
}

func (e *echoAdapter) ParseUsage(stdout string) (agent.UsageReport, bool) {
	c := &agent.Claude{}
	return c.ParseUsage(stdout)
}

func (e *echoAdapter) ParseReply(stdout string) (string, bool) {
	c := &agent.Claude{}
	return c.ParseReply(stdout)
}

func (e *echoAdapter) ReportedError(stdout string) bool {
	c := &agent.Claude{}
	return c.ReportedError(stdout)
}

func envelope(result string, isError bool, in, out int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "result": result, "is_error": isError,
		"total_cost_usd": 0.25,
		"modelUsage":     map[string]any{"claude-opus-5": map[string]any{}},
		"usage": map[string]any{
			"input_tokens": in, "output_tokens": out,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	})
	return string(b)
}

// converseFixture builds a real Manager on the local driver with a real store,
// a real IPC bus and a real usage meter — only the model is a stub.
func converseFixture(t *testing.T, ad *echoAdapter) (*Manager, store.Container, store.Agent) {
	t.Helper()

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "T"},
		{"commit", "-qm", "initial", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "aurium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	bus := events.New(st)
	msgs := ipc.New(st, bus, nil)
	m := &Manager{
		Store: st, Events: bus,
		Drivers:  driver.Registry{"local": driver.NewLocal()},
		Adapters: agent.Registry{"echo": ad},
		HomeRoot: t.TempDir(),
		IPC:      msgs,
	}

	ctx := context.Background()
	p, _ := st.CreateProject(ctx, "app", root)
	repo, _ := st.CreateRepository(ctx, p.ID, root, "main", "")

	cfg, err := config.Parse([]byte(config.Template("app", "main", "local", "x", "shell")))
	if err != nil {
		t.Fatal(err)
	}

	c, err := m.Create(ctx, CreateOpts{
		ProjectID: p.ID, RepoID: repo.ID, RepoRoot: root,
		Branch: "feature", ParentBranch: "main", Config: cfg,
		Adapter: "echo", Role: store.RolePrimary, OriginKind: store.OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	agents, err := st.ListAgents(ctx, c.ID)
	if err != nil || len(agents) != 1 {
		t.Fatalf("want one agent, got %v (%v)", agents, err)
	}
	return m, c, agents[0]
}

func transcript(t *testing.T, m *Manager, agentID string) []ipc.Message {
	t.Helper()
	msgs, err := m.IPC.History(context.Background(), agentID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// The whole point: a human types, and an answer comes back in the transcript.
func TestConverseAnswersInTheTranscript(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("the answer is 42", false, 1200, 40)}
	m, _, a := converseFixture(t, ad)
	ctx := context.Background()

	if err := m.Converse(ctx, a.ID, "what is the answer?"); err != nil {
		t.Fatal(err)
	}

	msgs := transcript(t, m, a.ID)
	if len(msgs) != 1 {
		t.Fatalf("want one reply, got %d", len(msgs))
	}
	// The envelope is unwrapped: showing the user raw JSON and letting them
	// find the sentence themselves is relocating an answer, not giving one.
	if msgs[0].Content != "the answer is 42" {
		t.Fatalf("reply = %q", msgs[0].Content)
	}
	if msgs[0].Type != ipc.TypeResponse {
		t.Fatalf("type = %q, want RESPONSE", msgs[0].Type)
	}
	if !msgs[0].To.Human {
		t.Fatal("a reply to a human must be addressed to one, or it lands in no inbox")
	}

	got, err := m.Store.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.AgentIdle {
		t.Fatalf("after answering, status = %q, want idle (green)", got.Status)
	}
}

// A failure reported inside a zero exit is still a failure. Painting that tile
// green would be a lie, and burying the reason in JSON would be useless.
func TestConverseSurfacesAnErrorReportedInsideAZeroExit(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("Not logged in · Please run /login", true, 0, 0)}
	m, _, a := converseFixture(t, ad)
	ctx := context.Background()

	if err := m.Converse(ctx, a.ID, "hello"); err != nil {
		t.Fatal(err)
	}

	msgs := transcript(t, m, a.ID)
	if len(msgs) != 1 {
		t.Fatalf("want one message, got %d", len(msgs))
	}
	if msgs[0].Type != ipc.TypeWarning {
		t.Fatalf("a failed turn must not read as a normal reply: type = %q", msgs[0].Type)
	}
	if !strings.Contains(msgs[0].Content, "Not logged in") {
		t.Fatalf("the agent's own explanation must survive: %q", msgs[0].Content)
	}
	// An auth failure knows what is wrong and nothing about where Aurium keeps
	// credentials; the two halves belong together.
	if !strings.Contains(msgs[0].Content, "Providers tab") {
		t.Fatalf("an auth failure must point at the fix: %q", msgs[0].Content)
	}

	got, _ := m.Store.GetAgent(ctx, a.ID)
	if got.Status != store.AgentError {
		t.Fatalf("status = %q, want error (red)", got.Status)
	}
}

func TestConverseMetersTheTurn(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("done", false, 1000, 100)}
	m, _, a := converseFixture(t, ad)
	m.Usage = &recordingMeter{}
	ctx := context.Background()

	if err := m.Converse(ctx, a.ID, "go"); err != nil {
		t.Fatal(err)
	}

	rec := m.Usage.(*recordingMeter)
	if len(rec.calls) != 1 {
		t.Fatalf("want one metered call, got %d", len(rec.calls))
	}
	got := rec.calls[0]
	if got.InputTokens != 1000 || got.OutputTokens != 100 {
		t.Fatalf("tokens = %d/%d", got.InputTokens, got.OutputTokens)
	}
	// The envelope has no top-level model; it lives in modelUsage, and a usage
	// row with no model cannot be priced or grouped.
	if got.Model != "claude-opus-5" {
		t.Fatalf("model = %q", got.Model)
	}
	if !got.HasCost || got.CostUSD != 0.25 {
		t.Fatalf("the provider's own cost figure must win: %v (has=%v)", got.CostUSD, got.HasCost)
	}
	if got.AgentID != a.ID {
		t.Fatalf("usage must be attributed to the agent, got %q", got.AgentID)
	}
}

// The first turn has no session, and asking to continue nothing is an error
// rather than a fresh start. Every turn after it is a continuation, which is
// what makes a sequence of headless runs a conversation.
func TestConverseContinuesAfterTheFirstTurn(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 10, 10)}
	m, _, a := converseFixture(t, ad)
	ctx := context.Background()

	if err := m.Converse(ctx, a.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if last := strings.Join(ad.lastCmd, " "); strings.Contains(last, "--continued") {
		t.Fatal("the first turn must not ask to continue a session that does not exist")
	}

	if err := m.Converse(ctx, a.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if last := strings.Join(ad.lastCmd, " "); !strings.Contains(last, "--continued") {
		t.Fatalf("the second turn must continue the conversation: %q", last)
	}
}

// Two turns at once in one worktree is the collision D15 exists to prevent,
// arriving by a different road.
func TestConverseRefusesAConcurrentTurn(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 10, 10)}
	m, _, a := converseFixture(t, ad)

	// Hold the slot the way a running turn does.
	inFlight.Store(a.ID, struct{}{})
	defer inFlight.Delete(a.ID)

	if err := m.Converse(context.Background(), a.ID, "hello"); err != ErrBusy {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestConverseRefusesAnAdapterThatCannotAnswer(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, c, a := converseFixture(t, ad)

	// Swap in an adapter with no headless mode, as `shell` is.
	m.Adapters["echo"] = &noHeadless{}
	err := m.Converse(context.Background(), a.ID, "hello")
	if err == nil {
		t.Fatal("an adapter that cannot run headless must say so")
	}
	// The message has to name the way out, which is a terminal.
	if !strings.Contains(err.Error(), "aurium attach "+c.ID) {
		t.Fatalf("the refusal must point at attach: %v", err)
	}
}

// In-container placement is the default and must behave exactly as it did
// before host placement existed: no resolver is even required for it, and a
// turn must not enable the shell denial meant only for the host.
func TestConverseInContainerPlacementLeavesHostSandboxedFalse(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	// m.Config is intentionally left nil: this is the state of every caller
	// before this field existed, and it must resolve to in-container.

	if err := m.Converse(context.Background(), a.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if ad.lastOpts.HostSandboxed {
		t.Error("in-container placement must not set HostSandboxed")
	}
	if ad.lastOpts.MCPConfigPath != "" {
		t.Errorf("in-container placement must not set an MCP config path, got %q", ad.lastOpts.MCPConfigPath)
	}
}

// fakeGateway stands in for auriumd's POST /mcp for the host preflight: a
// turn that cannot reach its gateway never starts, so every host-placement
// test needs one that works.
//
// tools controls what it advertises and token what it accepts, so a test can
// reproduce each of the three ways a host turn silently ends up with no tools.
type fakeGateway struct {
	tools []string
	token string
	calls int
}

func (g *fakeGateway) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		g.calls++
		if g.token != "" && r.Header.Get("Authorization") != "Bearer "+g.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		list := make([]map[string]any, 0, len(g.tools))
		for _, name := range g.tools {
			list = append(list, map[string]any{"name": name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"tools": list},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// hostPlaced switches a converse fixture to host placement with a working
// gateway: the config the agent will present is written where
// HeadlessCommand will point --mcp-config, exactly as Prepare writes it.
func hostPlaced(t *testing.T, m *Manager, agentID string, gw *fakeGateway) *httptest.Server {
	t.Helper()
	srv := gw.start(t)
	m.SnapshotHome = t.TempDir()
	m.HostAuriumURL = srv.URL
	m.Config = func(context.Context, string) (*config.Config, error) {
		return &config.Config{Sandbox: config.Sandbox{AgentPlacement: config.PlacementHost}}, nil
	}
	writeHostMCPConfigForTest(t, agent.MCPConfigPath(m.SnapshotHome, agentID), srv.URL, gw.token)
	return srv
}

func writeHostMCPConfigForTest(t *testing.T, path, baseURL, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		`{"mcpServers":{"aurium":{"type":"http","url":%q,"headers":{"Authorization":%q}}}}`,
		agent.MCPEndpoint(baseURL), "Bearer "+token)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Host placement is the one case where HeadlessCommand denies the agent's own
// shell (§ agent.ExecOpts.HostSandboxed), which is the whole security property
// of the design. If this is ever false under host placement, the agent's
// shell runs fully enabled on the host.
func TestConverseHostPlacementSetsHostSandboxedAndMCPConfigPath(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	hostPlaced(t, m, a.ID, &fakeGateway{tools: []string{"aurium_exec"}, token: "tok_test"})

	if err := m.Converse(context.Background(), a.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if !ad.lastOpts.HostSandboxed {
		t.Fatal("host placement must set HostSandboxed")
	}
	want := agent.MCPConfigPath(m.SnapshotHome, a.ID)
	if ad.lastOpts.MCPConfigPath != want {
		t.Errorf("MCPConfigPath = %q, want %q", ad.lastOpts.MCPConfigPath, want)
	}
	if ad.lastOpts.MCPConfigPath == "" {
		t.Fatal("host placement must never produce an empty MCPConfigPath")
	}
}

// HostSandboxed and MCPConfigPath must be set together or not at all: the
// combination of HostSandboxed=true with an empty path makes HeadlessCommand
// emit a malformed `--mcp-config ""`, which fails far from its cause. Rather
// than let that happen, a turn that cannot name the path fails clearly here.
func TestConverseHostPlacementErrorsRatherThanEmitAnEmptyMCPConfigPath(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	m.SnapshotHome = "" // no way to name the path
	m.Config = func(context.Context, string) (*config.Config, error) {
		return &config.Config{Sandbox: config.Sandbox{AgentPlacement: config.PlacementHost}}, nil
	}

	err := m.Converse(context.Background(), a.ID, "hello")
	if err == nil {
		t.Fatal("host placement with no way to name the MCP config path must be an error")
	}
	if ad.lastCmd != nil {
		t.Error("the turn must not have run at all")
	}

	got, _ := m.Store.GetAgent(context.Background(), a.ID)
	if got.Status == store.AgentRunning {
		t.Errorf("status must not be left running after the turn failed to start, got %q", got.Status)
	}
}

// A resolver that fails must not be absorbed into a quiet fallback to
// in-container: a project on host placement because the container does not
// fit on the machine would then get a container anyway, with nothing
// connecting the eventual memory failure to a config read that failed here.
func TestConverseFailsWhenPlacementCannotBeResolved(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	resolverErr := fmt.Errorf("aurium.yaml: permission denied")
	m.Config = func(context.Context, string) (*config.Config, error) {
		return nil, resolverErr
	}

	err := m.Converse(context.Background(), a.ID, "hello")
	if err == nil {
		t.Fatal("a failed placement resolver must fail the turn, not fall back to in-container")
	}
	if !strings.Contains(err.Error(), "placement") {
		t.Errorf("error must name placement/config resolution so the cause is findable: %v", err)
	}
	if !errors.Is(err, resolverErr) {
		t.Errorf("the resolver's own error must survive: %v", err)
	}
	if ad.lastCmd != nil {
		t.Error("the turn must not have run at all")
	}

	got, _ := m.Store.GetAgent(context.Background(), a.ID)
	if got.Status != store.AgentError {
		t.Errorf("status = %q, want error", got.Status)
	}
}

// The delivery test. A host turn reads the DEVELOPER's ~/.claude/CLAUDE.md,
// never the one Prepare wrote under the container's $HOME, so asserting that
// file was written proves nothing about what the agent receives. This asserts
// the argv the process is actually launched with — the ExecOpts the runtime
// built, run through the real Claude adapter that builds the command line.
func TestConverseHostPlacementPutsTheNoticeAndContextInTheArgv(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, c, a := converseFixture(t, ad)
	hostPlaced(t, m, a.ID, &fakeGateway{tools: []string{"aurium_exec"}, token: "tok_test"})

	// The projection the context engine writes into the worktree; this is the
	// agent's objective, constraints and stack position.
	projection := filepath.Join(c.Worktree, gitx.AuriumDir, contextengine.ProjectionFile)
	if err := os.MkdirAll(filepath.Dir(projection), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projection,
		[]byte("# Objective\nMake the parser accept trailing commas.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Converse(context.Background(), a.ID, "hello"); err != nil {
		t.Fatal(err)
	}

	// Through the REAL adapter, not a stub: this is the command line the host
	// process is launched with.
	argv := (&agent.Claude{}).HeadlessCommand("hello", ad.lastOpts)
	var prompt string
	for i, arg := range argv {
		if arg == "--append-system-prompt" && i+1 < len(argv) {
			prompt = argv[i+1]
		}
	}
	if prompt == "" {
		t.Fatalf("the launched process receives no system prompt at all: %v", argv)
	}
	if !strings.Contains(prompt, "Use the aurium_exec tool for every command.") {
		t.Errorf("the placement notice never reaches the process:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Make the parser accept trailing commas.") {
		t.Errorf("the context projection never reaches the process:\n%s", prompt)
	}
}

// In-container placement delivers its instructions through the container's
// own ~/.claude/CLAUDE.md, which its process genuinely reads. It must not
// start carrying them in argv as well.
func TestConverseInContainerPlacementCarriesNoProjectContextInArgv(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)

	if err := m.Converse(context.Background(), a.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if ad.lastOpts.ProjectContext != "" {
		t.Errorf("in-container placement must not carry the projection in argv, got %q",
			ad.lastOpts.ProjectContext)
	}
}

// The silent failure this whole preflight exists for. With its MCP server
// unreachable or unauthorized, `claude -p --output-format json` returns
// "is_error": false and an empty permission_denials; with Bash denied too,
// the agent has no tools and no shell. So the turn must not run at all.
func TestConverseHostPlacementFailsWhenTheGatewayRefusesItsToken(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	gw := &fakeGateway{tools: []string{"aurium_exec"}, token: "the-real-token"}
	srv := hostPlaced(t, m, a.ID, gw)
	// Overwrite the config with a token the gateway will not accept — the
	// production case, where Projection.Token was empty.
	writeHostMCPConfigForTest(t, agent.MCPConfigPath(m.SnapshotHome, a.ID), srv.URL, "")

	err := m.Converse(context.Background(), a.ID, "hello")
	if err == nil {
		t.Fatal("a host turn whose gateway refuses its token must fail, not report an empty success")
	}
	if !errors.Is(err, ErrHostGatewayUnusable) {
		t.Errorf("the failure must be nameable: %v", err)
	}
	if ad.lastCmd != nil {
		t.Error("the turn must not have run at all")
	}
	// The human has to see it, so it goes in the transcript as a warning
	// rather than only into a log.
	msgs := transcript(t, m, a.ID)
	if len(msgs) != 1 || msgs[0].Type != ipc.TypeWarning {
		t.Fatalf("want one WARNING in the transcript, got %+v", msgs)
	}
}

func TestConverseHostPlacementFailsWhenTheDaemonIsNotListening(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	gw := &fakeGateway{tools: []string{"aurium_exec"}}
	srv := hostPlaced(t, m, a.ID, gw)
	srv.Close() // the daemon is simply not running

	err := m.Converse(context.Background(), a.ID, "hello")
	if err == nil {
		t.Fatal("an unreachable gateway must fail the turn by name")
	}
	if !errors.Is(err, ErrHostGatewayUnusable) {
		t.Errorf("the failure must be nameable: %v", err)
	}
	if ad.lastCmd != nil {
		t.Error("the turn must not have run at all")
	}
}

// A gateway that answers but offers no aurium_exec leaves the agent with no
// way to run anything, since its own shell is denied. A check that only
// pinged the endpoint would pass here; this one asks for the tool.
func TestConverseHostPlacementFailsWhenTheGatewayOffersNoExecTool(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	hostPlaced(t, m, a.ID, &fakeGateway{tools: []string{"aurium_context_query"}})

	err := m.Converse(context.Background(), a.ID, "hello")
	if err == nil {
		t.Fatal("a gateway with no aurium_exec must fail the turn")
	}
	if !strings.Contains(err.Error(), "aurium_exec") {
		t.Errorf("the error must name the missing tool: %v", err)
	}
}

// In-container placement must never pay for any of this: no preflight, no
// gateway, no new dependency on a daemon being up.
func TestConverseInContainerPlacementNeverPreflightsAGateway(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	gw := &fakeGateway{tools: []string{"aurium_exec"}}
	srv := gw.start(t)
	m.HostAuriumURL = srv.URL

	if err := m.Converse(context.Background(), a.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if gw.calls != 0 {
		t.Errorf("in-container placement must not call the gateway, got %d calls", gw.calls)
	}
}

// Before host placement a chat turn never read aurium.yaml at all. A project
// with no config — a moved root, a deleted file — must therefore keep
// answering exactly as it did, rather than breaking because a setting it
// never asked for cannot be resolved.
func TestConverseTreatsAMissingConfigAsInContainer(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	m.Config = func(context.Context, string) (*config.Config, error) {
		// Exactly what config.Load returns for a file that is not there.
		return nil, fmt.Errorf("config: %w", os.ErrNotExist)
	}

	if err := m.Converse(context.Background(), a.ID, "hello"); err != nil {
		t.Fatalf("a missing aurium.yaml must not break a turn: %v", err)
	}
	if ad.lastOpts.HostSandboxed {
		t.Error("a missing config must resolve to in-container")
	}
	msgs := transcript(t, m, a.ID)
	if len(msgs) != 1 || msgs[0].Type != ipc.TypeResponse {
		t.Fatalf("the turn must have answered normally, got %+v", msgs)
	}
}

// A config that EXISTS but cannot be parsed is a different thing: it may well
// say agent_placement: host, and guessing in-container would put an agent
// somewhere the project did not ask for.
func TestConverseStillFailsOnAnUnparseableConfig(t *testing.T) {
	ad := &echoAdapter{envelope: envelope("ok", false, 1, 1)}
	m, _, a := converseFixture(t, ad)
	m.Config = func(context.Context, string) (*config.Config, error) {
		return nil, fmt.Errorf("config: /r/aurium.yaml: yaml: line 4: did not find expected key")
	}

	if err := m.Converse(context.Background(), a.ID, "hello"); err == nil {
		t.Fatal("a broken config must still fail the turn")
	}
	if ad.lastCmd != nil {
		t.Error("the turn must not have run at all")
	}
}

type noHeadless struct{ echoAdapter }

func (n *noHeadless) Capabilities() agent.Caps { return agent.Caps{} }

// recordingMeter captures what was metered.
type recordingMeter struct{ calls []Metered }

func (r *recordingMeter) Meter(_ context.Context, m Metered) error {
	r.calls = append(r.calls, m)
	return nil
}
