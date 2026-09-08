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

	baseBranch, err := g.CurrentBranch(ctx)
	if err != nil {
		return wrap(CodeGit, err)
	}

	cfgPath := filepath.Join(root, config.Filename)
	if _, err := os.Stat(cfgPath); err == nil {
		return exitf(CodeUsage, "%s already exists; this project is already initialised", cfgPath)
	}

	body := fmt.Sprintf(`version: 1

project:
  name: %s
  base_branch: %s
  # Directories indexed for aurium_context_query.
  context:
    - ./docs

sandbox:
  driver: %s
  image: %s
  agent: %s
  resources: { cpus: 2, memory: 2g, pids: 2048 }
  idle_pause_minutes: 15
  # Ports are published on 127.0.0.1 only.
  ports: []
  volumes:
    # Cloned on fork/stack; these hold derived, per-container state.
    per_sandbox: []
    # Mounted into every container and never cloned.
    shared: []
  env: {}
  # Only the agent's own credential belongs here. Integration secrets stay on
  # the host and are reached through the MCP gateway.
  env_passthrough: [ ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN ]

hooks:
  post_create: []
  post_sync: []
  pre_destroy: []

stack:
  auto_sync: false
  autostash: false
  push: { remote: origin, force_with_lease: true }

snapshot:
  auto_on: [ task_transition, pre_sync, pre_restore ]
  keep_last: 10
  # Gitignored build output is derivable; keeping it would bloat every snapshot.
  include_ignored: false
`, filepath.Base(root), baseBranch, driverName, baseImage, adapterName)

	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		return wrap(CodeUsage, err)
	}

	// Validate what we just wrote rather than trusting it.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return wrap(CodeUsage, err)
	}

	if err := gitx.InstallHooks(root); err != nil {
		return wrap(CodeGit, err)
	}
	if err := gitx.EnsureExcluded(root); err != nil {
		return wrap(CodeGit, err)
	}

	p, err := a.Store.ProjectByRoot(ctx, root)
	if err != nil {
		p, err = a.Store.CreateProject(ctx, cfg.Project.Name, root)
		if err != nil {
			return wrap(CodeUsage, err)
		}
	}
	if _, err := a.Store.RepositoryByPath(ctx, p.ID, root); err != nil {
		remote, _ := g.Run(ctx, "remote", "get-url", "origin")
		if _, err := a.Store.CreateRepository(ctx, p.ID, root, cfg.Project.BaseBranch, remote); err != nil {
			return wrap(CodeUsage, err)
		}
	}

	// Seed the default context permissions (§8.3). Without these every agent
	// is denied everything, since context is default-deny.
	if err := a.Context.SeedDefaults(ctx, p.ID); err != nil {
		return wrap(CodeUsage, err)
	}

	if err := a.Events.Emit(ctx, events.Event{
		Type: events.ProjectCreated, Actor: events.ActorHuman, ProjectID: p.ID,
		Payload: map[string]any{"root": root, "base_branch": cfg.Project.BaseBranch},
	}); err != nil {
		return wrap(CodeUsage, err)
	}

	if emit(map[string]any{"project": p.ID, "root": root, "config": cfgPath}) {
		return nil
	}
	fmt.Printf("Initialised %s (%s)\n", cfg.Project.Name, p.ID)
	fmt.Printf("  config      %s\n", cfgPath)
	fmt.Printf("  guard hooks %s\n", gitx.HooksPath(root))
	fmt.Printf("  base branch %s\n", cfg.Project.BaseBranch)
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
	fmt.Printf("\nNext: aurium container create <branch>\n")
	_ = store.ContainerCreating
	return nil
}
