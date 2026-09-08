package contextengine

import (
	"context"
	"errors"
	"testing"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/store"
)

type grantFixture struct {
	engine  *Engine
	project store.Project
	parent  store.Container
	child   store.Container
	other   store.Container
}

func newGrantFixture(t *testing.T) grantFixture {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")

	mk := func(branch, parentID string) store.Container {
		c, err := s.CreateContainer(ctx, store.Container{
			ProjectID: p.ID, RepoID: repo.ID, Branch: branch, Slug: branch,
			ParentBranch: "main", ParentContainerID: parentID,
			BaseSHA: "abc", Driver: "local", Worktree: "/r/wt/" + branch,
			Status: store.ContainerRunning, OriginKind: store.OriginFresh,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	parent := mk("A", "")
	child := mk("B", parent.ID)
	other := mk("C", "")

	e := New(s, events.New(s))
	if err := e.SeedDefaults(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	return grantFixture{engine: e, project: p, parent: parent, child: child, other: other}
}

// Default deny is the whole basis of the model.
func TestUngrantedAccessIsDenied(t *testing.T) {
	f := newGrantFixture(t)
	perm, err := f.engine.ResolvePerm(context.Background(), Subject{
		AgentID: "a_1", Role: "unknown-role", ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}, Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "secrets/master-plan"})
	if err != nil {
		t.Fatal(err)
	}
	if perm != PermNone {
		t.Fatalf("an unmatched subject got %q, want none", perm)
	}
}

// The §8.3 table, as an executable claim.
func TestSeededRoleDefaultsMatchTheSpecTable(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()

	cases := []struct {
		role string
		key  string
		want Perm
	}{
		{store.RolePrimary, "anything/at/all", PermRead},
		{store.RolePrimary, "architecture/layers", PermPropose},
		{store.RolePrimary, "conventions/naming", PermPropose},
		{store.RolePrimary, "decisions/2026-09-07-jwt", PermAppend},
		{store.RoleMaster, "architecture/layers", PermPropose},
		{store.RoleMaster, "decisions/anything", PermAppend},
		// A worker may read the project and nothing more: it is doing a
		// bounded subtask, not reshaping the project.
		{store.RoleWorker, "architecture/layers", PermRead},
		{store.RoleWorker, "decisions/anything", PermRead},
	}

	for _, c := range cases {
		got, err := f.engine.ResolvePerm(ctx, Subject{
			AgentID: "a_1", Role: c.role, ContainerID: f.parent.ID, ProjectID: f.project.ID,
		}, Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: c.key})
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("role %s on %q = %q, want %q", c.role, c.key, got, c.want)
		}
	}
}

// §8.1: a stacked child reads its parent chain's container context, so B sees
// A's decisions without touching A's files.
func TestStackedChildReadsItsParentChainContext(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()

	childSubject := Subject{
		AgentID: "a_child", Role: store.RolePrimary,
		ContainerID: f.child.ID, ProjectID: f.project.ID,
	}
	parentRef := Ref{Scope: ScopeContainer, ScopeID: f.parent.ID, Key: "decisions/approach"}

	perm, err := f.engine.ResolvePerm(ctx, childSubject, parentRef)
	if err != nil {
		t.Fatal(err)
	}
	if perm != PermRead {
		t.Fatalf("child got %q on its parent's context, want read", perm)
	}
	// Read, not write: Invariant 1 applies to context as well as to branches.
	if perm.Allows(PermWrite) {
		t.Fatal("a child must not be able to write its parent's context")
	}
}

// An unrelated container must see nothing, or the whole isolation story fails.
func TestUnrelatedContainerCannotReadAnothersContext(t *testing.T) {
	f := newGrantFixture(t)
	perm, err := f.engine.ResolvePerm(context.Background(), Subject{
		AgentID: "a_other", Role: store.RolePrimary,
		ContainerID: f.other.ID, ProjectID: f.project.ID,
	}, Ref{Scope: ScopeContainer, ScopeID: f.parent.ID, Key: "decisions/approach"})
	if err != nil {
		t.Fatal(err)
	}
	if perm != PermNone {
		t.Fatalf("an unrelated container got %q on another's context, want none", perm)
	}
}

// A parent must not inherit its child's context — the walk is one-directional.
func TestParentCannotReadItsChildsContext(t *testing.T) {
	f := newGrantFixture(t)
	perm, _ := f.engine.ResolvePerm(context.Background(), Subject{
		AgentID: "a_parent", Role: store.RolePrimary,
		ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}, Ref{Scope: ScopeContainer, ScopeID: f.child.ID, Key: "plan"})
	if perm != PermNone {
		t.Fatalf("parent got %q on its child's context, want none", perm)
	}
}

func TestContainerOwnsItsOwnContext(t *testing.T) {
	f := newGrantFixture(t)
	perm, _ := f.engine.ResolvePerm(context.Background(), Subject{
		AgentID: "a_1", Role: store.RoleWorker,
		ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}, Ref{Scope: ScopeContainer, ScopeID: f.parent.ID, Key: "plan"})
	if perm != PermWrite {
		t.Fatalf("a container got %q on its own context, want write", perm)
	}
}

// A more specific subject level decides outright, so a container-scoped grant
// can narrow what a project-scoped one opened.
func TestMoreSpecificSubjectWins(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()

	ref := Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "architecture/layers"}
	subject := Subject{
		AgentID: "a_narrow", Role: store.RolePrimary,
		ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}

	// The role default grants propose.
	if got, _ := f.engine.ResolvePerm(ctx, subject, ref); got != PermPropose {
		t.Fatalf("precondition: role default = %q, want propose", got)
	}

	// An agent-level grant is more specific and decides, even downward.
	if _, err := f.engine.GrantPerm(ctx, Grant{
		Scope: ScopeProject, ScopeID: f.project.ID, KeyGlob: "**",
		SubjectType: "agent", SubjectID: "a_narrow", Perm: PermRead,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.engine.ResolvePerm(ctx, subject, ref); got != PermRead {
		t.Fatalf("agent-level grant should decide, got %q", got)
	}
}

func TestHumanIsAlwaysAllowed(t *testing.T) {
	f := newGrantFixture(t)
	perm, _ := f.engine.ResolvePerm(context.Background(), Subject{Human: true},
		Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "anything"})
	if perm != PermWrite {
		t.Fatalf("the human at the CLI got %q, want write", perm)
	}
}

// `*` must not cross a path separator, or every "narrow" grant is wide.
func TestKeyGlobSemantics(t *testing.T) {
	cases := []struct {
		glob, key string
		want      bool
	}{
		{"**", "anything/at/all", true},
		{"decisions/**", "decisions/jwt", true},
		{"decisions/**", "decisions/auth/jwt", true},
		{"decisions/**", "decisions", true},
		{"decisions/**", "plans/jwt", false},
		{"decisions/*", "decisions/jwt", true},
		{"decisions/*", "decisions/auth/jwt", false},
		{"*", "plan", true},
		{"*", "decisions/jwt", false},
		{"plan", "plan", true},
		{"plan", "plans", false},
	}
	for _, c := range cases {
		if got := matchKey(c.glob, c.key); got != c.want {
			t.Errorf("matchKey(%q, %q) = %v, want %v", c.glob, c.key, got, c.want)
		}
	}
}

func TestRequireReturnsAnActionableError(t *testing.T) {
	f := newGrantFixture(t)
	ref := Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "architecture/layers"}

	err := f.engine.Require(context.Background(), Subject{
		AgentID: "a_1", Role: store.RoleWorker,
		ContainerID: f.other.ID, ProjectID: f.project.ID,
	}, ref, PermWrite, "aurium_context_write")

	var perr *ErrPermission
	if !errors.As(err, &perr) {
		t.Fatalf("want ErrPermission, got %v", err)
	}
	if perr.Want != PermWrite || perr.Have != PermRead {
		t.Fatalf("error should say what was wanted and held: %+v", perr)
	}
	if perr.Reason != "aurium_context_write" {
		t.Errorf("the error should name the action, got %q", perr.Reason)
	}
}

// A corrupt parent chain must not hang the resolver.
func TestAncestorWalkToleratesCycles(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()

	// Point the parent at its own child, making a loop.
	if _, err := f.engine.Store.DB().ExecContext(ctx,
		`UPDATE containers SET parent_container_id = ? WHERE id = ?`,
		f.child.ID, f.parent.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan Perm, 1)
	go func() {
		p, _ := f.engine.ResolvePerm(ctx, Subject{
			ContainerID: f.child.ID, ProjectID: f.project.ID, Role: store.RolePrimary,
		}, Ref{Scope: ScopeContainer, ScopeID: f.other.ID, Key: "plan"})
		done <- p
	}()

	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("resolving against a cyclic parent chain hung")
	}
}
