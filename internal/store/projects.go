package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/RhyChaw/aurium/internal/ids"
)

// CreateProject registers a repository root with Aurium.
func (s *Store) CreateProject(ctx context.Context, name, root string) (Project, error) {
	p := Project{
		ID:        ids.New(ids.Project),
		Name:      name,
		Root:      root,
		CreatedAt: ids.Now(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, root, created_at) VALUES (?, ?, ?, ?)`,
		p.ID, p.Name, p.Root, p.CreatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("store: create project %q: %w", root, err)
	}
	return p, nil
}

func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	return s.scanProject(s.db.QueryRowContext(ctx,
		`SELECT id, name, root, created_at FROM projects WHERE id = ?`, id))
}

// ProjectByRoot finds the project whose root is exactly root. Callers that
// need "the project containing this directory" walk up the tree themselves.
func (s *Store) ProjectByRoot(ctx context.Context, root string) (Project, error) {
	return s.scanProject(s.db.QueryRowContext(ctx,
		`SELECT id, name, root, created_at FROM projects WHERE root = ?`, root))
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, root, created_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Root, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) scanProject(row *sql.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Root, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

// CreateRepository adds a git repository to a project.
func (s *Store) CreateRepository(ctx context.Context, projectID, path, baseBranch, remote string) (Repository, error) {
	r := Repository{
		ID:         ids.New("repo"),
		ProjectID:  projectID,
		Path:       path,
		BaseBranch: baseBranch,
		Remote:     remote,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repositories (id, project_id, path, base_branch, remote) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.ProjectID, r.Path, r.BaseBranch, nullable(r.Remote))
	if err != nil {
		return Repository{}, fmt.Errorf("store: create repository %q: %w", path, err)
	}
	return r, nil
}

func (s *Store) GetRepository(ctx context.Context, id string) (Repository, error) {
	return s.scanRepository(s.db.QueryRowContext(ctx,
		`SELECT id, project_id, path, base_branch, COALESCE(remote,'') FROM repositories WHERE id = ?`, id))
}

func (s *Store) RepositoryByPath(ctx context.Context, projectID, path string) (Repository, error) {
	return s.scanRepository(s.db.QueryRowContext(ctx,
		`SELECT id, project_id, path, base_branch, COALESCE(remote,'')
		 FROM repositories WHERE project_id = ? AND path = ?`, projectID, path))
}

func (s *Store) scanRepository(row *sql.Row) (Repository, error) {
	var r Repository
	err := row.Scan(&r.ID, &r.ProjectID, &r.Path, &r.BaseBranch, &r.Remote)
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, ErrNotFound
	}
	if err != nil {
		return Repository{}, err
	}
	return r, nil
}

// nullable turns "" into a SQL NULL so optional columns stay NULL rather than
// storing an empty string that later reads back as a real value.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
