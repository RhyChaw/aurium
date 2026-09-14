package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/project"
	"github.com/RhyChaw/aurium/internal/store"
)

// Provisioning lives here rather than in internal/cli because `aurium init`
// and the dashboard's "new project" button must do the same thing. D21 says
// the API is the product boundary; two implementations of "make a project"
// would mean one of them is the real one and the other drifts.

// RepoSpec names a repository to attach.
type RepoSpec struct {
	// Path is a directory inside or at the root of a git repository. It is
	// resolved to the repository's toplevel, so pointing at a subdirectory
	// works the way every git command does.
	Path string `json:"path"`
	// BaseBranch defaults to whatever branch the repository is on.
	BaseBranch string `json:"base_branch,omitempty"`
}

// NewProject describes a project to create.
type NewProject struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Root        string     `json:"root,omitempty"`
	Repos       []RepoSpec `json:"repos,omitempty"`
	// Context lists directories indexed for aurium_context_query.
	Context []string `json:"context,omitempty"`
	// Driver, Image and Agent seed the aurium.yaml of any member repo that
	// does not already have one.
	Driver string `json:"driver,omitempty"`
	Image  string `json:"image,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// CreateProject writes a descriptor, registers the project, and attaches each
// repository.
//
// A repository that cannot be attached fails the whole call rather than
// producing a project quietly missing a third of itself — the same reason the
// descriptor rejects unknown keys.
func (a *App) CreateProject(ctx context.Context, np NewProject) (store.Project, []store.Repository, error) {
	name := strings.TrimSpace(np.Name)
	if name == "" {
		return store.Project{}, nil, fmt.Errorf("app: a project needs a name")
	}

	// An unspecified driver becomes one that can actually run here. The
	// alternative — defaulting to docker on a machine with Docker stopped —
	// makes a project whose every agent fails.
	if strings.TrimSpace(np.Driver) == "" {
		np.Driver, _ = a.DefaultDriver(ctx)
	}

	root, err := a.projectRoot(np)
	if err != nil {
		return store.Project{}, nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return store.Project{}, nil, err
	}
	root = CanonicalPath(root)

	descriptorPath := filepath.Join(root, project.Filename)
	if _, err := os.Stat(descriptorPath); err == nil {
		return store.Project{}, nil, fmt.Errorf(
			"app: %s already exists; that directory is already a project", descriptorPath)
	}

	// Resolve every repository before writing anything. Half a project on
	// disk is worse than a clean failure, because nothing cleans it up.
	resolved := make([]RepoSpec, 0, len(np.Repos))
	for _, r := range np.Repos {
		top, branch, err := a.inspectRepo(ctx, root, r)
		if err != nil {
			return store.Project{}, nil, err
		}
		base := r.BaseBranch
		if base == "" {
			base = branch
		}
		resolved = append(resolved, RepoSpec{Path: top, BaseBranch: base})
	}

	d := &project.Descriptor{
		Version: project.SchemaVersion,
		Project: project.Meta{Name: name, Description: np.Description},
		Defaults: project.Defaults{
			Driver: np.Driver, Image: np.Image, Agent: np.Agent,
		},
		Context: np.Context,
	}
	for _, r := range resolved {
		// Relative where it can be, so the descriptor travels with the
		// project; absolute where the repo lives outside it, because a
		// ../../../ chain is neither portable nor readable.
		d.Repos = append(d.Repos, project.Repo{
			Path: relativeIfInside(root, r.Path), BaseBranch: r.BaseBranch,
		})
	}
	if err := d.Write(descriptorPath); err != nil {
		return store.Project{}, nil, err
	}

	p, err := a.Store.CreateProject(ctx, name, root)
	if err != nil {
		_ = os.Remove(descriptorPath)
		return store.Project{}, nil, err
	}
	if err := a.Store.SetProjectDescriptor(ctx, p.ID, descriptorPath); err != nil {
		return store.Project{}, nil, err
	}
	p.Descriptor = descriptorPath

	// Seed the default context permissions (§8.3). Without these every agent
	// is denied everything, since context is default-deny.
	if err := a.Context.SeedDefaults(ctx, p.ID); err != nil {
		return store.Project{}, nil, err
	}

	if err := a.Events.Emit(ctx, events.Event{
		Type: events.ProjectCreated, Actor: events.ActorHuman, ProjectID: p.ID,
		Payload: map[string]any{
			"root": root, "descriptor": descriptorPath, "repos": len(resolved),
		},
	}); err != nil {
		return store.Project{}, nil, err
	}

	var repos []store.Repository
	for _, r := range resolved {
		repo, err := a.AttachRepository(ctx, p, r, np.Driver, np.Image, np.Agent)
		if err != nil {
			return store.Project{}, nil, err
		}
		repos = append(repos, repo)
	}
	return p, repos, nil
}

// AttachRepository registers one git repository with a project: guard hooks,
// the .aurium exclude, an aurium.yaml if it has none, and the row.
//
// It is idempotent. Running it on a repository already in this project returns
// the existing row rather than failing, because "add these three repos" where
// one is already a member is an ordinary thing to ask for.
func (a *App) AttachRepository(ctx context.Context, p store.Project, spec RepoSpec,
	driver, image, adapter string) (store.Repository, error) {

	top, branch, err := a.inspectRepo(ctx, p.Root, spec)
	if err != nil {
		return store.Repository{}, err
	}
	base := spec.BaseBranch
	if base == "" {
		base = branch
	}

	if existing, err := a.Store.RepositoryByPathAny(ctx, top); err == nil {
		if existing.ProjectID != p.ID {
			other, _ := a.Store.GetProject(ctx, existing.ProjectID)
			return store.Repository{}, fmt.Errorf(
				"app: %s already belongs to project %q (%s); a repository is in one project at a time",
				top, other.Name, existing.ProjectID)
		}
		return existing, nil
	}

	cfgPath := filepath.Join(top, config.Filename)
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if strings.TrimSpace(driver) == "" {
			driver, _ = a.DefaultDriver(ctx)
		}
		if err := writeRepoConfig(cfgPath, filepath.Base(top), base, driver, image, adapter); err != nil {
			return store.Repository{}, err
		}
	}
	// Validate what is there — written now or written by hand earlier —
	// rather than trusting it.
	if _, err := config.Load(cfgPath); err != nil {
		return store.Repository{}, err
	}

	if err := gitx.InstallHooks(top); err != nil {
		return store.Repository{}, err
	}
	if err := gitx.EnsureExcluded(top); err != nil {
		return store.Repository{}, err
	}

	remote, _ := gitx.New(top).Run(ctx, "remote", "get-url", "origin")
	repo, err := a.Store.CreateRepository(ctx, p.ID, top, base, remote)
	if err != nil {
		return store.Repository{}, err
	}

	if err := a.Events.Emit(ctx, events.Event{
		Type: RepositoryAttached, Actor: events.ActorHuman, ProjectID: p.ID,
		Payload: map[string]any{"repo": repo.ID, "path": top, "base_branch": base},
	}); err != nil {
		return store.Repository{}, err
	}
	return repo, nil
}

// RepositoryAttached is emitted when a repository joins a project. It is
// declared here rather than in internal/events because it belongs to the
// multi-repo model this file implements, and events is the ERD's §11.1 list.
const RepositoryAttached = "project.repository_attached"

// SyncDescriptor rewrites a project's descriptor from the database, so a repo
// attached through the API is in the file a human later edits by hand.
func (a *App) SyncDescriptor(ctx context.Context, p store.Project) error {
	if p.Descriptor == "" {
		return nil // standalone: there is no descriptor to keep in step
	}
	d, err := project.Load(p.Descriptor)
	if err != nil {
		return err
	}
	repos, err := a.Store.ListRepositories(ctx, p.ID)
	if err != nil {
		return err
	}
	d.Repos = d.Repos[:0]
	for _, r := range repos {
		d.Repos = append(d.Repos, project.Repo{
			Path: relativeIfInside(p.Root, r.Path), BaseBranch: r.BaseBranch,
		})
	}
	return d.Write(p.Descriptor)
}

// inspectRepo resolves a path to a git toplevel and reads its current branch.
func (a *App) inspectRepo(ctx context.Context, base string, spec RepoSpec) (top, branch string, err error) {
	path, err := project.ResolveFrom(base, spec.Path)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", "", fmt.Errorf("app: %s: %w", spec.Path, err)
	}

	g := gitx.New(path)
	top, err = g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("app: %s is not a git repository", path)
	}
	top = CanonicalPath(top)

	// A worktree Aurium made is not a repository to attach; attaching one
	// would register a container's own checkout as a project member.
	if _, ok := projectRootOfWorktree(top); ok {
		return "", "", fmt.Errorf("app: %s is an Aurium worktree, not a repository", path)
	}

	branch, err = gitx.New(top).CurrentBranch(ctx)
	if err != nil {
		return "", "", err
	}
	return top, branch, nil
}

// projectRoot decides where a project's descriptor lives.
func (a *App) projectRoot(np NewProject) (string, error) {
	if strings.TrimSpace(np.Root) != "" {
		return project.ResolveFrom("", np.Root)
	}
	// No root given: keep it under ~/.aurium/projects, where it is out of the
	// way of the repositories themselves. A descriptor written into one member
	// repo would make that repo quietly special.
	return filepath.Join(a.Home, "projects", slug(np.Name)), nil
}

// relativeIfInside renders p relative to root when it sits under it, and
// absolute otherwise. A ../../../ chain is neither portable nor readable, so
// it is not produced.
func relativeIfInside(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	if rel == "." {
		return "."
	}
	return "./" + filepath.ToSlash(rel)
}

// slug makes a directory name out of a project name.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "project"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// writeRepoConfig writes the aurium.yaml a member repository needs, with the
// same content and the same comments `aurium init` writes.
func writeRepoConfig(path, name, baseBranch, driver, image, adapter string) error {
	if driver == "" {
		driver = "docker"
	}
	if image == "" {
		image = "node:20-alpine"
	}
	if adapter == "" {
		adapter = "claude"
	}
	return os.WriteFile(path, []byte(config.Template(name, baseBranch, driver, image, adapter)), 0o644)
}
