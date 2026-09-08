// Package events is Aurium's event bus: the audit log, the dashboard feed and
// the plugin surface, all the same stream (D19).
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// subscriberBuffer is how many events a subscriber may fall behind before it
// is dropped. Emit is called inside state transitions and must never block, so
// a consumer that stops reading loses its subscription rather than wedging the
// daemon.
const subscriberBuffer = 256

// Event is one state transition.
type Event struct {
	ID          int64          `json:"id"`
	TS          string         `json:"ts"`
	Type        string         `json:"type"`
	Actor       string         `json:"actor"`
	ProjectID   string         `json:"project_id,omitempty"`
	ContainerID string         `json:"container_id,omitempty"`
	AgentID     string         `json:"agent_id,omitempty"`
	TaskID      string         `json:"task_id,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
}

// Filter selects a subset of the stream.
type Filter struct {
	Types       []string
	ProjectID   string
	ContainerID string
	AgentID     string
	TaskID      string
}

func (f Filter) matches(e Event) bool {
	if len(f.Types) > 0 && !slices.Contains(f.Types, e.Type) {
		return false
	}
	if f.ProjectID != "" && e.ProjectID != f.ProjectID {
		return false
	}
	if f.ContainerID != "" && e.ContainerID != f.ContainerID {
		return false
	}
	if f.AgentID != "" && e.AgentID != f.AgentID {
		return false
	}
	if f.TaskID != "" && e.TaskID != f.TaskID {
		return false
	}
	return true
}

type subscriber struct {
	ch     chan Event
	filter Filter
	once   sync.Once
}

// Bus persists events and fans them out to live subscribers.
type Bus struct {
	store *store.Store

	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

// New returns a bus backed by a store.
func New(s *store.Store) *Bus {
	return &Bus{store: s, subs: map[*subscriber]struct{}{}}
}

// Emit persists an event and delivers it to subscribers.
//
// D19: persistence happens before this returns, and callers call it before
// replying to an API request. A crash between the state change and the reply
// therefore cannot lose the record that the change happened.
func (b *Bus) Emit(ctx context.Context, e Event) error {
	_, err := b.EmitReturning(ctx, e)
	return err
}

// EmitReturning is Emit, also giving back the assigned id and timestamp.
func (b *Bus) EmitReturning(ctx context.Context, e Event) (Event, error) {
	if e.Type == "" {
		return e, fmt.Errorf("events: event has no type")
	}
	if e.Actor == "" {
		// Every event answers "who did this"; an unattributed one is a bug.
		return e, fmt.Errorf("events: event %q has no actor", e.Type)
	}
	e.TS = ids.Now()

	payload := []byte("{}")
	if e.Payload != nil {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return e, fmt.Errorf("events: marshal payload for %s: %w", e.Type, err)
		}
		payload = b
	}

	res, err := b.store.DB().ExecContext(ctx,
		`INSERT INTO events (ts, project_id, container_id, agent_id, task_id, type, actor, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, nullify(e.ProjectID), nullify(e.ContainerID), nullify(e.AgentID), nullify(e.TaskID),
		e.Type, e.Actor, string(payload))
	if err != nil {
		return e, fmt.Errorf("events: persist %s: %w", e.Type, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}

	b.fanout(e)
	return e, nil
}

// fanout delivers to subscribers without blocking. A subscriber that has
// fallen more than subscriberBuffer events behind is closed and dropped.
func (b *Bus) fanout(e Event) {
	b.mu.RLock()
	targets := make([]*subscriber, 0, len(b.subs))
	for s := range b.subs {
		if s.filter.matches(e) {
			targets = append(targets, s)
		}
	}
	b.mu.RUnlock()

	var stalled []*subscriber
	for _, s := range targets {
		select {
		case s.ch <- e:
		default:
			stalled = append(stalled, s)
		}
	}
	for _, s := range stalled {
		b.remove(s)
	}
}

// Subscribe returns a channel of matching events and a cancel function. The
// caller must call cancel, or the subscription leaks until it stalls.
func (b *Bus) Subscribe(f Filter) (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, subscriberBuffer), filter: f}

	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	return s.ch, func() { b.remove(s) }
}

func (b *Bus) remove(s *subscriber) {
	b.mu.Lock()
	_, present := b.subs[s]
	delete(b.subs, s)
	b.mu.Unlock()

	if present {
		s.once.Do(func() { close(s.ch) })
	}
}

// Replay returns persisted events with id greater than sinceID.
//
// This is what makes a reconnecting SSE client neither miss nor duplicate
// events: it reports the last id it saw and resumes from there.
func (b *Bus) Replay(ctx context.Context, sinceID int64, f Filter, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}

	query := `SELECT id, ts, COALESCE(project_id,''), COALESCE(container_id,''),
	                 COALESCE(agent_id,''), COALESCE(task_id,''), type, actor, payload_json
	          FROM events WHERE id > ?`
	args := []any{sinceID}

	if f.ProjectID != "" {
		query += ` AND project_id = ?`
		args = append(args, f.ProjectID)
	}
	if f.ContainerID != "" {
		query += ` AND container_id = ?`
		args = append(args, f.ContainerID)
	}
	if f.AgentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, f.AgentID)
	}
	if f.TaskID != "" {
		query += ` AND task_id = ?`
		args = append(args, f.TaskID)
	}
	query += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := b.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.ID, &e.TS, &e.ProjectID, &e.ContainerID,
			&e.AgentID, &e.TaskID, &e.Type, &e.Actor, &payload); err != nil {
			return nil, err
		}
		if payload != "" && payload != "{}" {
			if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
				return nil, fmt.Errorf("events: decode payload of event %d: %w", e.ID, err)
			}
		}
		// Type filtering happens here rather than in SQL so a long IN list
		// does not have to be built for every request.
		if len(f.Types) > 0 && !slices.Contains(f.Types, e.Type) {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Latest returns the highest event id, so a client can subscribe from "now".
func (b *Bus) Latest(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	if err := b.store.DB().QueryRowContext(ctx, `SELECT max(id) FROM events`).Scan(&id); err != nil {
		return 0, err
	}
	return id.Int64, nil
}

func nullify(s string) any {
	if s == "" {
		return nil
	}
	return s
}
