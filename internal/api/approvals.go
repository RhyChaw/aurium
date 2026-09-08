package api

import (
	"encoding/json"
	"net/http"

	"github.com/RhyChaw/aurium/internal/gateway"
)

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		writeJSON(w, http.StatusOK, map[string]any{"approvals": []any{}})
		return
	}
	// Default to pending: the inbox a human actually wants is "what needs me".
	status := r.URL.Query().Get("status")
	if status == "" {
		status = gateway.ApprovalPending
	}
	if status == "all" {
		status = ""
	}

	list, err := s.Gateway.ListApprovals(r.Context(), status)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		writeError(w, http.StatusServiceUnavailable, "the MCP gateway is not configured")
		return
	}

	var body struct {
		Decision string `json:"decision"`
		By       string `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.By == "" {
		body.By = "human"
	}

	ap, err := s.Gateway.Decide(r.Context(), r.PathValue("approval"), body.Decision, body.By)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ap)
}
