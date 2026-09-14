package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/ids"
)

// Providers Aurium knows how to spend money on (§D24).
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// How an account authenticates.
const (
	// AuthAPIKey is a pay-as-you-go key billed per token.
	AuthAPIKey = "api_key"
	// AuthSubscription is a seat — Claude Pro/Max, ChatGPT Plus/Pro — reached
	// through the provider CLI's own login (§D25). Token counts still exist;
	// a per-token price does not, so usage on these accounts is metered but
	// deliberately not costed.
	AuthSubscription = "subscription"
)

// How the credential reached Aurium. This is not decoration: when an account
// stops working, "you pasted this" and "this host was already logged in when
// you connected it" have different fixes.
const (
	SourcePasted   = "pasted"
	SourceHostEnv  = "host_env"
	SourceCLILogin = "cli_login"
)

// Account statuses.
const (
	AccountConnected = "connected"
	AccountError     = "error"
)

// ProviderAccount is a connected provider login.
//
// It never carries a credential. SecretRef names where the credential lives,
// exactly as integrations.secret_ref does, and no API route turns a reference
// back into a secret.
type ProviderAccount struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Label     string `json:"label"`
	AuthKind  string `json:"auth_kind"`
	EnvVar    string `json:"env_var"`
	SecretRef string `json:"secret_ref"`
	Source    string `json:"source"`
	Status    string `json:"status"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt string `json:"created_at"`
}

const providerColumns = `id, provider, label, auth_kind, env_var, secret_ref,
	source, status, COALESCE(last_error,''), created_at`

// ErrDuplicate is returned when a uniqueness constraint rejects an insert.
// Callers surface it as 409 rather than 500: "you already have an account with
// that name" is the user's to fix, not a daemon fault.
var ErrDuplicate = errors.New("store: already exists")

// CreateProviderAccount records a connected account.
func (s *Store) CreateProviderAccount(ctx context.Context, a ProviderAccount) (ProviderAccount, error) {
	if a.ID == "" {
		a.ID = ids.New(ids.ProviderAccount)
	}
	if a.Status == "" {
		a.Status = AccountConnected
	}
	a.CreatedAt = ids.Now()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO provider_accounts
		   (id, provider, label, auth_kind, env_var, secret_ref, source, status, last_error, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Provider, a.Label, a.AuthKind, a.EnvVar, a.SecretRef,
		a.Source, a.Status, nullable(a.LastError), a.CreatedAt)
	if err != nil {
		if isConstraintErr(err) {
			return ProviderAccount{}, fmt.Errorf("%w: a %s account labelled %q",
				ErrDuplicate, a.Provider, a.Label)
		}
		return ProviderAccount{}, fmt.Errorf("store: create provider account: %w", err)
	}
	return a, nil
}

func (s *Store) GetProviderAccount(ctx context.Context, id string) (ProviderAccount, error) {
	return s.scanProviderAccount(s.db.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM provider_accounts WHERE id = ?`, id))
}

// ListProviderAccounts returns every connected account, oldest first so the
// order matches the ids and does not shuffle between reloads.
func (s *Store) ListProviderAccounts(ctx context.Context) ([]ProviderAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+providerColumns+` FROM provider_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderAccount
	for rows.Next() {
		var a ProviderAccount
		if err := rows.Scan(&a.ID, &a.Provider, &a.Label, &a.AuthKind, &a.EnvVar,
			&a.SecretRef, &a.Source, &a.Status, &a.LastError, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DefaultProviderAccount returns the account an agent of this provider uses
// when nothing more specific was chosen: the oldest connected one, so the
// choice is stable rather than changing every time another is added.
func (s *Store) DefaultProviderAccount(ctx context.Context, provider string) (ProviderAccount, error) {
	return s.scanProviderAccount(s.db.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM provider_accounts
		 WHERE provider = ? AND status = ? ORDER BY id LIMIT 1`, provider, AccountConnected))
}

// SetProviderAccountStatus records that an account started or stopped working.
func (s *Store) SetProviderAccountStatus(ctx context.Context, id, status, lastError string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE provider_accounts SET status = ?, last_error = ? WHERE id = ?`,
		status, nullable(lastError), id)
	if err != nil {
		return err
	}
	return mustAffect(res, "provider account", id)
}

// DeleteProviderAccount removes the row. Deleting the credential itself is the
// caller's job; this package deliberately knows nothing about secrets.
func (s *Store) DeleteProviderAccount(ctx context.Context, id string) error {
	// Agents keep running after their account is disconnected — the credential
	// is already in their environment — so the link is cleared rather than the
	// agent being cascaded away with it.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE agents SET provider_account_id = NULL WHERE provider_account_id = ?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM provider_accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res, "provider account", id)
}

// SetAgentProviderAccount records which account an agent runs on, which is
// what makes "which company, which agent" answerable (§D24).
func (s *Store) SetAgentProviderAccount(ctx context.Context, agentID, accountID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET provider_account_id = ? WHERE id = ?`, nullable(accountID), agentID)
	if err != nil {
		return err
	}
	return mustAffect(res, "agent", agentID)
}

// SetAgentDisplayName names an agent for the rail.
func (s *Store) SetAgentDisplayName(ctx context.Context, agentID, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET display_name = ? WHERE id = ?`, name, agentID)
	if err != nil {
		return err
	}
	return mustAffect(res, "agent", agentID)
}

func (s *Store) scanProviderAccount(row *sql.Row) (ProviderAccount, error) {
	var a ProviderAccount
	err := row.Scan(&a.ID, &a.Provider, &a.Label, &a.AuthKind, &a.EnvVar,
		&a.SecretRef, &a.Source, &a.Status, &a.LastError, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderAccount{}, ErrNotFound
	}
	if err != nil {
		return ProviderAccount{}, err
	}
	return a, nil
}

// isConstraintErr reports whether err is SQLite refusing a write because a
// UNIQUE or CHECK constraint said no. The pure-Go driver does not export a
// typed error for this, so the text is matched — narrowly, and only to choose
// a status code, never to decide whether the write happened.
func isConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
