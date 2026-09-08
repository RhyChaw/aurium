package ids

import (
	"testing"
	"time"
)

func TestTimeRoundTripsCreationTimestamp(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Millisecond)
	id := New(Snapshot)
	after := time.Now().UTC()

	got := Time(id)
	if got.Before(before) || got.After(after.Add(time.Millisecond)) {
		t.Fatalf("Time(%q) = %v, want within [%v, %v]", id, got, before, after)
	}
}

func TestTimeOfMalformedIDIsZero(t *testing.T) {
	for _, bad := range []string{"garbage", "", "c_", "c_short", "nounderscore"} {
		if !Time(bad).IsZero() {
			t.Fatalf("Time(%q) must be the zero time", bad)
		}
	}
}
