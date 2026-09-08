package contextengine

import (
	"context"
	"strings"
	"testing"
)

func TestProposalLifecycleAcceptApplies(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("architecture/layers")
	e.Write(ctx, ref, 0, "three layers", "human", "")

	p, err := e.Propose(ctx, ref, 1, "four layers", "agent:a_1", "we need a gateway")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != ProposalOpen {
		t.Fatalf("status = %q", p.Status)
	}

	decided, err := e.Decide(ctx, p.ID, ProposalAccepted, "human")
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != ProposalAccepted || decided.DecidedBy != "human" {
		t.Fatalf("decided = %+v", decided)
	}

	// Accepting applies the content, and attribution stays with the proposer.
	item, _ := e.Get(ctx, ref)
	if item.Content != "four layers" {
		t.Fatalf("content = %q", item.Content)
	}
	if item.Version != 2 {
		t.Fatalf("version = %d, want 2", item.Version)
	}
	if item.UpdatedBy != "agent:a_1" {
		t.Errorf("the proposer should be recorded as the author, got %q", item.UpdatedBy)
	}

	// And history records who approved it.
	history, _ := e.History(ctx, ref, 10)
	if !strings.Contains(history[0].Reason, "accepted proposal") ||
		!strings.Contains(history[0].Reason, "human") {
		t.Errorf("history should record the approval, got %q", history[0].Reason)
	}
}

func TestRejectDoesNotApply(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("plan")
	e.Write(ctx, ref, 0, "original", "human", "")

	p, _ := e.Propose(ctx, ref, 1, "replacement", "agent", "")
	if _, err := e.Decide(ctx, p.ID, ProposalRejected, "human"); err != nil {
		t.Fatal(err)
	}

	item, _ := e.Get(ctx, ref)
	if item.Content != "original" || item.Version != 1 {
		t.Fatalf("a rejected proposal changed the item: %+v", item)
	}
}

// A proposal written against v1 must not be applied after the item reaches v2:
// accepting would silently discard whatever happened in between.
func TestProposalGoesStaleWhenTheItemMoves(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("conventions/naming")
	e.Write(ctx, ref, 0, "v1", "human", "")

	p, _ := e.Propose(ctx, ref, 1, "proposed against v1", "agent", "")
	e.Write(ctx, ref, 1, "v2 by somebody else", "other", "")

	got, err := e.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ProposalStale {
		t.Fatalf("status = %q, want stale", got.Status)
	}

	if _, err := e.Decide(ctx, p.ID, ProposalAccepted, "human"); err == nil {
		t.Fatal("accepting a stale proposal must be refused")
	}
	item, _ := e.Get(ctx, ref)
	if item.Content != "v2 by somebody else" {
		t.Fatalf("a stale proposal was applied over newer content: %q", item.Content)
	}
}

func TestProposalCannotBeDecidedTwice(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("plan")
	e.Write(ctx, ref, 0, "original", "human", "")

	p, _ := e.Propose(ctx, ref, 1, "new", "agent", "")
	if _, err := e.Decide(ctx, p.ID, ProposalAccepted, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Decide(ctx, p.ID, ProposalRejected, "human"); err == nil {
		t.Fatal("a decided proposal must not be decided again")
	}
}

func TestProposeAgainstAStaleVersionIsRefusedUpFront(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	ref := projectRef("plan")
	e.Write(ctx, ref, 0, "v1", "human", "")
	e.Write(ctx, ref, 1, "v2", "human", "")

	if _, err := e.Propose(ctx, ref, 1, "based on v1", "agent", ""); err == nil {
		t.Fatal("proposing against an already-stale version should be refused immediately")
	}
}

func TestListProposalsFiltersByStatus(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	a := projectRef("a")
	b := projectRef("b")
	e.Write(ctx, a, 0, "x", "h", "")
	e.Write(ctx, b, 0, "y", "h", "")

	p1, _ := e.Propose(ctx, a, 1, "x2", "agent", "")
	e.Propose(ctx, b, 1, "y2", "agent", "")
	e.Decide(ctx, p1.ID, ProposalAccepted, "human")

	open, err := e.ListProposals(ctx, ProposalOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open proposals = %d, want 1", len(open))
	}
}
