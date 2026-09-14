// Package ipc is agent-to-agent and agent-to-human messaging (§9.5).
//
// Delivery is at-least-once with explicit acknowledgement, because the
// alternative — assuming an agent read what was sent — fails silently and
// invisibly. A message is `delivered` when an inbox call returns it and
// `acked` only when the agent says so.
package ipc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// Message types (§9.5) and their side effects.
const (
	TypeRequest  = "REQUEST"
	TypeResponse = "RESPONSE"
	TypeInfo     = "INFORMATION"
	TypeWarning  = "WARNING"
	// TypeBlocked marks the sender blocked and notifies a human.
	TypeBlocked = "BLOCKED"
	// TypeApprovalRequired always routes to a human, whatever the addressee.
	TypeApprovalRequired = "APPROVAL_REQUIRED"
	// TypeArtifact records an artifacts row.
	TypeArtifact = "ARTIFACT"
	// TypeDependency records a dependency, so a later context change notifies
	// the consumer.
	TypeDependency = "DEPENDENCY"
	// TypeConflict is raised by the stack engine on a rebase conflict.
	TypeConflict = "CONFLICT"
)

// Message statuses.
const (
	StatusQueued    = "queued"
	StatusDelivered = "delivered"
	StatusAcked     = "acked"
)

// Priorities.
const (
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

// RenudgeAfter is how long an undelivered high-priority message waits before
// the recipient is nudged again (§9.5).
const RenudgeAfter = 5 * time.Minute

// Addr is a message destination.
type Addr struct {
	AgentID     string `json:"agent,omitempty"`
	ContainerID string `json:"container,omitempty"`
	Human       bool   `json:"human,omitempty"`
}

// Refs are the things a message points at.
type Refs struct {
	Task       string   `json:"task,omitempty"`
	Artifact   string   `json:"artifact,omitempty"`
	ContextKey string   `json:"context_key,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Files      []string `json:"files,omitempty"`
}

// Message is one IPC message.
type Message struct {
	ID          string `json:"id"`
	TS          string `json:"ts"`
	ProjectID   string `json:"project_id"`
	From        Addr   `json:"from"`
	To          Addr   `json:"to"`
	Type        string `json:"type"`
	Priority    string `json:"priority"`
	Content     string `json:"content"`
	Refs        Refs   `json:"refs,omitempty"`
	InReplyTo   string `json:"in_reply_to,omitempty"`
	Status      string `json:"status"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	AckedAt     string `json:"acked_at,omitempty"`
}

// Nudger delivers a one-line notice to a running agent. It is an interface so
// the daemon can supply tmux and tests can supply a recorder.
type Nudger interface {
	Nudge(ctx context.Context, containerID, text string) error
}

// Clock lets tests drive the re-nudge timer without waiting five minutes.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Bus sends and delivers messages.
type Bus struct {
	Store  *store.Store
	Events *events.Bus
	Nudger Nudger
	Clock  Clock
}

// New returns an IPC bus.
func New(s *store.Store, e *events.Bus, n Nudger) *Bus {
	return &Bus{Store: s, Events: e, Nudger: n, Clock: realClock{}}
}

func (b *Bus) now() time.Time {
	if b.Clock == nil {
		return time.Now().UTC()
	}
	return b.Clock.Now()
}

var validTypes = map[string]bool{
	TypeRequest: true, TypeResponse: true, TypeInfo: true, TypeWarning: true,
	TypeBlocked: true, TypeApprovalRequired: true, TypeArtifact: true,
	TypeDependency: true, TypeConflict: true,
}

// Send delivers a message and applies its type's side effects (§9.5).
func (b *Bus) Send(ctx context.Context, m Message) (Message, error) {
	if !validTypes[m.Type] {
		return Message{}, fmt.Errorf("ipc: %q is not a message type", m.Type)
	}
	if strings.TrimSpace(m.Content) == "" {
		return Message{}, fmt.Errorf("ipc: a message needs content")
	}
	if m.To.AgentID == "" && m.To.ContainerID == "" && !m.To.Human {
		return Message{}, fmt.Errorf("ipc: a message needs a recipient")
	}

	// APPROVAL_REQUIRED always reaches a human, whatever the sender addressed.
	// An agent cannot approve on another agent's behalf, so routing it
	// anywhere else would strand it.
	if m.Type == TypeApprovalRequired {
		m.To.Human = true
	}
	if m.Type == TypeBlocked {
		m.To.Human = true
	}

	m.ID = ids.New(ids.Message)
	m.TS = ids.Now()
	m.Status = StatusQueued
	if m.Priority == "" {
		m.Priority = PriorityNormal
	}
	if m.Priority != PriorityNormal && m.Priority != PriorityHigh {
		return Message{}, fmt.Errorf("ipc: %q is not a priority", m.Priority)
	}

	refsJSON, err := json.Marshal(m.Refs)
	if err != nil {
		return Message{}, err
	}

	if _, err := b.Store.DB().ExecContext(ctx,
		`INSERT INTO messages (id, project_id, from_agent_id, from_container_id,
			to_agent_id, to_container_id, to_human, type, priority, content,
			refs_json, in_reply_to, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ProjectID, nullIfEmpty(m.From.AgentID), nullIfEmpty(m.From.ContainerID),
		nullIfEmpty(m.To.AgentID), nullIfEmpty(m.To.ContainerID), boolToInt(m.To.Human),
		m.Type, m.Priority, m.Content, string(refsJSON), nullIfEmpty(m.InReplyTo),
		m.Status, m.TS); err != nil {
		return Message{}, fmt.Errorf("ipc: send: %w", err)
	}

	if err := b.applySideEffects(ctx, m); err != nil {
		return Message{}, err
	}

	if b.Events != nil {
		_ = b.Events.Emit(ctx, events.Event{
			Type: events.AgentMessageSent, Actor: actorOf(m.From),
			ProjectID: m.ProjectID, ContainerID: m.From.ContainerID, AgentID: m.From.AgentID,
			Payload: map[string]any{
				"message": m.ID, "type": m.Type, "priority": m.Priority,
				"to_human": m.To.Human, "to_agent": m.To.AgentID, "to_container": m.To.ContainerID,
			},
		})
	}

	b.nudge(ctx, m)
	return m, nil
}

// applySideEffects performs the per-type consequences from §9.5.
func (b *Bus) applySideEffects(ctx context.Context, m Message) error {
	switch m.Type {
	case TypeBlocked:
		// The agent is stuck and the task is stuck; both must say so, or a
		// human watching the dashboard sees a container that looks busy.
		if m.From.AgentID != "" {
			_ = b.Store.UpdateAgentStatus(ctx, m.From.AgentID, store.AgentBlocked)
		}
		if m.Refs.Task != "" {
			_ = b.Store.TransitionTask(ctx, m.Refs.Task, store.TaskBlocked)
		}

	case TypeArtifact:
		if m.Refs.Artifact != "" || m.Refs.Branch != "" {
			ref := m.Refs.Artifact
			kind := "file"
			if ref == "" {
				ref, kind = m.Refs.Branch, "report"
			}
			if _, err := b.Store.DB().ExecContext(ctx,
				`INSERT INTO artifacts (id, container_id, task_id, kind, ref, meta_json, created_at)
				 VALUES (?,?,?,?,?,?,?)`,
				ids.New("art"), nullIfEmpty(m.From.ContainerID), nullIfEmpty(m.Refs.Task),
				kind, ref, m.Content, ids.Now()); err != nil {
				return fmt.Errorf("ipc: record artifact: %w", err)
			}
		}

	case TypeDependency:
		// Recording the dependency is what lets a later context change notify
		// the consumer automatically (see NotifyContextChange).
		if m.From.AgentID != "" && m.To.AgentID != "" && m.Refs.ContextKey != "" {
			if _, err := b.Store.DB().ExecContext(ctx,
				`INSERT OR IGNORE INTO dependencies (from_agent_id, to_agent_id, subject, created_at)
				 VALUES (?,?,?,?)`,
				m.To.AgentID, m.From.AgentID, m.Refs.ContextKey, ids.Now()); err != nil {
				return fmt.Errorf("ipc: record dependency: %w", err)
			}
		}
	}
	return nil
}

// Inbox returns undelivered and unacked messages for a recipient, marking them
// delivered.
//
// Already-delivered-but-unacked messages are returned again: at-least-once
// means an agent that crashed mid-turn still sees what it was sent.
func (b *Bus) Inbox(ctx context.Context, to Addr, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}

	var where []string
	var args []any
	if to.AgentID != "" {
		where = append(where, "to_agent_id = ?")
		args = append(args, to.AgentID)
	}
	if to.ContainerID != "" {
		where = append(where, "to_container_id = ?")
		args = append(args, to.ContainerID)
	}
	if to.Human {
		where = append(where, "to_human = 1")
	}
	if len(where) == 0 {
		return nil, fmt.Errorf("ipc: inbox needs a recipient")
	}

	query := `SELECT id, project_id, COALESCE(from_agent_id,''), COALESCE(from_container_id,''),
	                 COALESCE(to_agent_id,''), COALESCE(to_container_id,''), to_human,
	                 type, priority, content, COALESCE(refs_json,'{}'),
	                 COALESCE(in_reply_to,''), status, created_at,
	                 COALESCE(delivered_at,''), COALESCE(acked_at,'')
	          FROM messages
	          WHERE (` + strings.Join(where, " OR ") + `) AND status != 'acked'
	          ORDER BY CASE priority WHEN 'high' THEN 0 ELSE 1 END, created_at
	          LIMIT ?`
	args = append(args, limit)

	rows, err := b.Store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Mark delivered after the cursor is closed.
	now := ids.Now()
	for i := range out {
		if out[i].Status == StatusQueued {
			if _, err := b.Store.DB().ExecContext(ctx,
				`UPDATE messages SET status = ?, delivered_at = ? WHERE id = ? AND status = ?`,
				StatusDelivered, now, out[i].ID, StatusQueued); err != nil {
				return nil, err
			}
			out[i].Status, out[i].DeliveredAt = StatusDelivered, now

			if b.Events != nil {
				_ = b.Events.Emit(ctx, events.Event{
					Type: events.AgentMessageDelivered, Actor: events.ActorDaemon,
					ProjectID: out[i].ProjectID, AgentID: out[i].To.AgentID,
					ContainerID: out[i].To.ContainerID,
					Payload:     map[string]any{"message": out[i].ID, "type": out[i].Type},
				})
			}
		}
	}
	return out, nil
}

// History returns an agent's conversation — everything it sent and everything
// sent to it — oldest first, without delivering anything.
//
// Inbox cannot be reused for this. Reading a transcript in the dashboard must
// not consume the agent's inbox: at-least-once delivery means a message is
// "delivered" when the recipient was handed it, and a human reading over its
// shoulder is not the recipient.
func (b *Bus) History(ctx context.Context, agentID string, limit int) ([]Message, error) {
	if agentID == "" {
		return nil, fmt.Errorf("ipc: history needs an agent")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	// Ordered newest-first for the LIMIT, then reversed, so a long-running
	// agent's transcript shows its most recent turns rather than its first.
	rows, err := b.Store.DB().QueryContext(ctx,
		`SELECT id, project_id, COALESCE(from_agent_id,''), COALESCE(from_container_id,''),
		        COALESCE(to_agent_id,''), COALESCE(to_container_id,''), to_human,
		        type, priority, content, COALESCE(refs_json,'{}'),
		        COALESCE(in_reply_to,''), status, created_at,
		        COALESCE(delivered_at,''), COALESCE(acked_at,'')
		 FROM messages
		 WHERE from_agent_id = ? OR to_agent_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`, agentID, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Ack marks messages handled. Until this is called the message is still
// outstanding and will be shown again.
func (b *Bus) Ack(ctx context.Context, msgIDs []string) (int, error) {
	now := ids.Now()
	acked := 0

	for _, id := range msgIDs {
		res, err := b.Store.DB().ExecContext(ctx,
			`UPDATE messages SET status = ?, acked_at = ? WHERE id = ? AND status != ?`,
			StatusAcked, now, id, StatusAcked)
		if err != nil {
			return acked, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			acked++
			if b.Events != nil {
				_ = b.Events.Emit(ctx, events.Event{
					Type: events.AgentMessageAcked, Actor: events.ActorDaemon,
					Payload: map[string]any{"message": id},
				})
			}
		}
	}
	return acked, nil
}

// Get returns one message.
func (b *Bus) Get(ctx context.Context, id string) (Message, error) {
	row := b.Store.DB().QueryRowContext(ctx,
		`SELECT id, project_id, COALESCE(from_agent_id,''), COALESCE(from_container_id,''),
		        COALESCE(to_agent_id,''), COALESCE(to_container_id,''), to_human,
		        type, priority, content, COALESCE(refs_json,'{}'),
		        COALESCE(in_reply_to,''), status, created_at,
		        COALESCE(delivered_at,''), COALESCE(acked_at,'')
		 FROM messages WHERE id = ?`, id)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, store.ErrNotFound
	}
	return m, err
}

// nudge tells a running agent that something arrived, since an agent deep in a
// long turn will not call its inbox on its own.
func (b *Bus) nudge(ctx context.Context, m Message) {
	if b.Nudger == nil || m.To.ContainerID == "" {
		return
	}
	text := fmt.Sprintf("1 new %s from %s (%s) — call aurium_ipc_inbox",
		m.Type, describeFrom(m.From), m.ID)
	_ = b.Nudger.Nudge(ctx, m.To.ContainerID, text)

	if _, err := b.Store.DB().ExecContext(ctx,
		`UPDATE messages SET nudged_at = ? WHERE id = ?`, ids.Now(), m.ID); err != nil {
		return
	}
}

// RenudgeStale re-notifies recipients of high-priority messages that have gone
// undelivered (§9.5). Called periodically by the daemon.
func (b *Bus) RenudgeStale(ctx context.Context) (int, error) {
	cutoff := b.now().Add(-RenudgeAfter).Format(time.RFC3339Nano)

	rows, err := b.Store.DB().QueryContext(ctx,
		`SELECT id, COALESCE(to_container_id,''), type, COALESCE(from_agent_id,'')
		 FROM messages
		 WHERE status = ? AND priority = ? AND to_container_id IS NOT NULL
		   AND COALESCE(nudged_at, created_at) < ?`,
		StatusQueued, PriorityHigh, cutoff)
	if err != nil {
		return 0, err
	}

	type pending struct{ id, container, typ, from string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.container, &p.typ, &p.from); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()

	now := ids.Now()
	for _, p := range todo {
		if b.Nudger != nil && p.container != "" {
			_ = b.Nudger.Nudge(ctx, p.container,
				fmt.Sprintf("still waiting: %s (%s) — call aurium_ipc_inbox", p.typ, p.id))
		}
		if _, err := b.Store.DB().ExecContext(ctx,
			`UPDATE messages SET nudged_at = ? WHERE id = ?`, now, p.id); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}

// NotifyContextChange tells consumers that a context key they depend on moved
// (§9.5 DEPENDENCY).
//
// This is the payoff for recording dependencies: a producer changing an API
// contract does not have to remember who is relying on it.
func (b *Bus) NotifyContextChange(ctx context.Context, projectID, contextKey string, version int) (int, error) {
	rows, err := b.Store.DB().QueryContext(ctx,
		`SELECT DISTINCT to_agent_id FROM dependencies WHERE subject = ?`, contextKey)
	if err != nil {
		return 0, err
	}
	var consumers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		consumers = append(consumers, id)
	}
	rows.Close()

	for _, agentID := range consumers {
		if _, err := b.Send(ctx, Message{
			ProjectID: projectID,
			From:      Addr{},
			To:        Addr{AgentID: agentID},
			Type:      TypeInfo,
			Content: fmt.Sprintf("%s changed (now version %d). Re-read it before relying on it.",
				contextKey, version),
			Refs: Refs{ContextKey: contextKey},
		}); err != nil {
			return 0, err
		}
	}
	return len(consumers), nil
}

// ---- helpers ----

type scanner interface{ Scan(...any) error }

func scanMessage(sc scanner) (Message, error) {
	var (
		m        Message
		toHuman  int
		refsJSON string
	)
	err := sc.Scan(&m.ID, &m.ProjectID, &m.From.AgentID, &m.From.ContainerID,
		&m.To.AgentID, &m.To.ContainerID, &toHuman, &m.Type, &m.Priority,
		&m.Content, &refsJSON, &m.InReplyTo, &m.Status, &m.TS,
		&m.DeliveredAt, &m.AckedAt)
	if err != nil {
		return Message{}, err
	}
	m.To.Human = toHuman == 1
	if refsJSON != "" {
		_ = json.Unmarshal([]byte(refsJSON), &m.Refs)
	}
	return m, nil
}

func describeFrom(a Addr) string {
	switch {
	case a.AgentID != "":
		return a.AgentID
	case a.ContainerID != "":
		return a.ContainerID
	default:
		return "aurium"
	}
}

func actorOf(a Addr) string {
	if a.AgentID != "" {
		return events.ActorAgent(a.AgentID)
	}
	return events.ActorDaemon
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
