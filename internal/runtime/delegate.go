package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/stack"
	"github.com/RhyChaw/aurium/internal/store"
)

// Delegation modes (§9.4).
const (
	// ModeFork gives the worker its own container and branch. This is the
	// default because it is the only mode that preserves D15: two agents in
	// one worktree is the collision the product exists to remove.
	ModeFork = "fork"
	// ModeSerial runs the worker headless inside the master's container while
	// the master waits. Safe only because nothing else is running there.
	ModeSerial = "serial"
)

// Delegation wires aurium_delegate and aurium_merge to the runtime.
type Delegation struct {
	Manager *Manager
	IPC     *ipc.Bus
	// Config resolves a project's aurium.yaml.
	Config func(ctx context.Context, projectID string) (*config.Config, error)
	// MaxDepth caps delegation chains. 1 means workers cannot delegate
	// (§9.4): without a cap, one prompt can fan out without bound.
	MaxDepth int
	// Usage meters headless runs. Optional: without it a serial delegation
	// still runs, it is just not counted.
	Usage UsageMeter
}

// UsageMeter records what a headless run spent (§D26). The interface keeps
// internal/runtime from importing internal/usage, which would otherwise need
// to import internal/runtime back to know what an agent is.
type UsageMeter interface {
	Meter(ctx context.Context, m Metered) error
}

// Metered is one headless run, as the runtime knows it.
type Metered struct {
	ProjectID    string
	ContainerID  string
	AgentID      string
	AccountID    string
	Adapter      string
	Model        string
	Kind         string
	InputTokens  int64
	OutputTokens int64
	// CostUSD is the provider's own figure, when it reported one. It beats
	// any price table: it knows the account's real rates.
	CostUSD float64
	HasCost bool
}

// Delegate runs a subtask (§9.4).
func (d *Delegation) Delegate(ctx context.Context, req gateway.DelegateRequest) (gateway.DelegateResult, error) {
	master, err := d.Manager.Store.GetContainer(ctx, req.MasterContainerID)
	if err != nil {
		return gateway.DelegateResult{}, err
	}
	masterAgent, err := d.Manager.Store.GetAgent(ctx, req.MasterAgentID)
	if err != nil {
		return gateway.DelegateResult{}, err
	}
	if masterAgent.Role != store.RoleMaster {
		return gateway.DelegateResult{}, fmt.Errorf(
			"runtime: only a master agent may delegate (this agent is %s)", masterAgent.Role)
	}

	depth, err := d.depthOf(ctx, masterAgent)
	if err != nil {
		return gateway.DelegateResult{}, err
	}
	maxDepth := d.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if depth >= maxDepth {
		return gateway.DelegateResult{}, fmt.Errorf(
			"runtime: delegation depth limit reached (%d). A worker cannot delegate further; "+
				"do this subtask yourself or ask the human to raise agents.delegation.max_depth",
			maxDepth)
	}

	cfg, err := d.Config(ctx, master.ProjectID)
	if err != nil {
		return gateway.DelegateResult{}, err
	}
	adapterName := req.Adapter
	if adapterName == "" {
		adapterName = cfg.Sandbox.Agent
	}
	mode := req.Mode
	if mode == "" {
		mode = cfg.Agents.Delegation.Mode
	}
	if mode == "" {
		mode = ModeFork
	}

	switch mode {
	case ModeSerial:
		return d.serial(ctx, master, masterAgent, adapterName, req)
	case ModeFork:
		return d.fork(ctx, master, masterAgent, cfg, adapterName, req)
	default:
		return gateway.DelegateResult{}, fmt.Errorf(
			"runtime: %q is not a delegation mode (want fork or serial)", mode)
	}
}

