package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/store"
)

// A conversation turn: the human types, the agent answers.
//
// The chat pane used to record a message and, on a driver with tmux, type it
// into a live REPL. On the `local` driver — the one that works with no Docker,
// which is most first runs — there was no session to type into, so the message
// sat in an inbox nothing would ever read. A chat box that never answers is
// worse than no chat box.
//
// So a turn runs the adapter's headless command instead: `claude -p "<prompt>"
// --output-format json`, in the container's worktree, with the container's
// environment (which is where the provider credential already is). That works
// on every driver, needs no tmux, and produces exactly the token counts the
// usage meter parses.
//
// It is deliberately NOT a replacement for `aurium attach`. An interactive
// session is a different thing and remains the way to take the wheel.

// ConversationTimeout caps one turn.
//
// Five minutes, not fifteen. A real task can take minutes, but the failure this
// bounds is not slowness — it is a CLI that hangs rather than erroring, which
// is exactly what `claude -p` does when handed a credential it cannot use
// (observed 2026-09-14: it prints one warning and then waits forever). Fifteen
// minutes of a tile stuck on "working" teaches the user the colours mean
// nothing.
const ConversationTimeout = 5 * time.Minute

// inFlight guards one turn per agent. Two `claude -p` processes in one worktree
// is the collision D15 exists to prevent, arriving by a different road.
var inFlight sync.Map // agentID -> struct{}

// ErrBusy is returned when an agent is already mid-turn.
var ErrBusy = fmt.Errorf("runtime: this agent is already working on something")

// IsThinking reports whether a turn is in flight for this agent right now.
//
// The chat pane needs this rather than the agent's status. `running` means
// "this agent is alive", which a `shell` agent is forever — showing a thinking
// indicator for all of it would make the indicator mean nothing.
func (m *Manager) IsThinking(agentID string) bool {
	_, busy := inFlight.Load(agentID)
	return busy
}

// CanConverse reports whether an adapter can answer in the chat pane at all.
func (m *Manager) CanConverse(adapterName string) bool {
	a, ok := m.Adapters.Get(adapterName)
	return ok && a.Capabilities().Headless
}

// Converse runs one turn and records the reply.
//
// It blocks for as long as the agent takes, so callers that must answer an
// HTTP request first should run it in a goroutine — the reply arrives at the
// dashboard over SSE either way, which is what makes that safe.
func (m *Manager) Converse(ctx context.Context, agentID, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("runtime: a turn needs a prompt")
	}
	if _, busy := inFlight.LoadOrStore(agentID, struct{}{}); busy {
		return ErrBusy
	}
	defer inFlight.Delete(agentID)

	a, err := m.Store.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	c, err := m.Store.GetContainer(ctx, a.ContainerID)
	if err != nil {
		return err
	}
	adapter, ok := m.Adapters.Get(a.Adapter)
	if !ok {
		return fmt.Errorf("runtime: unknown agent adapter %q", a.Adapter)
	}
	if !adapter.Capabilities().Headless {
		return fmt.Errorf(
			"runtime: the %s adapter cannot answer here; attach to it with `aurium attach %s`",
			a.Adapter, c.ID)
	}

	// Working, so the rail turns the colour that means "busy, nothing needed"
	// for as long as this takes. Without it a turn that runs for two minutes
	// looks identical to an agent doing nothing.
	_ = m.Store.UpdateAgentStatus(ctx, agentID, store.AgentRunning)
	m.emit(ctx, events.AgentActive, c, a, map[string]any{"turn": "started"})

	// Continue the conversation when there is one to continue. The first turn
	// has no session, and `--continue` against nothing is an error rather than
	// a fresh start.
	opts := agent.ExecOpts{Model: a.Model, Continue: m.hasSpoken(ctx, agentID)}
	cmd := adapter.HeadlessCommand(prompt, opts)
	if len(cmd) == 0 {
		_ = m.Store.UpdateAgentStatus(ctx, agentID, store.AgentIdle)
		return fmt.Errorf("runtime: adapter %q has no headless command", a.Adapter)
	}

	drv, err := m.Drivers.Get(c.Driver)
	if err != nil {
		_ = m.Store.UpdateAgentStatus(ctx, agentID, store.AgentError)
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, ConversationTimeout)
	defer cancel()

	res, err := drv.Exec(runCtx, c.RuntimeID, cmd, execOptsFor(c))
	if err != nil {
		// The agent is not broken as an agent — the call failed — but the
		// human needs to see it, so it goes in the transcript rather than only
		// in a log, and the tile goes red.
		_ = m.Store.UpdateAgentStatus(ctx, agentID, store.AgentError)
		m.reply(ctx, c, a, runFailure(runCtx, err, res.Stdout, res.Stderr), true)
		return err
	}

	reply := strings.TrimSpace(res.Stdout)

	// Three ways to fail, and only one of them is a non-zero exit. `claude -p`
	// reports a refusal or an error inside a zero exit, with is_error set.
	failed := res.ExitCode != 0
	if reporter, ok := adapter.(agent.ErrorReporter); ok && reporter.ReportedError(res.Stdout) {
		failed = true
	}
	// Usage first: it is a record of something that already happened, and a
	// failure to render the reply must not lose the fact that it cost money.
	m.meterTurn(ctx, c, a, adapter, res.Stdout)

	// The envelope carries a readable message on failure as well as success —
	// "Not logged in · Please run /login" is the observed case — so it is
	// parsed either way. Showing a user the raw JSON of a failed run and
	// letting them find that sentence themselves is not reporting an error,
	// it is relocating it.
	if parser, ok := adapter.(agent.ReplyParser); ok {
		if text, ok := parser.ParseReply(res.Stdout); ok {
			reply = text
		}
	}
	if failed {
		if reply == "" || reply == strings.TrimSpace(res.Stdout) {
			reply = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		}
		if reply == "" {
			reply = fmt.Sprintf("The agent exited with code %d and said nothing.", res.ExitCode)
		}
		reply = withHint(reply)
	}
	if reply == "" {
		reply = "(the agent returned nothing)"
	}

	status := store.AgentIdle
	if failed {
		status = store.AgentError
	}
	_ = m.Store.UpdateAgentStatus(ctx, agentID, status)
	m.reply(ctx, c, a, reply, failed)
	return nil
}

