// Package contextengine implements Aurium's context store (§8): versioned,
// permissioned items that agents read and write through MCP tools.
//
// The design problem it solves is that several agents share knowledge but must
// not silently overwrite each other. The answer is compare-and-set on a version
// (§8.4): a writer states the version it read, and a write against a stale
// version is refused rather than applied. That turns a lost update — invisible,
// discovered days later — into a 409 the agent can reconcile immediately.
package contextengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// Scopes (§8.1). An agent's effective context is the union, with the more
// specific scope shadowing on key collision.
const (
	ScopeProject   = "project"
	ScopeContainer = "container"
	ScopeAgent     = "agent"
)

// Permissions (§8.3). Note there is no "commit": committing is the outcome of
// a write (direct) or of an accepted proposal, not a permission of its own.
type Perm string

const (
	PermNone    Perm = ""
	PermRead    Perm = "read"
	PermAppend  Perm = "append"
	PermPropose Perm = "propose"
	PermWrite   Perm = "write"
)

// rank orders permissions so a subject holding a stronger one satisfies a
// weaker requirement: write implies propose implies append implies read.
var rank = map[Perm]int{PermNone: 0, PermRead: 1, PermAppend: 2, PermPropose: 3, PermWrite: 4}

// Allows reports whether holding p satisfies a requirement for want.
func (p Perm) Allows(want Perm) bool { return rank[p] >= rank[want] }

// Ref identifies an item.
type Ref struct {
	Scope   string
	ScopeID string
	Key     string
}

func (r Ref) String() string { return r.Scope + ":" + r.ScopeID + "/" + r.Key }

