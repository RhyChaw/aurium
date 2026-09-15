package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// A host turn's silent failure, and the only cure for it.
//
// Verified against the real CLI: with `--mcp-config` naming a server that is
// unreachable or unauthorized, `claude -p --output-format json` returns
// "is_error": false and an empty permission_denials. Aurium records that as an
// ordinary reply. Add `--disallowedTools Bash` — which host placement always
// does — and the agent has no shell and no tools, and nothing downstream
// notices, because nothing downstream is looking at whether the tool path
// works.
//
// Three live triggers reach exactly this state: a daemon listening on a
// non-default --addr, a config written with an empty bearer token, and a
// daemon that is simply not running. Two of those are fixed at their source
// (the URL is derived from the daemon's real address; an empty token refuses
// to write a config at all). This is the third defence and the general one: a
// POSITIVE check that the tool path works, made once before the turn, so any
// cause at all — including ones nobody has thought of — fails by name instead
// of arriving as a cheerful empty answer.

// HostPreflightTimeout bounds the preflight request. It is a loopback call to
// a daemon that is either up or not, so it is short: a turn must not spend a
// noticeable part of its budget deciding whether it can start.
const HostPreflightTimeout = 10 * time.Second

// hostGatewayTool is the one tool a host-sandboxed turn cannot work without —
// its shell is denied, so this is where every project command goes.
const hostGatewayTool = "aurium_exec"

// ErrHostGatewayUnusable is returned when the gateway a host turn depends on
// cannot be reached, will not accept its token, or does not offer the tool
// that replaces its shell.
var ErrHostGatewayUnusable = errors.New("runtime: a host-placed agent cannot reach the aurium gateway")

// hostMCPConfig is the shape internal/agent writes for a host turn.
type hostMCPConfig struct {
	MCPServers map[string]struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"mcpServers"`
}

// preflightHost checks the gateway before the manager starts a host turn.
func (m *Manager) preflightHost(ctx context.Context, mcpConfigPath string) error {
	return preflightHostGateway(ctx, mcpConfigPath, http.DefaultClient)
}

// preflightHostGateway asks the endpoint the agent's own MCP config names, with
// the credential that config carries, for its tool list.
//
// Reading the config rather than taking a URL and token as arguments is
// deliberate: this must exercise what the agent will actually present, not a
// second copy of it that could be right while the file is wrong.
func preflightHostGateway(ctx context.Context, mcpConfigPath string, client *http.Client) error {
	body, err := os.ReadFile(mcpConfigPath)
	if err != nil {
		return fmt.Errorf("%w: its MCP config %s could not be read: %w",
			ErrHostGatewayUnusable, mcpConfigPath, err)
	}
	var cfg hostMCPConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return fmt.Errorf("%w: its MCP config %s is not valid JSON: %w",
			ErrHostGatewayUnusable, mcpConfigPath, err)
	}
	server, ok := cfg.MCPServers["aurium"]
	if !ok || server.URL == "" {
		return fmt.Errorf("%w: its MCP config %s names no aurium server",
			ErrHostGatewayUnusable, mcpConfigPath)
	}

	ctx, cancel := context.WithTimeout(ctx, HostPreflightTimeout)
	defer cancel()

	// tools/list, not initialize: initialize answers before the caller's
	// scopes are consulted, so it would say yes to a token that cannot
	// actually call anything. The tool list is the thing being checked.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL,
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHostGatewayUnusable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range server.Headers {
		req.Header.Set(k, v)
	}

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s is not answering. Is `auriumd` running, and on "+
			"this address? (%w)", ErrHostGatewayUnusable, server.URL, err)
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s refused its token (HTTP %d). Its MCP config at %s "+
			"carries a credential this daemon will not accept",
			ErrHostGatewayUnusable, server.URL, res.StatusCode, mcpConfigPath)
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: %s answered HTTP %d, not a tool list",
			ErrHostGatewayUnusable, server.URL, res.StatusCode)
	}

	// JSON-RPC errors travel inside a 200 (internal/api), so the status code
	// above is only half the answer.
	var out struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return fmt.Errorf("%w: %s did not answer with a tool list: %w",
			ErrHostGatewayUnusable, server.URL, err)
	}
	if out.Error != nil {
		return fmt.Errorf("%w: %s refused the request: %s",
			ErrHostGatewayUnusable, server.URL, out.Error.Message)
	}
	for _, tool := range out.Result.Tools {
		if tool.Name == hostGatewayTool {
			return nil
		}
	}
	return fmt.Errorf("%w: %s offers no %s tool, and with its own shell denied the "+
		"agent would have no way to run anything at all",
		ErrHostGatewayUnusable, server.URL, hostGatewayTool)
}
