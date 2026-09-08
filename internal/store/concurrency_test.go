package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

// A single-connection pool deadlocks whenever code queries while iterating an
// open cursor. That is an easy mistake to make and impossible to see in
// review, so the pool must tolerate it.
func TestQueryingWhileIteratingACursorDoesNotDeadlock(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.CreateProject(ctx, "p", "/root/"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	go func() {
		rows, err := s.DB().QueryContext(ctx, `SELECT id FROM projects`)
		if err != nil {
			done <- err
			return
		}
		defer rows.Close()

		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				done <- err
				return
			}
			// A second query while the cursor is open.
			var n int
			if err := s.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM containers WHERE project_id = ?`, id).Scan(&n); err != nil {
				done <- err
				return
			}
		}
		done <- rows.Err()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-timeoutCh():
		t.Fatal("querying inside an open cursor deadlocked the pool")
	}
}

// Write serialisation must still hold with a multi-connection pool: two
// transactions reading then writing the same row must not both succeed.
func TestConcurrentTransactionsSerialise(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")
	c, _ := s.CreateContainer(ctx, Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "f", Slug: "f",
		ParentBranch: "main", BaseSHA: "start", Driver: "local",
		Worktree: "/r/wt", Status: ContainerRunning, OriginKind: OriginFresh,
	})

	const n = 8
	var wg sync.WaitGroup
	winners := make(chan int, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Tx(ctx, func(tx *sql.Tx) error {
				var base string
				if err := tx.QueryRowContext(ctx,
					`SELECT base_sha FROM containers WHERE id = ?`, c.ID).Scan(&base); err != nil {
					return err
				}
				if base != "start" {
					return errAlreadyClaimed
				}
				_, err := tx.ExecContext(ctx,
					`UPDATE containers SET base_sha = ? WHERE id = ?`, "claimed", c.ID)
				return err
			})
			if err == nil {
				winners <- i
			}
		}(i)
	}
	wg.Wait()
	close(winners)

	count := 0
	for range winners {
		count++
	}
	if count != 1 {
		t.Fatalf("%d transactions claimed the same row, want exactly 1", count)
	}
}
