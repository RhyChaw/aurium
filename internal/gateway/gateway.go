package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/mcp"
	"github.com/RhyChaw/aurium/internal/store"
)

// Capability is one tool an integration exposes.
type Capability struct {
	IntegrationID string         `json:"integration_id"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	InputSchema   map[string]any `json:"input_schema,omitempty"`
	Risk          Risk           `json:"risk"`
	Removed       bool           `json:"removed"`
}

// Upstream is a connected MCP server the gateway proxies to.
type Upstream interface {
	Name() string
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.ToolResult, error)
	Close() error
}

// Gateway is the single MCP server every agent sees (§10.1).
//
// Agents never reach an upstream directly. That is the whole design: one
// server means one place that decides what a given agent may do, one place
// that holds credentials, and one audit log. An agent given a second MCP
// server would bypass all three.
type Gateway struct {
	Store     *store.Store
	Events    *events.Bus
	Context   *contextengine.Engine
	IPC       *ipc.Bus
	Snapshots SnapshotTaker
	Delegator Delegator

	// upstreams are keyed by integration id.
	upstreams map[string]Upstream
}

// SnapshotTaker lets an agent snapshot its own container.
type SnapshotTaker interface {
	SnapshotContainer(ctx context.Context, containerID, label string) (store.Snapshot, error)
}

// Delegator handles aurium_delegate and aurium_merge (§9.4).
type Delegator interface {
	Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error)
	Merge(ctx context.Context, masterContainerID, childRef string) (MergeResult, error)
}

// DelegateRequest asks for a subtask to be run.
type DelegateRequest struct {
	MasterAgentID     string
	MasterContainerID string
	Title             string
	Adapter           string
	Mode              string
	Prompt            string
}

// DelegateResult describes what delegation produced.
type DelegateResult struct {
	ContainerID string `json:"container_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Mode        string `json:"mode"`
	Output      string `json:"output,omitempty"`
}

