package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
)

type delFixture struct {
	*fixture
	del    *Delegation
	ipc    *ipc.Bus
	master store.Container
	agent  store.Agent
}

func newDelegation(t *testing.T) *delFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()

	// Do this BEFORE creating any container. The default fixture's
	// post_create hook writes an untracked marker file, which would leave the
	// master worktree permanently dirty and block every merge. Real hooks
	// (npm ci) write gitignored paths; this fixture uses no hook at all.
	cfg, err := config.Parse([]byte(`
version: 1
project: {name: app, base_branch: main}
sandbox:
  driver: local
  agent: shell
`))
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg

	master := f.create("master-task", func(o *CreateOpts) {
		o.Role = store.RoleMaster
	})
	agents, _ := f.store.ListAgents(ctx, master.ID)
	if len(agents) == 0 {
		t.Fatal("the master container has no agent")
	}
	// Ensure the role is master (Create records what it was given).
	f.store.DB().ExecContext(ctx, `UPDATE agents SET role = ? WHERE id = ?`,
		store.RoleMaster, agents[0].ID)
	masterAgent, _ := f.store.GetAgent(ctx, agents[0].ID)

	msgs := ipc.New(f.store, f.bus, nil)
	del := &Delegation{
		Manager: f.mgr, IPC: msgs, MaxDepth: 1,
		Config: func(ctx context.Context, projectID string) (*config.Config, error) {
			return f.cfg, nil
		},
	}
	f.mgr.SnapshotHome = t.TempDir()

	return &delFixture{fixture: f, del: del, ipc: msgs, master: master, agent: masterAgent}
}

func TestForkDelegationGivesTheWorkerItsOwnContainerAndBranch(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	res, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "Write auth tests", Mode: ModeFork,
		Prompt: "Add integration coverage for AuthService.login()",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeFork || res.ContainerID == "" || res.Branch == "" {
		t.Fatalf("result = %+v", res)
	}

	worker, err := f.store.GetContainer(ctx, res.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	// D15: the worker gets its own worktree rather than sharing the master's.
	if worker.Worktree == f.master.Worktree {
		t.Fatal("a forked worker must not share the master's worktree")
	}
	if worker.ParentContainerID != f.master.ID {
		t.Errorf("the worker should track its master, parent = %q", worker.ParentContainerID)
	}
	if _, err := os.Stat(worker.Worktree); err != nil {
		t.Errorf("the worker's worktree was not created: %v", err)
	}

	// The parent link is what caps depth and routes the result back.
	wa, _ := f.store.ListAgents(ctx, worker.ID)
	if len(wa) != 1 {
		t.Fatalf("worker has %d agents, want 1", len(wa))
	}
	if wa[0].Role != store.RoleWorker {
		t.Errorf("worker role = %q", wa[0].Role)
	}
	if wa[0].ParentAgentID != f.agent.ID {
		t.Errorf("worker parent agent = %q, want %q", wa[0].ParentAgentID, f.agent.ID)
	}

	// And the worker is told what to do and how to report back.
	inbox, _ := f.ipc.Inbox(ctx, ipc.Addr{AgentID: wa[0].ID}, 10)
	if len(inbox) == 0 {
		t.Fatal("the worker was never given its instructions")
	}
	if !strings.Contains(inbox[0].Content, "AuthService.login") {
		t.Errorf("the prompt did not reach the worker: %q", inbox[0].Content)
	}
	if !strings.Contains(inbox[0].Content, "aurium_task_status") {
		t.Error("the worker should be told how to hand its work back")
	}
}

// §9.4: workers cannot delegate. Without a cap, one prompt fans out unbounded.
func TestWorkersCannotDelegate(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	res, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "subtask", Prompt: "do a thing", Mode: ModeFork,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerAgents, _ := f.store.ListAgents(ctx, res.ContainerID)
	worker := workerAgents[0]

	// A worker is not a master, so it is refused on role alone.
	_, err = f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: worker.ID, MasterContainerID: res.ContainerID,
		Title: "sub-subtask", Prompt: "recurse", Mode: ModeFork,
	})
	if err == nil {
		t.Fatal("a worker must not be able to delegate")
	}
	if !strings.Contains(err.Error(), "master") {
		t.Errorf("the error should explain the role requirement: %v", err)
	}
}

