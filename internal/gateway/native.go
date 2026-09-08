package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/mcp"
	"github.com/RhyChaw/aurium/internal/store"
)

// obj is shorthand for a JSON Schema object.
func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func arr(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// nativeTools returns the aurium_* tools a caller may see (§10.2).
//
// Descriptions are written for a model, not a human browsing docs: they say
// when to reach for the tool and what the failure modes mean, because that is
// what determines whether it gets used correctly.
func (g *Gateway) nativeTools(caller Caller) []mcp.Tool {
	tools := []mcp.Tool{
		{
			Name: "aurium_context_query",
			Description: "Search this project's shared context and documentation. " +
				"Use it before asking a human anything, and before re-deriving a decision " +
				"somebody already made. Only context you are permitted to read is returned.",
			InputSchema: obj(map[string]any{
				"q": str("what to search for, in plain words"),
				"k": num("maximum results (default 10)"),
			}, "q"),
		},
		{
			Name: "aurium_context_get",
			Description: "Read one context item by key, for example \"task/objective\" or " +
				"\"decisions/2026-09-07-jwt\". Returns its content AND its version — keep the " +
				"version if you intend to write it back.",
			InputSchema: obj(map[string]any{
				"key":   str("the item key"),
				"scope": str("project | container | agent (default: search container then project)"),
			}, "key"),
		},
		{
			Name:        "aurium_context_list",
			Description: "List context item keys under a prefix, for example \"decisions/\".",
			InputSchema: obj(map[string]any{
				"prefix": str("key prefix; empty for everything you can read"),
				"scope":  str("project | container | agent"),
			}),
		},
		{
			Name: "aurium_context_append",
			Description: "Append a block to a context item, creating it if needed. Appends never " +
				"conflict, so this is the right tool for recording a decision or a discovery " +
				"while other agents are working.",
			InputSchema: obj(map[string]any{
				"key":   str("the item key, e.g. \"discoveries/auth-flow\""),
				"text":  str("the block to append"),
				"scope": str("project | container | agent (default: container)"),
			}, "key", "text"),
		},
		{
			Name: "aurium_context_write",
			Description: "Replace a context item's content. You must pass the base_version you " +
				"read, and the write is refused if the item changed in the meantime — that " +
				"refusal is not an error to retry blindly: re-read, reconcile, then write again.",
			InputSchema: obj(map[string]any{
				"key":          str("the item key"),
				"base_version": num("the version you read; 0 to create a new item"),
				"content":      str("the full new content"),
				"reason":       str("why you are changing it"),
				"scope":        str("project | container | agent (default: container)"),
			}, "key", "base_version", "content"),
		},
		{
			Name: "aurium_context_propose",
			Description: "Propose a change for a human or the master agent to review, for items " +
				"you may not write directly. Returns immediately; the decision arrives later.",
			InputSchema: obj(map[string]any{
				"key":          str("the item key"),
				"base_version": num("the version you read"),
				"content":      str("the full proposed content"),
				"reason":       str("why this change is right"),
				"scope":        str("project | container | agent"),
			}, "key", "base_version", "content"),
		},
		{
			Name: "aurium_ipc_send",
			Description: "Send a message to another agent, a container, or a human. Use BLOCKED " +
				"when you cannot proceed without a decision, REQUEST to ask another agent for " +
				"work, INFORMATION to share something, ARTIFACT to hand over a result.",
			InputSchema: obj(map[string]any{
				"to_agent":     str("recipient agent id"),
				"to_container": str("recipient container id (delivers to its agent)"),
				"to_human":     map[string]any{"type": "boolean", "description": "send to the human"},
				"type":         str("REQUEST | RESPONSE | INFORMATION | WARNING | BLOCKED | ARTIFACT | DEPENDENCY | CONFLICT"),
				"content":      str("the message"),
				"priority":     str("normal | high"),
				"context_key":  str("a context key this message is about"),
				"branch":       str("a branch this message is about"),
				"task":         str("a task id this message is about"),
			}, "type", "content"),
		},
		{
			Name: "aurium_ipc_inbox",
			Description: "Read your messages. Messages stay in your inbox until you acknowledge " +
				"them with aurium_ipc_ack, so nothing is lost if you are interrupted.",
			InputSchema: obj(map[string]any{"limit": num("maximum messages (default 20)")}),
		},
		{
			Name:        "aurium_ipc_ack",
			Description: "Acknowledge messages you have handled, so they stop being redelivered.",
			InputSchema: obj(map[string]any{"ids": arr("message ids")}, "ids"),
		},
		{
			Name: "aurium_task_info",
			Description: "Your task, your position in the container stack, your ports, and whether " +
				"your parent has moved. Check this before assuming your branch is current.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "aurium_task_status",
			Description: "Report your progress. Use \"blocked\" with a reason when you need a " +
				"human — that notifies one immediately rather than leaving you stuck silently. " +
				"Use \"review\" when your work is ready.",
			InputSchema: obj(map[string]any{
				"status":  str("planning | running | blocked | review | pr_ready | completed"),
				"note":    str("what you did, or what you are blocked on"),
				"options": arr("if blocked, the choices a human could pick between"),
			}, "status"),
		},
		{
			Name: "aurium_snapshot",
			Description: "Snapshot your container: source (including uncommitted work), the " +
				"filesystem and volumes. Take one before anything risky — it is how you undo.",
			InputSchema: obj(map[string]any{"label": str("a name to remember it by")}),
		},
		{
			Name: "aurium_request_approval",
			Description: "Ask a human to approve something outside the tool system, such as a " +
				"destructive shell command. Returns immediately; the answer arrives as a message.",
			InputSchema: obj(map[string]any{
				"action": str("exactly what you intend to do"),
				"reason": str("why it is necessary"),
			}, "action", "reason"),
		},
	}

	// Delegation is a master-only capability (§9.2). A worker that could
	// delegate would let one prompt fan out without bound.
	if caller.Role == store.RoleMaster {
		tools = append(tools,
			mcp.Tool{
				Name: "aurium_delegate",
				Description: "Hand a bounded subtask to a worker agent. \"fork\" gives the worker " +
					"its own container and branch (use this by default); \"serial\" runs it " +
					"headless inside your container while you wait, for small tasks.",
				InputSchema: obj(map[string]any{
					"title":   str("short description of the subtask"),
					"prompt":  str("the full instruction for the worker"),
					"adapter": str("agent adapter (default: the project's)"),
					"mode":    str("fork | serial (default fork)"),
				}, "title", "prompt"),
			},
			mcp.Tool{
				Name: "aurium_merge",
				Description: "Merge a worker's branch into yours once it reports review. Refused " +
					"if your worktree is dirty — commit or stash first.",
				InputSchema: obj(map[string]any{
					"child": str("the worker's container id or branch"),
				}, "child"),
			},
		)
	}
	return tools
}

// callNative dispatches an aurium_* tool.
func (g *Gateway) callNative(ctx context.Context, caller Caller, name string, raw json.RawMessage) (*mcp.ToolResult, *mcp.Error) {
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, mcp.Errorf(mcp.CodeInvalidParams, "arguments must be a JSON object: %v", err)
		}
	}

	subject := contextengine.Subject{
		AgentID: caller.AgentID, Role: caller.Role,
		ContainerID: caller.ContainerID, ProjectID: caller.ProjectID,
		Human: caller.Human,
	}

	switch name {
	case "aurium_context_query":
		if !caller.Token.Has(store.ScopeContextRead) && !caller.Human {
			return deniedScope("context:read")
		}
		hits, err := g.Context.Query(ctx, subject, argStr(args, "q"), argInt(args, "k", 10))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		if len(hits) == 0 {
			return mcp.TextResult("No matches. Nothing in this project's context or documentation mentions that."), nil
		}
		return mcp.JSONResult(hits), nil

	case "aurium_context_get":
		ref := g.refFor(caller, args, "")
		if err := g.Context.Require(ctx, subject, ref, contextengine.PermRead, name); err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		it, err := g.Context.Get(ctx, ref)
		if err != nil {
			// Try the project scope before giving up: an agent asking for
			// "task/objective" should not have to know which scope holds it.
			alt := ref
			alt.Scope, alt.ScopeID = contextengine.ScopeProject, caller.ProjectID
			if it2, err2 := g.Context.Get(ctx, alt); err2 == nil {
				return mcp.JSONResult(it2), nil
			}
			return mcp.ErrorResult("no context item %q", ref.Key), nil
		}
		return mcp.JSONResult(it), nil

	case "aurium_context_list":
		scope, scopeID := g.scopeFor(caller, args, contextengine.ScopeContainer)
		items, err := g.Context.List(ctx, scope, scopeID, argStr(args, "prefix"))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		keys := make([]map[string]any, 0, len(items))
		for _, it := range items {
			keys = append(keys, map[string]any{"key": it.Key, "version": it.Version})
		}
		return mcp.JSONResult(keys), nil

	case "aurium_context_append":
		ref := g.refFor(caller, args, contextengine.ScopeContainer)
		if err := g.Context.Require(ctx, subject, ref, contextengine.PermAppend, name); err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		it, err := g.Context.Append(ctx, ref, argStr(args, "text"), callerActor(caller))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.JSONResult(map[string]any{"key": it.Key, "version": it.Version}), nil

	case "aurium_context_write":
		ref := g.refFor(caller, args, contextengine.ScopeContainer)
		if err := g.Context.Require(ctx, subject, ref, contextengine.PermWrite, name); err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		it, err := g.Context.Write(ctx, ref, argInt(args, "base_version", 0),
			argStr(args, "content"), callerActor(caller), argStr(args, "reason"))
		if err != nil {
			var stale *contextengine.ErrStale
			if asStale(err, &stale) {
				// Hand back everything needed to reconcile, so the agent does
				// not have to guess or retry blindly.
				return mcp.JSONResult(map[string]any{
					"error":           "stale",
					"current_version": stale.CurrentVersion,
					"current_content": stale.CurrentContent,
					"advice": "Somebody changed this while you were working. Read the current " +
						"content above, merge your intent into it, and write again with " +
						"base_version set to current_version.",
				}), nil
			}
			return mcp.ErrorResult("%v", err), nil
		}
		if g.IPC != nil {
			_, _ = g.IPC.NotifyContextChange(ctx, caller.ProjectID, it.Key, it.Version)
		}
		return mcp.JSONResult(map[string]any{"key": it.Key, "version": it.Version}), nil

	case "aurium_context_propose":
		ref := g.refFor(caller, args, contextengine.ScopeProject)
		if err := g.Context.Require(ctx, subject, ref, contextengine.PermPropose, name); err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		p, err := g.Context.Propose(ctx, ref, argInt(args, "base_version", 0),
			argStr(args, "content"), callerActor(caller), argStr(args, "reason"))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		if g.IPC != nil {
			_, _ = g.IPC.Send(ctx, ipc.Message{
				ProjectID: caller.ProjectID,
				From:      ipc.Addr{AgentID: caller.AgentID, ContainerID: caller.ContainerID},
				To:        ipc.Addr{Human: true},
				Type:      ipc.TypeApprovalRequired,
				Content:   fmt.Sprintf("Proposal %s for %q: %s", p.ID, ref.Key, argStr(args, "reason")),
				Refs:      ipc.Refs{ContextKey: ref.Key},
			})
		}
		return mcp.JSONResult(map[string]any{
			"proposal": p.ID, "status": p.Status,
			"note": "A reviewer has been notified. Continue with other work; " +
				"the decision will arrive in your inbox.",
		}), nil

	case "aurium_ipc_send":
		if !caller.Token.Has("ipc:send") && !caller.Human {
			return deniedScope("ipc:send")
		}
		m, err := g.IPC.Send(ctx, ipc.Message{
			ProjectID: caller.ProjectID,
			From:      ipc.Addr{AgentID: caller.AgentID, ContainerID: caller.ContainerID},
			To: ipc.Addr{
				AgentID:     argStr(args, "to_agent"),
				ContainerID: argStr(args, "to_container"),
				Human:       argBool(args, "to_human"),
			},
			Type:     strings.ToUpper(argStr(args, "type")),
			Priority: argStr(args, "priority"),
			Content:  argStr(args, "content"),
			Refs: ipc.Refs{
				Task:       argStr(args, "task"),
				ContextKey: argStr(args, "context_key"),
				Branch:     argStr(args, "branch"),
			},
		})
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.JSONResult(map[string]any{"message": m.ID, "delivered_to_human": m.To.Human}), nil

	case "aurium_ipc_inbox":
		msgs, err := g.IPC.Inbox(ctx, ipc.Addr{
			AgentID: caller.AgentID, ContainerID: caller.ContainerID,
		}, argInt(args, "limit", 20))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		if len(msgs) == 0 {
			return mcp.TextResult("Your inbox is empty."), nil
		}
		return mcp.JSONResult(msgs), nil

	case "aurium_ipc_ack":
		ids := argStrings(args, "ids")
		n, err := g.IPC.Ack(ctx, ids)
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.TextResult(fmt.Sprintf("Acknowledged %d message(s).", n)), nil

	case "aurium_task_info":
		return g.taskInfo(ctx, caller)

	case "aurium_task_status":
		return g.taskStatus(ctx, caller, args)

	case "aurium_snapshot":
		// §14: snapshot:self must not let a container snapshot another.
		if !caller.Token.Has(store.ScopeSnapshotSelf) && !caller.Human {
			return deniedScope("snapshot:self")
		}
		if g.Snapshots == nil {
			return mcp.ErrorResult("snapshots are not available in this configuration"), nil
		}
		sn, err := g.Snapshots.SnapshotContainer(ctx, caller.ContainerID, argStr(args, "label"))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.JSONResult(map[string]any{
			"snapshot": sn.ID, "seq": sn.Seq,
			"restore": fmt.Sprintf("aurium restore %s %d", caller.ContainerID, sn.Seq),
		}), nil

	case "aurium_request_approval":
		ap, err := g.RequestManualApproval(ctx, caller, argStr(args, "action"), argStr(args, "reason"))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.JSONResult(map[string]any{
			"status": "pending_approval", "id": ap.ID, "expires": ap.ExpiresAt,
			"note": "Continue with other work; the answer will arrive in your inbox.",
		}), nil

	case "aurium_delegate":
		if caller.Role != store.RoleMaster {
			return nil, mcp.Errorf(mcp.CodeMethodNotFound, "unknown tool %q", name)
		}
		if g.Delegator == nil {
			return mcp.ErrorResult("delegation is not available in this configuration"), nil
		}
		res, err := g.Delegator.Delegate(ctx, DelegateRequest{
			MasterAgentID: caller.AgentID, MasterContainerID: caller.ContainerID,
			Title: argStr(args, "title"), Adapter: argStr(args, "adapter"),
			Mode: argStr(args, "mode"), Prompt: argStr(args, "prompt"),
		})
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.JSONResult(res), nil

	case "aurium_merge":
		if caller.Role != store.RoleMaster {
			return nil, mcp.Errorf(mcp.CodeMethodNotFound, "unknown tool %q", name)
		}
		if g.Delegator == nil {
			return mcp.ErrorResult("delegation is not available in this configuration"), nil
		}
		res, err := g.Delegator.Merge(ctx, caller.ContainerID, argStr(args, "child"))
		if err != nil {
			return mcp.ErrorResult("%v", err), nil
		}
		return mcp.JSONResult(res), nil
	}

	return nil, mcp.Errorf(mcp.CodeMethodNotFound, "unknown tool %q", name)
}

