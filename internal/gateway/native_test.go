package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeExecutor stands in for the runtime's container exec, the same way
// fakeUpstream stands in for a connected MCP server.
type fakeExecutor struct{ got string }

func (f *fakeExecutor) ExecInContainer(ctx context.Context, containerID, command string) (string, int, error) {
	f.got = command
	return "ok\n", 0, nil
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
