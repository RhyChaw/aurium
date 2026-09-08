package contextengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// Proposal states.
const (
	ProposalOpen     = "open"
	ProposalAccepted = "accepted"
	ProposalRejected = "rejected"
	// ProposalStale means the item moved past the proposal's base version, so
	// accepting it would silently discard whatever happened in between.
	ProposalStale = "stale"
)

// Proposal is a reviewed change to an item (§8.3 `propose`).
type Proposal struct {
	ID          string `json:"id"`
	ItemID      string `json:"item_id"`
	Key         string `json:"key"`
	BaseVersion int    `json:"base_version"`
	Content     string `json:"content"`
	Author      string `json:"author"`
	Reason      string `json:"reason,omitempty"`
	Status      string `json:"status"`
	DecidedBy   string `json:"decided_by,omitempty"`
	CreatedAt   string `json:"created_at"`
	DecidedAt   string `json:"decided_at,omitempty"`
}

// Propose records a proposed change for a human or a master agent to decide.
//
// The item must already exist: proposing a change to something that does not
// exist is a write, and the reviewer would have no baseline to compare against.
func (e *Engine) Propose(ctx context.Context, ref Ref, baseVersion int, content, author, reason string) (Proposal, error) {
	item, err := e.Get(ctx, ref)
	if err != nil {
		return Proposal{}, fmt.Errorf("context: cannot propose against %s: %w", ref, err)
	}
	if item.Version != baseVersion {
		return Proposal{}, &ErrStale{
			Ref: ref, ExpectedVersion: baseVersion,
			CurrentVersion: item.Version, CurrentContent: item.Content,
		}
	}

	p := Proposal{
		ID: ids.New(ids.Proposal), ItemID: item.ID, Key: ref.Key,
		BaseVersion: baseVersion, Content: content, Author: author,
		Reason: reason, Status: ProposalOpen, CreatedAt: ids.Now(),
	}
	if _, err := e.Store.DB().ExecContext(ctx,
		`INSERT INTO context_proposals (id, item_id, base_version, content, author, reason, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.ItemID, p.BaseVersion, p.Content, p.Author, nullIfEmpty(p.Reason), p.Status, p.CreatedAt); err != nil {
		return Proposal{}, err
	}

	if e.Events != nil {
		_ = e.Events.Emit(ctx, events.Event{
			Type: events.ContextProposed, Actor: author,
			ContainerID: containerScope(ref),
			Payload: map[string]any{
				"proposal": p.ID, "key": ref.Key, "base_version": baseVersion,
			},
		})
	}
	return p, nil
}

// GetProposal returns one proposal, refreshing its staleness first.
func (e *Engine) GetProposal(ctx context.Context, id string) (Proposal, error) {
	p, err := e.readProposal(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	return e.refreshStale(ctx, p)
}

// ListProposals returns proposals, optionally filtered by status.
func (e *Engine) ListProposals(ctx context.Context, status string) ([]Proposal, error) {
	query := `SELECT p.id, p.item_id, i.key, p.base_version, p.content, p.author,
	                 COALESCE(p.reason,''), p.status, COALESCE(p.decided_by,''),
	                 p.created_at, COALESCE(p.decided_at,'')
	          FROM context_proposals p JOIN context_items i ON i.id = p.item_id`
	var args []any
	if status != "" {
		query += ` WHERE p.status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY p.id DESC`

	rows, err := e.Store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	var out []Proposal
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.ItemID, &p.Key, &p.BaseVersion, &p.Content,
			&p.Author, &p.Reason, &p.Status, &p.DecidedBy, &p.CreatedAt, &p.DecidedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// Close before refreshing. The pool is limited to a single connection
	// (the daemon is the only writer), so querying while this cursor is still
	// open would wait forever for a connection the cursor itself holds.
	rows.Close()

	for i := range out {
		refreshed, err := e.refreshStale(ctx, out[i])
		if err != nil {
			return nil, err
		}
		out[i] = refreshed
	}
	return out, nil
}

// refreshStale marks an open proposal stale once its item has moved past the
// base version it was written against.
//
// This is computed rather than watched: a proposal's staleness is a fact about
// the item, and deriving it on read means it can never be out of date.
func (e *Engine) refreshStale(ctx context.Context, p Proposal) (Proposal, error) {
	if p.Status != ProposalOpen {
		return p, nil
	}
	var version int
	err := e.Store.DB().QueryRowContext(ctx,
		`SELECT version FROM context_items WHERE id = ?`, p.ItemID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version = -1 // the item was deleted; the proposal cannot apply
	} else if err != nil {
		return p, err
	}

	if version != p.BaseVersion {
		if _, err := e.Store.DB().ExecContext(ctx,
			`UPDATE context_proposals SET status = ? WHERE id = ? AND status = ?`,
			ProposalStale, p.ID, ProposalOpen); err != nil {
			return p, err
		}
		p.Status = ProposalStale
	}
	return p, nil
}

// Decide accepts or rejects a proposal.
//
// Accepting applies the content through the ordinary compare-and-set path, so
// a proposal cannot bypass the concurrency rules that protect every other
// write.
func (e *Engine) Decide(ctx context.Context, id, decision, decidedBy string) (Proposal, error) {
	if decision != ProposalAccepted && decision != ProposalRejected {
		return Proposal{}, fmt.Errorf("context: %q is not a decision (want %s or %s)",
			decision, ProposalAccepted, ProposalRejected)
	}

	p, err := e.GetProposal(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	switch p.Status {
	case ProposalStale:
		return p, fmt.Errorf("context: proposal %s is stale; the item moved past version %d",
			id, p.BaseVersion)
	case ProposalAccepted, ProposalRejected:
		return p, fmt.Errorf("context: proposal %s was already %s", id, p.Status)
	}

	var item Item
	if err := e.Store.DB().QueryRowContext(ctx,
		`SELECT scope, scope_id, key FROM context_items WHERE id = ?`, p.ItemID).
		Scan(&item.Scope, &item.ScopeID, &item.Key); err != nil {
		return Proposal{}, err
	}
	ref := Ref{Scope: item.Scope, ScopeID: item.ScopeID, Key: item.Key}

	if decision == ProposalAccepted {
		if _, err := e.Write(ctx, ref, p.BaseVersion, p.Content, p.Author,
			"accepted proposal "+p.ID+" by "+decidedBy); err != nil {
			return Proposal{}, err
		}
	}

	now := ids.Now()
	if _, err := e.Store.DB().ExecContext(ctx,
		`UPDATE context_proposals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
		decision, decidedBy, now, id); err != nil {
		return Proposal{}, err
	}
	p.Status, p.DecidedBy, p.DecidedAt = decision, decidedBy, now

	if e.Events != nil {
		_ = e.Events.Emit(ctx, events.Event{
			Type: events.ContextProposalDecided, Actor: decidedBy,
			ContainerID: containerScope(ref),
			Payload: map[string]any{
				"proposal": p.ID, "key": ref.Key, "decision": decision,
			},
		})
	}
	return p, nil
}

func (e *Engine) readProposal(ctx context.Context, id string) (Proposal, error) {
	var p Proposal
	err := e.Store.DB().QueryRowContext(ctx,
		`SELECT p.id, p.item_id, i.key, p.base_version, p.content, p.author,
		        COALESCE(p.reason,''), p.status, COALESCE(p.decided_by,''),
		        p.created_at, COALESCE(p.decided_at,'')
		 FROM context_proposals p JOIN context_items i ON i.id = p.item_id
		 WHERE p.id = ?`, id).
		Scan(&p.ID, &p.ItemID, &p.Key, &p.BaseVersion, &p.Content, &p.Author,
			&p.Reason, &p.Status, &p.DecidedBy, &p.CreatedAt, &p.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, store.ErrNotFound
	}
	return p, err
}