// fork gives the worker its own container, branch and agent.
func (d *Delegation) fork(ctx context.Context, master store.Container, masterAgent store.Agent,
	cfg *config.Config, adapterName string, req gateway.DelegateRequest) (gateway.DelegateResult, error) {

	// A worker branches from the master's current state, so it starts from
	// work the master has already done rather than from the base branch.
	branch := uniqueBranch(ctx, d.Manager, master, req.Title)

	worker, err := d.Manager.Fork(ctx, master.ID, cfg, ForkOpts{
		Branch:  branch,
		Stack:   true, // a worker tracks its master, so a sync brings the master's work down
		Adapter: adapterName,
		Role:    store.RoleWorker,
		TaskID:  master.TaskID,
		NoAgent: true, // the agent is created below with a parent link
	})
	if err != nil {
		return gateway.DelegateResult{}, fmt.Errorf("runtime: fork a worker container: %w", err)
	}

	// Start the agent first, then link it to its delegator. Creating the row
	// up front would put two agents in the container and trip the "one
	// interactive agent" check that startAgent performs (D15).
	workerAgent, err := d.Manager.startAgent(ctx, worker, adapterName, store.RoleWorker, "", "",
		agent.StartOpts{Prompt: req.Prompt})
	if err != nil {
		// Leave the container: the human can inspect what went wrong, and
		// destroying it would discard the fork's snapshot lineage.
		return gateway.DelegateResult{}, fmt.Errorf("runtime: start the worker agent: %w", err)
	}
	// This link is what caps delegation depth and routes the result back.
	if err := d.Manager.Store.SetAgentParent(ctx, workerAgent.ID, masterAgent.ID); err != nil {
		return gateway.DelegateResult{}, err
	}
	workerAgent.ParentAgentID = masterAgent.ID

	// The worker is told what to do and who to report to.
	if d.IPC != nil {
		if _, err := d.IPC.Send(ctx, ipc.Message{
			ProjectID: master.ProjectID,
			From:      ipc.Addr{AgentID: masterAgent.ID, ContainerID: master.ID},
			To:        ipc.Addr{AgentID: workerAgent.ID, ContainerID: worker.ID},
			Type:      ipc.TypeRequest, Priority: ipc.PriorityHigh,
			Content: fmt.Sprintf("%s\n\nYou are a worker on branch %s. When your work is "+
				"ready, call aurium_task_status(\"review\") — that hands your branch to the "+
				"master agent for merging.", req.Prompt, worker.Branch),
			Refs: ipc.Refs{Task: master.TaskID, Branch: worker.Branch},
		}); err != nil {
			return gateway.DelegateResult{}, fmt.Errorf(
				"runtime: worker started but could not be given its instructions: %w", err)
		}

		// And the master records the dependency, so it is told when the
		// worker's branch is ready.
		_, _ = d.IPC.Send(ctx, ipc.Message{
			ProjectID: master.ProjectID,
			From:      ipc.Addr{AgentID: workerAgent.ID, ContainerID: worker.ID},
			To:        ipc.Addr{AgentID: masterAgent.ID, ContainerID: master.ID},
			Type:      ipc.TypeDependency,
			Content:   fmt.Sprintf("Worker started on %s for: %s", worker.Branch, req.Title),
			Refs:      ipc.Refs{Branch: worker.Branch, Task: master.TaskID},
		})
	}

	if d.Manager.Events != nil {
		_ = d.Manager.Events.Emit(ctx, events.Event{
			Type: events.AgentStarted, Actor: events.ActorAgent(masterAgent.ID),
			ProjectID: master.ProjectID, ContainerID: worker.ID, AgentID: workerAgent.ID,
			Payload: map[string]any{
				"role": store.RoleWorker, "delegated_by": masterAgent.ID,
				"mode": ModeFork, "title": req.Title, "branch": worker.Branch,
			},
		})
	}

	return gateway.DelegateResult{
		Mode: ModeFork, ContainerID: worker.ID, AgentID: workerAgent.ID, Branch: worker.Branch,
	}, nil
}

