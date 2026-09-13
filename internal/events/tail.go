package events

import (
	"context"
	"time"
)

// DefaultTailInterval is how often the bus looks for events written by another
// process. It is a human-facing latency (the dashboard), so a fifth of a
// second is imperceptible and costs one indexed query.
const DefaultTailInterval = 200 * time.Millisecond

// Tail fans out events written to the database by processes other than this
// one, until ctx is cancelled.
//
// It exists because the in-process pub/sub only sees what this process emits.
// Until every client goes through the daemon's API (D17), the CLI writes to
// the same database directly, and without this the dashboard would silently
// miss everything the user did at the terminal — the events most worth
// watching.
//
// Events this process emitted are not re-delivered: Emit records the highest
// id it has published, and the tailer only reads past it.
func (b *Bus) Tail(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultTailInterval
	}

	// Start from where the log already is, so a starting daemon does not
	// replay history to every live subscriber.
	if latest, err := b.Latest(ctx); err == nil {
		b.markPublished(latest)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.drainExternal(ctx)
		}
	}
}

// drainExternal publishes any events past the high-water mark.
func (b *Bus) drainExternal(ctx context.Context) {
	b.publishedMu.Lock()
	from := b.published
	b.publishedMu.Unlock()

	fresh, err := b.Replay(ctx, from, Filter{}, 500)
	if err != nil || len(fresh) == 0 {
		return
	}
	for _, e := range fresh {
		// markPublished before fanout so a concurrent Emit cannot interleave
		// and cause the same id to go out twice.
		if b.markPublished(e.ID) {
			b.fanout(e)
		}
	}
}

// markPublished raises the high-water mark, reporting whether id was new.
func (b *Bus) markPublished(id int64) bool {
	b.publishedMu.Lock()
	defer b.publishedMu.Unlock()
	if id <= b.published {
		return false
	}
	b.published = id
	return true
}