// Item is a context entry.
type Item struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	ScopeID   string `json:"scope_id"`
	Key       string `json:"key"`
	Version   int    `json:"version"`
	Content   string `json:"content"`
	MIME      string `json:"mime"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// ErrStale is returned when a compare-and-set write loses the race.
//
// It carries the current version and content so the caller can reconcile
// without a second round trip — an agent that has to ask again is an agent
// that will probably just retry blindly.
type ErrStale struct {
	Ref            Ref
	ExpectedVersion int
	CurrentVersion  int
	CurrentContent  string
}

func (e *ErrStale) Error() string {
	return fmt.Sprintf("context: %s moved to version %d while you held version %d",
		e.Ref, e.CurrentVersion, e.ExpectedVersion)
}

// ErrPermission is returned when a subject lacks the permission for an action.
type ErrPermission struct {
	Ref    Ref
	Want   Perm
	Have   Perm
	Reason string
}

func (e *ErrPermission) Error() string {
	have := string(e.Have)
	if have == "" {
		have = "none"
	}
	return fmt.Sprintf("context: %s requires %s permission on %s (have %s)",
		e.Reason, e.Want, e.Ref, have)
}

// Engine reads and writes context.
type Engine struct {
	Store  *store.Store
	Events *events.Bus
}

// New returns an engine.
func New(s *store.Store, bus *events.Bus) *Engine {
	return &Engine{Store: s, Events: bus}
}

// Get returns an item.
func (e *Engine) Get(ctx context.Context, ref Ref) (Item, error) {
	var it Item
	err := e.Store.DB().QueryRowContext(ctx,
		`SELECT id, scope, scope_id, key, version, content, mime, updated_by, updated_at
		 FROM context_items WHERE scope = ? AND scope_id = ? AND key = ?`,
		ref.Scope, ref.ScopeID, ref.Key).
		Scan(&it.ID, &it.Scope, &it.ScopeID, &it.Key, &it.Version,
			&it.Content, &it.MIME, &it.UpdatedBy, &it.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, store.ErrNotFound
	}
	return it, err
}

// List returns items in a scope whose key has the given prefix.
func (e *Engine) List(ctx context.Context, scope, scopeID, prefix string) ([]Item, error) {
	rows, err := e.Store.DB().QueryContext(ctx,
		`SELECT id, scope, scope_id, key, version, content, mime, updated_by, updated_at
		 FROM context_items WHERE scope = ? AND scope_id = ? AND key LIKE ?
		 ORDER BY key`, scope, scopeID, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Scope, &it.ScopeID, &it.Key, &it.Version,
			&it.Content, &it.MIME, &it.UpdatedBy, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MaxVersion returns the highest version in a scope, which a snapshot records
// so a restore can say what context the container saw (§6.1).
func (e *Engine) MaxVersion(ctx context.Context, scope, scopeID string) (int, error) {
	var v sql.NullInt64
	err := e.Store.DB().QueryRowContext(ctx,
		`SELECT max(version) FROM context_items WHERE scope = ? AND scope_id = ?`,
		scope, scopeID).Scan(&v)
	return int(v.Int64), err
}

// Write replaces an item's content, using compare-and-set on baseVersion
// (§8.4).
//
// Passing baseVersion 0 creates the item and fails if it already exists, so
// "create" and "blind overwrite" cannot be confused.
func (e *Engine) Write(ctx context.Context, ref Ref, baseVersion int, content, author, reason string) (Item, error) {
	var result Item

	err := e.Store.Tx(ctx, func(tx *sql.Tx) error {
		var (
			id      string
			version int
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, version FROM context_items WHERE scope = ? AND scope_id = ? AND key = ?`,
			ref.Scope, ref.ScopeID, ref.Key).Scan(&id, &version)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if baseVersion != 0 {
				// The caller believed it was updating something that is gone.
				// Silently creating it would hide a deletion.
				return &ErrStale{Ref: ref, ExpectedVersion: baseVersion, CurrentVersion: 0}
			}
			id = ids.New(ids.ContextItem)
			version = 1
			now := ids.Now()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO context_items (id, scope, scope_id, key, version, content, mime, updated_by, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,?)`,
				id, ref.Scope, ref.ScopeID, ref.Key, version, content, "text/markdown", author, now); err != nil {
				return err
			}
			result = Item{ID: id, Scope: ref.Scope, ScopeID: ref.ScopeID, Key: ref.Key,
				Version: version, Content: content, MIME: "text/markdown",
				UpdatedBy: author, UpdatedAt: now}

		case err != nil:
			return err

		default:
			if version != baseVersion {
				var current string
				tx.QueryRowContext(ctx, `SELECT content FROM context_items WHERE id = ?`, id).Scan(&current)
				return &ErrStale{
					Ref: ref, ExpectedVersion: baseVersion,
					CurrentVersion: version, CurrentContent: current,
				}
			}
			version++
			now := ids.Now()
			if _, err := tx.ExecContext(ctx,
				`UPDATE context_items SET version = ?, content = ?, updated_by = ?, updated_at = ?
				 WHERE id = ?`, version, content, author, now, id); err != nil {
				return err
			}
			result = Item{ID: id, Scope: ref.Scope, ScopeID: ref.ScopeID, Key: ref.Key,
				Version: version, Content: content, MIME: "text/markdown",
				UpdatedBy: author, UpdatedAt: now}
		}

		// Every mutation writes a version row, so history is complete and an
		// agent can be shown what changed under it.
		_, err = tx.ExecContext(ctx,
			`INSERT INTO context_versions (item_id, version, content, author, reason, created_at)
			 VALUES (?,?,?,?,?,?)`,
			result.ID, result.Version, content, author, nullIfEmpty(reason), result.UpdatedAt)
		return err
	})
	if err != nil {
		return Item{}, err
	}

	if e.Events != nil {
		_ = e.Events.Emit(ctx, events.Event{
			Type: events.ContextUpdated, Actor: author,
			ContainerID: containerScope(ref),
			Payload: map[string]any{
				"scope": ref.Scope, "scope_id": ref.ScopeID,
				"key": ref.Key, "version": result.Version,
			},
		})
	}
	return result, nil
}

// Append adds a block to the end of an item.
//
// It needs no version check: appends commute, so two agents appending
// concurrently both succeed and neither loses work. That is why `append` is a
// separate permission from `write` — it is the safe way to let many agents
// contribute to one document.
func (e *Engine) Append(ctx context.Context, ref Ref, text, author string) (Item, error) {
	var result Item

	err := e.Store.Tx(ctx, func(tx *sql.Tx) error {
		var (
			id      string
			version int
			content string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, version, content FROM context_items WHERE scope = ? AND scope_id = ? AND key = ?`,
			ref.Scope, ref.ScopeID, ref.Key).Scan(&id, &version, &content)

		now := ids.Now()
		if errors.Is(err, sql.ErrNoRows) {
			id = ids.New(ids.ContextItem)
			version = 1
			content = text
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO context_items (id, scope, scope_id, key, version, content, mime, updated_by, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,?)`,
				id, ref.Scope, ref.ScopeID, ref.Key, version, content, "text/markdown", author, now); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			version++
			if content != "" && !strings.HasSuffix(content, "\n") {
				content += "\n"
			}
			content += text
			if _, err := tx.ExecContext(ctx,
				`UPDATE context_items SET version = ?, content = ?, updated_by = ?, updated_at = ? WHERE id = ?`,
				version, content, author, now, id); err != nil {
				return err
			}
		}

		result = Item{ID: id, Scope: ref.Scope, ScopeID: ref.ScopeID, Key: ref.Key,
			Version: version, Content: content, MIME: "text/markdown",
			UpdatedBy: author, UpdatedAt: now}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO context_versions (item_id, version, content, author, reason, created_at)
			 VALUES (?,?,?,?,?,?)`, id, version, content, author, "append", now)
		return err
	})
	if err != nil {
		return Item{}, err
	}

	if e.Events != nil {
		_ = e.Events.Emit(ctx, events.Event{
			Type: events.ContextUpdated, Actor: author,
			ContainerID: containerScope(ref),
			Payload: map[string]any{
				"scope": ref.Scope, "scope_id": ref.ScopeID,
				"key": ref.Key, "version": result.Version, "append": true,
			},
		})
	}
	return result, nil
}

// History returns an item's versions, newest first.
func (e *Engine) History(ctx context.Context, ref Ref, limit int) ([]Version, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := e.Store.DB().QueryContext(ctx,
		`SELECT v.version, v.content, v.author, COALESCE(v.reason,''), v.created_at
		 FROM context_versions v
		 JOIN context_items i ON i.id = v.item_id
		 WHERE i.scope = ? AND i.scope_id = ? AND i.key = ?
		 ORDER BY v.version DESC LIMIT ?`,
		ref.Scope, ref.ScopeID, ref.Key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.Version, &v.Content, &v.Author, &v.Reason, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Version is one historical revision.
type Version struct {
	Version   int    `json:"version"`
	Content   string `json:"content"`
	Author    string `json:"author"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"created_at"`
}

// containerScope returns the container id for container-scoped refs, so events
// about them appear on that container's stream.
func containerScope(ref Ref) string {
	if ref.Scope == ScopeContainer {
		return ref.ScopeID
	}
	return ""
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
