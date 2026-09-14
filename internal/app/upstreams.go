package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/RhyChaw/aurium/internal/github"
	"github.com/RhyChaw/aurium/internal/mcp"
)

// upstreamConfig is what `integration connect` stored in config_json.
type upstreamConfig struct {
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	SecretEnv string   `json:"secret_env"`
	URL       string   `json:"url"`
	// TokenFrom names a credential the machine already holds, instead of one
	// Aurium stores. "gh_cli" means `gh auth token` (§D28).
	//
	// It is read fresh on every daemon start, which is the point: `gh` rotates
	// its token, and a copy in the keyring would go stale silently and fail
	// every tool call with an error about the wrong thing.
	TokenFrom string `json:"token_from"`
}

// TokenFromGitHubCLI is the upstreamConfig.TokenFrom value meaning "ask `gh`".
const TokenFromGitHubCLI = "gh_cli"

// ConnectUpstreams spawns every configured MCP server and registers it with
// the gateway.
//
// The daemon must do this on startup: `aurium integration connect` spawns a
// server only long enough to learn its tools, so without this an agent's first
// call reports the integration as disconnected — which is exactly what
// happened before this existed.
//
// Credentials are read from the keyring here and passed into the child
// process's environment. They never enter the database, an event, or a
// container (D17, §10.6).
func (a *App) ConnectUpstreams(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	rows, err := a.Store.DB().QueryContext(ctx,
		`SELECT id, name, kind, config_json, COALESCE(secret_ref,'') FROM integrations`)
	if err != nil {
		return err
	}
	type row struct{ id, name, kind, configJSON, secretRef string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.kind, &r.configJSON, &r.secretRef); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()

	for _, r := range all {
		var cfg upstreamConfig
		if err := json.Unmarshal([]byte(r.configJSON), &cfg); err != nil {
			a.markIntegration(ctx, r.id, "error", "unreadable configuration: "+err.Error())
			continue
		}
		if cfg.Command == "" {
			continue
		}

		env := os.Environ()
		if cfg.TokenFrom == TokenFromGitHubCLI && cfg.SecretEnv != "" {
			token := github.CLIToken(ctx)
			if token == "" {
				a.markIntegration(ctx, r.id, "error",
					"`gh` is not logged in on this machine; run `gh auth login`")
				log.Warn("aurium: integration credential unavailable",
					"integration", r.name, "reason", "gh not logged in")
				continue
			}
			env = append(env, cfg.SecretEnv+"="+token)
		} else if cfg.SecretEnv != "" && r.secretRef != "" {
			secret, err := a.Secrets.Get(r.secretRef)
			if err != nil {
				// Say which integration and why, rather than failing every
				// tool call later with a confusing "not connected".
				a.markIntegration(ctx, r.id, "error",
					fmt.Sprintf("credential %s is not in the keyring: %v", r.secretRef, err))
				log.Warn("aurium: integration credential unavailable",
					"integration", r.name, "err", err)
				continue
			}
			env = append(env, cfg.SecretEnv+"="+secret)
		}

		client, err := mcp.SpawnStdio(ctx, cfg.Command, cfg.Args, env)
		if err != nil {
			a.markIntegration(ctx, r.id, "error", err.Error())
			log.Warn("aurium: could not start integration", "integration", r.name, "err", err)
			continue
		}
		if _, err := client.Initialize(ctx); err != nil {
			client.Close()
			a.markIntegration(ctx, r.id, "error", err.Error())
			log.Warn("aurium: integration handshake failed", "integration", r.name, "err", err)
			continue
		}

		// Re-list on reconnect (§10.3): tools may have changed, and existing
		// grants are kept for anything that comes back.
		if tools, err := client.ListTools(ctx); err == nil {
			if _, err := a.Gateway.RegisterCapabilities(ctx, r.id, tools, nil); err != nil {
				log.Warn("aurium: re-registering capabilities", "integration", r.name, "err", err)
			}
		}

		a.Gateway.RegisterUpstream(r.id, &stdioUpstream{name: r.name, client: client})
		a.markIntegration(ctx, r.id, "connected", "")
		log.Info("aurium: integration connected", "integration", r.name)
	}
	return nil
}

func (a *App) markIntegration(ctx context.Context, id, status, lastErr string) {
	_, _ = a.Store.DB().ExecContext(ctx,
		`UPDATE integrations SET status = ?, last_error = ? WHERE id = ?`,
		status, nullIfEmpty(lastErr), id)
}

// stdioUpstream adapts an MCP stdio client to the gateway's Upstream.
type stdioUpstream struct {
	name   string
	client *mcp.StdioClient
}

func (u *stdioUpstream) Name() string { return u.name }

func (u *stdioUpstream) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	return u.client.ListTools(ctx)
}

func (u *stdioUpstream) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.ToolResult, error) {
	return u.client.CallTool(ctx, name, args)
}

func (u *stdioUpstream) Close() error { return u.client.Close() }

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
