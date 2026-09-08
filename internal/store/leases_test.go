package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func leaseFixture(t *testing.T) (*Store, Container) {
	t.Helper()
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")
	c, err := s.CreateContainer(ctx, Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "feature", Slug: "feature",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: "/r/.aurium/wt/feature", Status: ContainerRunning, OriginKind: OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

// The property that matters: the CLI and the daemon must not rebase one
// worktree at the same moment. Two holders race for the same container; one
// wins, the other is told who has it.
func TestTwoHoldersCannotLeaseTheSameContainer(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()

	cli := HolderID("cli")
	daemon := HolderID("daemon-watcher")

	if _, err := s.AcquireLease(ctx, c.ID, cli, "sync", 0); err != nil {
		t.Fatal(err)
	}

	_, err := s.AcquireLease(ctx, c.ID, daemon, "auto-sync", 0)
	var held *ErrLeaseHeld
	if !errors.As(err, &held) {
		t.Fatalf("the second holder must be refused, got %v", err)
	}
	if held.Holder != cli || held.Operation != "sync" {
		t.Fatalf("the refusal must name who holds it and why: %+v", held)
	}

	// After release the other process can proceed.
	if err := s.ReleaseLease(ctx, c.ID, cli); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, c.ID, daemon, "auto-sync", 0); err != nil {
		t.Fatalf("the lease should be free after release: %v", err)
	}
}

// The test the review asked for: two concurrent syncs on one container
// serialize. Both go through WithLease; the critical section must never be
// entered twice at once.
func TestConcurrentSyncsOnOneContainerSerialize(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()

	var (
		inside    atomic.Int32
		maxInside atomic.Int32
		succeeded atomic.Int32
		refused   atomic.Int32
		wg        sync.WaitGroup
	)

	// Stand-in for the git work a sync does. If two ever run at once, the
	// observed concurrency rises above 1 and the test fails.
	rebase := func() error {
		n := inside.Add(1)
		for {
			m := maxInside.Load()
			if n <= m || maxInside.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inside.Add(-1)
		return nil
	}

	const attempts = 12
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half pose as the CLI, half as the daemon's watcher: the race
			// this prevents is across processes, not within one.
			holder := HolderID("cli")
			if i%2 == 1 {
				holder = HolderID("daemon-watcher")
			}
			err := s.WithLease(ctx, c.ID, holder, "sync", rebase)
			switch {
			case err == nil:
				succeeded.Add(1)
			case isLeaseHeld(err):
				refused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := maxInside.Load(); got > 1 {
		t.Fatalf("%d syncs ran concurrently on one worktree; the lease did not serialize them", got)
	}
	if succeeded.Load()+refused.Load() != attempts {
		t.Fatalf("accounted for %d of %d attempts", succeeded.Load()+refused.Load(), attempts)
	}
	if succeeded.Load() == 0 {
		t.Fatal("every attempt was refused; at least one must proceed")
	}
	t.Logf("of %d concurrent syncs: %d ran, %d were refused, peak concurrency %d",
		attempts, succeeded.Load(), refused.Load(), maxInside.Load())
}

// Different containers must not block each other, or the whole point of
// running agents in parallel is lost.
func TestLeasesArePerContainer(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()

	repo, _ := s.RepositoryByPath(ctx, c.ProjectID, "/r")
	other, err := s.CreateContainer(ctx, Container{
		ProjectID: c.ProjectID, RepoID: repo.ID, Branch: "other", Slug: "other",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: "/r/.aurium/wt/other", Status: ContainerRunning, OriginKind: OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AcquireLease(ctx, c.ID, HolderID("cli"), "sync", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, other.ID, HolderID("daemon"), "sync", 0); err != nil {
		t.Fatalf("an unrelated container must not be blocked: %v", err)
	}
}

// A holder that dies must not wedge the container forever. That failure would
// be worse than the race the lease prevents.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()

	dead := HolderID("crashed-cli")
	if _, err := s.AcquireLease(ctx, c.ID, dead, "sync", -time.Minute); err != nil {
		t.Fatal(err)
	}

	l, err := s.AcquireLease(ctx, c.ID, HolderID("daemon"), "auto-sync", 0)
	if err != nil {
		t.Fatalf("an expired lease must be takeable: %v", err)
	}
	if l.Holder == dead {
		t.Fatal("the new lease should belong to the new holder")
	}
}

// A process whose lease was taken over must not release the new holder's claim
// as it unwinds.
func TestReleaseOnlyAffectsYourOwnLease(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()

	loser := HolderID("expired-cli")
	s.AcquireLease(ctx, c.ID, loser, "sync", -time.Minute)
	winner := HolderID("daemon")
	if _, err := s.AcquireLease(ctx, c.ID, winner, "auto-sync", 0); err != nil {
		t.Fatal(err)
	}

	// The dead process finally unwinds and releases.
	if err := s.ReleaseLease(ctx, c.ID, loser); err != nil {
		t.Fatal(err)
	}

	l, err := s.GetLease(ctx, c.ID)
	if err != nil {
		t.Fatalf("the winner's lease was released by somebody else: %v", err)
	}
	if l.Holder != winner {
		t.Fatalf("lease holder = %q, want %q", l.Holder, winner)
	}
}

func TestWithLeaseReleasesOnError(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()
	boom := errors.New("rebase failed")

	err := s.WithLease(ctx, c.ID, HolderID("cli"), "sync", func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("WithLease must return the callback error, got %v", err)
	}
	if _, err := s.GetLease(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed operation must still release its lease")
	}
}

func TestDestroyingAContainerDropsItsLease(t *testing.T) {
	s, c := leaseFixture(t)
	ctx := context.Background()
	s.AcquireLease(ctx, c.ID, HolderID("cli"), "sync", 0)

	if err := s.DeleteContainer(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLease(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a destroyed container must not leave a lease behind")
	}
}

func isLeaseHeld(err error) bool {
	var held *ErrLeaseHeld
	return errors.As(err, &held)
}
