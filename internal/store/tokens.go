package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/RhyChaw/aurium/internal/ids"
)

// Scopes a container token can hold (§11.2).
const (
	ScopeContextAll   = "context:*"
	ScopeContextRead  = "context:read"
	ScopeIPCAll       = "ipc:*"
	ScopeTaskAll      = "task:*"
	ScopeSnapshotSelf = "snapshot:self"
	ScopeGatewayAll   = "gateway:*"
	ScopeAdmin        = "admin:*"
)

// DefaultContainerScopes is what an ordinary container agent gets. Note what
// is absent: no admin, and snapshot is limited to the container's own state.
var DefaultContainerScopes = []string{
	ScopeContextAll, ScopeIPCAll, ScopeTaskAll, ScopeSnapshotSelf, ScopeGatewayAll,
}

// TokenInfo is an authenticated token.
type TokenInfo struct {
	ID          string
	ContainerID string
	AgentID     string
	Scopes      []string
	ExpiresAt   string
}

// Has reports whether the token carries a scope, honouring `prefix:*` wildcards.
//
// The match is deliberately one-directional: holding "context:*" satisfies
// "context:read", but holding "context:read" does NOT satisfy "context:write".
// A wildcard grants everything under its prefix; a specific scope grants only
// itself.
func (t *TokenInfo) Has(want string) bool {
	for _, got := range t.Scopes {
		if got == want || got == ScopeAdmin {
			return true
		}
		if prefix, ok := strings.CutSuffix(got, ":*"); ok {
			if wantPrefix, _, found := strings.Cut(want, ":"); found && wantPrefix == prefix {
				return true
			}
		}
	}
	return false
}

// hashToken is how tokens are stored. The plaintext exists only in the
// container that received it; the database holds a digest, so reading the
// database does not yield a usable credential (D17).
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// CreateToken issues a scoped bearer token, returning the plaintext exactly
// once. It is never recoverable afterwards.
func (s *Store) CreateToken(ctx context.Context, containerID, agentID string,
	scopes []string, ttl time.Duration) (string, TokenInfo, error) {

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", TokenInfo{}, fmt.Errorf("store: generate token: %w", err)
	}
	plaintext := "aur_" + base64.RawURLEncoding.EncodeToString(raw)

	info := TokenInfo{
		ID:          ids.New(ids.Token),
		ContainerID: containerID,
		AgentID:     agentID,
		Scopes:      scopes,
	}
	// Any non-zero TTL sets an expiry. Testing ttl > 0 would silently turn a
	// negative TTL — a clock skew, an arithmetic slip — into a token that
	// never expires, which is the opposite of what the caller asked for.
	// Only an explicit zero means "no expiry".
	if ttl != 0 {
		info.ExpiresAt = time.Now().UTC().Add(ttl).Format(time.RFC3339Nano)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (id, hash, container_id, agent_id, scopes, expires_at, revoked)
		 VALUES (?,?,?,?,?,?,0)`,
		info.ID, hashToken(plaintext), nullable(containerID), nullable(agentID),
		strings.Join(scopes, ","), nullable(info.ExpiresAt))
	if err != nil {
		return "", TokenInfo{}, fmt.Errorf("store: create token: %w", err)
	}
	return plaintext, info, nil
}

// ErrUnauthorized is returned for any token that cannot be used, without
// distinguishing why — an attacker learns nothing from the difference between
// "unknown", "revoked" and "expired".
var ErrUnauthorized = fmt.Errorf("store: unauthorized")

// AuthenticateToken validates a bearer token.
func (s *Store) AuthenticateToken(ctx context.Context, plaintext string) (TokenInfo, error) {
	if plaintext == "" {
		return TokenInfo{}, ErrUnauthorized
	}

	var (
		info      TokenInfo
		scopes    string
		revoked   int
		expiresAt string
		hash      string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, hash, COALESCE(container_id,''), COALESCE(agent_id,''), scopes,
		        COALESCE(expires_at,''), revoked
		 FROM tokens WHERE hash = ?`, hashToken(plaintext)).
		Scan(&info.ID, &hash, &info.ContainerID, &info.AgentID, &scopes, &expiresAt, &revoked)
	if err != nil {
		return TokenInfo{}, ErrUnauthorized
	}

	// Constant-time compare even though the lookup was by hash: it costs
	// nothing and keeps the comparison honest if the query ever changes.
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(plaintext))) != 1 {
		return TokenInfo{}, ErrUnauthorized
	}
	if revoked != 0 {
		return TokenInfo{}, ErrUnauthorized
	}
	if expiresAt != "" {
		exp, err := time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil || time.Now().UTC().After(exp) {
			return TokenInfo{}, ErrUnauthorized
		}
	}

	// A token outlives its container only as a row. If the container is gone,
	// the token is worthless — this is what makes `destroy` a real revocation.
	if info.ContainerID != "" {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM containers WHERE id = ?`, info.ContainerID).Scan(&n); err != nil || n == 0 {
			return TokenInfo{}, ErrUnauthorized
		}
	}

	info.Scopes = strings.Split(scopes, ",")
	info.ExpiresAt = expiresAt
	return info, nil
}

// RevokeTokensForContainer invalidates every token a container holds.
func (s *Store) RevokeTokensForContainer(ctx context.Context, containerID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE container_id = ?`, containerID)
	return err
}
