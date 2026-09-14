package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
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

type noHeadless struct{ echoAdapter }

func (n *noHeadless) Capabilities() agent.Caps { return agent.Caps{} }

// recordingMeter captures what was metered.
type recordingMeter struct{ calls []Metered }

func (r *recordingMeter) Meter(_ context.Context, m Metered) error {
	r.calls = append(r.calls, m)
	return nil
}
