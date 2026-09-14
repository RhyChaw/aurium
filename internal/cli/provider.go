package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/providers"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/RhyChaw/aurium/internal/usage"
	"github.com/spf13/cobra"
)

// D21 says the API is the product boundary and every client of it is equal.
// The dashboard could connect a provider account and read what it spent; the
// CLI could not. These commands close that, so neither surface is the one that
// really works.

func newProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "provider",
		Short:   "Connect the model accounts agents run on",
		Aliases: []string{"providers"},
		Long: "A credential is stored in your OS keyring; the database keeps only a\n" +
			"reference to it. Nothing here ever prints a secret back.",
	}
	cmd.AddCommand(
		newProviderListCmd(),
		newProviderDetectCmd(),
		newProviderConnectCmd(),
		newProviderForgetCmd(),
	)
	return cmd
}

func newProviderListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "Connected accounts",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				accounts, err := a.Store.ListProviderAccounts(ctx)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(map[string]any{"accounts": accounts}) {
					return nil
				}
				if len(accounts) == 0 {
					fmt.Println("No provider accounts connected.")
					fmt.Println("Agents fall back to whatever this terminal exported, which is not")
					fmt.Println("a choice anybody made. See `aurium provider detect`.")
					return nil
				}

				tw := newTabWriter(os.Stdout)
				fmt.Fprintln(tw, "ID\tPROVIDER\tLABEL\tKIND\tSOURCE\tSTATUS")
				for _, p := range accounts {
					status := p.Status
					if p.LastError != "" {
						status += " (" + truncate(p.LastError, 40) + ")"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						p.ID, p.Provider, p.Label, p.AuthKind, p.Source, status)
				}
				return tw.Flush()
			})
		},
	}
}

func newProviderDetectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "What this host already offers",
		Long: "Reports which credentials exist on this machine — environment variables that\n" +
			"are set, and provider CLIs that are logged in. It never prints their values.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				found, err := a.Providers.Detect(ctx)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(map[string]any{"providers": found, "keyring": a.Secrets.Available()}) {
					return nil
				}
				for _, d := range found {
					fmt.Printf("%s  (adapter: %s, %d connected)\n", d.Display, d.Adapter, d.Connected)
					if len(d.HostEnv) > 0 {
						fmt.Printf("  set on this host  %s\n", strings.Join(d.HostEnv, ", "))
					} else {
						fmt.Printf("  set on this host  nothing\n")
					}
					if d.CLILogin != "" {
						fmt.Printf("  CLI login         %s\n", d.CLILogin)
					} else if d.SubscriptionCommand != "" {
						fmt.Printf("  CLI login         none — run `%s`\n", d.SubscriptionCommand)
					}
					fmt.Println()
				}
				if !a.Secrets.Available() {
					fmt.Fprintln(os.Stderr,
						"warning: no OS keyring is available; credentials fall back to a 0600 "+
							"file under ~/.aurium that this build does not encrypt at rest.")
				}
				return nil
			})
		},
	}
}

func newProviderConnectCmd() *cobra.Command {
	var label, authKind string
	var fromEnv, fromCLI bool

	cmd := &cobra.Command{
		Use:   "connect <anthropic|openai>",
		Short: "Connect an account",
		Long: "By default the credential is read from standard input, so it never appears\n" +
			"in your shell history or in the process table:\n\n" +
			"    aurium provider connect anthropic --label work < key.txt\n\n" +
			"--from-env takes the value this host already exports. --from-cli-login\n" +
			"records that the provider's own CLI is logged in here and stores nothing:\n" +
			"copying a credential out of another program's config would create a second\n" +
			"thing to revoke.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				req := providers.ConnectRequest{
					Provider: args[0], Label: label, AuthKind: authKind,
					UseHostEnv: fromEnv, UseCLILogin: fromCLI,
				}
				if !fromEnv && !fromCLI {
					secret, err := readSecret()
					if err != nil {
						return wrap(CodeUsage, err)
					}
					req.Secret = secret
				}

				acct, err := a.Providers.Connect(ctx, req)
				if err != nil {
					return exitf(CodeUsage, "%v", err)
				}
				if emit(acct) {
					return nil
				}
				fmt.Printf("Connected %s (%s)\n", acct.Label, acct.ID)
				fmt.Printf("  provider  %s\n", acct.Provider)
				fmt.Printf("  kind      %s\n", acct.AuthKind)
				if acct.SecretRef != "" {
					fmt.Printf("  stored at %s\n", acct.SecretRef)
				} else {
					fmt.Printf("  stored at nothing — the provider CLI holds its own login\n")
				}
				fmt.Printf("\nAgents on the %s adapter will use it.\n",
					providers.Specs[acct.Provider].Adapter)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "a name for this account")
	cmd.Flags().StringVar(&authKind, "kind", store.AuthAPIKey,
		"api_key (billed per token) or subscription (a seat; metered, not costed)")
	cmd.Flags().BoolVar(&fromEnv, "from-env", false,
		"use the credential this host already exports")
	cmd.Flags().BoolVar(&fromCLI, "from-cli-login", false,
		"use the provider CLI's own login on this machine, storing nothing")
	return cmd
}

func newProviderForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "forget <account>",
		Aliases: []string{"disconnect"},
		Short:   "Drop an account and the credential it stored",
		Long: "Agents already running keep going: the credential is in their environment\n" +
			"and Aurium does not reach into a live container to take it back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				if err := a.Providers.Disconnect(ctx, args[0]); err != nil {
					return exitf(CodeUsage, "%v", err)
				}
				if emit(map[string]any{"forgotten": args[0]}) {
					return nil
				}
				fmt.Printf("Dropped %s, and the credential it held.\n", args[0])
				return nil
			})
		},
	}
}

// readSecret takes the credential from standard input.
//
// Not from a flag: a credential in argv is a credential in the shell history
// and in every `ps` on the machine.
func readSecret() (string, error) {
	stat, err := os.Stdin.Stat()
	if err == nil && stat.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Paste the credential, then Enter: ")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no credential on standard input")
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return "", fmt.Errorf("no credential on standard input")
	}
	return secret, nil
}

// ---- usage ----

func newUsageCmd() *cobra.Command {
	var window, groupBy string

	cmd := &cobra.Command{
		Use:   "usage",
		Short: "What the fleet has spent",
		Long: "Metered where a provider reports it: headless runs through the claude\n" +
			"adapter. Interactive sessions surface nothing the daemon can read, so they\n" +
			"are not counted and the total is a lower bound.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				d, err := time.ParseDuration(window)
				if err != nil || d <= 0 {
					return exitf(CodeUsage,
						"--window %q is not a positive duration (try 1h, 24h, 168h)", window)
				}
				q := store.UsageQuery{Since: time.Now().UTC().Add(-d).Format(time.RFC3339Nano)}

				totals, err := a.Store.UsageTotals(ctx, q)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				groups, err := a.Store.AggregateUsage(ctx, q, groupBy)
				if err != nil {
					return exitf(CodeUsage, "%v", err)
				}

				if emit(map[string]any{
					"window": window, "totals": totals, "group_by": groupBy, "groups": groups,
				}) {
					return nil
				}

				fmt.Printf("Last %s: %d calls, %d in / %d out, $%.2f priced\n",
					window, totals.Calls, totals.InputTokens, totals.OutputTokens, totals.CostUSD)
				if totals.UnpricedTokens > 0 {
					// Said separately rather than folded into the cost: a
					// subscription seat has no per-token price, and a model
					// absent from the table has no price Aurium knows.
					fmt.Printf("  %d tokens are unpriced (a subscription seat, or a model with no price).\n",
						totals.UnpricedTokens)
				}
				if len(groups) == 0 {
					fmt.Println("\nNothing metered in this window.")
					return nil
				}

				fmt.Printf("\nBy %s:\n", groupBy)
				tw := newTabWriter(os.Stdout)
				fmt.Fprintln(tw, "  KEY\tCALLS\tIN\tOUT\tCOST")
				for _, g := range groups {
					key := g.Key
					if key == "" {
						key = "(unattributed)"
					}
					cost := fmt.Sprintf("$%.2f", g.CostUSD)
					if g.CostUSD == 0 && g.UnpricedTokens > 0 {
						cost = "unpriced"
					}
					fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%s\n",
						key, g.Calls, g.InputTokens, g.OutputTokens, cost)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				fmt.Printf("\nList prices verified %s; discounts, batch pricing and cache tiers are not modelled.\n",
					usage.PricesVerified)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&window, "window", "24h", "how far back to look")
	cmd.Flags().StringVar(&groupBy, "by", "provider",
		fmt.Sprintf("group by one of %v", store.UsageGroupings()))
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
