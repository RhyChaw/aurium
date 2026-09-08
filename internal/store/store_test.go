package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestOpenAppliesSchemaAndPragmas(t *testing.T) {
	s := openTest(t)

	var fk int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatal("foreign_keys must be ON")
	}

	for _, table := range []string{
		"projects", "repositories", "tasks", "containers", "snapshots", "agents", "tokens",
		"context_items", "context_versions", "context_grants", "context_proposals", "context_docs",
		"context_fts", "docs_fts",
		"messages", "dependencies", "integrations", "capabilities", "grants", "approvals",
		"artifacts", "events",
	} {
		var n int
		if err := s.DB().QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %q missing from schema", table)
		}
	}
}

func TestWALIsEnabledOnDiskDatabases(t *testing.T) {
	s, err := Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var mode string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal (D20)", mode)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s := openTest(t)
	_, err := s.DB().Exec(
		`INSERT INTO tasks (id, project_id, title, status, created_at, updated_at)
		 VALUES ('t_x','p_nonexistent','t','created','now','now')`)
	if err == nil {
		t.Fatal("insert with a dangling project_id must fail")
	}
}

func TestCheckConstraintRejectsUnknownStatus(t *testing.T) {
	s := openTest(t)
	seedProject(t, s, "p_1")
	_, err := s.DB().Exec(
		`INSERT INTO tasks (id, project_id, title, status, created_at, updated_at)
		 VALUES ('t_x','p_1','t','not_a_status','now','now')`)
	if err == nil {
		t.Fatal("CHECK constraint must reject an unknown task status")
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	s := openTest(t)
	sentinel := errors.New("boom")

	err := s.Tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO projects (id,name,root,created_at) VALUES ('p_1','n','/r','now')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx must return the callback error, got %v", err)
	}

	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM projects").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a failed Tx must roll back")
	}
}

func TestTxCommitsOnSuccess(t *testing.T) {
	s := openTest(t)
	err := s.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (id,name,root,created_at) VALUES ('p_1','n','/r','now')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow("SELECT count(*) FROM projects").Scan(&n)
	if n != 1 {
		t.Fatal("a successful Tx must commit")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()

	// Reopening must not attempt to re-apply 0001 and fail on "table exists".
	s2, err := Open(dir + "/aurium.db")
	if err != nil {
		t.Fatalf("reopening an already-migrated database must succeed: %v", err)
	}
	defer s2.Close()

	var n int
	s2.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n)
	if n == 0 {
		t.Fatal("schema_migrations must record applied migrations")
	}
}

// The FTS5 index is external-content, so it only populates via triggers.
// Without them every context query silently returns nothing.
func TestContextFTSIsPopulatedByTriggers(t *testing.T) {
	s := openTest(t)
	seedProject(t, s, "p_1")
	_, err := s.DB().Exec(
		`INSERT INTO context_items (id, scope, scope_id, key, version, content, mime, updated_by, updated_at)
		 VALUES ('ci_1','project','p_1','decisions/jwt',1,'we chose asymmetric JWT signing','text/markdown','human','now')`)
	if err != nil {
		t.Fatal(err)
	}

	var key string
	err = s.DB().QueryRow(
		`SELECT key FROM context_fts WHERE context_fts MATCH 'asymmetric'`).Scan(&key)
	if err != nil {
		t.Fatalf("FTS must find the inserted item: %v", err)
	}
	if key != "decisions/jwt" {
		t.Fatalf("got key %q", key)
	}

	// And an update must replace, not duplicate, the indexed row.
	if _, err := s.DB().Exec(
		`UPDATE context_items SET content = 'we chose symmetric signing instead' WHERE id = 'ci_1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM context_fts WHERE context_fts MATCH 'asymmetric'`).Scan(&n)
	if n != 0 {
		t.Fatal("stale FTS row survived an update")
	}
	s.DB().QueryRow(`SELECT count(*) FROM context_fts WHERE context_fts MATCH 'symmetric'`).Scan(&n)
	if n != 1 {
		t.Fatalf("updated content not indexed, got %d rows", n)
	}
}

// ---- helpers ----

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedProject(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT INTO projects (id,name,root,created_at) VALUES (?,?,?,?)`,
		id, "proj", "/root/"+id, "2026-09-08T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}