// serial runs a headless one-shot inside the master's container.
//
// Safe only because the master is paused for the duration: two agents writing
// one worktree concurrently is exactly what D15 forbids.
func (d *Delegation) serial(ctx context.Context, master store.Container, masterAgent store.Agent,
	adapterName string, req gateway.DelegateRequest) (gateway.DelegateResult, error) {

	adapter, ok := d.Manager.Adapters.Get(adapterName)
	if !ok {
		return gateway.DelegateResult{}, fmt.Errorf("runtime: unknown adapter %q", adapterName)
	}
	if !adapter.Capabilities().Headless {
		return gateway.DelegateResult{}, fmt.Errorf(
			"runtime: the %s adapter cannot run headless, so it cannot be used in serial mode; "+
				"use mode \"fork\"", adapterName)
	}

	drv, err := d.Manager.Drivers.Get(master.Driver)
	if err != nil {
		return gateway.DelegateResult{}, err
	}
	cmd := adapter.HeadlessCommand(req.Prompt, agent.ExecOpts{})
	if len(cmd) == 0 {
		return gateway.DelegateResult{}, fmt.Errorf("runtime: adapter %q has no headless command", adapterName)
	}

	res, err := drv.Exec(ctx, master.RuntimeID, cmd, execOptsFor(master))
	if err != nil {
		return gateway.DelegateResult{}, fmt.Errorf("runtime: serial delegation: %w", err)
	}

	// Meter what the run reported. A failure to record is never allowed to
	// fail the delegation: the work already happened, and losing the count is
	// a smaller harm than losing the result.
	d.meter(ctx, master, masterAgent, adapter, res.Stdout)

	output := res.Stdout
	if res.ExitCode != 0 {
		output = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	}
	return gateway.DelegateResult{Mode: ModeSerial, Output: output}, nil
}

