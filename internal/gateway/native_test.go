package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/store"
)

// fakeExecutor stands in for the runtime's container exec, the same way
// fakeUpstream stands in for a connected MCP server.
type fakeExecutor struct{ got string }

func (f *fakeExecutor) ExecInContainer(ctx context.Context, containerID, command string) (string, int, error) {
	f.got = command
	return "ok\n", 0, nil
}

// fakeFailingExecutor always fails, so tests can assert a failed command is
// recorded too — an audit trail with only successes in it is not one.
type fakeFailingExecutor struct{ got string }

func (f *fakeFailingExecutor) ExecInContainer(ctx context.Context, containerID, command string) (string, int, error) {
	f.got = command
	return "", 1, errors.New("boom")
}

func TestAuriumExecRunsInTheCallersContainer(t *testing.T) {
	f := newGateway(t)
	exec := &fakeExecutor{}
	f.g.Executor = exec

	res, rpcErr := f.g.CallTool(context.Background(), f.caller(f.aMaster, f.cA), "aurium_exec",
		json.RawMessage(`{"command":"go test ./..."}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if exec.got != "go test ./..." {
		t.Errorf("command not passed through: %q", exec.got)
	}
	if res.IsError {
		t.Fatalf("call reported an error: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "ok") {
		t.Errorf("output not returned: %v", res)
	}
}

// Without an executor the tool must say so, not pretend to have run.
func TestAuriumExecWithoutAnExecutorSaysSo(t *testing.T) {
	f := newGateway(t)
	f.g.Executor = nil

	res, rpcErr := f.g.CallTool(context.Background(), f.caller(f.aMaster, f.cA), "aurium_exec",
		json.RawMessage(`{"command":"ls"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !res.IsError {
		t.Error("expected an error result when no executor is configured")
	}
	if !strings.Contains(strings.ToLower(res.Content[0].Text), "not available") {
		t.Errorf("want an explicit unavailable result, got %v", res)
	}
}

// aurium_exec deliberately skips ClassifyRisk-based gating (a shell string
// like "rm -rf /" splits into the identifiers "rm" and "rf", neither of
// which matches a highVerbs word, so it would be misclassified as low risk).
// Recording every call is the actual safeguard, so nothing may quietly drop
// it.
func TestAuriumExecEmitsAnAuditEvent(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	f.g.Executor = &fakeExecutor{}

	_, rpcErr := f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "aurium_exec",
		json.RawMessage(`{"command":"go test ./..."}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	evs, err := f.g.Events.Replay(ctx, 0, events.Filter{Types: []string{events.AgentExecuted}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want one agent.executed event, got %d", len(evs))
	}
	if evs[0].Payload["command"] != "go test ./..." {
		t.Errorf("command not recorded in the event: %+v", evs[0].Payload)
	}
	if evs[0].Payload["status"] != "ok" {
		t.Errorf("successful command should be recorded as ok, got %+v", evs[0].Payload)
	}
}

// A command that fails is often the more interesting one, so it must be
// recorded too, not just successes.
func TestAuriumExecRecordsAFailedCommandToo(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	f.g.Executor = &fakeFailingExecutor{}

	_, rpcErr := f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "aurium_exec",
		json.RawMessage(`{"command":"rm -rf /"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	evs, err := f.g.Events.Replay(ctx, 0, events.Filter{Types: []string{events.AgentExecuted}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want one agent.executed event for the failed call, got %d", len(evs))
	}
	if evs[0].Payload["command"] != "rm -rf /" {
		t.Errorf("command not recorded in the event: %+v", evs[0].Payload)
	}
	if evs[0].Payload["status"] != "error" {
		t.Errorf("failed command should be recorded with status error, got %+v", evs[0].Payload)
	}
}

// Every other tool here gates on a scope: aurium_context_* on context:read,
// aurium_ipc_send on ipc:send, aurium_snapshot on snapshot:self. aurium_exec
// is the most powerful of them — arbitrary shell in the caller's container,
// and under host placement the ONLY place the agent's commands run — so a
// token minted with nothing but context:read must not carry it.
func TestAuriumExecRequiresItsOwnScope(t *testing.T) {
	f := newGateway(t)
	exec := &fakeExecutor{}
	f.g.Executor = exec

	caller := f.caller(f.aMaster, f.cA)
	caller.Token = store.TokenInfo{
		ContainerID: f.cA.ID, Scopes: []string{store.ScopeContextRead},
	}

	res, rpcErr := f.g.CallTool(context.Background(), caller, "aurium_exec",
		json.RawMessage(`{"command":"rm -rf /"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !res.IsError {
		t.Fatal("a token without the exec scope must not get code execution")
	}
	if exec.got != "" {
		t.Errorf("the command must not have run at all, got %q", exec.got)
	}
	if !strings.Contains(res.Content[0].Text, store.ScopeExecSelf) {
		t.Errorf("the refusal must name the missing scope: %v", res.Content[0].Text)
	}
}

// And the scope an ordinary container agent is issued must actually carry it,
// or host placement — whose every command goes through this tool — could
// never run one.
func TestDefaultContainerScopesCarryTheExecScope(t *testing.T) {
	f := newGateway(t)
	f.g.Executor = &fakeExecutor{}

	res, rpcErr := f.g.CallTool(context.Background(), f.caller(f.aMaster, f.cA), "aurium_exec",
		json.RawMessage(`{"command":"go test ./..."}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if res.IsError {
		t.Fatalf("an ordinary container agent must be able to call aurium_exec: %+v", res)
	}
}
