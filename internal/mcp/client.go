package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// StdioClient speaks MCP to an upstream server over its stdin/stdout.
//
// Aurium spawns these on the host, never inside a container, because they hold
// credentials (§10.6). A container reaches them only through the gateway.
type StdioClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan *Response

	closeOnce sync.Once
	done      chan struct{}
	// stderr keeps the last lines the server printed, so a failure can be
	// explained rather than reported as a bare timeout.
	stderrTail *ringBuffer
}

// SpawnStdio starts an upstream MCP server.
func SpawnStdio(ctx context.Context, name string, args []string, env []string) (*StdioClient, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: spawn %s: %w", name, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrame)

	c := &StdioClient{
		cmd: cmd, stdin: stdin, stdout: scanner,
		pending:    map[string]chan *Response{},
		done:       make(chan struct{}),
		stderrTail: newRingBuffer(20),
	}
	go c.readLoop()
	go c.drainStderr(stderr)
	return c, nil
}

func (c *StdioClient) readLoop() {
	defer close(c.done)

	for c.stdout.Scan() {
		line := c.stdout.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // a server logging to stdout must not break the session
		}
		if len(resp.ID) == 0 {
			continue // a notification from the server
		}

		c.mu.Lock()
		ch, ok := c.pending[string(resp.ID)]
		delete(c.pending, string(resp.ID))
		c.mu.Unlock()

		if ok {
			r := resp
			ch <- &r
		}
	}
}

func (c *StdioClient) drainStderr(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		c.stderrTail.add(scanner.Text())
	}
}

// Call sends a request and waits for its response.
func (c *StdioClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	ch := make(chan *Response, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
	}()

	var raw json.RawMessage
	if params != nil {
		body, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = body
	}
	body, err := json.Marshal(Request{JSONRPC: "2.0", ID: id, Method: method, Params: raw})
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c.stdin, "%s\n", body); err != nil {
		// A write can fail either because the server already exited or for an
		// ordinary I/O reason. "broken pipe" tells nobody anything; if the
		// process is gone, its own output is the only useful explanation.
		return nil, c.exitError(err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.exitError(nil)
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		body, err := json.Marshal(resp.Result)
		if err != nil {
			return nil, err
		}
		return body, nil
	}
}

// exitError explains a failure in terms of the upstream's own output when the
// process has exited, falling back to the underlying error otherwise.
//
// It waits briefly for the reader goroutines to notice the exit: a write can
// fail with "broken pipe" a moment before the process is reaped, and reporting
// the pipe error would hide the message the server printed on its way out.
func (c *StdioClient) exitError(cause error) error {
	select {
	case <-c.done:
	case <-time.After(250 * time.Millisecond):
		if cause != nil {
			return fmt.Errorf("mcp: write to %s: %w", c.cmd.Path, cause)
		}
	}
	return fmt.Errorf("mcp: %s exited; last output: %s", c.cmd.Path, c.stderrTail.String())
}

// Initialize performs the MCP handshake.
func (c *StdioClient) Initialize(ctx context.Context) (*InitializeResult, error) {
	raw, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "aurium", "version": "0.1.0"},
	})
	if err != nil {
		return nil, err
	}
	var out InitializeResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}

	// The notification the spec requires after a successful initialize.
	body, _ := json.Marshal(Request{JSONRPC: "2.0", Method: "notifications/initialized"})
	fmt.Fprintf(c.stdin, "%s\n", body)
	return &out, nil
}

// ListTools fetches the upstream's tool list.
func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out ToolsListResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool invokes an upstream tool.
func (c *StdioClient) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	raw, err := c.Call(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}
	var out ToolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Close shuts the upstream down.
func (c *StdioClient) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.stdin.Close()
		// Give it a moment to exit cleanly before killing it, so a server with
		// buffered state can flush.
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		err = c.cmd.Wait()
	})
	return err
}

// ringBuffer keeps the last n lines.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	n     int
}

func newRingBuffer(n int) *ringBuffer { return &ringBuffer{n: n} }

func (r *ringBuffer) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.n {
		r.lines = r.lines[len(r.lines)-r.n:]
	}
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := ""
	for i, l := range r.lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	if out == "" {
		return "(no output)"
	}
	return out
}
