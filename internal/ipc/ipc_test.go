package ipc

import (
	"context"
	"testing"
	"time"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/store"
)

type recordingNudger struct {
	nudges []string
}

func (r *recordingNudger) Nudge(ctx context.Context, containerID, text string) error {
	r.nudges = append(r.nudges, containerID+": "+text)
	return nil
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

type fixture struct {
	bus     *Bus
	store   *store.Store
	nudger  *recordingNudger
	clock   *fakeClock
	project store.Project
	cA, cB  store.Container
	aA, aB  store.Agent
	task    store.Task
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")

	mk := func(branch string) (store.Container, store.Agent) {
		c, err := s.CreateContainer(ctx, store.Container{
			ProjectID: p.ID, RepoID: repo.ID, Branch: branch, Slug: branch,
			ParentBranch: "main", BaseSHA: "abc", Driver: "local",
			Worktree: "/r/wt/" + branch, Status: store.ContainerRunning,
			OriginKind: store.OriginFresh,
		})
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.CreateAgent(ctx, store.Agent{
			ContainerID: c.ID, Adapter: "shell", Role: store.RolePrimary,
			TmuxSession: "agent", Status: store.AgentRunning,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c, a
	}

	cA, aA := mk("A")
	cB, aB := mk("B")
	task, _ := s.CreateTask(ctx, p.ID, "Implement OAuth", "")

	n := &recordingNudger{}
	clock := &fakeClock{t: time.Now().UTC()}
	bus := New(s, events.New(s), n)
	bus.Clock = clock

	return &fixture{bus: bus, store: s, nudger: n, clock: clock,
		project: p, cA: cA, cB: cB, aA: aA, aB: aB, task: task}
}

func TestSendDeliverAck(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	sent, err := f.bus.Send(ctx, Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID, ContainerID: f.cB.ID},
		Type:      TypeRequest,
		Content:   "Please add integration coverage for AuthService.login()",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != StatusQueued {
		t.Fatalf("status = %q, want queued", sent.Status)
	}

	inbox, err := f.bus.Inbox(ctx, Addr{AgentID: f.aB.ID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != sent.ID {
		t.Fatalf("inbox = %+v", inbox)
	}
	if inbox[0].Status != StatusDelivered {
		t.Fatalf("an inbox call must mark the message delivered, got %q", inbox[0].Status)
	}

	// At-least-once: a delivered-but-unacked message is shown again, so an
	// agent that crashed mid-turn still sees what it was sent.
	again, _ := f.bus.Inbox(ctx, Addr{AgentID: f.aB.ID}, 10)
	if len(again) != 1 {
		t.Fatalf("an unacked message must be redelivered, got %d", len(again))
	}

	n, err := f.bus.Ack(ctx, []string{sent.ID})
	if err != nil || n != 1 {
		t.Fatalf("ack = %d, %v", n, err)
	}
	after, _ := f.bus.Inbox(ctx, Addr{AgentID: f.aB.ID}, 10)
	if len(after) != 0 {
		t.Fatalf("an acked message must not be redelivered, got %d", len(after))
	}
}

func TestSendNudgesTheRecipientContainer(t *testing.T) {
	f := newFixture(t)
	// An agent deep in a long turn will not poll its inbox unprompted.
	f.bus.Send(context.Background(), Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID, ContainerID: f.cB.ID},
		Type:      TypeRequest, Content: "look at this",
	})
	if len(f.nudger.nudges) != 1 {
		t.Fatalf("expected one nudge, got %v", f.nudger.nudges)
	}
	if !contains(f.nudger.nudges[0], "aurium_ipc_inbox") {
		t.Errorf("the nudge should say how to read the message: %q", f.nudger.nudges[0])
	}
}

// §9.5: BLOCKED marks the agent and the task blocked and reaches a human.
func TestBlockedMarksAgentAndTaskAndReachesAHuman(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	sent, err := f.bus.Send(ctx, Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID}, // addressed to an agent...
		Type:      TypeBlocked,
		Content:   "I need a decision on the token lifetime",
		Refs:      Refs{Task: f.task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	// ...but a human must see it, because only a human can unblock it.
	if !sent.To.Human {
		t.Error("BLOCKED must route to a human")
	}

	agent, _ := f.store.GetAgent(ctx, f.aA.ID)
	if agent.Status != store.AgentBlocked {
		t.Errorf("agent status = %q, want blocked", agent.Status)
	}
	task, _ := f.store.GetTask(ctx, f.task.ID)
	if task.Status != store.TaskBlocked {
		t.Errorf("task status = %q, want blocked", task.Status)
	}

	human, _ := f.bus.Inbox(ctx, Addr{Human: true}, 10)
	if len(human) != 1 {
		t.Fatalf("the human inbox has %d messages, want 1", len(human))
	}
}

// An agent cannot approve on another agent's behalf, so an approval addressed
// elsewhere would be stranded.
func TestApprovalRequiredAlwaysReachesAHuman(t *testing.T) {
	f := newFixture(t)
	sent, err := f.bus.Send(context.Background(), Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID},
		Type:      TypeApprovalRequired,
		Content:   "may I merge this PR?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sent.To.Human {
		t.Fatal("APPROVAL_REQUIRED must be routed to a human regardless of addressee")
	}
}

func TestArtifactCreatesAnArtifactRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.bus.Send(ctx, Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID},
		Type:      TypeArtifact,
		Content:   "worker finished: 3 files, 120 insertions",
		Refs:      Refs{Branch: "worker-auth-tests", Task: f.task.ID},
	}); err != nil {
		t.Fatal(err)
	}

	var n int
	f.store.DB().QueryRow(`SELECT count(*) FROM artifacts WHERE ref = ?`, "worker-auth-tests").Scan(&n)
	if n != 1 {
		t.Fatalf("ARTIFACT must record an artifacts row, got %d", n)
	}
}

