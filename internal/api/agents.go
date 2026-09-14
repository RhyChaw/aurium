package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/store"
)

// getAgent is what the chat pane's header needs: the agent, its container, and
// which account it is billed against.
func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.App.Store.GetAgent(ctx, r.PathValue("agent"))
	if err != nil {
		storeError(w, err)
		return
	}
	if !s.mayReachAgent(r, a) {
		writeError(w, http.StatusForbidden, "this token is scoped to a different container")
		return
	}

	c, err := s.App.Store.GetContainer(ctx, a.ContainerID)
	if err != nil {
		storeError(w, err)
		return
	}

	out := map[string]any{
		"agent": a, "container": c, "state": StateOf(a.Status),
		"thinking": s.App.Manager.IsThinking(a.ID),
		"can_chat": s.App.Manager.CanConverse(a.Adapter),
	}
	if a.ProviderAccountID != "" {
		if acct, err := s.App.Store.GetProviderAccount(ctx, a.ProviderAccountID); err == nil {
			// The account, never its credential: GetProviderAccount returns a
			// reference and nothing that can be replayed.
			out["account"] = acct
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// agentMessages returns an agent's transcript without delivering anything.
//
// Reading over an agent's shoulder must not consume its inbox, which is why
// this uses History rather than Inbox.
func (s *Server) agentMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.App.Store.GetAgent(ctx, r.PathValue("agent"))
	if err != nil {
		storeError(w, err)
		return
	}
	if !s.mayReachAgent(r, a) {
		writeError(w, http.StatusForbidden, "this token is scoped to a different container")
		return
	}

	msgs, err := s.App.IPC.History(ctx, a.ID, int(atoiOr(r.URL.Query().Get("limit"), 200)))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// sendToAgent is the chat pane's composer.
//
// Two things happen, and both matter. The message is recorded as IPC, so it is
// in the agent's inbox whether or not it is looking; and for an interactive
// agent it is also typed into the session, so an agent sitting at a REPL
// prompt sees it now rather than at its next tool call.
func (s *Server) sendToAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content  string `json:"content"`
		Type     string `json:"type"`
		Priority string `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "a message needs content")
		return
	}
	if body.Type == "" {
		body.Type = ipc.TypeRequest
	}

	ctx := r.Context()
	a, err := s.App.Store.GetAgent(ctx, r.PathValue("agent"))
	if err != nil {
		storeError(w, err)
		return
	}
	if !s.mayReachAgent(r, a) {
		writeError(w, http.StatusForbidden, "this token is scoped to a different container")
		return
	}
	c, err := s.App.Store.GetContainer(ctx, a.ContainerID)
	if err != nil {
		storeError(w, err)
		return
	}

	// A message from the dashboard has no sending agent. That absence is how
	// every reader tells a human's words from another agent's.
	m, err := s.App.IPC.Send(ctx, ipc.Message{
		ProjectID: c.ProjectID,
		To:        ipc.Addr{AgentID: a.ID, ContainerID: c.ID},
		Type:      body.Type,
		Priority:  body.Priority,
		Content:   body.Content,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	typed := s.typeIntoSession(r, c, a, body.Content)

	// Where there is no live session to type into, run the turn headlessly so
	// the chat pane actually answers. On the `local` driver — the one that
	// works with no Docker, which is most first runs — this is the only path
	// that reaches the model at all.
	//
	// It runs detached from the request: a turn takes minutes, and an HTTP
	// call that blocks that long is one the browser abandons. The reply
	// reaches the dashboard over SSE either way.
	thinking := false
	if !typed && s.App.Manager.CanConverse(a.Adapter) {
		thinking = true
		go func() {
			// Not the request context: that is cancelled the moment this
			// handler returns, which would kill every turn at birth.
			ctx := context.WithoutCancel(r.Context())
			if err := s.App.Manager.Converse(ctx, a.ID, body.Content); err != nil {
				s.Log.Warn("api: conversation turn", "agent", a.ID, "err", err)
			}
		}()
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"message": m,
		// typed: it went into a live session. thinking: a headless turn is
		// running and a reply will arrive. Neither: it is in the inbox and
		// will be read when the agent next looks.
		"typed":    typed,
		"thinking": thinking,
	})
}

// typeIntoSession sends the text to an interactive agent's tmux session.
//
// A driver without session supervision has nothing to type into, and that is
// not an error: the message is in the inbox and will be read. The bool tells
// the dashboard which of the two happened, so it can say "queued" rather than
// implying the agent has already seen it.
func (s *Server) typeIntoSession(r *http.Request, c store.Container, a store.Agent, text string) bool {
	drv, err := s.App.Manager.Drivers.Get(c.Driver)
	if err != nil || !drv.Capabilities().Tmux || c.RuntimeID == "" {
		return false
	}
	session := &agent.Session{Driver: drv, ContainerID: c.RuntimeID, Name: a.TmuxSession}
	if err := session.SendText(r.Context(), text); err != nil {
		s.Log.Warn("api: typing into agent session", "agent", a.ID, "err", err)
		return false
	}
	_ = s.App.Events.Emit(r.Context(), events.Event{
		Type: events.AgentActive, Actor: events.ActorHuman,
		ProjectID: c.ProjectID, ContainerID: c.ID, AgentID: a.ID,
		Payload: map[string]any{"source": "dashboard"},
	})
	return true
}

// mayReachAgent enforces the container scope for agent-addressed routes.
//
// authenticate() only checks the {container} path value, and these routes carry
// an {agent} instead; without this an agent token could read any other agent's
// transcript, which is the kind of hole that opens when an authorisation check
// keys off a URL shape rather than the object.
func (s *Server) mayReachAgent(r *http.Request, a store.Agent) bool {
	info, ok := tokenFrom(r)
	if !ok || info.ContainerID == "" {
		return true // the host token
	}
	return a.ContainerID == info.ContainerID
}
