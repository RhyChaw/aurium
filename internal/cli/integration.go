package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/mcp"
	"github.com/spf13/cobra"
)

func newIntegrationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "integration",
		Short:   "Connect and govern external capabilities",
		Aliases: []string{"int"},
	}

	var mcpCmd, secretEnv string
	connect := &cobra.Command{
		Use:   "connect <name>",
		Short: "Connect an MCP server and register its capabilities",
		Long: "The credential is read from an environment variable and stored in your OS\n" +
			"keyring. It is never written to the database and never enters a container:\n" +
			"agents reach the capability through the gateway instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				if mcpCmd == "" {
					return exitf(CodeUsage, "--mcp \"<command>\" is required")
				}
				p, _, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				name := args[0]

				// The secret goes to the keyring; only a reference is stored.
				secretRef := ""
				if secretEnv != "" {
					value := os.Getenv(secretEnv)
					if value == "" {
						return exitf(CodeUsage,
							"$%s is not set; export the credential before connecting", secretEnv)
					}
					secretRef, err = a.Secrets.Set(cfg.Project.Name, name, value)
					if err != nil {
						return wrap(CodeUsage, err)
					}
				}

				fields := strings.Fields(mcpCmd)
				configJSON, _ := json.Marshal(map[string]any{
					"command": fields[0], "args": fields[1:], "secret_env": secretEnv,
				})

				integrationID := ids.New(ids.Integration)
				if _, err := a.Store.DB().ExecContext(ctx,
					`INSERT INTO integrations (id, project_id, kind, name, config_json, secret_ref, status)
					 VALUES (?,?,?,?,?,?,?)`,
					integrationID, p.ID, "mcp_stdio", name, string(configJSON),
					nullOrEmpty(secretRef), "connecting"); err != nil {
					return exitf(CodeUsage, "an integration named %q already exists", name)
				}

				// Spawn it to learn its tools. The credential is injected into
				// this process's environment only.
				env := os.Environ()
				if secretEnv != "" {
					value, err := a.Secrets.Get(secretRef)
					if err != nil {
						return wrap(CodeUsage, err)
					}
					env = append(env, secretEnv+"="+value)
				}

				client, err := mcp.SpawnStdio(ctx, fields[0], fields[1:], env)
				if err != nil {
					a.Store.DB().ExecContext(ctx,
						`UPDATE integrations SET status = ?, last_error = ? WHERE id = ?`,
						"error", err.Error(), integrationID)
					return wrap(CodeUsage, err)
				}
				defer client.Close()

				if _, err := client.Initialize(ctx); err != nil {
					return wrap(CodeUsage, err)
				}
				tools, err := client.ListTools(ctx)
				if err != nil {
					return wrap(CodeUsage, err)
				}

				overrides := map[string]string{}
				if in, ok := cfg.Integrations[name]; ok {
					overrides = in.RiskOverrides
				}
				caps, err := a.Gateway.RegisterCapabilities(ctx, integrationID, tools, overrides)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if err := a.Gateway.SeedGrants(ctx, integrationID, p.ID, caps); err != nil {
					return wrap(CodeUsage, err)
				}
				a.Store.DB().ExecContext(ctx,
					`UPDATE integrations SET status = ? WHERE id = ?`, "connected", integrationID)

				if err := a.Events.Emit(ctx, events.Event{
					Type: events.IntegrationConnected, Actor: events.ActorHuman, ProjectID: p.ID,
					Payload: map[string]any{"integration": name, "capabilities": len(caps)},
				}); err != nil {
					return wrap(CodeUsage, err)
				}

				fmt.Printf("Connected %s with %d capabilities.\n", name, len(caps))
				byRisk := map[gateway.Risk]int{}
				for _, c := range caps {
					byRisk[c.Risk]++
				}
				fmt.Printf("  %d low (allowed), %d medium (allowed), %d high (require approval)\n",
					byRisk[gateway.RiskLow], byRisk[gateway.RiskMedium], byRisk[gateway.RiskHigh])
				if secretRef != "" {
					fmt.Printf("  credential stored in your keyring as %s\n", secretRef)
					fmt.Printf("  it is not in the database and will not enter any container\n")
				}
				return nil
			})
		},
	}
	connect.Flags().StringVar(&mcpCmd, "mcp", "", "the MCP server command to run")
	connect.Flags().StringVar(&secretEnv, "secret-env", "",
		"environment variable holding the credential (stored in the keyring)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List connected integrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, _, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				rows, err := a.Store.DB().QueryContext(ctx,
					`SELECT id, name, kind, status, COALESCE(secret_ref,''), COALESCE(last_error,'')
					 FROM integrations WHERE project_id = ? ORDER BY name`, p.ID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				defer rows.Close()

				w := tabWriter()
				fmt.Fprintln(w, "NAME\tKIND\tSTATUS\tCREDENTIAL\tERROR")
				n := 0
				for rows.Next() {
					var id, name, kind, status, ref, lastErr string
					rows.Scan(&id, &name, &kind, &status, &ref, &lastErr)
					cred := "none"
					if ref != "" {
						cred = "keyring"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, kind, status, cred, dash(lastErr))
					n++
				}
				w.Flush()
				if n == 0 {
					fmt.Println("No integrations. Connect one with: aurium integration connect <name> --mcp \"...\"")
				}
				return nil
			})
		},
	}

	capabilities := &cobra.Command{
		Use:   "capabilities <name>",
		Short: "Show an integration's capabilities and who may use them",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, _, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				var integrationID string
				if err := a.Store.DB().QueryRowContext(ctx,
					`SELECT id FROM integrations WHERE project_id = ? AND name = ?`,
					p.ID, args[0]).Scan(&integrationID); err != nil {
					return exitf(CodeUsage, "no integration named %q", args[0])
				}

				caps, err := a.Gateway.ListCapabilities(ctx, integrationID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(caps) {
					return nil
				}

				w := tabWriter()
				fmt.Fprintln(w, "CAPABILITY\tRISK\tPROJECT\tWORKER")
				for _, c := range caps {
					if c.Removed {
						continue
					}
					projectMode, _, _ := a.Gateway.Resolve(ctx,
						gateway.Subject{ProjectID: p.ID, Role: "primary"}, integrationID, c.Name)
					workerMode, _, _ := a.Gateway.Resolve(ctx,
						gateway.Subject{ProjectID: p.ID, Role: "worker"}, integrationID, c.Name)
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.Risk, projectMode, workerMode)
				}
				return w.Flush()
			})
		},
	}

	var denyContainer string
	grant := &cobra.Command{
		Use:   "grant <name>",
		Short: "Change who may use an integration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, _, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				var integrationID string
				if err := a.Store.DB().QueryRowContext(ctx,
					`SELECT id FROM integrations WHERE project_id = ? AND name = ?`,
					p.ID, args[0]).Scan(&integrationID); err != nil {
					return exitf(CodeUsage, "no integration named %q", args[0])
				}
				if denyContainer == "" {
					return exitf(CodeUsage, "--deny <container> is required")
				}
				c, err := resolveContainer(ctx, a, denyContainer)
				if err != nil {
					return err
				}
				if err := a.Gateway.RevokeForContainer(ctx, integrationID, c.ID, "human"); err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("%s can no longer use %s. Other containers are unaffected.\n",
					c.Branch, args[0])
				return nil
			})
		},
	}
	grant.Flags().StringVar(&denyContainer, "deny", "", "container to revoke access for")

	cmd.AddCommand(connect, list, capabilities, grant)
	return cmd
}

func nullOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