func deniedScope(scope string) (*mcp.ToolResult, *mcp.Error) {
	return mcp.ErrorResult("this container's token does not carry the %s scope", scope), nil
}

// refFor builds a context Ref from tool arguments.
func (g *Gateway) refFor(caller Caller, args map[string]any, defaultScope string) contextengine.Ref {
	scope, scopeID := g.scopeFor(caller, args, defaultScope)
	return contextengine.Ref{Scope: scope, ScopeID: scopeID, Key: argStr(args, "key")}
}

func (g *Gateway) scopeFor(caller Caller, args map[string]any, defaultScope string) (string, string) {
	scope := argStr(args, "scope")
	if scope == "" {
		scope = defaultScope
	}
	switch scope {
	case contextengine.ScopeProject:
		return contextengine.ScopeProject, caller.ProjectID
	case contextengine.ScopeAgent:
		return contextengine.ScopeAgent, caller.AgentID
	case contextengine.ScopeContainer:
		return contextengine.ScopeContainer, caller.ContainerID
	default:
		return contextengine.ScopeContainer, caller.ContainerID
	}
}

// ---- argument helpers ----
//
// Arguments come from a language model, so every accessor tolerates a missing
// key or the wrong type rather than panicking. A malformed call should produce
// a readable message the model can correct, not a crashed daemon.

func argStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func argBool(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func argStrings(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		// A model that sends one id as a bare string means one id.
		return []string{v}
	}
	return nil
}
