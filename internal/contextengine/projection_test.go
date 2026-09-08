package contextengine

import (
	"os"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

func baseData() ProjectionData {
	return ProjectionData{
		Container: store.Container{
			ID: "c_01J", Branch: "implement-oauth", ParentBranch: "implement-auth",
			BaseSHA: "9f8e7d6c5b4a3210", Worktree: "/wt",
		},
		Task:       store.Task{Title: "Implement OAuth"},
		ParentDesc: "`implement-auth` (c_01H)",
		SyncStatus: "up_to_date",
		Tools:      []string{"aurium_context_get", "github_create_pull_request"},
	}
}

func TestProjectionStatesTheThingsAnAgentMustKnow(t *testing.T) {
	out := string(Render(baseData()))

	for _, want := range []string{
		"c_01J",
		"Implement OAuth",
		"implement-oauth",
		"implement-auth",
		// The environment rule: violating it loses work silently.
		"re-derived",
		"hooks.post_create",
		// The branch rule: the guard hook will refuse anything else, and an
		// agent that does not know this wastes a turn discovering it.
		"You may commit only to `implement-oauth`",
		"aurium_context_get",
		"github_create_pull_request",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("projection is missing %q\n---\n%s", want, out)
		}
	}
}

func TestProjectionWarnsWhenTheParentHasMoved(t *testing.T) {
	d := baseData()
	d.Stale = true
	d.SyncStatus = "stale — parent is 3 commit(s) ahead"
	out := string(Render(d))

	if !strings.Contains(out, "parent has moved") {
		t.Errorf("a stale container must be told to sync:\n%s", out)
	}
	if !strings.Contains(out, "aurium sync") {
		t.Errorf("it should say how:\n%s", out)
	}

	// And an up-to-date container must not be nagged.
	if strings.Contains(string(Render(baseData())), "parent has moved") {
		t.Error("an up-to-date container should not be told to sync")
	}
}

func TestProjectionRendersDecisionsAndDiscoveries(t *testing.T) {
	d := baseData()
	d.Decisions = []Item{
		{Key: "decisions/2026-09-07-jwt", Version: 3, Content: "Asymmetric signing, RS256."},
	}
	d.Discoveries = []Item{
		{Key: "discoveries/auth-flow", Version: 1, Content: "The legacy flow double-encodes state."},
	}
	out := string(Render(d))

	for _, want := range []string{
		"decisions/2026-09-07-jwt", "(v3)", "Asymmetric signing",
		"discoveries/auth-flow", "double-encodes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

// Empty sections must say how to fill them. An agent that sees a blank heading
// learns nothing; one that sees the tool name can act.
func TestEmptySectionsExplainThemselves(t *testing.T) {
	out := string(Render(baseData()))
	if !strings.Contains(out, "aurium_context_write") {
		t.Errorf("an unset objective should name the tool that sets it:\n%s", out)
	}
	if !strings.Contains(out, "aurium_context_append") {
		t.Errorf("empty decisions should name the tool that records them:\n%s", out)
	}
}

func TestInboxCountIsGrammatical(t *testing.T) {
	cases := map[int]string{
		0: "No unread messages",
		1: "1 unread message —",
		5: "5 unread messages",
	}
	for n, want := range cases {
		d := baseData()
		d.UnreadMessages = n
		if out := string(Render(d)); !strings.Contains(out, want) {
			t.Errorf("with %d unread, want %q\n---\n%s", n, want, out)
		}
	}
}

func TestProjectionSaysNotToEditIt(t *testing.T) {
	out := string(Render(baseData()))
	if !strings.Contains(out, "Do not edit") {
		t.Error("the file is regenerated, so it must say so or edits are silently lost")
	}
	if !strings.Contains(out, "aurium_context_") {
		t.Error("it should point at the tools that do persist changes")
	}
}

// An agent may read this file at any instant, so a half-written projection
// must never be observable.
func TestWriteProjectionIsAtomic(t *testing.T) {
	e := newEngine(t)
	dir := t.TempDir()

	// Seed a container whose worktree is the temp dir.
	ctx := ctxBackground()
	p, _ := e.Store.ProjectByRoot(ctx, "/r")
	repo, _ := e.Store.CreateRepository(ctx, p.ID, "/r", "main", "")
	c, err := e.Store.CreateContainer(ctx, store.Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "f", Slug: "f",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: dir, Status: store.ContainerRunning, OriginKind: store.OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := e.WriteProjection(ctx, c.ID, []string{"aurium_context_get"}); err != nil {
		t.Fatal(err)
	}

	path := ProjectionPath(dir)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), c.ID) {
		t.Fatalf("projection does not describe the container:\n%s", body)
	}
	// No temp file left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file used for the atomic write was left behind")
	}
}

func TestGatherPrefersContainerScopeOverProject(t *testing.T) {
	e := newEngine(t)
	ctx := ctxBackground()
	dir := t.TempDir()

	p, _ := e.Store.ProjectByRoot(ctx, "/r")
	repo, _ := e.Store.CreateRepository(ctx, p.ID, "/r", "main", "")
	c, _ := e.Store.CreateContainer(ctx, store.Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "f", Slug: "f",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: dir, Status: store.ContainerRunning, OriginKind: store.OriginFresh,
	})

	e.Write(ctx, Ref{Scope: ScopeProject, ScopeID: p.ID, Key: "task/objective"},
		0, "the project-wide objective", "human", "")
	e.Write(ctx, Ref{Scope: ScopeContainer, ScopeID: c.ID, Key: "task/objective"},
		0, "this container's own objective", "human", "")

	d, err := e.Gather(ctx, c.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// §8.1: the more specific scope shadows on key collision.
	if d.Objective != "this container's own objective" {
		t.Fatalf("objective = %q, want the container-scoped one", d.Objective)
	}
}
