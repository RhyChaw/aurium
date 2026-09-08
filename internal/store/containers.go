package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/RhyChaw/aurium/internal/ids"
)

const containerColumns = `
	id, project_id, COALESCE(task_id,''), repo_id, branch, slug,
	COALESCE(parent_container_id,''), parent_branch,
	COALESCE(origin_snapshot_id,''), COALESCE(origin_kind,''),
	base_sha, COALESCE(pending_base_sha,''), COALESCE(head_sha,''),
	driver, COALESCE(runtime_id,''), COALESCE(image,''), worktree,
	COALESCE(ports_json,''), COALESCE(network,''),
	status, COALESCE(last_error,''), created_at, updated_at`

// CreateContainer inserts a container row. The caller supplies everything
// except the id and timestamps. The UNIQUE (repo_id, branch) constraint is the
// database-level statement of D15: one container owns one branch.
func (s *Store) CreateContainer(ctx context.Context, c Container) (Container, error) {
	if c.ID == "" {
		c.ID = ids.New(ids.Container)
	}
	now := ids.Now()
	c.CreatedAt, c.UpdatedAt = now, now

	portsJSON, err := marshalPorts(c.Ports)
	if err != nil {
		return Container{}, err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO containers (
			id, project_id, task_id, repo_id, branch, slug,
			parent_container_id, parent_branch, origin_snapshot_id, origin_kind,
			base_sha, pending_base_sha, head_sha,
			driver, runtime_id, image, worktree, ports_json, network,
			status, last_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,?, ?,?,?,?, ?,?,?, ?,?,?,?,?,?, ?,?,?,?)`,
		c.ID, c.ProjectID, nullable(c.TaskID), c.RepoID, c.Branch, c.Slug,
		nullable(c.ParentContainerID), c.ParentBranch, nullable(c.OriginSnapshotID), nullable(c.OriginKind),
		c.BaseSHA, nullable(c.PendingBaseSHA), nullable(c.HeadSHA),
		c.Driver, nullable(c.RuntimeID), nullable(c.Image), c.Worktree, nullable(portsJSON), nullable(c.Network),
		c.Status, nullable(c.LastError), c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return Container{}, fmt.Errorf("store: create container %q: %w", c.Branch, err)
	}
	return c, nil
}

func (s *Store) GetContainer(ctx context.Context, id string) (Container, error) {
	return s.scanContainer(s.db.QueryRowContext(ctx,
		`SELECT `+containerColumns+` FROM containers WHERE id = ?`, id))
}

func (s *Store) GetContainerByBranch(ctx context.Context, repoID, branch string) (Container, error) {
	return s.scanContainer(s.db.QueryRowContext(ctx,
		`SELECT `+containerColumns+` FROM containers WHERE repo_id = ? AND branch = ?`, repoID, branch))
}

func (s *Store) GetContainerBySlug(ctx context.Context, projectID, slug string) (Container, error) {
	return s.scanContainer(s.db.QueryRowContext(ctx,
		`SELECT `+containerColumns+` FROM containers WHERE project_id = ? AND slug = ?`, projectID, slug))
}

func (s *Store) ListContainers(ctx context.Context, projectID string) ([]Container, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+containerColumns+` FROM containers WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.collectContainers(rows)
}

// ListChildren returns the containers whose git parent is this container.
func (s *Store) ListChildren(ctx context.Context, containerID string) ([]Container, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+containerColumns+` FROM containers WHERE parent_container_id = ? ORDER BY id`, containerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.collectContainers(rows)
}

func (s *Store) collectContainers(rows *sql.Rows) ([]Container, error) {
	var out []Container
	for rows.Next() {
		c, err := scanContainerRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetContainerRuntime records what the driver actually created: its id, the
// image it came from, the network it joined, and the host ports it published.
func (s *Store) SetContainerRuntime(ctx context.Context, id, runtimeID, image, network string, ports map[int]int) error {
	portsJSON, err := marshalPorts(ports)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE containers SET runtime_id = ?, image = ?, network = ?, ports_json = ?, updated_at = ?
		 WHERE id = ?`,
		nullable(runtimeID), nullable(image), nullable(network), nullable(portsJSON), ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "container", id)
}

// UpdateContainerStatus sets the lifecycle status and an optional error note.
func (s *Store) UpdateContainerStatus(ctx context.Context, id, status, lastError string) error {
	if !slices.Contains(ContainerStatuses, status) {
		return fmt.Errorf("store: %q is not a container status (want one of %v)", status, ContainerStatuses)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE containers SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, nullable(lastError), ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "container", id)
}

// UpdateContainerBaseSHA advances the recorded base after a successful sync
// (D6/§6.5). This is the single most consequential write in the stack engine:
// it is what makes the next rebase compute the right range.
func (s *Store) UpdateContainerBaseSHA(ctx context.Context, id, baseSHA string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE containers SET base_sha = ?, pending_base_sha = NULL, updated_at = ? WHERE id = ?`,
		baseSHA, ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "container", id)
}

func (s *Store) UpdateContainerHeadSHA(ctx context.Context, id, headSHA string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE containers SET head_sha = ?, updated_at = ? WHERE id = ?`,
		nullable(headSHA), ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "container", id)
}

// SetContainerOrigin records where a forked/stacked/restored container came
// from, so retention (§6.7) knows which snapshots are still referenced.
func (s *Store) SetContainerOrigin(ctx context.Context, id, snapshotID, kind string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE containers SET origin_snapshot_id = ?, origin_kind = ?, updated_at = ? WHERE id = ?`,
		nullable(snapshotID), nullable(kind), ids.Now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "container", id)
}

func (s *Store) DeleteContainer(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM containers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res, "container", id)
}

func (s *Store) scanContainer(row *sql.Row) (Container, error) {
	c, err := scanContainerRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Container{}, ErrNotFound
	}
	return c, err
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanContainerRow(sc scanner) (Container, error) {
	var c Container
	var portsJSON string
	err := sc.Scan(
		&c.ID, &c.ProjectID, &c.TaskID, &c.RepoID, &c.Branch, &c.Slug,
		&c.ParentContainerID, &c.ParentBranch, &c.OriginSnapshotID, &c.OriginKind,
		&c.BaseSHA, &c.PendingBaseSHA, &c.HeadSHA,
		&c.Driver, &c.RuntimeID, &c.Image, &c.Worktree, &portsJSON, &c.Network,
		&c.Status, &c.LastError, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Container{}, err
	}
	c.Ports, err = unmarshalPorts(portsJSON)
	return c, err
}

// Ports are stored as JSON because SQLite has no map type. JSON object keys
// must be strings, so the int port numbers round-trip through strconv.
func marshalPorts(p map[int]int) (string, error) {
	if len(p) == 0 {
		return "", nil
	}
	m := make(map[string]int, len(p))
	for internal, host := range p {
		m[strconv.Itoa(internal)] = host
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("store: marshal ports: %w", err)
	}
	return string(b), nil
}

func unmarshalPorts(s string) (map[int]int, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("store: unmarshal ports %q: %w", s, err)
	}
	out := make(map[int]int, len(m))
	for k, host := range m {
		internal, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("store: port key %q is not a number: %w", k, err)
		}
		out[internal] = host
	}
	return out, nil
}
