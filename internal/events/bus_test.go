package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/RhyChaw/aurium/internal/store"
)

func newBus(t *testing.T) (*Bus, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s), s
}

// D19: the event is the audit log. If it were written after the API replied,
// a crash between the two would lose the record of a state change that
// actually happened.
func TestEmitPersistsBeforeReturning(t *testing.T) {
	b, s := newBus(t)
	ctx := context.Background()

	if err := b.Emit(ctx, Event{
		Type: ContainerCreated, Actor: "human", ContainerID: "c_1",
		Payload: map[string]any{"branch": "feature"},
	}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events WHERE type = ?`, ContainerCreated).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("Emit must persist the event before it returns")
	}
}

func TestEmitAssignsMonotonicIDsAndTimestamps(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()

	var last int64
	for i := 0; i < 5; i++ {
		e, err := b.EmitReturning(ctx, Event{Type: AgentStarted, Actor: "daemon"})
		if err != nil {
			t.Fatal(err)
		}
		if e.ID <= last {
			t.Fatalf("event ids must increase: %d after %d", e.ID, last)
		}
		if e.TS == "" {
			t.Fatal("every event needs a timestamp")
		}
		last = e.ID
	}
}

func TestSubscribeReceivesSubsequentEvents(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()

	ch, cancel := b.Subscribe(Filter{})
	defer cancel()

	if err := b.Emit(ctx, Event{Type: TaskCreated, Actor: "human"}); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-ch:
		if e.Type != TaskCreated {
			t.Fatalf("got %q", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the event")
	}
}

func TestSubscribeFiltersByTypeAndContainer(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()

	ch, cancel := b.Subscribe(Filter{Types: []string{ContainerSynced}, ContainerID: "c_wanted"})
	defer cancel()

	b.Emit(ctx, Event{Type: ContainerCreated, ContainerID: "c_wanted", Actor: "d"}) // wrong type
	b.Emit(ctx, Event{Type: ContainerSynced, ContainerID: "c_other", Actor: "d"})   // wrong container
	b.Emit(ctx, Event{Type: ContainerSynced, ContainerID: "c_wanted", Actor: "d"})  // match

	select {
	case e := <-ch:
		if e.Type != ContainerSynced || e.ContainerID != "c_wanted" {
			t.Fatalf("filter leaked: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching event never arrived")
	}
}

// A dashboard tab left open on a laptop that went to sleep must not be able to
// wedge the daemon. Emit is called while holding state transitions together,
// so it can never block on a consumer.
func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()

	_, cancel := b.Subscribe(Filter{}) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBuffer*3; i++ {
			if err := b.Emit(ctx, Event{Type: AgentActive, Actor: "daemon"}); err != nil {
				t.Error(err)
				break
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a subscriber that stopped reading")
	}
}

func TestReplayReturnsEventsAfterAnID(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()

	first, _ := b.EmitReturning(ctx, Event{Type: TaskCreated, Actor: "h"})
	b.Emit(ctx, Event{Type: ContainerCreated, Actor: "h"})
	b.Emit(ctx, Event{Type: AgentStarted, Actor: "h"})

	got, err := b.Replay(ctx, first.ID, Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Replay returned %d events, want the 2 after the first", len(got))
	}
	if got[0].ID >= got[1].ID {
		t.Fatal("Replay must be ordered by id ascending")
	}
	// Reconnecting SSE clients rely on this to not miss or duplicate events.
	if got[0].ID != first.ID+1 {
		t.Fatalf("Replay should resume immediately after the given id")
	}
}

func TestReplayRoundTripsPayload(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()

	b.Emit(ctx, Event{
		Type: ContainerParentChanged, Actor: "daemon", ContainerID: "c_1",
		Payload: map[string]any{"commits": 3, "parent": "A"},
	})
	got, err := b.Replay(ctx, 0, Filter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	// JSON numbers decode as float64; the payload must survive intact.
	if got[0].Payload["commits"] != float64(3) {
		t.Fatalf("payload did not round-trip: %+v", got[0].Payload)
	}
}

func TestConcurrentEmitIsSafe(t *testing.T) {
	b, s := newBus(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Emit(ctx, Event{Type: AgentActive, Actor: "daemon"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	var n int
	s.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&n)
	if n != 20 {
		t.Fatalf("persisted %d of 20 concurrent events", n)
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	b, _ := newBus(t)
	ch, cancel := b.Subscribe(Filter{})
	cancel()

	b.Emit(context.Background(), Event{Type: TaskCreated, Actor: "h"})

	select {
	case _, open := <-ch:
		if open {
			t.Fatal("a cancelled subscriber must not receive events")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel should close the channel")
	}
}
