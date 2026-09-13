package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// maxFrame bounds one JSON-RPC message. Tool results carrying file contents or
// search output routinely exceed bufio's 64 KB default, which would truncate
// them into unparseable JSON.
const maxFrame = 8 << 20

// Handler processes one request and returns a response, or nil for a
// notification.
type Handler interface {
	Handle(ctx context.Context, req *Request) *Response
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, req *Request) *Response

func (f HandlerFunc) Handle(ctx context.Context, req *Request) *Response { return f(ctx, req) }

// Serve reads newline-delimited JSON-RPC from r and writes responses to w.
//
// A malformed frame produces a parse error and the loop continues. Killing the
// connection would take down the agent's only channel to Aurium over one bad
// line.
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrame)

	// stdout may be written from more than one goroutine once a handler
	// notifies; interleaved frames would corrupt every message.
	var mu sync.Mutex
	write := func(resp *Response) error {
		body, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		_, err = fmt.Fprintf(w, "%s\n", body)
		return err
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := write(NewError(nil, Errorf(CodeParseError, "invalid JSON: %v", err))); err != nil {
				return err
			}
			continue
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			if err := write(NewError(req.ID, Errorf(CodeInvalidRequest,
				"unsupported jsonrpc version %q", req.JSONRPC))); err != nil {
				return err
			}
			continue
		}

		resp := h.Handle(ctx, &req)
		if resp == nil {
			continue // notification
		}
		if err := write(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}
