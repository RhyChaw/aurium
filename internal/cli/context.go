package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/app"
	ce "github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/spf13/cobra"
)

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "context",
		Short:   "Read and write shared context",
		Aliases: []string{"ctx"},
	}

	var scopeProject bool
	var scopeContainer string
	var baseVersion int

	// resolveScope turns the flags into a scope for whichever subcommand ran.
	resolveScope := func(ctx context.Context, a *app.App) (string, string, error) {
		p, _, _, err := a.Project(ctx, cwd())
		if err != nil {
			return "", "", wrap(CodeUsage, err)
		}
		if scopeContainer != "" {
			c, err := resolveContainer(ctx, a, scopeContainer)
			if err != nil {
				return "", "", err
			}
			return ce.ScopeContainer, c.ID, nil
		}
		return ce.ScopeProject, p.ID, nil
	}

	get := &cobra.Command{
		Use:   "get <key>",
		Short: "Read a context item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				scope, scopeID, err := resolveScope(ctx, a)
				if err != nil {
					return err
				}
				it, err := a.Context.Get(ctx, ce.Ref{Scope: scope, ScopeID: scopeID, Key: args[0]})
				if err != nil {
					return exitf(CodeUsage, "no context item %q in %s scope", args[0], scope)
				}
				if emit(it) {
					return nil
				}
				fmt.Printf("# %s (v%d, by %s)\n\n%s\n", it.Key, it.Version, it.UpdatedBy, it.Content)
				return nil
			})
		},
	}

	set := &cobra.Command{
		Use:   "set <key> <content>",
		Short: "Write a context item",
		Long: "Writes are compare-and-set. Pass --base-version with the version you read;\n" +
			"omit it to create a new item.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				scope, scopeID, err := resolveScope(ctx, a)
				if err != nil {
					return err
				}
				it, err := a.Context.Write(ctx,
					ce.Ref{Scope: scope, ScopeID: scopeID, Key: args[0]},
					baseVersion, args[1], "human", "set from the CLI")
				if err != nil {
					var stale *ce.ErrStale
					if asStaleErr(err, &stale) {
						fmt.Printf("Refused: %q is now at version %d, but you passed %d.\n",
							args[0], stale.CurrentVersion, stale.ExpectedVersion)
						fmt.Printf("Current content:\n%s\n", stale.CurrentContent)
						return &ExitError{Code: CodeConflict}
					}
					return wrap(CodeUsage, err)
				}
				fmt.Printf("%s is now version %d\n", it.Key, it.Version)
				return nil
			})
		},
	}
	set.Flags().IntVar(&baseVersion, "base-version", 0, "the version you read (0 to create)")

	appendCmd := &cobra.Command{
		Use:   "append <key> <text>",
		Short: "Append a block to a context item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				scope, scopeID, err := resolveScope(ctx, a)
				if err != nil {
					return err
				}
				it, err := a.Context.Append(ctx,
					ce.Ref{Scope: scope, ScopeID: scopeID, Key: args[0]}, args[1], "human")
				if err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("%s is now version %d\n", it.Key, it.Version)
				return nil
			})
		},
	}

	list := &cobra.Command{
		Use:   "list [prefix]",
		Short: "List context keys",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				scope, scopeID, err := resolveScope(ctx, a)
				if err != nil {
					return err
				}
				prefix := ""
				if len(args) == 1 {
					prefix = args[0]
				}
				items, err := a.Context.List(ctx, scope, scopeID, prefix)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(items) {
					return nil
				}
				if len(items) == 0 {
					fmt.Println("No context items.")
					return nil
				}
				w := tabWriter()
				fmt.Fprintln(w, "KEY\tVERSION\tUPDATED BY\tUPDATED")
				for _, it := range items {
					fmt.Fprintf(w, "%s\tv%d\t%s\t%s\n", it.Key, it.Version, it.UpdatedBy, it.UpdatedAt)
				}
				return w.Flush()
			})
		},
	}

	query := &cobra.Command{
		Use:   "query <words...>",
		Short: "Search context and indexed documentation",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, _, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				hits, err := a.Context.Query(ctx, ce.Subject{Human: true, ProjectID: p.ID},
					strings.Join(args, " "), 20)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(hits) {
					return nil
				}
				if len(hits) == 0 {
					fmt.Println("No matches.")
					return nil
				}
				for _, h := range hits {
					where := h.Key
					if h.Kind == "doc" {
						where = h.Path
						if h.Heading != "" {
							where += " § " + h.Heading
						}
					}
					fmt.Printf("%-40s %s\n", where, h.Snippet)
				}
				return nil
			})
		},
	}

	index := &cobra.Command{
		Use:   "index",
		Short: "Index the project's documentation for search",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				n, err := a.Context.IndexDocs(ctx, p.ID, cfg.Project.Context)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("Indexed %d section(s) from %v\n", n, cfg.Project.Context)
				return nil
			})
		},
	}

	proposals := &cobra.Command{
		Use:   "proposals",
		Short: "List open proposals",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				ps, err := a.Context.ListProposals(ctx, ce.ProposalOpen)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(ps) {
					return nil
				}
				if len(ps) == 0 {
					fmt.Println("No open proposals.")
					return nil
				}
				for _, p := range ps {
					fmt.Printf("%s  %s (v%d) by %s\n  %s\n\n",
						p.ID, p.Key, p.BaseVersion, p.Author, p.Reason)
				}
				return nil
			})
		},
	}

	decide := func(decision string) *cobra.Command {
		return &cobra.Command{
			Use:   decision + " <proposal>",
			Short: strings.Title(decision) + " a proposal",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withApp(func(ctx context.Context, a *app.App) error {
					want := ce.ProposalAccepted
					if decision == "reject" {
						want = ce.ProposalRejected
					}
					p, err := a.Context.Decide(ctx, args[0], want, "human")
					if err != nil {
						return wrap(CodeUsage, err)
					}
					fmt.Printf("Proposal %s %s\n", p.ID, p.Status)
					return nil
				})
			},
		}
	}

	for _, c := range []*cobra.Command{get, set, appendCmd, list, query, index, proposals} {
		c.Flags().BoolVar(&scopeProject, "project", false, "use project scope (default)")
		c.Flags().StringVar(&scopeContainer, "container", "", "use a container's scope")
	}
	cmd.AddCommand(get, set, appendCmd, list, query, index, proposals,
		decide("accept"), decide("reject"))
	return cmd
}

