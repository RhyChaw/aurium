package events

import (
	"context"
	"testing"
	"time"

	"github.com/RhyChaw/aurium/internal/store"
)

// The CLI writes to the same database in-process, so events it creates never
// pass through the daemon's in-memory bus. Without tailing, the dashboard
// silently misses everything the user does at the terminal.
func TestTailDeliversEventsWrittenByAnotherProcess(t *testing.T) {
	dir := t.TempDir() + "/aurium.db"
	s1, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	// A second handle stands in for the separate CLI process.
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	daemonBus := New(s1)
	cliBus := New(s2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub := daemonBus.Subscribe(Filter{})
	defer unsub()

	go daemonBus.Tail(ctx, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // let the tailer set its high-water mark

	if err := cliBus.Emit(ctx, Event{
		Type: ContainerSynced, Actor: ActorHuman, ContainerID: "c_1",
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-ch:
		if e.Type != ContainerSynced {
			t.Fatalf("got %q", e.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an event written by another process never reached the subscriber")
	}
}

// A tailing bus must not deliver its own events twice.
func TestTailDoesNotDuplicateLocallyEmittedEvents(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	b := New(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub := b.Subscribe(Filter{})
	defer unsub()

	go b.Tail(ctx, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if err := b.Emit(ctx, Event{Type: TaskCreated, Actor: ActorHuman}); err != nil {
		t.Fatal(err)
	}

	seen := 0
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-ch:
			seen++
			if seen > 1 {
				t.Fatal("a locally emitted event was delivered twice")
			}
		case <-deadline:
			if seen != 1 {
				t.Fatalf("delivered %d times, want exactly 1", seen)
			}
			return
		}
	}
}

// A starting daemon must not replay the whole log to live subscribers.
func TestTailStartsFromTheCurrentEndOfTheLog(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	b := New(s)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		b.Emit(ctx, Event{Type: TaskCreated, Actor: ActorHuman})
	}

	// A fresh bus over the same database, as if the daemon just started.
	fresh := New(s)
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, unsub := fresh.Subscribe(Filter{})
	defer unsub()

	go fresh.Tail(tctx, 20*time.Millisecond)

	select {
	case e := <-ch:
		t.Fatalf("history was replayed to a live subscriber: %+v", e)
	case <-time.After(300 * time.Millisecond):
		// Correct: nothing new happened, so nothing was delivered.
	}
}
