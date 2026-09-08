package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/store"
)

// ApprovalTTL is how long a held call waits for a human (§10.5).
//
// It expires rather than waiting forever because the arguments describe an
// action against a live system, and approving "delete this branch" an hour
// later may mean something entirely different.
const ApprovalTTL = 30 * time.Minute

// Approval statuses.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

// Approval is a held call awaiting a human decision.
type Approval struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id,omitempty"`
	ContainerID   string `json:"container_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	IntegrationID string `json:"integration_id,omitempty"`
	Capability    string `json:"capability"`
	ArgsJSON      string `json:"args_json"`
	Reason        string `json:"reason,omitempty"`
	Status        string `json:"status"`
	DecidedBy     string `json:"decided_by,omitempty"`
	CreatedAt     string `json:"created_at"`
	DecidedAt     string `json:"decided_at,omitempty"`
	ExpiresAt     string `json:"expires_at"`
}

// RequestApproval holds an approve-mode call for a human (§10.5).
func (g *Gateway) RequestApproval(ctx context.Context, caller Caller, in integration,
	capability string, args json.RawMessage) (Approval, error) {

	ap := Approval{
		ID: ids.New(ids.Approval), ProjectID: caller.ProjectID,
		ContainerID: caller.ContainerID, AgentID: caller.AgentID,
		IntegrationID: in.ID, Capability: in.Name + "_" + capability,
		ArgsJSON: string(args), Status: ApprovalPending,
		CreatedAt: ids.Now(),
		ExpiresAt: time.Now().UTC().Add(ApprovalTTL).Format(time.RFC3339Nano),
	}
	if err := g.insertApproval(ctx, ap); err != nil {
		return Approval{}, err
	}

	if g.Events != nil {
		_ = g.Events.Emit(ctx, events.Event{
			Type: events.ApprovalRequested, Actor: callerActor(caller),
			ProjectID: caller.ProjectID, ContainerID: caller.ContainerID, AgentID: caller.AgentID,
			Payload: map[string]any{
				"approval": ap.ID, "capability": ap.Capability,
				"integration": in.Name, "expires": ap.ExpiresAt,
			},
		})
	}
	// Not swallowed: an approval nobody is told about is an agent waiting
	// forever on a request no human can see.
	if g.IPC != nil {
		if _, err := g.IPC.Send(ctx, ipc.Message{
			ProjectID: caller.ProjectID,
			From:      ipc.Addr{AgentID: caller.AgentID, ContainerID: caller.ContainerID},
			To:        ipc.Addr{Human: true},
			Type:      ipc.TypeApprovalRequired,
			Priority:  ipc.PriorityHigh,
			Content: fmt.Sprintf("%s wants to call %s. Approve with `aurium approve %s`.",
				describeCaller(caller), ap.Capability, ap.ID),
		}); err != nil {
			return Approval{}, fmt.Errorf("gateway: could not notify a human about approval %s: %w",
				ap.ID, err)
		}
	}
	return ap, nil
}

// RequestManualApproval holds an action outside the tool system (§10.2's
// aurium_request_approval), such as a destructive shell command.
func (g *Gateway) RequestManualApproval(ctx context.Context, caller Caller, action, reason string) (Approval, error) {
	if action == "" {
		return Approval{}, fmt.Errorf("gateway: an approval request needs an action")
	}
	ap := Approval{
		ID: ids.New(ids.Approval), ProjectID: caller.ProjectID,
		ContainerID: caller.ContainerID, AgentID: caller.AgentID,
		Capability: "manual:" + action, Reason: reason,
		ArgsJSON: "{}", Status: ApprovalPending, CreatedAt: ids.Now(),
		ExpiresAt: time.Now().UTC().Add(ApprovalTTL).Format(time.RFC3339Nano),
	}
	if err := g.insertApproval(ctx, ap); err != nil {
		return Approval{}, err
	}

	if g.Events != nil {
		_ = g.Events.Emit(ctx, events.Event{
			Type: events.ApprovalRequested, Actor: callerActor(caller),
			ProjectID: caller.ProjectID, ContainerID: caller.ContainerID, AgentID: caller.AgentID,
			Payload: map[string]any{"approval": ap.ID, "action": action, "reason": reason},
		})
	}
	if g.IPC != nil {
		if _, err := g.IPC.Send(ctx, ipc.Message{
			ProjectID: caller.ProjectID,
			From:      ipc.Addr{AgentID: caller.AgentID, ContainerID: caller.ContainerID},
			To:        ipc.Addr{Human: true},
			Type:      ipc.TypeApprovalRequired, Priority: ipc.PriorityHigh,
			Content: fmt.Sprintf("%s asks to: %s\nWhy: %s\nApprove with `aurium approve %s`.",
				describeCaller(caller), action, reason, ap.ID),
		}); err != nil {
			return Approval{}, fmt.Errorf("gateway: could not notify a human about approval %s: %w",
				ap.ID, err)
		}
	}
	return ap, nil
}

func (g *Gateway) insertApproval(ctx context.Context, ap Approval) error {
	_, err := g.Store.DB().ExecContext(ctx,
		`INSERT INTO approvals (id, container_id, agent_id, integration_id, capability,
			args_json, reason, status, created_at, expires_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		ap.ID, nullIfEmpty(ap.ContainerID), nullIfEmpty(ap.AgentID), nullIfEmpty(ap.IntegrationID),
		ap.Capability, ap.ArgsJSON, nullIfEmpty(ap.Reason), ap.Status, ap.CreatedAt, ap.ExpiresAt)
	if err != nil {
		return fmt.Errorf("gateway: record approval: %w", err)
	}
	return nil
}

