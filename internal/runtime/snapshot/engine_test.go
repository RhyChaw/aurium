package snapshot

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/store"
)

// newTestSnapshot takes a real snapshot (via Take, through the same fixture
// TestGCKeepsRecentLabelledAndReferencedSnapshots uses) under the given
// agent_placement, and returns the resulting store row.
func newTestSnapshot(t *testing.T, placement string) store.Snapshot {
	t.Helper()
	d, c := gcFixture(t)
	snap, err := Take(ctx(), d, c.ID, TakeOpts{Placement: placement})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Under host placement the agent's process runs on the machine, not in the
// container: its transcript lives in ~/.claude on the host, outside the
// rootfs a snapshot captures. A restore must not silently promise a
// conversation it cannot resume.
func TestSnapshotRecordsThatHostPlacementOmitsTheConversation(t *testing.T) {
	snap := newTestSnapshot(t, config.PlacementHost)
	if snap.IncludesConversation {
		t.Error("under host placement the transcript is outside the rootfs and is not captured")
	}
	if snap.Note == "" {
		t.Error("a snapshot that omits the conversation must say so, not omit it silently")
	}
}

// Under in-container placement, the transcript lives in $HOME inside the
// rootfs docker commit captures — unchanged by this feature.
func TestSnapshotUnderInContainerStillCapturesTheConversation(t *testing.T) {
	snap := newTestSnapshot(t, config.PlacementInContainer)
	if !snap.IncludesConversation {
		t.Error("in-container placement keeps the transcript in the captured rootfs")
	}
}
