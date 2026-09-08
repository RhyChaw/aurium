package store

import (
	"context"
	"errors"
	"testing"
)

func TestProjectRoundTripAndLookupByRoot(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, "my-app", "/Users/a/code/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.CreatedAt == "" {
		t.Fatalf("CreateProject must populate ID and CreatedAt, got %+v", p)
	}

	got, err := s.ProjectByRoot(ctx, "/Users/a/code/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID {
		t.Fatalf("ProjectByRoot returned %s, want %s", got.ID, p.ID)
	}

	if _, err := s.ProjectByRoot(ctx, "/nowhere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project must be ErrNotFound, got %v", err)
	}
}

func TestProjectRootIsUnique(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateProject(ctx, "a", "/same"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, "b", "/same"); err == nil {
		t.Fatal("two projects must not share a root")
	}
}

func TestTaskTransitionBumpsUpdatedAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")

	task, err := s.CreateTask(ctx, p.ID, "Implement OAuth", "")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskCreated {
		t.Fatalf("new task status = %q, want %q", task.Status, TaskCreated)
	}

	if err := s.TransitionTask(ctx, task.ID, TaskRunning); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetTask(ctx, task.ID)
	if after.Status != TaskRunning {
		t.Fatalf("status = %q, want running", after.Status)
	}
	if after.UpdatedAt == task.UpdatedAt {
		t.Fatal("TransitionTask must bump updated_at")
	}
}

func TestTransitionTaskRejectsUnknownStatus(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	task, _ := s.CreateTask(ctx, p.ID, "t", "")

	if err := s.TransitionTask(ctx, task.ID, "teleported"); err == nil {
		t.Fatal("an unknown status must be rejected")
	}
}

func TestContainerRoundTripWithPorts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, err := s.CreateRepository(ctx, p.ID, "/r", "main", "origin")
	if err != nil {
		t.Fatal(err)
	}

	c, err := s.CreateContainer(ctx, Container{
		ProjectID:    p.ID,
		RepoID:       repo.ID,
		Branch:       "implement-oauth",
		Slug:         "implement-oauth",
		ParentBranch: "main",
		BaseSHA:      "9f8e7d6",
		Driver:       "docker",
		Worktree:     "/r/.aurium/wt/implement-oauth",
		Status:       ContainerCreating,
		OriginKind:   OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetContainerRuntime(ctx, c.ID, "deadbeef", "aurium-local/node:abc",
		"aurium-r-implement-oauth", map[int]int{3000: 53012}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetContainer(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ports[3000] != 53012 {
		t.Fatalf("ports did not round-trip: %+v", got.Ports)
	}
	if got.RuntimeID != "deadbeef" || got.Image != "aurium-local/node:abc" {
		t.Fatalf("runtime fields did not persist: %+v", got)
	}
}

func TestOneContainerPerBranchPerRepo(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")

	base := Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "feature", Slug: "feature",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: "/r/.aurium/wt/feature", Status: ContainerRunning, OriginKind: OriginFresh,
	}
	if _, err := s.CreateContainer(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateContainer(ctx, base); err == nil {
		t.Fatal("two containers must not share (repo, branch) — that is the collision the product removes")
	}
}

func TestUpdateContainerBaseSHAAndStatus(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	c := seedContainer(t, s, "feature")

	if err := s.UpdateContainerBaseSHA(ctx, c.ID, "newbase"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateContainerStatus(ctx, c.ID, ContainerStale, "parent moved"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetContainer(ctx, c.ID)
	if got.BaseSHA != "newbase" {
		t.Fatalf("base_sha = %q", got.BaseSHA)
	}
	if got.Status != ContainerStale || got.LastError != "parent moved" {
		t.Fatalf("status/last_error = %q/%q", got.Status, got.LastError)
	}
}

func TestListContainersScopedToProject(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedContainer(t, s, "one")
	seedContainer(t, s, "two")

	other, _ := s.CreateProject(ctx, "other", "/other")
	otherRepo, _ := s.CreateRepository(ctx, other.ID, "/other", "main", "")
	s.CreateContainer(ctx, Container{
		ProjectID: other.ID, RepoID: otherRepo.ID, Branch: "x", Slug: "x",
		ParentBranch: "main", BaseSHA: "a", Driver: "local",
		Worktree: "/other/wt/x", Status: ContainerRunning, OriginKind: OriginFresh,
	})

	p, _ := s.ProjectByRoot(ctx, "/r")
	list, err := s.ListContainers(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListContainers returned %d, want 2 (must not leak other projects)", len(list))
	}
}

// seedContainer creates project /r + repo + one container on the given branch.
func seedContainer(t *testing.T, s *Store, branch string) Container {
	t.Helper()
	ctx := context.Background()
	p, err := s.ProjectByRoot(ctx, "/r")
	if errors.Is(err, ErrNotFound) {
		p, err = s.CreateProject(ctx, "app", "/r")
	}
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.RepositoryByPath(ctx, p.ID, "/r")
	if errors.Is(err, ErrNotFound) {
		repo, err = s.CreateRepository(ctx, p.ID, "/r", "main", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateContainer(ctx, Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: branch, Slug: branch,
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: "/r/.aurium/wt/" + branch, Status: ContainerRunning, OriginKind: OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
