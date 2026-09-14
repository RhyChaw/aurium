package cli

import (
	"testing"
	"time"
)

// A daemon left running across a rebuild serves its own dashboard and API, not
// the binary you just built. `aurium up` says "already running" either way, so
// without this check the user is told everything is fine while looking at a
// version of the app that is missing the thing they came to see.
func TestStaleDetectsADaemonThatPredatesThisBuild(t *testing.T) {
	now := time.Now().UTC()

	cases := []struct {
		name   string
		health map[string]any
		want   bool
	}{
		{
			// Every build carrying this check reports a pid. Its absence is
			// therefore proof, not a guess.
			name:   "no pid at all",
			health: map[string]any{"status": "ok", "version": "0.1.0-dev"},
			want:   true,
		},
		{
			name: "started moments ago",
			health: map[string]any{
				"pid": float64(123), "started_at": now.Add(-3 * time.Second).Format(time.RFC3339),
			},
			want: false,
		},
		{
			name: "up for days",
			health: map[string]any{
				"pid": float64(123), "started_at": now.Add(-72 * time.Hour).Format(time.RFC3339),
			},
			want: true,
		},
		{
			// A pid but an unreadable timestamp: the build is recent enough to
			// report identity, so do not cry wolf.
			name:   "pid but no timestamp",
			health: map[string]any{"pid": float64(123)},
			want:   false,
		},
		{
			name:   "pid but a malformed timestamp",
			health: map[string]any{"pid": float64(123), "started_at": "not a time"},
			want:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stale(c.health) != ""
			if got != c.want {
				t.Fatalf("stale(%v) = %v, want %v (reason: %q)",
					c.health, got, c.want, stale(c.health))
			}
		})
	}
}

// The warning has to name a fix. "Something is wrong" that does not say what to
// type is a dead end.
func TestStaleReasonNamesTheProblem(t *testing.T) {
	why := stale(map[string]any{"status": "ok"})
	if why == "" {
		t.Fatal("a daemon reporting no pid must be flagged")
	}
	if len(why) < 20 {
		t.Fatalf("the reason must be a sentence, got %q", why)
	}
}
