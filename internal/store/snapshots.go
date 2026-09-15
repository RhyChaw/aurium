package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/RhyChaw/aurium/internal/ids"
)

const snapshotColumns = `id, container_id, seq, COALESCE(label,''), trigger,
	head_sha, tree_ref, base_sha, image_ref, manifest_path, context_version,
	COALESCE(bytes,0), created_at, includes_conversation, COALESCE(note,'')`

// NextSnapshotSeq returns the next per-container sequence number.
//
// Sequence numbers are per container and dense, because they are what the user
// types (`aurium restore c_1 7`). A ULID would be unambiguous but unusable.
func (s *Store) NextSnapshotSeq(ctx context.Context, containerID string) (int, error) {
	var max sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT max(seq) FROM snapshots WHERE container_id = ?`, containerID).Scan(&max)
	if err != nil {
		return 0, err
	}
	return int(max.Int64) + 1, nil
}

func (s *Store) CreateSnapshot(ctx context.Context, sn Snapshot) (Snapshot, error) {
	if sn.ID == "" {
		sn.ID = ids.New(ids.Snapshot)
	}
	sn.CreatedAt = ids.Now()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshots (id, container_id, seq, label, trigger, head_sha, tree_ref,
			base_sha, image_ref, manifest_path, context_version, bytes, created_at,
			includes_conversation, note)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sn.ID, sn.ContainerID, sn.Seq, nullable(sn.Label), sn.Trigger, sn.HeadSHA, sn.TreeRef,
		sn.BaseSHA, sn.ImageRef, sn.ManifestPath, sn.ContextVersion, sn.Bytes, sn.CreatedAt,
		sn.IncludesConversation, nullable(sn.Note))
	if err != nil {
		return Snapshot{}, fmt.Errorf("store: create snapshot %d for %s: %w", sn.Seq, sn.ContainerID, err)
	}
	return sn, nil
}

func (s *Store) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	return s.scanSnapshot(s.db.QueryRowContext(ctx,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE id = ?`, id))
}

// GetSnapshotBySeq is the lookup the CLI uses, since users refer to snapshots
// by their container-local sequence number.
func (s *Store) GetSnapshotBySeq(ctx context.Context, containerID string, seq int) (Snapshot, error) {
	return s.scanSnapshot(s.db.QueryRowContext(ctx,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE container_id = ? AND seq = ?`,
		containerID, seq))
}

// LatestSnapshot returns the most recent snapshot of a container.
func (s *Store) LatestSnapshot(ctx context.Context, containerID string) (Snapshot, error) {
	return s.scanSnapshot(s.db.QueryRowContext(ctx,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE container_id = ?
		 ORDER BY seq DESC LIMIT 1`, containerID))
}

func (s *Store) ListSnapshots(ctx context.Context, containerID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE container_id = ? ORDER BY seq DESC`,
		containerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Snapshot
	for rows.Next() {
		sn, err := scanSnapshotRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// ReferencedSnapshotIDs returns snapshots a container was forked or stacked
// from. Retention must never delete these: the lineage would become
// unexplainable and the child unreproducible (§6.7).
func (s *Store) ReferencedSnapshotIDs(ctx context.Context, projectID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT origin_snapshot_id FROM containers
		 WHERE project_id = ? AND origin_snapshot_id IS NOT NULL`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

func (s *Store) scanSnapshot(row *sql.Row) (Snapshot, error) {
	sn, err := scanSnapshotRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return sn, err
}

func scanSnapshotRow(sc scanner) (Snapshot, error) {
	var sn Snapshot
	err := sc.Scan(&sn.ID, &sn.ContainerID, &sn.Seq, &sn.Label, &sn.Trigger,
		&sn.HeadSHA, &sn.TreeRef, &sn.BaseSHA, &sn.ImageRef, &sn.ManifestPath,
		&sn.ContextVersion, &sn.Bytes, &sn.CreatedAt,
		&sn.IncludesConversation, &sn.Note)
	return sn, err
}
