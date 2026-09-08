package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func tokenFixture(t *testing.T) (*Store, Container) {
	t.Helper()
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")
	c, err := s.CreateContainer(ctx, Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "feature", Slug: "feature",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: "/r/wt", Status: ContainerRunning, OriginKind: OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestTokenRoundTrip(t *testing.T) {
	s, c := tokenFixture(t)
	ctx := context.Background()

	plain, info, err := s.CreateToken(ctx, c.ID, "", DefaultContainerScopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "aur_") {
		t.Errorf("token should be recognisable: %q", plain)
	}

	got, err := s.AuthenticateToken(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != info.ID || got.ContainerID != c.ID {
		t.Fatalf("authenticated as %+v", got)
	}
}

// D17: the database holds a digest, not a credential. Reading it must not
// yield anything usable.
func TestPlaintextTokenIsNeverStored(t *testing.T) {
	s, c := tokenFixture(t)
	ctx := context.Background()
	plain, _, _ := s.CreateToken(ctx, c.ID, "", DefaultContainerScopes, 0)

	rows, err := s.DB().QueryContext(ctx, `SELECT id, hash, scopes FROM tokens`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, hash, scopes string
		rows.Scan(&id, &hash, &scopes)
		if strings.Contains(hash, plain) || hash == plain {
			t.Fatal("the plaintext token was stored")
		}
	}
}

func TestUnknownRevokedAndExpiredTokensAreAllRejected(t *testing.T) {
	s, c := tokenFixture(t)
	ctx := context.Background()

	if _, err := s.AuthenticateToken(ctx, "aur_nonsense"); !errors.Is(err, ErrUnauthorized) {
		t.Error("an unknown token must be rejected")
	}
	if _, err := s.AuthenticateToken(ctx, ""); !errors.Is(err, ErrUnauthorized) {
		t.Error("an empty token must be rejected")
	}

	expired, _, _ := s.CreateToken(ctx, c.ID, "", DefaultContainerScopes, -time.Minute)
	if _, err := s.AuthenticateToken(ctx, expired); !errors.Is(err, ErrUnauthorized) {
		t.Error("an expired token must be rejected")
	}

	live, _, _ := s.CreateToken(ctx, c.ID, "", DefaultContainerScopes, time.Hour)
	if err := s.RevokeTokensForContainer(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateToken(ctx, live); !errors.Is(err, ErrUnauthorized) {
		t.Error("a revoked token must be rejected")
	}
}

// Destroying a container must really revoke its access, not merely orphan the
// row — otherwise a leaked token outlives the thing it was scoped to.
func TestTokenOfADestroyedContainerIsRejected(t *testing.T) {
	s, c := tokenFixture(t)
	ctx := context.Background()
	plain, _, _ := s.CreateToken(ctx, c.ID, "", DefaultContainerScopes, 0)

	if _, err := s.AuthenticateToken(ctx, plain); err != nil {
		t.Fatal("precondition: token should work before destroy")
	}
	if err := s.DeleteContainer(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateToken(ctx, plain); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("a destroyed container's token must stop working")
	}
}

func TestScopeMatchingIsOneDirectional(t *testing.T) {
	wide := &TokenInfo{Scopes: []string{"context:*", "ipc:*"}}
	if !wide.Has("context:read") || !wide.Has("context:write") {
		t.Error("a prefix wildcard must satisfy specific scopes under it")
	}
	if wide.Has("gateway:call") {
		t.Error("a wildcard must not reach outside its prefix")
	}

	narrow := &TokenInfo{Scopes: []string{"context:read"}}
	if narrow.Has("context:write") {
		t.Error("a specific scope must NOT satisfy a different scope in the same family")
	}
	if !narrow.Has("context:read") {
		t.Error("a specific scope must satisfy itself")
	}

	// §14: snapshot:self must not permit snapshotting another container.
	self := &TokenInfo{Scopes: []string{ScopeSnapshotSelf}}
	if self.Has("snapshot:other") {
		t.Error("snapshot:self must not grant snapshotting another container")
	}
}

func TestAdminScopeSatisfiesEverything(t *testing.T) {
	admin := &TokenInfo{Scopes: []string{ScopeAdmin}}
	for _, want := range []string{"context:write", "gateway:call", "snapshot:self", "anything:else"} {
		if !admin.Has(want) {
			t.Errorf("admin should satisfy %q", want)
		}
	}
}