func TestDelegationDepthIsCapped(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	// Promote a worker to master to get past the role check, so the depth cap
	// is what is actually being tested.
	res, _ := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "subtask", Prompt: "x", Mode: ModeFork,
	})
	workerAgents, _ := f.store.ListAgents(ctx, res.ContainerID)
	f.store.DB().ExecContext(ctx, `UPDATE agents SET role = ? WHERE id = ?`,
		store.RoleMaster, workerAgents[0].ID)

	_, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: workerAgents[0].ID, MasterContainerID: res.ContainerID,
		Title: "deeper", Prompt: "y", Mode: ModeFork,
	})
	if err == nil {
		t.Fatal("delegation past the depth limit must be refused")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("the error should name the limit: %v", err)
	}
}

func TestWorkerBranchNamesDoNotCollide(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	first, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "Write tests", Prompt: "a", Mode: ModeFork,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "Write tests", Prompt: "b", Mode: ModeFork,
	})
	if err != nil {
		t.Fatalf("a second worker with the same title must still get a branch: %v", err)
	}
	if first.Branch == second.Branch {
		t.Fatalf("two workers share the branch %q", first.Branch)
	}
}

// D8 applies to merges too: merging into a dirty worktree would mix the
// master's uncommitted work into the merge.
func TestMergeRefusesADirtyMasterWorktree(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	res, _ := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "work", Prompt: "x", Mode: ModeFork,
	})

	os.WriteFile(filepath.Join(f.master.Worktree, "uncommitted.txt"), []byte("wip"), 0o644)

	out, err := f.del.Merge(ctx, f.master.ID, res.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Merged {
		t.Fatal("a merge into a dirty worktree must be refused")
	}
	if !strings.Contains(out.Reason, "uncommitted") {
		t.Errorf("the reason should say why: %q", out.Reason)
	}
}

func TestMergeBringsTheWorkersWorkIntoTheMaster(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	res, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "work", Prompt: "x", Mode: ModeFork,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := f.store.GetContainer(ctx, res.ContainerID)

	// The worker does its job.
	writeAndCommit(t, worker.Worktree, "worker.txt", "the worker's contribution", "worker: done")

	out, err := f.del.Merge(ctx, f.master.ID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Merged {
		t.Fatalf("merge refused: %s", out.Reason)
	}

	if _, err := os.Stat(filepath.Join(f.master.Worktree, "worker.txt")); err != nil {
		t.Fatalf("the worker's file did not reach the master: %v", err)
	}
	// --no-ff keeps the contribution visible as a unit.
	log := gitLog(t, f.master.Worktree)
	if !strings.Contains(log, "Merge worker branch") {
		t.Errorf("expected a merge commit:\n%s", log)
	}
}

func TestMergeRefusesAContainerThatIsNotAWorker(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	stranger := f.create("unrelated")
	if _, err := f.del.Merge(ctx, f.master.ID, stranger.ID); err == nil {
		t.Fatal("merging an unrelated container must be refused")
	}
}

func TestSerialDelegationRefusesAnAdapterThatCannotRunHeadless(t *testing.T) {
	f := newDelegation(t)
	ctx := context.Background()

	// The shell adapter is interactive only.
	_, err := f.del.Delegate(ctx, gateway.DelegateRequest{
		MasterAgentID: f.agent.ID, MasterContainerID: f.master.ID,
		Title: "quick", Prompt: "x", Mode: ModeSerial, Adapter: "shell",
	})
	if err == nil {
		t.Fatal("serial mode needs a headless adapter")
	}
	if !strings.Contains(err.Error(), "fork") {
		t.Errorf("the error should suggest the alternative: %v", err)
	}
}

// ---- helpers ----

func writeAndCommit(t *testing.T, dir, name, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-qm", msg)
}

func gitLog(t *testing.T, dir string) string {
	t.Helper()
	out, err := gitx.New(dir).Run(context.Background(), "log", "--format=%s", "-20")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var _ = driver.ExecOpts{}
var _ = agent.StartOpts{}
var _ = events.Event{}
