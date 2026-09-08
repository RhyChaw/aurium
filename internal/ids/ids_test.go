package ids

import (
	"strings"
	"testing"
	"time"
)

func TestNewHasPrefixAndIsSortable(t *testing.T) {
	a := New(Container)
	if !strings.HasPrefix(a, "c_") {
		t.Fatalf("want c_ prefix, got %q", a)
	}
	if len(a) != len("c_")+26 {
		t.Fatalf("want 26 ULID chars, got %d in %q", len(a)-2, a)
	}
	time.Sleep(2 * time.Millisecond)
	b := New(Container)
	if !(a < b) {
		t.Fatalf("ULIDs must sort lexicographically by time: %q !< %q", a, b)
	}
}

func TestNewIsUniqueWithinTheSameMillisecond(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := New(Message)
		if seen[id] {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = true
	}
}

func TestValidRejectsWrongPrefix(t *testing.T) {
	c := New(Container)
	if Valid(c, Snapshot) {
		t.Fatal("container id must not validate as a snapshot id")
	}
	if !Valid(c, Container) {
		t.Fatal("container id must validate as a container id")
	}
	if Valid("c_short", Container) {
		t.Fatal("malformed id must not validate")
	}
	if Valid("c_UUUUUUUUUUUUUUUUUUUUUUUUUU", Container) {
		t.Fatal("id with characters outside the Crockford alphabet must not validate")
	}
}

func TestPrefixExtracts(t *testing.T) {
	if got := Prefix(New(Approval)); got != Approval {
		t.Fatalf("Prefix = %q, want %q", got, Approval)
	}
	if got := Prefix("garbage"); got != "" {
		t.Fatalf("Prefix of a malformed id should be empty, got %q", got)
	}
}

func TestPrefixesAreDistinct(t *testing.T) {
	all := []string{
		Project, Task, Container, Snapshot, Agent, Message,
		Integration, Approval, Grant, ContextItem, Proposal, Token,
	}
	seen := map[string]bool{}
	for _, p := range all {
		if seen[p] {
			t.Fatalf("duplicate type prefix %q", p)
		}
		seen[p] = true
	}
}

func TestNowIsRFC3339UTC(t *testing.T) {
	n := Now()
	parsed, err := time.Parse(time.RFC3339Nano, n)
	if err != nil {
		t.Fatalf("Now() not RFC3339: %v", err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("Now() must be UTC, got %v", parsed.Location())
	}
	if !strings.HasSuffix(n, "Z") {
		t.Fatalf("Now() must end in Z, got %q", n)
	}
}
