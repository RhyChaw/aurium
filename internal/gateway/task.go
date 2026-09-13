package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/mcp"
	"github.com/RhyChaw/aurium/internal/stack"
	"github.com/RhyChaw/aurium/internal/store"
)

// taskInfo answers "where am I and is my branch current" (§10.2).
func (g *Gateway) taskInfo(ctx context.Context, caller Caller) (*mcp.ToolResult, *mcp.Error) {
	c, err := g.Store.GetContainer(ctx, caller.ContainerID)
	if err != nil {
		return mcp.ErrorResult("this container is not registered: %v", err), nil
	}

	info := map[string]any{
		"container": c.ID,
		"branch":    c.Branch,
		"parent":    c.ParentBranch,
		"base_sha":  c.BaseSHA,
		"worktree":  c.Worktree,
		"ports":     c.Ports,
	}
	if c.TaskID != "" {
		if t, err := g.Store.GetTask(ctx, c.TaskID); err == nil {
			info["task"] = map[string]any{"id": t.ID, "title": t.Title, "status": t.Status}
		}
	}

	// Recomputed from git: this is the number an agent decides on.
	if res, err := stack.CheckEligibility(ctx, gitx.New(c.Worktree), stack.Target{
		Branch: c.Branch, ParentBranch: c.ParentBranch, BaseSHA: c.BaseSHA,
	}); err == nil {
		sync := map[string]any{"status": string(res.Eligibility)}
		if res.Behind > 0 {
			sync["parent_ahead_by"] = res.Behind
			sync["advice"] = "Your parent has moved. Sync before building further, " +
				"or your work will need rebasing later."
		}
		if res.Eligibility == stack.Conflict {
			sync["conflicts"] = res.ConflictedFiles
		}
		info["sync"] = sync
	}

	if unread, err := g.unreadCount(ctx, caller); err == nil && unread > 0 {
		info["unread_messages"] = unread
	}
	return mcp.JSONResult(info), nil
}

func (g *Gateway) unreadCount(ctx context.Context, caller Caller) (int, error) {
	var n int
	err := g.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM messages
		 WHERE (to_agent_id = ? OR to_container_id = ?) AND status != 'acked'`,
		caller.AgentID, caller.ContainerID).Scan(&n)
	return n, err
}

// taskStatus records an agent's progress and, when blocked, gets a human
// involved immediately (§9.3).
func (g *Gateway) taskStatus(ctx context.Context, caller Caller, args map[string]any) (*mcp.ToolResult, *mcp.Error) {
	status := strings.ToLower(argStr(args, "status"))
	note := argStr(args, "note")
	options := argStrings(args, "options")

	valid := map[string]bool{
		store.TaskPlanning: true, store.TaskRunning: true, store.TaskBlocked: true,
		store.TaskReview: true, store.TaskPRReady: true, store.TaskCompleted: true,
	}
	if !valid[status] {
		return mcp.ErrorResult(
			"%q is not a status. Use one of: planning, running, blocked, review, pr_ready, completed.",
			status), nil
	}

	c, err := g.Store.GetContainer(ctx, caller.ContainerID)
	if err != nil {
		return mcp.ErrorResult("%v", err), nil
	}
	if c.TaskID != "" {
		if err := g.Store.TransitionTask(ctx, c.TaskID, status); err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
	}

	agentStatus := store.AgentRunning
	if status == store.TaskBlocked {
		agentStatus = store.AgentBlocked
	}
	if caller.AgentID != "" {
		_ = g.Store.UpdateAgentStatus(ctx, caller.AgentID, agentStatus)
	}

	if g.Events != nil {
		_ = g.Events.Emit(ctx, events.Event{
			Type: events.TaskTransitioned, Actor: callerActor(caller),
			ProjectID: c.ProjectID, ContainerID: c.ID, AgentID: caller.AgentID, TaskID: c.TaskID,
			Payload: map[string]any{"status": status, "note": note},
		})
	}

	switch status {
	case store.TaskBlocked:
		// §9.3: being blocked is the one state an agent cannot resolve alone,
		// so it must reach a human rather than sit in a status field.
		content := fmt.Sprintf("Blocked: %s", note)
		if len(options) > 0 {
			content += "\nOptions:\n"
			for i, o := range options {
				content += fmt.Sprintf("  %c) %s\n", 'A'+i, o)
			}
			content += fmt.Sprintf("Answer with `aurium approve --choose <letter>` or reply.")
		}
		if g.IPC != nil {
			// The agent is told the truth about whether anyone heard it. A
			// silent failure here leaves it waiting on a human who was never
			// notified — the exact situation "blocked" exists to prevent.
			if _, err := g.IPC.Send(ctx, ipc.Message{
				ProjectID: c.ProjectID,
				From:      ipc.Addr{AgentID: caller.AgentID, ContainerID: c.ID},
				To:        ipc.Addr{Human: true},
				Type:      ipc.TypeBlocked, Priority: ipc.PriorityHigh,
				Content: content, Refs: ipc.Refs{Task: c.TaskID},
			}); err != nil {
				return mcp.ErrorResult(
					"Recorded as blocked, but the notification to a human FAILED: %v. "+
						"Nobody has been told. Say so in your next output so the human "+
						"watching the session can act.", err), nil
			}
		}
		return mcp.TextResult("Recorded as blocked and a human has been notified. " +
			"Continue with anything that does not depend on this, or wait for a reply in your inbox."), nil

	case store.TaskReview:
		// A worker reporting review is handing its branch to its master.
		if caller.Role == store.RoleWorker && g.IPC != nil {
			ag, err := g.Store.GetAgent(ctx, caller.AgentID)
			if err == nil && ag.ParentAgentID != "" {
				_, _ = g.IPC.Send(ctx, ipc.Message{
					ProjectID: c.ProjectID,
					From:      ipc.Addr{AgentID: caller.AgentID, ContainerID: c.ID},
					To:        ipc.Addr{AgentID: ag.ParentAgentID},
					Type:      ipc.TypeArtifact, Priority: ipc.PriorityHigh,
					Content: fmt.Sprintf("Work ready on branch %s. %s", c.Branch, note),
					Refs:    ipc.Refs{Branch: c.Branch, Task: c.TaskID},
				})
				return mcp.TextResult(fmt.Sprintf(
					"Recorded as ready for review. Your branch %s was handed to the master agent.",
					c.Branch)), nil
			}
		}
	}
	return mcp.TextResult(fmt.Sprintf("Status recorded as %s.", status)), nil
}
