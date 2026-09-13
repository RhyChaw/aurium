// Package mcp implements the Model Context Protocol over JSON-RPC 2.0.
//
// Hand-rolled rather than taken from a library: the gateway is the security
// boundary between agents and every integration Aurium holds credentials for
// (§10), and the framing is a few hundred lines. A dependency there would be
// a fast-moving component in the one place that must not surprise anyone.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this build speaks.
const ProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes, plus the MCP-relevant ones.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	// CodeMethodNotFound is what a call to a tool the caller cannot see
	// returns. §10.1 requires ungranted tools to be invisible, so "not
	// granted" and "does not exist" must be indistinguishable.
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request is a JSON-RPC 2.0 request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether no response is expected.
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: %d %s", e.Code, e.Message) }

// Errorf builds an error response payload.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// NewResponse builds a success response.
func NewResponse(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

// NewError builds an error response.
func NewError(id json.RawMessage, err *Error) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: err}
}

// ---- MCP types ----

// Tool is a tool advertised to a client.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema is JSON Schema. Agents choose arguments from it, so a vague
	// schema produces vague calls.
	InputSchema map[string]any `json:"inputSchema"`
}

// Content is one piece of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextContent builds a text content block.
func TextContent(s string) Content { return Content{Type: "text", Text: s} }

// ToolResult is what tools/call returns.
type ToolResult struct {
	Content []Content `json:"content"`
	// IsError marks a tool-level failure. It is distinct from a JSON-RPC
	// error: the call succeeded, the tool reported a problem, and the agent
	// should read it and adapt rather than treat the transport as broken.
	IsError bool `json:"isError,omitempty"`
}

// TextResult builds a successful text result.
func TextResult(s string) *ToolResult {
	return &ToolResult{Content: []Content{TextContent(s)}}
}

// ErrorResult builds a tool-level failure the agent can read and act on.
func ErrorResult(format string, args ...any) *ToolResult {
	return &ToolResult{Content: []Content{TextContent(fmt.Sprintf(format, args...))}, IsError: true}
}

// JSONResult renders a value as pretty JSON for an agent to read.
func JSONResult(v any) *ToolResult {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrorResult("could not encode result: %v", err)
	}
	return TextResult(string(body))
}

// InitializeResult is the response to `initialize`.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions,omitempty"`
}

// Capabilities advertises what the server supports.
type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// ToolsCapability says whether the tool list can change during a session. It
// can: a grant added or revoked while an agent runs changes what it may see.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// ServerInfo identifies the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolsListResult is the response to `tools/list`.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// CallParams are the parameters of `tools/call`.
type CallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}
