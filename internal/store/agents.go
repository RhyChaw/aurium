package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/RhyChaw/aurium/internal/ids"
)

const agentColumns = `id, container_id, adapter, role, COALESCE(parent_agent_id,''),
	tmux_session, COALESCE(model,''), status, COALESCE(started_at,''), COALESCE(last_activity_at,'')`

// CreateAgent registers an agent in a container.
func (s *Store) CreateAgent(ctx context.Context, a Agent) (Agent, error) {
	if a.ID == "" {
		a.ID = ids.New(ids.Agent)
	}
	if a.TmuxSession == "" {
		a.TmuxSession = "agent"
	}
	if a.Status == "" {
		a.Status = AgentStarting
	}
	a.StartedAt = ids.Now()
	a.LastActivityAt = a.StartedAt

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (id, container_id, adapter, role, parent_agent_id,
			tmux_session, model, status, started_at, last_activity_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ContainerID, a.Adapter, a.Role, nullable(a.ParentAgentID),
		a.TmuxSession, nullable(a.Model), a.Status, a.StartedAt, a.LastActivityAt)
	if err != nil {
		return Agent{}, fmt.Errorf("store: create agent in %s: %w", a.ContainerID, err)
	}
	return a, nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (Agent, error) {
	var a Agent
	err := s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = ?`, id).
		Scan(&a.ID, &a.ContainerID, &a.Adapter, &a.Role, &a.ParentAgentID,
			&a.TmuxSession, &a.Model, &a.Status, &a.StartedAt, &a.LastActivityAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// ListAgents returns every agent in a container.
func (s *Store) ListAgents(ctx context.Context, containerID string) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE container_id = ? ORDER BY id`, containerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.ContainerID, &a.Adapter, &a.Role, &a.ParentAgentID,
			&a.TmuxSession, &a.Model, &a.Status, &a.StartedAt, &a.LastActivityAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListLiveAgents returns agents that have not exited. D15 is enforced against
// this set: an exited agent does not block starting a replacement.
func (s *Store) ListLiveAgents(ctx context.Context, containerID string) ([]Agent, error) {
	all, err := s.ListAgents(ctx, containerID)
	if err != nil {
		return nil, err
	}
	var live []Agent
	for _, a := range all {
		if a.Status != AgentExited && a.Status != AgentError {
			live = append(live, a)
		}
	}
	return live, nil
}

func (s *Store) UpdateAgentStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status = ?, last_activity_at = ? WHERE id = ?`,
		status, ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "agent", id)
}

func (s *Store) TouchAgent(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET last_activity_at = ? WHERE id = ?`, ids.Now(), id)
	return err
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	return err
}
