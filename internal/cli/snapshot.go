package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/runtime/snapshot"
	"github.com/spf13/cobra"
)

func newSnapshotCmd() *cobra.Command {
	var label string

	cmd := &cobra.Command{
		Use:   "snapshot <container>",
		Short: "Capture a container's state",
		Long: "Captures source (including uncommitted and untracked work), the container\n" +
			"filesystem, and per-container volumes, while the container is paused.\n" +
			"Process memory is not captured; the agent's conversation is, because it\n" +
			"lives in $HOME inside the container filesystem.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				c, err := resolveContainer(ctx, a, args[0])
				if err != nil {
					return err
				}
				_, _, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}

				sn, err := a.Manager.Snapshot(ctx, c.ID, cfg, label, snapshot.TriggerManual)
				if err != nil {
					return wrap(CodeDriver, err)
				}
				if emit(sn) {
					return nil
				}
				fmt.Printf("Snapshot %d of %s (%s)\n", sn.Seq, c.Branch, sn.ID)
				if label != "" {
					fmt.Printf("  label %s\n", label)
				}
				fmt.Printf("  head  %s\n", shortSHA(sn.HeadSHA))
				if sn.Bytes > 0 {
					fmt.Printf("  size  %s of volume archives\n", humanBytes(sn.Bytes))
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "name this snapshot so retention keeps it")

	cmd.AddCommand(newSnapshotListCmd(), newSnapshotGCCmd())
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list <container>",
		Short:   "List a container's snapshots",
		Aliases: []string{"ls"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				c, err := resolveContainer(ctx, a, args[0])
				if err != nil {
					return err
				}
				snaps, err := a.Store.ListSnapshots(ctx, c.ID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(snaps) {
					return nil
				}
				if len(snaps) == 0 {
					fmt.Printf("No snapshots of %s.\n", c.Branch)
					return nil
				}
				w := tabWriter()
				fmt.Fprintln(w, "SEQ\tLABEL\tTRIGGER\tHEAD\tSIZE\tCREATED")
				for _, s := range snaps {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
						s.Seq, dash(s.Label), s.Trigger, shortSHA(s.HeadSHA),
						humanBytes(s.Bytes), s.CreatedAt)
				}
				return w.Flush()
			})
		},
	}
}

func newRestoreCmd() *cobra.Command {
	var noBackup bool

	cmd := &cobra.Command{
		Use:   "restore <container> <seq>",
		Short: "Return a container to a previous snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				c, err := resolveContainer(ctx, a, args[0])
				if err != nil {
					return err
				}
				seq, err := strconv.Atoi(args[1])
				if err != nil {
					return exitf(CodeUsage, "%q is not a snapshot number", args[1])
				}
				_, _, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}

				// Backup by default: restore is the one operation that
				// deliberately destroys current state, so it must be undoable.
				if err := a.Manager.Restore(ctx, c.ID, seq, cfg, !noBackup); err != nil {
					return wrap(CodeDriver, err)
				}
				fmt.Printf("Restored %s to snapshot %d\n", c.Branch, seq)
				if !noBackup {
					fmt.Println("  a pre_restore snapshot of the previous state was taken first")
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip the pre-restore snapshot")
	return cmd
}

func newForkCmd() *cobra.Command {
	var at int
	var branch, adapterName string

	cmd := &cobra.Command{
		Use:   "fork <container>",
		Short: "Create an independent copy of a container",
		Long: "The fork is a peer of the source, not a child: it inherits the source's\n" +
			"git parent, and the source's commits become the fork's own.",
		Args:    cobra.ExactArgs(1),
		Aliases: []string{"clone"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFork(args[0], branch, adapterName, at, false)
		},
	}
	cmd.Flags().IntVar(&at, "at", 0, "snapshot sequence to fork from (default: snapshot now)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch for the new container")
	cmd.Flags().StringVar(&adapterName, "agent", "", "agent adapter")
	return cmd
}

func newStackCmd() *cobra.Command {
	var at int
	var on, adapterName string

	cmd := &cobra.Command{
		Use:   "stack <branch> --on <container>",
		Short: "Create a container stacked on another",
		Long: "The new container is a child: it tracks the parent's branch, and the\n" +
			"parent's commits are inherited rather than owned, so syncing replays\n" +
			"only what the child adds.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if on == "" {
				return exitf(CodeUsage, "--on <container> is required")
			}
			return runFork(on, args[0], adapterName, at, true)
		},
	}
	cmd.Flags().StringVar(&on, "on", "", "container to stack on")
	cmd.Flags().IntVar(&at, "at", 0, "snapshot sequence to stack from (default: snapshot now)")
	cmd.Flags().StringVar(&adapterName, "agent", "", "agent adapter")
	return cmd
}

func runFork(sourceRef, branch, adapterName string, at int, stack bool) error {
	return withApp(func(ctx context.Context, a *app.App) error {
		source, err := resolveContainer(ctx, a, sourceRef)
		if err != nil {
			return err
		}
		_, _, cfg, err := a.Project(ctx, cwd())
		if err != nil {
			return wrap(CodeUsage, err)
		}
		if branch == "" {
			branch = source.Branch + "-fork"
		}

		created, err := a.Manager.Fork(ctx, source.ID, cfg, runtime.ForkOpts{
			At: at, Branch: branch, Stack: stack, Adapter: adapterName,
		})
		if err != nil {
			return wrap(CodeDriver, err)
		}
		if emit(created) {
			return nil
		}

		verb := "Forked"
		if stack {
			verb = "Stacked"
		}
		fmt.Printf("%s %s from %s\n", verb, created.Branch, source.Branch)
		fmt.Printf("  container %s\n", created.ID)
		fmt.Printf("  git parent %s @ %s\n", created.ParentBranch, shortSHA(created.BaseSHA))
		if stack {
			fmt.Printf("  syncs with: aurium sync %s\n", created.Branch)
		}
		return nil
	})
}

func newSnapshotGCCmd() *cobra.Command {
	var dryRun bool

	return &cobra.Command{
		Use:   "gc",
		Short: "Delete snapshots outside the retention policy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				report, err := snapshot.GC(ctx, a.Manager.SnapshotDeps(), p.ID, snapshot.Policy{
					KeepLast: cfg.Snapshot.KeepLast,
					DryRun:   dryRun,
				})
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(report) {
					return nil
				}
				verb := "Removed"
				if dryRun {
					verb = "Would remove"
				}
				fmt.Printf("%s %d snapshot(s), freeing %s\n",
					verb, len(report.Deleted), humanBytes(report.BytesFreed))
				for _, r := range report.Kept {
					fmt.Printf("  kept %s (%s)\n", r.ID, r.Reason)
				}
				return nil
			})
		},
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