// The payoff for recording a dependency: a producer changing a contract need
// not remember who relies on it.
func TestDependencyNotifiesTheConsumerWhenTheKeyChanges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// B declares that it depends on A's api-contract.
	if _, err := f.bus.Send(ctx, Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aB.ID, ContainerID: f.cB.ID},
		To:        Addr{AgentID: f.aA.ID},
		Type:      TypeDependency,
		Content:   "I am building against your API contract",
		Refs:      Refs{ContextKey: "deps/api-contract"},
	}); err != nil {
		t.Fatal(err)
	}

	notified, err := f.bus.NotifyContextChange(ctx, f.project.ID, "deps/api-contract", 8)
	if err != nil {
		t.Fatal(err)
	}
	if notified != 1 {
		t.Fatalf("notified %d consumers, want 1", notified)
	}

	inbox, _ := f.bus.Inbox(ctx, Addr{AgentID: f.aB.ID}, 10)
	found := false
	for _, m := range inbox {
		if m.Type == TypeInfo && contains(m.Content, "deps/api-contract") && contains(m.Content, "version 8") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the consumer was not told the contract changed: %+v", inbox)
	}
}

func TestHighPriorityMessagesComeFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.bus.Send(ctx, Message{
		ProjectID: f.project.ID, From: Addr{AgentID: f.aA.ID},
		To: Addr{AgentID: f.aB.ID}, Type: TypeInfo, Content: "fyi",
	})
	f.bus.Send(ctx, Message{
		ProjectID: f.project.ID, From: Addr{AgentID: f.aA.ID},
		To: Addr{AgentID: f.aB.ID}, Type: TypeWarning, Content: "urgent",
		Priority: PriorityHigh,
	})

	inbox, _ := f.bus.Inbox(ctx, Addr{AgentID: f.aB.ID}, 10)
	if len(inbox) != 2 {
		t.Fatalf("got %d messages", len(inbox))
	}
	if inbox[0].Content != "urgent" {
		t.Fatalf("high priority must sort first, got %q", inbox[0].Content)
	}
}

// §9.5: undelivered high-priority messages are re-nudged after five minutes.
func TestHighPriorityMessagesAreRenudged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.bus.Send(ctx, Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID, ContainerID: f.cB.ID},
		Type:      TypeWarning, Content: "the build is broken", Priority: PriorityHigh,
	})
	f.nudger.nudges = nil

	// Not yet due.
	f.clock.advance(2 * time.Minute)
	if n, _ := f.bus.RenudgeStale(ctx); n != 0 {
		t.Fatalf("re-nudged %d messages before the interval elapsed", n)
	}

	f.clock.advance(4 * time.Minute)
	n, err := f.bus.RenudgeStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-nudged %d, want 1", n)
	}
	if len(f.nudger.nudges) != 1 || !contains(f.nudger.nudges[0], "still waiting") {
		t.Fatalf("nudges = %v", f.nudger.nudges)
	}
}

func TestDeliveredMessagesAreNotRenudged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.bus.Send(ctx, Message{
		ProjectID: f.project.ID,
		From:      Addr{AgentID: f.aA.ID, ContainerID: f.cA.ID},
		To:        Addr{AgentID: f.aB.ID, ContainerID: f.cB.ID},
		Type:      TypeWarning, Content: "seen already", Priority: PriorityHigh,
	})
	f.bus.Inbox(ctx, Addr{AgentID: f.aB.ID}, 10) // deliver it
	f.nudger.nudges = nil

	f.clock.advance(10 * time.Minute)
	if n, _ := f.bus.RenudgeStale(ctx); n != 0 {
		t.Fatalf("a delivered message was re-nudged %d times", n)
	}
}

func TestSendValidatesItsInput(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base := Message{ProjectID: f.project.ID, From: Addr{AgentID: f.aA.ID}, Content: "x"}

	bad := base
	bad.Type = "SHOUTING"
	bad.To = Addr{AgentID: f.aB.ID}
	if _, err := f.bus.Send(ctx, bad); err == nil {
		t.Error("an unknown message type must be rejected")
	}

	noRecipient := base
	noRecipient.Type = TypeInfo
	if _, err := f.bus.Send(ctx, noRecipient); err == nil {
		t.Error("a message with no recipient must be rejected")
	}

	empty := base
	empty.Type = TypeInfo
	empty.To = Addr{AgentID: f.aB.ID}
	empty.Content = "   "
	if _, err := f.bus.Send(ctx, empty); err == nil {
		t.Error("an empty message must be rejected")
	}
}

func TestRefsRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	sent, _ := f.bus.Send(ctx, Message{
		ProjectID: f.project.ID, From: Addr{AgentID: f.aA.ID},
		To: Addr{AgentID: f.aB.ID}, Type: TypeRequest, Content: "see these",
		Refs: Refs{Task: f.task.ID, ContextKey: "deps/api", Files: []string{"src/auth.ts"}},
	})

	got, err := f.bus.Get(ctx, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refs.Task != f.task.ID || got.Refs.ContextKey != "deps/api" {
		t.Fatalf("refs = %+v", got.Refs)
	}
	if len(got.Refs.Files) != 1 || got.Refs.Files[0] != "src/auth.ts" {
		t.Fatalf("files = %v", got.Refs.Files)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