// meter records a headless run's usage when the adapter reported any.
//
// An adapter that does not implement UsageParser reports nothing, and nothing
// is recorded — no estimate, no zero row. §D26: a cost view that quietly
// under-reports is worse than one that shows a gap.
func (d *Delegation) meter(ctx context.Context, c store.Container, a store.Agent,
	adapter agent.Adapter, stdout string) {

	if d.Usage == nil {
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
	_ = d.Usage.Meter(ctx, Metered{
		ProjectID: c.ProjectID, ContainerID: c.ID, AgentID: a.ID,
		AccountID: a.ProviderAccountID, Adapter: adapter.Name(), Model: model,
		Kind:         "delegation",
		InputTokens:  report.InputTokens,
		OutputTokens: report.OutputTokens,
		CostUSD:      report.CostUSD,
		HasCost:      report.HasCost,
	})
}

// Merge brings a worker's branch into the master's (§9.4).
func (d *Delegation) Merge(ctx context.Context, masterContainerID, childRef string) (gateway.MergeResult, error) {
	master, err := d.Manager.Store.GetContainer(ctx, masterContainerID)
	if err != nil {
		return gateway.MergeResult{}, err
	}
	repo, err := d.Manager.Store.GetRepository(ctx, master.RepoID)
	if err != nil {
		return gateway.MergeResult{}, err
	}

	child, err := d.resolveChild(ctx, master, childRef)
	if err != nil {
		return gateway.MergeResult{}, err
	}
	if child.ParentContainerID != master.ID {
		return gateway.MergeResult{}, fmt.Errorf(
			"runtime: %s is not a worker of this container", childRef)
	}

	// D8, again: merging into a dirty worktree would mix the master's
	// uncommitted work into the merge commit, or fail halfway.
	g := gitx.New(master.Worktree)
	clean, err := g.IsClean(ctx)
	if err != nil {
		return gateway.MergeResult{}, err
	}
	if !clean {
		return gateway.MergeResult{
			Branch: child.Branch,
			Reason: "your worktree has uncommitted changes; commit or stash them before merging",
		}, nil
	}

	// The worker branched from the master and the master may have moved since.
	// Rebasing the worker first is the same eligibility logic sync uses, so a
	// merge never produces a surprise conflict the agent cannot explain.
	res, err := stack.Sync(ctx, gitx.New(child.Worktree), stack.Target{
		Branch: child.Branch, ParentBranch: master.Branch, BaseSHA: child.BaseSHA,
	}, stack.Options{})
	if err != nil {
		return gateway.MergeResult{}, err
	}
	switch res.Eligibility {
	case stack.Conflict:
		return gateway.MergeResult{
			Branch: child.Branch, Conflicts: res.ConflictedFiles,
			Reason: "the worker's branch conflicts with yours; ask the worker to resolve it " +
				"in its own container, then merge again",
		}, nil
	case stack.Synced:
		if err := d.Manager.Store.UpdateContainerBaseSHA(ctx, child.ID, res.NewBaseSHA); err != nil {
			return gateway.MergeResult{}, err
		}
	case stack.Drifted, stack.MissingParent:
		return gateway.MergeResult{Branch: child.Branch, Reason: res.Reason}, nil
	}

	// --no-ff so the worker's contribution stays visible as a unit in history.
	if err := g.RunOK(ctx, "merge", "--no-ff", "-m",
		fmt.Sprintf("Merge worker branch %s", child.Branch), child.Branch); err != nil {
		if gitx.IsConflict(err) {
			_ = g.RunOK(ctx, "merge", "--abort")
			return gateway.MergeResult{
				Branch: child.Branch,
				Reason: "the merge conflicted and was aborted; your branch is unchanged",
			}, nil
		}
		return gateway.MergeResult{}, fmt.Errorf("runtime: merge %s: %w", child.Branch, err)
	}

	head, _ := g.RevParse(ctx, "HEAD")
	_ = d.Manager.Store.UpdateContainerHeadSHA(ctx, master.ID, head)
	_ = repo

	if d.Manager.Events != nil {
		_ = d.Manager.Events.Emit(ctx, events.Event{
			Type: events.ContainerSynced, Actor: events.ActorDaemon,
			ProjectID: master.ProjectID, ContainerID: master.ID,
			Payload: map[string]any{
				"merged_worker": child.ID, "branch": child.Branch, "head": head,
			},
		})
	}
	return gateway.MergeResult{Merged: true, Branch: child.Branch}, nil
}

// resolveChild accepts a container id or a branch name.
func (d *Delegation) resolveChild(ctx context.Context, master store.Container, ref string) (store.Container, error) {
	if c, err := d.Manager.Store.GetContainer(ctx, ref); err == nil {
		return c, nil
	}
	if c, err := d.Manager.Store.GetContainerByBranch(ctx, master.RepoID, ref); err == nil {
		return c, nil
	}
	return store.Container{}, fmt.Errorf("runtime: no worker container matches %q", ref)
}

// depthOf counts how many delegation hops produced this agent.
func (d *Delegation) depthOf(ctx context.Context, a store.Agent) (int, error) {
	depth := 0
	seen := map[string]bool{a.ID: true}

	for a.ParentAgentID != "" && depth < 64 {
		parent, err := d.Manager.Store.GetAgent(ctx, a.ParentAgentID)
		if err != nil {
			return depth, nil
		}
		if seen[parent.ID] {
			return depth, nil // corrupt data must not loop
		}
		seen[parent.ID] = true
		depth++
		a = parent
	}
	return depth, nil
}

// uniqueBranch derives a branch name for a worker that does not collide.
func uniqueBranch(ctx context.Context, m *Manager, master store.Container, title string) string {
	base := master.Branch + "-" + Slugify(strings.ToLower(title))
	if len(base) > 80 {
		base = base[:80]
	}

	candidate := base
	for i := 2; i < 100; i++ {
		if _, err := m.Store.GetContainerByBranch(ctx, master.RepoID, candidate); err != nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%s", base, master.ID[len(master.ID)-6:])
}
