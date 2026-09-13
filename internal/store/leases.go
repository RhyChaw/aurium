package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// DefaultLeaseTTL bounds how long one worktree operation may hold a container.
//
// A rebase with hooks and a post_sync test can legitimately take minutes, so
// this is generous. It exists to recover from a dead holder, not to interrupt
// a slow one — a caller that needs longer renews.
const DefaultLeaseTTL = 5 * time.Minute

// ErrLeaseHeld is returned when another process holds the container.
type ErrLeaseHeld struct {
	ContainerID string
	Holder      string
	Operation   string
	ExpiresAt   string
}

func (e *ErrLeaseHeld) Error() string {
	return fmt.Sprintf("store: %s is busy: %s is running %q until %s",
		e.ContainerID, e.Holder, e.Operation, e.ExpiresAt)
}

// Lease is a held claim on a container's worktree.
type Lease struct {
	ContainerID string `json:"container_id"`
	Holder      string `json:"holder"`
	Operation   string `json:"operation"`
	AcquiredAt  string `json:"acquired_at"`
	ExpiresAt   string `json:"expires_at"`
}

// HolderID identifies this process in a lease.
//
// It carries the pid so a stuck lease names something a human can inspect or
// kill, rather than an opaque token.
func HolderID(role string) string {
	return fmt.Sprintf("%s/pid-%d", role, os.Getpid())
}

// AcquireLease claims a container's worktree for one operation.
//
// The whole check-and-take runs in a single immediate transaction, so two
// processes racing cannot both observe the container as free.
func (s *Store) AcquireLease(ctx context.Context, containerID, holder, operation string, ttl time.Duration) (Lease, error) {
	// Only an explicit zero means "use the default". Testing ttl <= 0 would
	// turn a negative duration — a clock skew, an arithmetic slip — into a
	// five-minute lease instead of an already-expired one, which is the
	// opposite of what the caller asked for. This is the same mistake that
	// made CreateToken issue non-expiring tokens; it is easy to make twice.
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	now := time.Now().UTC()
	lease := Lease{
		ContainerID: containerID,
		Holder:      holder,
		Operation:   operation,
		AcquiredAt:  now.Format(time.RFC3339Nano),
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}

	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var existingHolder, existingOp, existingExpiry string
		err := tx.QueryRowContext(ctx,
			`SELECT holder, operation, expires_at FROM leases WHERE container_id = ?`,
			containerID).Scan(&existingHolder, &existingOp, &existingExpiry)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Free.
		case err != nil:
			return err
		default:
			expiry, perr := time.Parse(time.RFC3339Nano, existingExpiry)
			// An unparseable expiry is treated as expired: a corrupt row must
			// not wedge a container forever.
			if perr == nil && now.Before(expiry) {
				return &ErrLeaseHeld{
					ContainerID: containerID, Holder: existingHolder,
					Operation: existingOp, ExpiresAt: existingExpiry,
				}
			}
			// Expired or corrupt: the previous holder is gone, take it over.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM leases WHERE container_id = ?`, containerID); err != nil {
				return err
			}
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO leases (container_id, holder, operation, acquired_at, expires_at)
			 VALUES (?,?,?,?,?)`,
			lease.ContainerID, lease.Holder, lease.Operation, lease.AcquiredAt, lease.ExpiresAt)
		return err
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// ReleaseLease drops a lease, but only if this holder still owns it.
//
// The holder check matters: without it, a process whose lease expired and was
// taken over would release somebody else's claim on the way out.
func (s *Store) ReleaseLease(ctx context.Context, containerID, holder string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM leases WHERE container_id = ? AND holder = ?`, containerID, holder)
	return err
}

// RenewLease extends a lease this holder owns.
func (s *Store) RenewLease(ctx context.Context, containerID, holder string, ttl time.Duration) error {
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE leases SET expires_at = ? WHERE container_id = ? AND holder = ?`,
		time.Now().UTC().Add(ttl).Format(time.RFC3339Nano), containerID, holder)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: %s no longer holds the lease on %s", holder, containerID)
	}
	return nil
}

// GetLease returns the current lease, if any.
func (s *Store) GetLease(ctx context.Context, containerID string) (Lease, error) {
	var l Lease
	err := s.db.QueryRowContext(ctx,
		`SELECT container_id, holder, operation, acquired_at, expires_at
		 FROM leases WHERE container_id = ?`, containerID).
		Scan(&l.ContainerID, &l.Holder, &l.Operation, &l.AcquiredAt, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	return l, err
}

// WithLease runs fn while holding a container's worktree, releasing afterwards.
//
// Every git operation that mutates a worktree must go through this, in both
// the CLI and the daemon. That is the entire mechanism: the lease is only
// worth anything if no code path skips it.
func (s *Store) WithLease(ctx context.Context, containerID, holder, operation string, fn func() error) error {
	if _, err := s.AcquireLease(ctx, containerID, holder, operation, DefaultLeaseTTL); err != nil {
		return err
	}
	// Released even on panic, and with a context that survives cancellation —
	// a cancelled sync that left its lease behind would block the container
	// until the TTL expired.
	defer func() {
		_ = s.ReleaseLease(context.WithoutCancel(ctx), containerID, holder)
	}()
	return fn()
}