// Decide approves or rejects a held call.
//
// On approval the gateway executes the call NOW and delivers the result to the
// agent as a message. The agent was told to continue with other work, so there
// is nothing waiting to receive a return value (§10.5).
func (g *Gateway) Decide(ctx context.Context, approvalID, decision, decidedBy string) (Approval, error) {
	ap, err := g.GetApproval(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}
	if ap.Status != ApprovalPending {
		return ap, fmt.Errorf("gateway: approval %s is already %s", approvalID, ap.Status)
	}
	if decision != ApprovalApproved && decision != ApprovalRejected {
		return ap, fmt.Errorf("gateway: %q is not a decision", decision)
	}

	now := ids.Now()
	if _, err := g.Store.DB().ExecContext(ctx,
		`UPDATE approvals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
		decision, decidedBy, now, approvalID); err != nil {
		return Approval{}, err
	}
	ap.Status, ap.DecidedBy, ap.DecidedAt = decision, decidedBy, now

	if g.Events != nil {
		_ = g.Events.Emit(ctx, events.Event{
			Type: events.ApprovalDecided, Actor: decidedBy,
			ContainerID: ap.ContainerID, AgentID: ap.AgentID,
			Payload: map[string]any{
				"approval": ap.ID, "capability": ap.Capability, "decision": decision,
			},
		})
	}

	outcome := fmt.Sprintf("Your request %s (%s) was rejected by %s.",
		ap.ID, ap.Capability, decidedBy)

	if decision == ApprovalApproved {
		outcome = fmt.Sprintf("Your request %s (%s) was approved by %s.",
			ap.ID, ap.Capability, decidedBy)

		if ap.IntegrationID != "" {
			if result, err := g.executeApproved(ctx, ap); err != nil {
				outcome += "\nThe call then failed: " + err.Error()
			} else {
				outcome += "\nResult:\n" + resultText(result)
			}
		}
	}

	if g.IPC != nil && ap.AgentID != "" {
		// Not swallowed. The agent was told to continue with other work, so
		// this message is the ONLY way it learns the outcome; losing it
		// silently would leave the agent waiting forever.
		if _, err := g.IPC.Send(ctx, ipc.Message{
			ProjectID: g.projectOf(ctx, ap),
			From:      ipc.Addr{Human: true},
			To:        ipc.Addr{AgentID: ap.AgentID, ContainerID: ap.ContainerID},
			Type:      ipc.TypeResponse, Priority: ipc.PriorityHigh, Content: outcome,
		}); err != nil {
			return ap, fmt.Errorf("gateway: approval %s was %s but the agent could not be told: %w",
				ap.ID, decision, err)
		}
	}
	return ap, nil
}

// projectOf resolves the project an approval belongs to, falling back to the
// container when the stored id is missing (approvals written before the field
// existed, or by a caller that did not set it).
func (g *Gateway) projectOf(ctx context.Context, ap Approval) string {
	if ap.ProjectID != "" {
		return ap.ProjectID
	}
	if ap.ContainerID != "" {
		if c, err := g.Store.GetContainer(ctx, ap.ContainerID); err == nil {
			return c.ProjectID
		}
	}
	return ""
}

// executeApproved runs the held upstream call.
func (g *Gateway) executeApproved(ctx context.Context, ap Approval) (string, error) {
	up, ok := g.upstreams[ap.IntegrationID]
	if !ok {
		return "", fmt.Errorf("integration is no longer connected")
	}
	// Capability was stored as "<integration>_<capability>".
	_, capability, _ := cut(ap.Capability, "_")

	res, err := up.CallTool(ctx, capability, json.RawMessage(ap.ArgsJSON))
	if err != nil {
		return "", err
	}

	if g.Events != nil {
		_ = g.Events.Emit(ctx, events.Event{
			Type: events.IntegrationCall, Actor: events.ActorHuman,
			ContainerID: ap.ContainerID, AgentID: ap.AgentID,
			Payload: map[string]any{
				"capability": ap.Capability, "approval": ap.ID,
				"args_hash": ids.HashHex([]byte(ap.ArgsJSON)), "status": "ok",
			},
		})
	}
	return resultText(res), nil
}

// ExpireStale marks overdue approvals expired and tells the waiting agents.
func (g *Gateway) ExpireStale(ctx context.Context) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	rows, err := g.Store.DB().QueryContext(ctx,
		`SELECT id, COALESCE(agent_id,''), COALESCE(container_id,''), capability
		 FROM approvals WHERE status = ? AND expires_at < ?`, ApprovalPending, now)
	if err != nil {
		return 0, err
	}
	type stale struct{ id, agent, container, capability string }
	var todo []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.agent, &s.container, &s.capability); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, s)
	}
	rows.Close()

	for _, s := range todo {
		if _, err := g.Store.DB().ExecContext(ctx,
			`UPDATE approvals SET status = ? WHERE id = ?`, ApprovalExpired, s.id); err != nil {
			return 0, err
		}
		if g.Events != nil {
			_ = g.Events.Emit(ctx, events.Event{
				Type: events.ApprovalExpired, Actor: events.ActorDaemon,
				ContainerID: s.container, AgentID: s.agent,
				Payload: map[string]any{"approval": s.id, "capability": s.capability},
			})
		}
		// The agent is told, because an approval that silently vanishes leaves
		// it waiting on something that will never arrive.
		if g.IPC != nil && s.agent != "" {
			_, _ = g.IPC.Send(ctx, ipc.Message{
				ProjectID: g.projectOf(ctx, Approval{ContainerID: s.container}),
				From:      ipc.Addr{Human: false},
				To:        ipc.Addr{AgentID: s.agent, ContainerID: s.container},
				Type:      ipc.TypeResponse,
				Content: fmt.Sprintf("Your request %s (%s) expired without a decision. "+
					"Ask again if you still need it.", s.id, s.capability),
			})
		}
	}
	return len(todo), nil
}

// GetApproval returns one approval.
func (g *Gateway) GetApproval(ctx context.Context, id string) (Approval, error) {
	var ap Approval
	err := g.Store.DB().QueryRowContext(ctx,
		`SELECT id, COALESCE(container_id,''), COALESCE(agent_id,''), COALESCE(integration_id,''),
		        capability, args_json, COALESCE(reason,''), status, COALESCE(decided_by,''),
		        created_at, COALESCE(decided_at,''), expires_at
		 FROM approvals WHERE id = ?`, id).
		Scan(&ap.ID, &ap.ContainerID, &ap.AgentID, &ap.IntegrationID, &ap.Capability,
			&ap.ArgsJSON, &ap.Reason, &ap.Status, &ap.DecidedBy,
			&ap.CreatedAt, &ap.DecidedAt, &ap.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, store.ErrNotFound
	}
	return ap, err
}

// ListApprovals returns approvals, optionally filtered by status.
func (g *Gateway) ListApprovals(ctx context.Context, status string) ([]Approval, error) {
	query := `SELECT id, COALESCE(container_id,''), COALESCE(agent_id,''), COALESCE(integration_id,''),
	                 capability, args_json, COALESCE(reason,''), status, COALESCE(decided_by,''),
	                 created_at, COALESCE(decided_at,''), expires_at FROM approvals`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC`

	rows, err := g.Store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Approval
	for rows.Next() {
		var ap Approval
		if err := rows.Scan(&ap.ID, &ap.ContainerID, &ap.AgentID, &ap.IntegrationID,
			&ap.Capability, &ap.ArgsJSON, &ap.Reason, &ap.Status, &ap.DecidedBy,
			&ap.CreatedAt, &ap.DecidedAt, &ap.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

// ---- small helpers ----

func describeCaller(c Caller) string {
	switch {
	case c.AgentID != "":
		return "Agent " + c.AgentID
	case c.ContainerID != "":
		return "Container " + c.ContainerID
	default:
		return "An agent"
	}
}

func resultText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		body, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(body)
	}
}

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func asStale(err error, target **contextengine.ErrStale) bool {
	return errors.As(err, target)
}