func newInboxCmd() *cobra.Command {
	var ackAll bool

	return &cobra.Command{
		Use:   "inbox",
		Short: "Read messages addressed to you",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				msgs, err := a.IPC.Inbox(ctx, ipcHuman(), 50)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(msgs) {
					return nil
				}
				if len(msgs) == 0 {
					fmt.Println("Your inbox is empty.")
					return nil
				}
				for _, m := range msgs {
					marker := " "
					if m.Priority == "high" {
						marker = "!"
					}
					fmt.Printf("%s %s  %-18s from %s\n   %s\n\n",
						marker, m.ID, m.Type, describeSender(m), indent(m.Content))
				}
				if ackAll {
					var ids []string
					for _, m := range msgs {
						ids = append(ids, m.ID)
					}
					n, _ := a.IPC.Ack(ctx, ids)
					fmt.Printf("Acknowledged %d message(s).\n", n)
				} else {
					fmt.Printf("Acknowledge with: aurium inbox --ack\n")
				}
				return nil
			})
		},
	}
}

func newApproveCmd(reject bool) *cobra.Command {
	verb := "approve"
	if reject {
		verb = "reject"
	}
	return &cobra.Command{
		Use:   verb + " [id]",
		Short: strings.Title(verb) + " a pending request",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				pending, err := a.Gateway.ListApprovals(ctx, "pending")
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if len(args) == 0 {
					if len(pending) == 0 {
						fmt.Println("Nothing is waiting for you.")
						return nil
					}
					fmt.Println("Pending requests:")
					for _, ap := range pending {
						fmt.Printf("  %s  %s\n    from %s, expires %s\n",
							ap.ID, ap.Capability, dash(ap.AgentID), ap.ExpiresAt)
						if ap.Reason != "" {
							fmt.Printf("    reason: %s\n", ap.Reason)
						}
					}
					fmt.Printf("\n%s one with: aurium %s <id>\n", strings.Title(verb), verb)
					return nil
				}

				decision := "approved"
				if reject {
					decision = "rejected"
				}

				// This must go through the daemon. Approving executes the held
				// call, and only the daemon holds the connected upstreams and
				// their credentials (D17). Deciding in this process would mark
				// the approval decided and then fail to run it.
				ap, err := decideViaDaemon(args[0], decision)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("%s %s (%s)\n", strings.Title(decision), ap.ID, ap.Capability)
				return nil
			})
		},
	}
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n   ")
}

// describeSender names who sent a message, in the form a human recognises.
func describeSender(m ipc.Message) string {
	switch {
	case m.From.AgentID != "":
		if m.From.ContainerID != "" {
			return m.From.AgentID + " in " + m.From.ContainerID
		}
		return m.From.AgentID
	case m.From.ContainerID != "":
		return m.From.ContainerID
	case m.From.Human:
		return "you"
	default:
		return "aurium"
	}
}

// ipcHuman addresses the human's own inbox.
func ipcHuman() ipc.Addr { return ipc.Addr{Human: true} }

func asStaleErr(err error, target **ce.ErrStale) bool { return errors.As(err, target) }