// withHint points an authentication failure at the place that fixes it.
//
// The agent's own message is right about what is wrong and has no idea where
// Aurium keeps credentials, so the two halves are worth putting together.
func withHint(reply string) string {
	lower := strings.ToLower(reply)
	switch {
	case strings.Contains(lower, "not logged in"),
		strings.Contains(lower, "invalid api key"),
		strings.Contains(lower, "authentication"),
		strings.Contains(lower, "unauthorized"):
		return reply + "\n\nAurium passes this agent whichever provider account is " +
			"connected, or else the credential your shell exported. Check the Providers tab."
	}
	return reply
}

// runFailure turns a failed exec into something a human can act on.
//
// "context deadline exceeded" tells the user nothing. What the process managed
// to print before it was killed usually tells them everything — the observed
// case is `claude` warning that ANTHROPIC_API_KEY is overriding a working
// login, then hanging.
func runFailure(ctx context.Context, err error, stdout, stderr string) string {
	var b strings.Builder
	if ctx.Err() != nil {
		fmt.Fprintf(&b, "The agent did not answer within %s and was stopped.",
			ConversationTimeout)
	} else {
		fmt.Fprintf(&b, "The agent could not be run: %v", err)
	}
	if out := strings.TrimSpace(stderr + "\n" + stdout); out != "" {
		fmt.Fprintf(&b, "\n\nWhat it printed before stopping:\n%s", truncateOutput(out))
	}
	return b.String()
}

// truncateOutput keeps a failure legible. A hung CLI can emit a great deal, and
// a chat bubble holding a megabyte of retries helps nobody.
func truncateOutput(s string) string {
	const limit = 2000
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n… (truncated)"
}

// hasSpoken reports whether this agent has already produced a reply, which is
// what makes a continuation valid.
func (m *Manager) hasSpoken(ctx context.Context, agentID string) bool {
	var n int
	err := m.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE from_agent_id = ? AND type = ?`,
		agentID, "RESPONSE").Scan(&n)
	return err == nil && n > 0
}

// reply records the agent's answer as a message addressed to the human, which
// is what puts it in the chat pane and on the event stream.
func (m *Manager) reply(ctx context.Context, c store.Container, a store.Agent, text string, failed bool) {
	if m.IPC == nil {
		return
	}
	kind := "RESPONSE"
	if failed {
		kind = "WARNING"
	}
	if _, err := m.IPC.Send(ctx, ipc.Message{
		ProjectID: c.ProjectID,
		From:      ipc.Addr{AgentID: a.ID, ContainerID: c.ID},
		To:        ipc.Addr{Human: true},
		Type:      kind,
		Content:   text,
	}); err != nil {
		// Losing the reply is bad, but failing the turn after the work is done
		// is worse; the event trail still records that the turn happened.
		m.emit(ctx, events.AgentActive, c, a,
			map[string]any{"turn": "reply_lost", "err": err.Error()})
	}
}

func (m *Manager) meterTurn(ctx context.Context, c store.Container, a store.Agent,
	adapter agent.Adapter, stdout string) {

	if m.Usage == nil {
		return
	}
	parser, ok := adapter.(agent.UsageParser)
	if !ok {
		return
	}
	report, ok := parser.ParseUsage(stdout)
	if !ok {
		return
	}
	model := report.Model
	if model == "" {
		model = a.Model
	}
	_ = m.Usage.Meter(ctx, Metered{
		ProjectID: c.ProjectID, ContainerID: c.ID, AgentID: a.ID,
		AccountID: a.ProviderAccountID, Adapter: adapter.Name(), Model: model,
		Kind:        "exec",
		InputTokens: report.InputTokens, OutputTokens: report.OutputTokens,
		CostUSD: report.CostUSD, HasCost: report.HasCost,
	})
}

func (m *Manager) emit(ctx context.Context, kind string, c store.Container, a store.Agent, payload map[string]any) {
	if m.Events == nil {
		return
	}
	_ = m.Events.Emit(ctx, events.Event{
		Type: kind, Actor: events.ActorAgent(a.ID),
		ProjectID: c.ProjectID, ContainerID: c.ID, AgentID: a.ID,
		Payload: payload,
	})
}
