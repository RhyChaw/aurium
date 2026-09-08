package cli

import (
	"context"
	"fmt"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/stack"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var dryRun, autostash, force, cascade bool

	cmd := &cobra.Command{
		Use:   "sync [container]",
		Short: "Rebase a container onto its parent's current tip",
		Long: "Replays this container's own commits — those after its recorded base —\n" +
			"onto the parent's tip, then advances the recorded base.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				ref := ""
				if len(args) == 1 {
					ref = args[0]
				} else {
					ref = "" // resolved from cwd below
				}

				var targets []store.Container
				if ref == "" {
					c, err := containerFromCwd(ctx, a)
					if err != nil {
						return err
					}
					targets = []store.Container{c}
				} else {
					c, err := resolveContainer(ctx, a, ref)
					if err != nil {
						return err
					}
					targets = []store.Container{c}
				}

				if cascade {
					p, _, cfg, err := a.Project(ctx, cwd())
					if err != nil {
						return wrap(CodeUsage, err)
					}
					all, err := a.Store.ListContainers(ctx, p.ID)
					if err != nil {
						return wrap(CodeUsage, err)
					}
					roots := stack.BuildForest(all, cfg.Project.BaseBranch)
					// Parents before children (D7), or a child lands on a tip
					// its parent is about to rewrite.
					var ordered []store.Container
					for _, n := range stack.TopoOrder(roots) {
						ordered = append(ordered, n.Container)
					}
					targets = ordered
				}

				exit := 0
				for _, c := range targets {
					res, err := stack.Sync(ctx, gitx.New(c.Worktree), stack.Target{
						Branch: c.Branch, ParentBranch: c.ParentBranch, BaseSHA: c.BaseSHA,
					}, stack.Options{DryRun: dryRun, Autostash: autostash, Force: force})
					if err != nil {
						return wrap(CodeGit, err)
					}

					reportSync(c, res)

					if dryRun {
						continue
					}
					switch res.Eligibility {
					case stack.Synced:
						if err := a.Store.UpdateContainerBaseSHA(ctx, c.ID, res.NewBaseSHA); err != nil {
							return wrap(CodeUsage, err)
						}
						_ = a.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerRunning, "")
						_ = a.Events.Emit(ctx, events.Event{
							Type: events.ContainerSynced, Actor: events.ActorHuman,
							ProjectID: c.ProjectID, ContainerID: c.ID,
							Payload: map[string]any{
								"parent": c.ParentBranch, "new_base": res.NewBaseSHA,
								"replayed": res.Replayed,
							},
						})
					case stack.Conflict:
						_ = a.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerConflict, res.Reason)
						_ = a.Events.Emit(ctx, events.Event{
							Type: events.ContainerConflict, Actor: events.ActorHuman,
							ProjectID: c.ProjectID, ContainerID: c.ID,
							Payload: map[string]any{"files": res.ConflictedFiles},
						})
						exit = CodeConflict
					case stack.Drifted:
						_ = a.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerDrifted, res.Reason)
						exit = CodeGit
					}
				}
				if exit != 0 {
					return &ExitError{Code: exit}
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen and change nothing")
	cmd.Flags().BoolVar(&autostash, "autostash", false, "carry uncommitted work across the rebase")
	cmd.Flags().BoolVar(&force, "force", false, "sync even with a dirty worktree")
	cmd.Flags().BoolVar(&cascade, "cascade", false, "sync the whole forest, parents first")
	return cmd
}

func reportSync(c store.Container, res stack.Result) {
	switch res.Eligibility {
	case stack.Synced:
		fmt.Printf("%s (%s): rebased %d commit(s) onto %s @ %s\n",
			c.Branch, c.ID, res.Replayed, c.ParentBranch, shortSHA(res.NewBaseSHA))
	case stack.UpToDate:
		fmt.Printf("%s (%s): up to date\n", c.Branch, c.ID)
	case stack.Eligible:
		fmt.Printf("%s (%s): would run %s\n", c.Branch, c.ID, res.Plan)
	case stack.Conflict:
		fmt.Printf("%s (%s): CONFLICT in %v\n", c.Branch, c.ID, res.ConflictedFiles)
		fmt.Printf("  The rebase is left in progress in %s.\n", c.Worktree)
		fmt.Printf("  Resolve it there, then: git rebase --continue\n")
	default:
		fmt.Printf("%s (%s): %s — %s\n", c.Branch, c.ID, res.Eligibility, res.Reason)
	}
}

// containerFromCwd finds the container whose worktree contains the working
// directory, so `aurium sync` works with no arguments from inside a container.
func containerFromCwd(ctx context.Context, a *app.App) (store.Container, error) {
	p, _, _, err := a.Project(ctx, cwd())
	if err != nil {
		return store.Container{}, wrap(CodeUsage, err)
	}
	cs, err := a.Store.ListContainers(ctx, p.ID)
	if err != nil {
		return store.Container{}, wrap(CodeUsage, err)
	}
	dir := cwd()
	for _, c := range cs {
		if dir == c.Worktree || len(dir) > len(c.Worktree) && dir[:len(c.Worktree)] == c.Worktree {
			return c, nil
		}
	}
	return store.Container{}, exitf(CodeUsage,
		"not inside a container worktree; name one explicitly (aurium sync <container>)")
}
