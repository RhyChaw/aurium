package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/store"
)

// Every "a human will be told" path must actually reach a human, and must
// report failure rather than swallowing it. An agent that believes it asked
// for help, when nobody was told, waits forever.
func TestApprovalRequestReachesAHuman(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	if _, rpcErr := f.g.CallTool(ctx, f.caller(f.aMaster, f.cA),
		"github_merge_pull_request", json.RawMessage(`{"pr":1}`)); rpcErr != nil {
		t.Fatal(rpcErr)
	}

	human, err := f.g.IPC.Inbox(ctx, ipc.Addr{Human: true}, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range human {
		if m.Type == ipc.TypeApprovalRequired && strings.Contains(m.Content, "aurium approve") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no human was told about the pending approval: %+v", human)
	}
}

func TestManualApprovalRequestReachesAHuman(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	res, rpcErr := f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "aurium_request_approval",
		json.RawMessage(`{"action":"rm -rf ./generated","reason":"the generator output is stale"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if res.IsError {
		t.Fatalf("request failed: %s", res.Content[0].Text)
	}

	human, _ := f.g.IPC.Inbox(ctx, ipc.Addr{Human: true}, 10)
	found := false
	for _, m := range human {
		if strings.Contains(m.Content, "rm -rf ./generated") && strings.Contains(m.Content, "stale") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the human was not told what the agent wants to do: %+v", human)
	}
}

func TestBlockedStatusNotifiesAHumanWithOptions(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	// Give the container a task so the transition has something to move.
	task, _ := f.store.CreateTask(ctx, f.project.ID, "Implement OAuth", "")
	f.store.DB().ExecContext(ctx, `UPDATE containers SET task_id = ? WHERE id = ?`, task.ID, f.cA.ID)

	res, rpcErr := f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "aurium_task_status",
		json.RawMessage(`{"status":"blocked","note":"token lifetime undecided","options":["15 minutes","1 hour"]}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if res.IsError {
		t.Fatalf("blocked status reported a failure: %s", res.Content[0].Text)
	}

	human, _ := f.g.IPC.Inbox(ctx, ipc.Addr{Human: true}, 10)
	var msg *ipc.Message
	for i := range human {
		if human[i].Type == ipc.TypeBlocked {
			msg = &human[i]
		}
	}
	if msg == nil {
		t.Fatalf("a blocked agent must reach a human: %+v", human)
	}
	// The options are what let a human answer in one word instead of a
	// conversation.
	if !strings.Contains(msg.Content, "15 minutes") || !strings.Contains(msg.Content, "A)") {
		t.Errorf("the options were not offered to the human: %q", msg.Content)
	}

	// And the state is recorded, so the dashboard does not show a busy agent.
	got, _ := f.store.GetTask(ctx, task.ID)
	if got.Status != store.TaskBlocked {
		t.Errorf("task status = %q, want blocked", got.Status)
	}
	agent, _ := f.store.GetAgent(ctx, f.aMaster.ID)
	if agent.Status != store.AgentBlocked {
		t.Errorf("agent status = %q, want blocked", agent.Status)
	}
}

func TestExpiredApprovalTellsTheAgent(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "github_merge_pull_request", json.RawMessage(`{}`))
	pending, _ := f.g.ListApprovals(ctx, ApprovalPending)

	// Backdate the expiry.
	f.store.DB().ExecContext(ctx,
		`UPDATE approvals SET expires_at = ? WHERE id = ?`, "2000-01-01T00:00:00Z", pending[0].ID)

	n, err := f.g.ExpireStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d approvals, want 1", n)
	}

	inbox, _ := f.g.IPC.Inbox(ctx, ipc.Addr{AgentID: f.aMaster.ID}, 10)
	found := false
	for _, m := range inbox {
		if strings.Contains(m.Content, "expired") {
			found = true
		}
	}
	if !found {
		t.Fatal("an expired approval must be reported, or the agent waits on something that will never come")
	}
	// And it must not execute afterwards.
	if len(f.upstream.calls) != 0 {
		t.Fatalf("an expired approval executed: %v", f.upstream.calls)
	}
}
