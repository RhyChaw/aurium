// Package store owns the Aurium SQLite database.
//
// Per D17 the daemon is the only writer, so this package assumes a single
// process. It still uses BEGIN IMMEDIATE for every write transaction because
// the compare-and-set semantics the context engine depends on (§8.4) need the
// write lock taken before the version is read, not after.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver; keeps CGO off so binaries stay static (D20)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is a handle on the Aurium database.
type Store struct {
	db   *sql.DB
	path string
}

// DB exposes the underlying pool for queries this package does not wrap.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the file the store was opened from.
func (s *Store) Path() string { return s.path }

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Open opens (creating if needed) the database at path and applies any
// migrations it is missing.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// WAL supports many concurrent readers alongside one writer, so the pool
	// holds several connections. A single connection would be simpler, but it
	// deadlocks the moment any code queries while iterating an open cursor —
	// the cursor holds the only connection and the query waits for it forever.
	// That is a trap laid for every future caller, so the pool is sized to
	// avoid it and write serialisation is left to BEGIN IMMEDIATE plus the
	// busy timeout, which is what SQLite provides it for.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds the connection string. The pragmas are D20's requirements: WAL so
// readers never block the writer, foreign keys so the §7 relations are actually
// enforced, and a busy timeout so a stray second connection waits rather than
// failing.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + path + "?" + q.Encode()
}

// Tx runs fn inside an immediate transaction, committing if fn returns nil and
// rolling back otherwise. The callback's error is returned unwrapped so callers
// can errors.Is against their own sentinels.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	// database/sql has no BEGIN IMMEDIATE option, so upgrade the deferred
	// transaction the driver started into a write transaction explicitly.
	if _, err := tx.ExecContext(ctx, "ROLLBACK; BEGIN IMMEDIATE"); err != nil {
		tx.Rollback()
		return fmt.Errorf("store: begin immediate: %w", err)
	}

	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// migrate applies every embedded migration whose name is not yet recorded in
// schema_migrations, in lexical order, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, datetime('now'))`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %s commit: %w", name, err)
		}
	}
	return nil
}
