package app

import (
	"context"
	"strings"
	"testing"
)

// The default has to be a driver that can actually run.
//
// `docker` is right in the ERD and wrong on a laptop where Docker Desktop is
// not running, which for a tool people are trying out is most laptops. That
// combination produced a project whose every agent failed several steps in,
// with a message about a missing file.
func TestDefaultDriverPicksSomethingThatWorks(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()

	drivers := a.Drivers(ctx)
	if len(drivers) == 0 {
		t.Fatal("no drivers registered")
	}

	// local has no prerequisites, so it must always be usable — it is the
	// reason DefaultDriver cannot fail.
	var local *DriverStatus
	for i := range drivers {
		if drivers[i].Name == "local" {
			local = &drivers[i]
		}
	}
	if local == nil || !local.Available {
		t.Fatalf("the local driver must always be available: %+v", local)
	}
	if local.Isolated {
		t.Fatal("the local driver runs host processes; calling it isolated would be a lie")
	}

	name, _ := a.DefaultDriver(ctx)
	byName := map[string]DriverStatus{}
	for _, d := range drivers {
		byName[d.Name] = d
	}
	if !byName[name].Available {
		t.Fatalf("DefaultDriver chose %q, which is not available", name)
	}

	// Exactly one recommendation, and it is the chosen one.
	recommended := 0
	for _, d := range drivers {
		if d.Recommended {
			recommended++
			if d.Name != name {
				t.Fatalf("recommended %q but chose %q", d.Name, name)
			}
		}
	}
	if recommended != 1 {
		t.Fatalf("want exactly one recommended driver, got %d", recommended)
	}

	// Isolation beats convenience: local is never preferred over a container
	// driver that works.
	if name == "local" {
		for _, d := range drivers {
			if d.Isolated && d.Available {
				t.Fatalf("%s is available and isolated; local should not have won", d.Name)
			}
		}
	}
}

// An unavailable driver has to say why in a form a human can act on.
func TestUnavailableDriverExplainsItself(t *testing.T) {
	a := newApp(t)
	for _, d := range a.Drivers(context.Background()) {
		if d.Available {
			continue
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s is unavailable with no reason given", d.Name)
		}
		// "exit status 1" is true and useless.
		if strings.HasPrefix(d.Reason, "exit status") {
			t.Errorf("%s: %q says nothing a user can act on", d.Name, d.Reason)
		}
	}
}
