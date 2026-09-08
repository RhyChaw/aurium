package contextengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/store"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	if _, err := s.CreateProject(ctx, "app", "/r"); err != nil {
		t.Fatal(err)
	}
	return New(s, events.New(s))
}

func projectRef(key string) Ref { return Ref{Scope: ScopeProject, ScopeID: "p_1", Key: key} }

func TestWriteCreatesThenVersions(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("task/objective")

	it, err := e.Write(ctx, ref, 0, "Implement OAuth", "human", "initial")
	if err != nil {
		t.Fatal(err)
	}
	if it.Version != 1 {
		t.Fatalf("first write should be version 1, got %d", it.Version)
	}

	it, err = e.Write(ctx, ref, 1, "Implement OAuth with PKCE", "human", "refined")
	if err != nil {
		t.Fatal(err)
	}
	if it.Version != 2 || it.Content != "Implement OAuth with PKCE" {
		t.Fatalf("second write = %+v", it)
	}

	got, _ := e.Get(ctx, ref)
	if got.Version != 2 {
		t.Fatalf("stored version = %d", got.Version)
	}
}

// §8.4: a write against a stale version is refused, and the error carries what
// the caller needs to reconcile without another round trip.
func TestWriteAgainstAStaleVersionIsRefused(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("plan")

	e.Write(ctx, ref, 0, "v1", "alice", "")
	e.Write(ctx, ref, 1, "v2", "bob", "")

	_, err := e.Write(ctx, ref, 1, "based on v1", "alice", "")
	var stale *ErrStale
	if !errors.As(err, &stale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if stale.CurrentVersion != 2 || stale.ExpectedVersion != 1 {
		t.Fatalf("stale = %+v", stale)
	}
	if stale.CurrentContent != "v2" {
		t.Errorf("ErrStale should carry the current content so the caller can reconcile, got %q",
			stale.CurrentContent)
	}

	// And the losing write must not have landed.
	got, _ := e.Get(ctx, ref)
	if got.Content != "v2" {
		t.Fatalf("a refused write modified the item: %q", got.Content)
	}
}

// The §14 test: two writers with the same base version — exactly one 200,
// one 409, and exactly one new version row.
func TestConcurrentWritesProduceExactlyOneWinner(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("decisions/jwt")

	e.Write(ctx, ref, 0, "undecided", "human", "")

	const writers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		conflicts int
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := e.Write(ctx, ref, 1, fmt.Sprintf("writer %d won", n), "agent", "")

			mu.Lock()
			defer mu.Unlock()
			var stale *ErrStale
			switch {
			case err == nil:
				wins++
			case errors.As(err, &stale):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d writers succeeded against the same base version, want exactly 1", wins)
	}
	if conflicts != writers-1 {
		t.Fatalf("got %d conflicts, want %d", conflicts, writers-1)
	}

	got, _ := e.Get(ctx, ref)
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2 — a lost update happened", got.Version)
	}

	history, _ := e.History(ctx, ref, 100)
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2 (the create and one winner)", len(history))
	}
}

// Creating with a non-zero base version means the caller believed it was
// updating something. Silently creating it would hide a deletion.
func TestWriteWithBaseVersionOnAMissingItemIsRefused(t *testing.T) {
	e := newEngine(t)
	_, err := e.Write(context.Background(), projectRef("gone"), 3, "content", "agent", "")
	var stale *ErrStale
	if !errors.As(err, &stale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if stale.CurrentVersion != 0 {
		t.Errorf("CurrentVersion should be 0 for a missing item, got %d", stale.CurrentVersion)
	}
}

func TestWriteWithZeroBaseOnAnExistingItemIsRefused(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("exists")
	e.Write(ctx, ref, 0, "original", "a", "")

	// A blind create against something that exists would be an accidental
	// overwrite; it must be refused rather than silently clobbering.
	_, err := e.Write(ctx, ref, 0, "clobber", "b", "")
	var stale *ErrStale
	if !errors.As(err, &stale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	got, _ := e.Get(ctx, ref)
	if got.Content != "original" {
		t.Fatalf("content was clobbered: %q", got.Content)
	}
}

// Appends commute, so concurrent appends must all survive. That property is
// exactly why `append` is a weaker, safer permission than `write`.
func TestConcurrentAppendsAllSurvive(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("discoveries/auth-flow")

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := e.Append(ctx, ref, fmt.Sprintf("finding %d", i), "agent"); err != nil {
				t.Errorf("append %d failed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := e.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != n {
		t.Fatalf("version = %d, want %d — an append was lost", got.Version, n)
	}
	for i := 0; i < n; i++ {
		if !contains(got.Content, fmt.Sprintf("finding %d", i)) {
			t.Errorf("finding %d is missing from the item", i)
		}
	}
}

func TestAppendSeparatesBlocksWithNewlines(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("notes")

	e.Append(ctx, ref, "first", "a")
	it, _ := e.Append(ctx, ref, "second", "a")
	if it.Content != "first\nsecond" {
		t.Fatalf("content = %q, want blocks separated by a newline", it.Content)
	}
}

func TestEveryMutationEmitsAnEvent(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("task/objective")

	e.Write(ctx, ref, 0, "one", "human", "")
	e.Write(ctx, ref, 1, "two", "human", "")
	e.Append(ctx, ref, "three", "human")

	evs, err := e.Events.Replay(ctx, 0, events.Filter{Types: []string{events.ContextUpdated}}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d context.updated events, want 3", len(evs))
	}
	if evs[2].Payload["version"] != float64(3) {
		t.Fatalf("last event version = %v, want 3", evs[2].Payload["version"])
	}
}

func TestListFiltersByPrefix(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	e.Write(ctx, projectRef("decisions/jwt"), 0, "a", "h", "")
	e.Write(ctx, projectRef("decisions/db"), 0, "b", "h", "")
	e.Write(ctx, projectRef("plan"), 0, "c", "h", "")

	got, err := e.List(ctx, ScopeProject, "p_1", "decisions/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("prefix list returned %d items, want 2", len(got))
	}
}

func TestPermRankImpliesWeakerPermissions(t *testing.T) {
	if !PermWrite.Allows(PermRead) || !PermWrite.Allows(PermAppend) || !PermWrite.Allows(PermPropose) {
		t.Error("write must imply every weaker permission")
	}
	if PermRead.Allows(PermWrite) {
		t.Error("read must not imply write")
	}
	if PermAppend.Allows(PermWrite) {
		t.Error("append must not imply write")
	}
	if PermNone.Allows(PermRead) {
		t.Error("no permission must allow nothing")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