// MergeResult describes a merge of a worker's branch.
type MergeResult struct {
	Merged    bool     `json:"merged"`
	Branch    string   `json:"branch"`
	Reason    string   `json:"reason,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// New builds a gateway.
func New(s *store.Store, e *events.Bus, cx *contextengine.Engine, msgs *ipc.Bus) *Gateway {
	return &Gateway{Store: s, Events: e, Context: cx, IPC: msgs, upstreams: map[string]Upstream{}}
}

// RegisterUpstream attaches a connected MCP server.
func (g *Gateway) RegisterUpstream(integrationID string, u Upstream) {
	if g.upstreams == nil {
		g.upstreams = map[string]Upstream{}
	}
	g.upstreams[integrationID] = u
}

// Caller identifies who is making a request, resolved from the bearer token.
type Caller struct {
	Subject
	Token store.TokenInfo
	Human bool
}

// ---- capability registry (§10.3) ----

// RegisterCapabilities records an upstream's tools with risk classifications.
func (g *Gateway) RegisterCapabilities(ctx context.Context, integrationID string,
	tools []mcp.Tool, overrides map[string]string) ([]Capability, error) {

	// Anything previously known is marked removed rather than deleted, so
	// existing grants survive a reconnect where a tool temporarily vanished.
	if _, err := g.Store.DB().ExecContext(ctx,
		`UPDATE capabilities SET removed = 1 WHERE integration_id = ?`, integrationID); err != nil {
		return nil, err
	}

	out := make([]Capability, 0, len(tools))
	for _, t := range tools {
		risk := ClassifyRisk(t.Name)
		if o, ok := overrides[t.Name]; ok {
			risk = Risk(o)
		}
		schema, _ := json.Marshal(t.InputSchema)

		if _, err := g.Store.DB().ExecContext(ctx,
			`INSERT INTO capabilities (integration_id, name, description, input_schema_json, risk, removed)
			 VALUES (?,?,?,?,?,0)
			 ON CONFLICT(integration_id, name) DO UPDATE SET
			   description = excluded.description,
			   input_schema_json = excluded.input_schema_json,
			   risk = excluded.risk,
			   removed = 0`,
			integrationID, t.Name, t.Description, string(schema), string(risk)); err != nil {
			return nil, fmt.Errorf("gateway: register capability %s: %w", t.Name, err)
		}
		out = append(out, Capability{
			IntegrationID: integrationID, Name: t.Name, Description: t.Description,
			InputSchema: t.InputSchema, Risk: risk,
		})
	}
	return out, nil
}

// ListCapabilities returns an integration's capabilities.
func (g *Gateway) ListCapabilities(ctx context.Context, integrationID string) ([]Capability, error) {
	rows, err := g.Store.DB().QueryContext(ctx,
		`SELECT integration_id, name, COALESCE(description,''), COALESCE(input_schema_json,''), risk, removed
		 FROM capabilities WHERE integration_id = ? ORDER BY name`, integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Capability
	for rows.Next() {
		var c Capability
		var schema, risk string
		var removed int
		if err := rows.Scan(&c.IntegrationID, &c.Name, &c.Description, &schema, &risk, &removed); err != nil {
			return nil, err
		}
		c.Risk, c.Removed = Risk(risk), removed == 1
		if schema != "" {
			_ = json.Unmarshal([]byte(schema), &c.InputSchema)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- MCP surface ----

// Handle dispatches one JSON-RPC request from an agent.
func (g *Gateway) Handle(ctx context.Context, caller Caller, req *mcp.Request) *mcp.Response {
	switch req.Method {
	case "initialize":
		return mcp.NewResponse(req.ID, mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			Capabilities: mcp.Capabilities{
				// The tool list changes when a grant is added or revoked while
				// an agent is running.
				Tools: &mcp.ToolsCapability{ListChanged: true},
			},
			ServerInfo:   mcp.ServerInfo{Name: "aurium", Version: Version},
			Instructions: instructions,
		})

	case "notifications/initialized", "initialized":
		return nil

	case "ping":
		return mcp.NewResponse(req.ID, map[string]any{})

	case "tools/list":
		tools, err := g.ToolsFor(ctx, caller)
		if err != nil {
			return mcp.NewError(req.ID, mcp.Errorf(mcp.CodeInternalError, "%v", err))
		}
		return mcp.NewResponse(req.ID, mcp.ToolsListResult{Tools: tools})

	case "tools/call":
		var params mcp.CallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return mcp.NewError(req.ID, mcp.Errorf(mcp.CodeInvalidParams, "%v", err))
		}
		result, rpcErr := g.CallTool(ctx, caller, params.Name, params.Arguments)
		if rpcErr != nil {
			return mcp.NewError(req.ID, rpcErr)
		}
		return mcp.NewResponse(req.ID, result)

	default:
		return mcp.NewError(req.ID, mcp.Errorf(mcp.CodeMethodNotFound,
			"unknown method %q", req.Method))
	}
}

// Version is stamped at build time.
var Version = "0.1.0-dev"

const instructions = `Aurium: an isolated container with its own git worktree and branch.

You may commit only to your own branch; other branches are protected by a git
hook. Your environment is re-derived on sync and restore, so declare installs
in aurium.yaml rather than installing ad hoc.

Read .aurium/CONTEXT.md for your objective, constraints and stack position.
Tools listed here are the ones you have been granted; others are not shown.`

// ToolsFor returns the tools a caller may see (§10.1).
//
// Ungranted tools are omitted entirely rather than listed and refused. A tool
// an agent can see is a tool it will try, and each attempt costs a turn and
// invites it to look for a way around the refusal.
func (g *Gateway) ToolsFor(ctx context.Context, caller Caller) ([]mcp.Tool, error) {
	tools := g.nativeTools(caller)

	integrations, err := g.listIntegrations(ctx, caller.ProjectID)
	if err != nil {
		return nil, err
	}
	for _, in := range integrations {
		caps, err := g.ListCapabilities(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range caps {
			if c.Removed {
				continue
			}
			mode, _, err := g.Resolve(ctx, caller.Subject, in.ID, c.Name)
			if err != nil {
				return nil, err
			}
			if mode == ModeDeny {
				continue
			}

			description := c.Description
			if mode == ModeApprove {
				// Saying so up front means the agent plans around the wait
				// instead of treating the delay as a failure.
				description += "\n\n(This action requires human approval. The call returns " +
					"immediately with a pending status; the result arrives later as a message.)"
			}
			tools = append(tools, mcp.Tool{
				Name:        in.Name + "_" + c.Name,
				Description: description,
				InputSchema: c.InputSchema,
			})
		}
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

type integration struct {
	ID     string
	Name   string
	Kind   string
	Status string
}

func (g *Gateway) listIntegrations(ctx context.Context, projectID string) ([]integration, error) {
	rows, err := g.Store.DB().QueryContext(ctx,
		`SELECT id, name, kind, status FROM integrations WHERE project_id = ? ORDER BY name`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []integration
	for rows.Next() {
		var in integration
		if err := rows.Scan(&in.ID, &in.Name, &in.Kind, &in.Status); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// CallTool dispatches a tool call, enforcing grants.
func (g *Gateway) CallTool(ctx context.Context, caller Caller, name string, args json.RawMessage) (*mcp.ToolResult, *mcp.Error) {
	if strings.HasPrefix(name, "aurium_") {
		return g.callNative(ctx, caller, name, args)
	}

	integrationName, capability, ok := strings.Cut(name, "_")
	if !ok {
		return nil, mcp.Errorf(mcp.CodeMethodNotFound, "unknown tool %q", name)
	}

	in, err := g.integrationByName(ctx, caller.ProjectID, integrationName)
	if err != nil {
		// Indistinguishable from "does not exist": an agent must not be able
		// to enumerate what it cannot use.
		return nil, mcp.Errorf(mcp.CodeMethodNotFound, "unknown tool %q", name)
	}

	mode, reason, err := g.Resolve(ctx, caller.Subject, in.ID, capability)
	if err != nil {
		return nil, mcp.Errorf(mcp.CodeInternalError, "%v", err)
	}

	switch mode {
	case ModeDeny:
		// §10.1: a call to a hidden tool returns -32601, the same as a tool
		// that does not exist.
		return nil, mcp.Errorf(mcp.CodeMethodNotFound, "unknown tool %q", name)

	case ModeApprove:
		ap, err := g.RequestApproval(ctx, caller, in, capability, args)
		if err != nil {
			return nil, mcp.Errorf(mcp.CodeInternalError, "%v", err)
		}
		// Return immediately. An agent cannot reliably block for minutes, and
		// a held connection would look like a hang (§10.5).
		return mcp.JSONResult(map[string]any{
			"status":  "pending_approval",
			"id":      ap.ID,
			"expires": ap.ExpiresAt,
			"note": "A human has been asked to approve this. The result will arrive " +
				"as a RESPONSE message in your inbox; continue with other work.",
		}), nil
	}

	return g.execute(ctx, caller, in, capability, args, reason)
}

// execute performs a granted upstream call and audits it.
func (g *Gateway) execute(ctx context.Context, caller Caller, in integration,
	capability string, args json.RawMessage, reason string) (*mcp.ToolResult, *mcp.Error) {

	up, ok := g.upstreams[in.ID]
	if !ok {
		return mcp.ErrorResult("integration %q is not connected right now", in.Name), nil
	}

	result, err := up.CallTool(ctx, capability, args)
	status := "ok"
	if err != nil {
		status = "error"
	}

	// §10.1: every call is audited. Arguments are hashed rather than stored —
	// they routinely contain issue bodies, file contents and occasionally
	// secrets a user pasted, none of which belong in a log.
	if g.Events != nil {
		_ = g.Events.Emit(ctx, events.Event{
			Type: events.IntegrationCall, Actor: callerActor(caller),
			ProjectID: caller.ProjectID, ContainerID: caller.ContainerID, AgentID: caller.AgentID,
			Payload: map[string]any{
				"integration": in.Name, "capability": capability,
				"args_hash": hashArgs(args), "status": status, "granted_by": reason,
			},
		})
	}
	if err != nil {
		return mcp.ErrorResult("%s failed: %v", capability, err), nil
	}
	return result, nil
}

func (g *Gateway) integrationByName(ctx context.Context, projectID, name string) (integration, error) {
	var in integration
	err := g.Store.DB().QueryRowContext(ctx,
		`SELECT id, name, kind, status FROM integrations WHERE project_id = ? AND name = ?`,
		projectID, name).Scan(&in.ID, &in.Name, &in.Kind, &in.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return integration{}, store.ErrNotFound
	}
	return in, err
}

func callerActor(c Caller) string {
	if c.Human {
		return events.ActorHuman
	}
	if c.AgentID != "" {
		return events.ActorAgent(c.AgentID)
	}
	return events.ActorDaemon
}

// hashArgs digests call arguments for the audit log.
func hashArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	return ids.HashHex(args)
}
