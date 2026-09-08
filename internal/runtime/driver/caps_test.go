package driver

import "testing"

// Capabilities are a contract the layers above branch on, so a driver that
// misreports one causes silent wrongness rather than a clean error.
func TestDriverCapabilitiesAreConsistent(t *testing.T) {
	local := NewLocal().Capabilities()
	if local.Snapshot || local.Pause || local.Tmux {
		t.Errorf("local must claim nothing it cannot do: %+v", local)
	}
	if local.Filesystem != FSShared {
		t.Error("local runs on the host filesystem")
	}

	dk := NewDocker("docker").Capabilities()
	if !dk.Snapshot || !dk.Pause || !dk.Tmux {
		t.Errorf("docker supports snapshot, pause and tmux: %+v", dk)
	}
	// CRIU is Linux-only, experimental, and absent from Docker Desktop and
	// OrbStack. Claiming it would make D14's "no process memory" a lie.
	if dk.Checkpoint {
		t.Error("no supported driver has working checkpoint/restore")
	}
}
