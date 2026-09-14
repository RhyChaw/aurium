package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/RhyChaw/aurium/internal/ids"
)

// projectColumns is every column ListProjects and the Get*s read, in the order
// scanProject expects them.
const projectColumns = `id, name, root, COALESCE(descriptor,''), created_at`

// CreateProject registers a project root with Aurium.
//
// A project created here is standalone: its descriptor is empty until
// SetProjectDescriptor records one (§D22). Keeping descriptor out of this
// signature means `aurium init` and the multi-repo path both go through the
// same constructor, and the difference between them is one explicit call.
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
		if isConstraintErr(err) {
			return Project{}, fmt.Errorf("%w: a project rooted at %q", ErrDuplicate, root)
		}
		return Project{}, fmt.Errorf("store: create project %q: %w", root, err)
	}
	return p, nil
}

// SetProjectDescriptor records where a project's descriptor file lives. It is
// how a standalone project becomes a real one: `aurium init` made the row,
// writing a descriptor later fills this in.
func (s *Store) SetProjectDescriptor(ctx context.Context, id, descriptor string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET descriptor = ? WHERE id = ?`, descriptor, id)
	if err != nil {
		return err
	}
	return mustAffect(res, "project", id)
}

// RenameProject changes the display name.
func (s *Store) RenameProject(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	return mustAffect(res, "project", id)
}

func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	return s.scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
}

// ProjectByRoot finds the project whose root is exactly root. Callers that
// need "the project containing this directory" walk up the tree themselves.
func (s *Store) ProjectByRoot(ctx context.Context, root string) (Project, error) {
	return s.scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE root = ?`, root))
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Root, &p.Descriptor, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) scanProject(row *sql.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Root, &p.Descriptor, &p.CreatedAt)
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

// ListRepositories returns every repository in a project. A project may hold
// several (§D22); the runtime creates containers in one repo at a time, but the
// dashboard shows them together because that is how the work is actually
// organised.
func (s *Store) ListRepositories(ctx context.Context, projectID string) ([]Repository, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, path, base_branch, COALESCE(remote,'')
		 FROM repositories WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Repository
	for rows.Next() {
		var r Repository
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Path, &r.BaseBranch, &r.Remote); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RepositoryByPathAny finds a repository by path across every project.
//
// D23: this is how a directory resolves to a project. Looking the project up
// by its root would tie a project to one repository forever, which is exactly
// the limitation multi-repo projects exist to remove — a repo may sit anywhere
// on disk and belong to a project rooted somewhere else entirely.
func (s *Store) RepositoryByPathAny(ctx context.Context, path string) (Repository, error) {
	return s.scanRepository(s.db.QueryRowContext(ctx,
		`SELECT id, project_id, path, base_branch, COALESCE(remote,'')
		 FROM repositories WHERE path = ?`, path))
}

// DeleteRepository detaches a repository from its project. Containers
// referencing it keep their rows, so nothing a user can see silently vanishes;
// they are reported as orphaned instead.
func (s *Store) DeleteRepository(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res, "repository", id)
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
