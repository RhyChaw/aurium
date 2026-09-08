package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// An echo server exercised through the real stdio transport.
type echoHandler struct{ calls []string }

func (h *echoHandler) Handle(ctx context.Context, req *Request) *Response {
	switch req.Method {
	case "initialize":
		return NewResponse(req.ID, InitializeResult{
			ProtocolVersion: ProtocolVersion,
			ServerInfo:      ServerInfo{Name: "echo", Version: "1"},
		})
	case "notifications/initialized":
		return nil
	case "tools/list":
		return NewResponse(req.ID, ToolsListResult{Tools: []Tool{
			{Name: "echo", Description: "Echo the input"},
		}})
	case "tools/call":
		var p CallParams
		json.Unmarshal(req.Params, &p)
		h.calls = append(h.calls, p.Name)
		return NewResponse(req.ID, TextResult("echoed "+p.Name))
	}
	return NewError(req.ID, Errorf(CodeMethodNotFound, "unknown method %q", req.Method))
}

func TestServeHandlesRequestsAndNotifications(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n")

	var out strings.Builder
	if err := Serve(context.Background(), in, &out, &echoHandler{}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// A notification produces no response, so two requests give two lines.
	if len(lines) != 2 {
		t.Fatalf("got %d responses, want 2 (a notification must not be answered):\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], `"tools"`) {
		t.Errorf("second response = %s", lines[1])
	}
}

// A single malformed line must not take down an agent's only channel to Aurium.
func TestServeRecoversFromAMalformedFrame(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`this is not json at all`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n")

	var out strings.Builder
	if err := Serve(context.Background(), in, &out, &echoHandler{}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 responses (ok, parse error, ok), got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], "-32700") {
		t.Errorf("a malformed frame should produce a parse error: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"tools"`) {
		t.Errorf("the session must continue after a bad frame: %s", lines[2])
	}
}

func TestServeRejectsAWrongProtocolVersion(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"tools/list"}` + "\n")
	var out strings.Builder
	Serve(context.Background(), in, &out, &echoHandler{})
	if !strings.Contains(out.String(), "-32600") {
		t.Fatalf("want invalid-request, got %s", out.String())
	}
}

// Tool results can carry file contents; the default 64 KB scanner limit would
// truncate them into unparseable JSON.
func TestServeHandlesLargeFrames(t *testing.T) {
	big := strings.Repeat("x", 200_000)
	frame, _ := json.Marshal(Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"` + big + `"}`),
	})

	var out strings.Builder
	if err := Serve(context.Background(), strings.NewReader(string(frame)+"\n"), &out, &echoHandler{}); err != nil {
		t.Fatalf("a large frame broke the transport: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("no response to a large frame")
	}
}

func TestIsNotification(t *testing.T) {
	cases := map[string]bool{
		`{"jsonrpc":"2.0","method":"x"}`:            true,
		`{"jsonrpc":"2.0","id":null,"method":"x"}`:  true,
		`{"jsonrpc":"2.0","id":1,"method":"x"}`:     false,
		`{"jsonrpc":"2.0","id":"abc","method":"x"}`: false,
	}
	for body, want := range cases {
		var r Request
		json.Unmarshal([]byte(body), &r)
		if got := r.IsNotification(); got != want {
			t.Errorf("%s IsNotification = %v, want %v", body, got, want)
		}
	}
}

func TestErrorResultIsDistinctFromATransportError(t *testing.T) {
	// A tool that failed is a successful call with isError set. Conflating it
	// with a JSON-RPC error would make an agent think the transport broke.
	res := ErrorResult("the file was not found")
	if !res.IsError {
		t.Error("ErrorResult must set isError")
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "not found") {
		t.Error("the agent needs to read what went wrong")
	}
}

// The client and server halves must agree, exercised over real pipes.
func TestStdioClientAgainstARealServerProcess(t *testing.T) {
	if os.Getenv("AURIUM_MCP_ECHO_CHILD") == "1" {
		// Child mode: act as an MCP echo server on stdio.
		Serve(context.Background(), os.Stdin, os.Stdout, &echoHandler{})
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := SpawnStdio(ctx, exe,
		[]string{"-test.run", "TestStdioClientAgainstARealServerProcess"},
		append(os.Environ(), "AURIUM_MCP_ECHO_CHILD=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	init, err := client.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if init.ServerInfo.Name != "echo" {
		t.Fatalf("server info = %+v", init.ServerInfo)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}

	res, err := client.CallTool(ctx, "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "echoed echo") {
		t.Fatalf("result = %+v", res)
	}
}

// A server that dies must produce an explanation, not a bare timeout.
func TestClientReportsUpstreamExitWithItsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell available")
	}
	client, err := SpawnStdio(ctx, sh,
		[]string{"-c", "echo 'fatal: missing GITHUB_TOKEN' >&2; exit 1"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_, err = client.Call(ctx, "tools/list", map[string]any{})
	if err == nil {
		t.Fatal("expected an error from a dead upstream")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("the error should carry the server's own output so the cause is visible, got: %v", err)
	}
}
