package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/store"
)

// withGateway attaches a gateway to the harness, as the daemon does.
func (h *harness) withGateway(t *testing.T) *gateway.Gateway {
	t.Helper()
	ctx := context.Background()

	cx := contextengine.New(h.app.Store, h.app.Events)
	if err := cx.SeedDefaults(ctx, h.proj.ID); err != nil {
		t.Fatal(err)
	}
	g := gateway.New(h.app.Store, h.app.Events, cx, ipc.New(h.app.Store, h.app.Events, nil))

	srv := New(h.app, hostToken, nil)
	srv.Gateway = g
	h.srv.Config.Handler = srv
	return g
}

func (h *harness) rpc(t *testing.T, token, method string, params any) *http.Response {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	return h.do("POST", "/mcp", token, string(raw))
}

func TestMCPEndpointRequiresTheGatewayScope(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)
	ctx := context.Background()

	narrow, _, err := h.app.Store.CreateToken(ctx, h.cA.ID, "", []string{store.ScopeContextRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res := h.rpc(t, narrow, "tools/list", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a token without gateway:* got %d, want 403", res.StatusCode)
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)
	ctx := context.Background()

	// An agent in the container, so the caller resolves to a real role.
	if _, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary,
		TmuxSession: "agent", Status: store.AgentRunning,
	}); err != nil {
		t.Fatal(err)
	}

	res := h.rpc(t, h.cToken, "initialize", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d", res.StatusCode)
	}

	res2 := h.rpc(t, h.cToken, "tools/list", nil)
	defer res2.Body.Close()
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	json.NewDecoder(res2.Body).Decode(&out)

	names := map[string]bool{}
	for _, tl := range out.Result.Tools {
		names[tl.Name] = true
	}
	if !names["aurium_context_query"] || !names["aurium_ipc_inbox"] {
		t.Fatalf("native tools missing from tools/list: %+v", out.Result.Tools)
	}
}

// A JSON-RPC error must arrive inside a 200, or the shim reports a transport
// failure instead of handing the agent an error it can act on.
func TestJSONRPCErrorsTravelInsideA200(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)

	res := h.rpc(t, h.cToken, "tools/teleport", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a JSON-RPC error inside", res.StatusCode)
	}
	var out struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if out.Error == nil || out.Error.Code != -32601 {
		t.Fatalf("expected a method-not-found error, got %+v", out.Error)
	}
}

func TestMalformedMCPBodyIsAParseError(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)

	res := h.do("POST", "/mcp", h.cToken, "{not json")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body := make([]byte, 512)
	n, _ := res.Body.Read(body)
	if !strings.Contains(string(body[:n]), "-32700") {
		t.Fatalf("want a parse error, got %s", body[:n])
	}
}

// The identity comes from the token, never the body. Otherwise an agent could
// name itself master and grant itself delegation.
func TestCallerIdentityComesFromTheTokenNotTheBody(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)
	ctx := context.Background()

	if _, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary,
		TmuxSession: "agent", Status: store.AgentRunning,
	}); err != nil {
		t.Fatal(err)
	}

	// Claim to be a master in the payload.
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"role":"master","container_id":"c_other"}}`
	res := h.do("POST", "/mcp", h.cToken, raw)
	defer res.Body.Close()

	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	for _, tl := range out.Result.Tools {
		if tl.Name == "aurium_delegate" {
			t.Fatal("an agent promoted itself to master through the request body")
		}
	}
}

func TestNotificationsGetNoBody(t *testing.T) {
	h := newHarness(t)
	h.withGateway(t)

	res := h.do("POST", "/mcp", h.cToken,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("a notification returned %d, want 202", res.StatusCode)
	}
}
