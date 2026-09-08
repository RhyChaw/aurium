package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/RhyChaw/aurium/internal/ids"
)

const taskColumns = `id, project_id, title, status, COALESCE(parent_task_id,''), created_at, updated_at`

// CreateTask records a unit of work. parentTaskID may be empty.
func (s *Store) CreateTask(ctx context.Context, projectID, title, parentTaskID string) (Task, error) {
	now := ids.Now()
	t := Task{
		ID:           ids.New(ids.Task),
		ProjectID:    projectID,
		Title:        title,
		Status:       TaskCreated,
		ParentTaskID: parentTaskID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, title, status, parent_task_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Title, t.Status, nullable(t.ParentTaskID), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return Task{}, fmt.Errorf("store: create task %q: %w", title, err)
	}
	return t, nil
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	err := s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.ProjectID, &t.Title, &t.Status, &t.ParentTaskID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

func (s *Store) ListTasks(ctx context.Context, projectID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Status,
			&t.ParentTaskID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransitionTask moves a task to a new status and bumps updated_at. The status
// is validated here as well as by the CHECK constraint so callers get a clear
// error naming the legal values rather than a SQLite constraint message.
func (s *Store) TransitionTask(ctx context.Context, id, status string) error {
	if !slices.Contains(TaskStatuses, status) {
		return fmt.Errorf("store: %q is not a task status (want one of %v)", status, TaskStatuses)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, status, ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "task", id)
}

func mustAffect(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("store: %s %s: %w", kind, id, ErrNotFound)
	}
	return nil
}
