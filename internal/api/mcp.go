package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/RhyChaw/aurium/internal/gateway"
	"github.com/RhyChaw/aurium/internal/mcp"
	"github.com/RhyChaw/aurium/internal/store"
)

// maxMCPBody bounds a single JSON-RPC request from a container.
const maxMCPBody = 8 << 20

// handleMCP is the endpoint aurium-mcp forwards to (§4.1, §11.2).
//
// The caller is derived entirely from the bearer token, never from the request
// body. An agent that could name its own container or role in the payload
// could grant itself anything.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		writeError(w, http.StatusServiceUnavailable, "the MCP gateway is not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBody))
	if err != nil {
		writeJSON(w, http.StatusOK, mcp.NewError(nil,
			mcp.Errorf(mcp.CodeParseError, "could not read request: %v", err)))
		return
	}

	var req mcp.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, mcp.NewError(nil,
			mcp.Errorf(mcp.CodeParseError, "invalid JSON: %v", err)))
		return
	}

	caller, err := s.callerFor(r)
	if err != nil {
		unauthorized(w)
		return
	}

	resp := s.Gateway.Handle(r.Context(), caller, &req)
	if resp == nil {
		// A notification. 202 with no body says "received, nothing to say".
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// JSON-RPC errors travel inside a 200: the HTTP transport succeeded, and
	// a non-2xx would make the shim report a transport failure instead of
	// handing the agent the error it can act on.
	writeJSON(w, http.StatusOK, resp)
}

// callerFor resolves the authenticated identity behind a request.
func (s *Server) callerFor(r *http.Request) (gateway.Caller, error) {
	info, ok := tokenFrom(r)
	if !ok {
		// The host token: a human at the CLI or the dashboard.
		return gateway.Caller{Human: true}, nil
	}

	c := gateway.Caller{Token: info}
	c.ContainerID = info.ContainerID
	c.AgentID = info.AgentID

	if info.ContainerID != "" {
		container, err := s.App.Store.GetContainer(r.Context(), info.ContainerID)
		if err != nil {
			return gateway.Caller{}, err
		}
		c.ProjectID = container.ProjectID

		// The role comes from the database, not the token or the request, so
		// an agent cannot promote itself to master.
		agents, err := s.App.Store.ListLiveAgents(r.Context(), info.ContainerID)
		if err == nil {
			for _, a := range agents {
				if info.AgentID == "" || a.ID == info.AgentID {
					c.AgentID, c.Role = a.ID, a.Role
					break
				}
			}
		}
		if c.Role == "" {
			c.Role = store.RolePrimary
		}
	}
	return c, nil
}
