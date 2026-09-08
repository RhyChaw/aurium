package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/spf13/cobra"
)

func newContainerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "container",
		Short:   "Create and manage containers",
		Aliases: []string{"c"},
	}
	cmd.AddCommand(newContainerCreateCmd(), newContainerListCmd())
	return cmd
}

func newContainerCreateCmd() *cobra.Command {
	var parent, adapterName, role, model, taskID string
	var noAgent bool

	cmd := &cobra.Command{
		Use:   "create <branch>",
		Short: "Create a container on a new branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, repo, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}

				parentBranch := cfg.Project.BaseBranch
				parentContainerID := ""
				if parent != "" {
					// --parent takes a branch or a container id; resolving both
					// means the user never has to remember which they have.
					if pc, err := a.Store.GetContainer(ctx, parent); err == nil {
						parentBranch, parentContainerID = pc.Branch, pc.ID
					} else if pc, err := a.Store.GetContainerByBranch(ctx, repo.ID, parent); err == nil {
						parentBranch, parentContainerID = pc.Branch, pc.ID
					} else {
						parentBranch = parent
					}
				}

				if adapterName == "" {
					adapterName = cfg.Sandbox.Agent
				}
				if role == "" {
					role = store.RolePrimary
				}

				c, err := a.Manager.Create(ctx, runtime.CreateOpts{
					ProjectID:         p.ID,
					RepoID:            repo.ID,
					RepoRoot:          repo.Path,
					TaskID:            taskID,
					Branch:            args[0],
					ParentBranch:      parentBranch,
					ParentContainerID: parentContainerID,
					Config:            cfg,
					Adapter:           adapterName,
					Role:              role,
					Model:             model,
					NoAgent:           noAgent,
				})
				if err != nil {
					return wrap(CodeDriver, err)
				}

				if emit(c) {
					return nil
				}
				printContainer(a, c)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&parent, "parent", "", "parent branch or container to stack on")
	cmd.Flags().StringVar(&adapterName, "agent", "", "agent adapter (default: project setting)")
	cmd.Flags().StringVar(&role, "role", "", "agent role: primary|master|worker")
	cmd.Flags().StringVar(&model, "model", "", "model for the agent")
	cmd.Flags().StringVar(&taskID, "task", "", "task this container belongs to")
	cmd.Flags().BoolVar(&noAgent, "no-agent", false, "create the container without starting an agent")
	return cmd
}

func printContainer(a *app.App, c store.Container) {
	fmt.Printf("Created %s on branch %s\n", c.ID, c.Branch)
	fmt.Printf("  worktree %s\n", c.Worktree)
	fmt.Printf("  parent   %s @ %s\n", c.ParentBranch, shortSHA(c.BaseSHA))
	if len(c.Ports) > 0 {
		var parts []string
		for internal, host := range c.Ports {
			parts = append(parts, fmt.Sprintf("%d -> 127.0.0.1:%d", internal, host))
		}
		fmt.Printf("  ports    %s\n", strings.Join(parts, ", "))
	}
	if c.RuntimeID != "" {
		fmt.Printf("\nAttach: aurium attach %s\n", c.ID)
	}
}

func newContainerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List containers in this project",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, _, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				cs, err := a.Store.ListContainers(ctx, p.ID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(cs) {
					return nil
				}
				if len(cs) == 0 {
					fmt.Println("No containers. Create one with: aurium container create <branch>")
					return nil
				}
				w := tabWriter()
				fmt.Fprintln(w, "ID\tBRANCH\tPARENT\tSTATUS\tWORKTREE")
				for _, c := range cs {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Branch, c.ParentBranch, c.Status, c.Worktree)
				}
				return w.Flush()
			})
		},
	}
}

func newDestroyCmd() *cobra.Command {
	var deleteBranch, keepVolumes, archive, cascade bool

	cmd := &cobra.Command{
		Use:   "destroy <container>",
		Short: "Destroy a container",
		Long:  "Removes the container and its worktree. The branch is kept unless --delete-branch.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				c, err := resolveContainer(ctx, a, args[0])
				if err != nil {
					return err
				}

				targets := []store.Container{c}
				if cascade {
					children, err := a.Store.ListChildren(ctx, c.ID)
					if err != nil {
						return wrap(CodeUsage, err)
					}
					// Children first: destroying a parent whose children still
					// reference it would orphan them.
					targets = append(children, c)
				} else {
					children, _ := a.Store.ListChildren(ctx, c.ID)
					if len(children) > 0 {
						return exitf(CodeUsage,
							"%s has %d stacked child container(s); destroy them first or pass --cascade",
							c.ID, len(children))
					}
				}

				for _, t := range targets {
					if err := a.Manager.Destroy(ctx, t.ID, runtime.DestroyOpts{
						KeepVolumes: keepVolumes, DeleteBranch: deleteBranch, Archive: archive,
					}); err != nil {
						return wrap(CodeDriver, err)
					}
					fmt.Printf("Destroyed %s (%s)\n", t.ID, t.Branch)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&deleteBranch, "delete-branch", false, "also delete the git branch")
	cmd.Flags().BoolVar(&keepVolumes, "keep-volumes", false, "keep per-container volumes")
	cmd.Flags().BoolVar(&archive, "archive", false, "mark archived instead of deleting the record")
	cmd.Flags().BoolVar(&cascade, "cascade", false, "destroy stacked children too")
	return cmd
}

// resolveContainer accepts a container id, a branch name or a slug.
func resolveContainer(ctx context.Context, a *app.App, ref string) (store.Container, error) {
	if c, err := a.Store.GetContainer(ctx, ref); err == nil {
		return c, nil
	}
	p, repo, _, err := a.Project(ctx, cwd())
	if err != nil {
		return store.Container{}, wrap(CodeUsage, err)
	}
	if c, err := a.Store.GetContainerByBranch(ctx, repo.ID, ref); err == nil {
		return c, nil
	}
	if c, err := a.Store.GetContainerBySlug(ctx, p.ID, ref); err == nil {
		return c, nil
	}
	return store.Container{}, exitf(CodeUsage, "no container matches %q (try an id, branch or slug)", ref)
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func tabWriter() *tabwriterFlusher { return newTabWriter(os.Stdout) }
