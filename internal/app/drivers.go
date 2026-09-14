package app

import (
	"context"
	"sort"
	"time"
)

// Choosing a driver that works.
//
// `docker` is the right default in the ERD and the wrong one on a laptop where
// Docker Desktop is not running — which, for a tool people try out, is most
// laptops most of the time. Defaulting to it there produced a project whose
// every agent failed, several steps in, with a message about a missing file.
//
// So the default is now whichever driver can actually run, preferring the one
// that isolates. The choice is reported rather than silent: an agent running as
// a host process with no isolation is a materially different thing from one in
// a container, and a user who did not ask for that must be told they got it.

// DriverStatus is one driver and whether it can be used.
type DriverStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	// Reason says why not, in one line.
	Reason string `json:"reason,omitempty"`
	// Isolated distinguishes a container from a host process.
	Isolated bool `json:"isolated"`
	// Recommended marks the one DefaultDriver would pick.
	Recommended bool `json:"recommended"`
}

// driverPreference orders drivers best-first. Isolation beats convenience:
// `local` is a documented degraded mode, not a peer.
var driverPreference = []string{"docker", "podman", "local"}

// Drivers reports every driver and whether it works on this machine.
func (a *App) Drivers(ctx context.Context) []DriverStatus {
	// Bounded: `docker info` against a stopped daemon can sit for a while, and
	// this runs behind a dialog somebody is waiting on.
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	rank := map[string]int{}
	for i, name := range driverPreference {
		rank[name] = i
	}

	var out []DriverStatus
	for name, drv := range a.Manager.Drivers {
		st := DriverStatus{Name: name, Isolated: drv.Capabilities().Snapshot}
		if err := drv.Available(ctx); err != nil {
			st.Reason = err.Error()
		} else {
			st.Available = true
		}
		out = append(out, st)
	}

	sort.Slice(out, func(i, j int) bool {
		ri, ok := rank[out[i].Name]
		if !ok {
			ri = len(rank)
		}
		rj, ok := rank[out[j].Name]
		if !ok {
			rj = len(rank)
		}
		return ri < rj
	})

	for i := range out {
		if out[i].Available {
			out[i].Recommended = true
			break
		}
	}
	return out
}

// DefaultDriver picks the best driver that actually works.
//
// It returns "local" when nothing does, rather than an error: the local driver
// has no prerequisites, so "nothing works" is not a state this can reach, and
// returning an error for an impossible case would make every caller handle it.
func (a *App) DefaultDriver(ctx context.Context) (name string, isolated bool) {
	for _, d := range a.Drivers(ctx) {
		if d.Available {
			return d.Name, d.Isolated
		}
	}
	return "local", false
}
