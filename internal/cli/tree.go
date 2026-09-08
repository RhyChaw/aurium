package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/stack"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/spf13/cobra"
)

func newTreeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tree",
		Short: "Show the container forest with sync status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, repo, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				cs, err := a.Store.ListContainers(ctx, p.ID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if len(cs) == 0 {
					fmt.Println("No containers.")
					return nil
				}

				roots := stack.BuildForest(cs, cfg.Project.BaseBranch)
				if emit(roots) {
					return nil
				}

				fmt.Printf("%s (base %s)\n", cfg.Project.Name, cfg.Project.BaseBranch)
				for _, r := range roots {
					printNode(ctx, a, repo, r, "")
				}
				return nil
			})
		},
	}
}

func printNode(ctx context.Context, a *app.App, repo store.Repository, n *stack.Node, prefix string) {
	c := n.Container
	status := c.Status

	// Recompute liveness from git rather than trusting the stored status: the
	// parent may have moved since the watcher last looked, and a stale answer
	// here is worse than a slow one.
	if res, err := stack.CheckEligibility(ctx, gitx.New(c.Worktree), stack.Target{
		Branch: c.Branch, ParentBranch: c.ParentBranch, BaseSHA: c.BaseSHA,
	}); err == nil {
		switch res.Eligibility {
		case stack.Eligible:
			status = "stale (parent moved)"
		case stack.UpToDate:
			status = "up-to-date"
		case stack.Dirty:
			status = "dirty"
		case stack.Drifted, stack.Conflict, stack.InProgress, stack.MissingParent:
			status = string(res.Eligibility)
		}
	}

	marker := "├─"
	fmt.Printf("%s%s %s  %s  [%s]", prefix, marker, c.Branch, c.ID, status)
	if n.Orphaned {
		fmt.Print("  (orphaned: parent container is gone)")
	}
	if len(c.Ports) > 0 {
		var parts []string
		for internal, host := range c.Ports {
			parts = append(parts, fmt.Sprintf("%d->%d", internal, host))
		}
		fmt.Printf("  ports %s", strings.Join(parts, ","))
	}
	fmt.Println()

	agents, _ := a.Store.ListAgents(ctx, c.ID)
	for _, ag := range agents {
		fmt.Printf("%s│    agent %s %s (%s)\n", prefix, ag.Adapter, ag.Role, ag.Status)
	}
	for _, child := range n.Children {
		printNode(ctx, a, repo, child, prefix+"│  ")
	}
}
