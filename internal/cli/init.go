package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/project"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var driverName, baseImage, adapterName string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Register the current repository with Aurium",
		Long: "Creates aurium.yaml, installs the guard hooks that enforce Invariant 1,\n" +
			"excludes .aurium/ from git, and records the project.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				return runInit(ctx, a, cwd(), driverName, baseImage, adapterName)
			})
		},
	}
	cmd.Flags().StringVar(&driverName, "driver", "docker", "container driver (docker|podman|local)")
	cmd.Flags().StringVar(&baseImage, "image", "node:20-alpine", "base image for containers")
	cmd.Flags().StringVar(&adapterName, "agent", "claude", "default agent adapter")
	return cmd
}

func runInit(ctx context.Context, a *app.App, dir, driverName, baseImage, adapterName string) error {
	g := gitx.New(dir)
	root, err := g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return exitf(CodeGit, "%s is not a git repository", dir)
	}
	// Store the symlink-free form: lookups compare roots as exact strings.
	root = app.CanonicalPath(root)

	if _, err := a.Store.RepositoryByPathAny(ctx, root); err == nil {
		return exitf(CodeUsage, "%s is already registered with Aurium", root)
	}
	cfgPath := filepath.Join(root, config.Filename)

	// If a descriptor sits above this repository, the user is adding a repo to
	// a project that already exists rather than starting a new one. Creating a
	// second project here would split one body of work in two.
	p, joined, err := enclosingProject(ctx, a, root)
	if err != nil {
		return wrap(CodeUsage, err)
	}
	if !joined {
		p, err = a.Store.CreateProject(ctx, filepath.Base(root), root)
		if err != nil {
			return wrap(CodeUsage, err)
		}
		// Seed the default context permissions (§8.3). Without these every
		// agent is denied everything, since context is default-deny.
		if err := a.Context.SeedDefaults(ctx, p.ID); err != nil {
			return wrap(CodeUsage, err)
		}
		if err := a.Events.Emit(ctx, events.Event{
			Type: events.ProjectCreated, Actor: events.ActorHuman, ProjectID: p.ID,
			Payload: map[string]any{"root": root, "standalone": true},
		}); err != nil {
			return wrap(CodeUsage, err)
		}
	}

	repo, err := a.AttachRepository(ctx, p,
		app.RepoSpec{Path: root}, driverName, baseImage, adapterName)
	if err != nil {
		return wrap(CodeUsage, err)
	}
	if err := a.SyncDescriptor(ctx, p); err != nil {
		return wrap(CodeUsage, err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return wrap(CodeUsage, err)
	}

	if emit(map[string]any{
		"project": p.ID, "repo": repo.ID, "root": root,
		"config": cfgPath, "joined": joined,
	}) {
		return nil
	}
	if joined {
		fmt.Printf("Added %s to project %s (%s)\n", filepath.Base(root), p.Name, p.ID)
	} else {
		fmt.Printf("Initialised %s (%s)\n", cfg.Project.Name, p.ID)
	}
	fmt.Printf("  config      %s\n", cfgPath)
	fmt.Printf("  guard hooks %s\n", gitx.HooksPath(root))
	fmt.Printf("  base branch %s\n", repo.BaseBranch)
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
	fmt.Printf("\nNext: aurium container create <branch>\n")
	_ = store.ContainerCreating
	return nil
}

// enclosingProject finds the project whose descriptor sits at or above root.
//
// A descriptor with no project row is a descriptor written by hand, or one
// whose database was thrown away; it is registered rather than ignored, so
// `aurium init` in a hand-written project does what it looks like it does.
func enclosingProject(ctx context.Context, a *app.App, root string) (store.Project, bool, error) {
	path, err := project.Find(root)
	if err != nil {
		return store.Project{}, false, nil // no descriptor above: standalone
	}
	descriptorRoot := app.CanonicalPath(filepath.Dir(path))

	if p, err := a.Store.ProjectByRoot(ctx, descriptorRoot); err == nil {
		return p, true, nil
	}

	d, err := project.Load(path)
	if err != nil {
		return store.Project{}, false, err
	}
	p, err := a.Store.CreateProject(ctx, d.Project.Name, descriptorRoot)
	if err != nil {
		return store.Project{}, false, err
	}
	if err := a.Store.SetProjectDescriptor(ctx, p.ID, path); err != nil {
		return store.Project{}, false, err
	}
	p.Descriptor = path
	if err := a.Context.SeedDefaults(ctx, p.ID); err != nil {
		return store.Project{}, false, err
	}
	return p, true, a.Events.Emit(ctx, events.Event{
		Type: events.ProjectCreated, Actor: events.ActorHuman, ProjectID: p.ID,
		Payload: map[string]any{"root": descriptorRoot, "descriptor": path, "adopted": true},
	})
}
