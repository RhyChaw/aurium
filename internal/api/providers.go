package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/RhyChaw/aurium/internal/providers"
	"github.com/RhyChaw/aurium/internal/store"
)

// Provider routes are host-only. An agent that could connect an account could
// hand itself a credential; one that could list them learns what accounts
// exist, which is not its business either.
func (s *Server) hostOnly(w http.ResponseWriter, r *http.Request) bool {
	if info, ok := tokenFrom(r); ok && info.ContainerID != "" {
		writeError(w, http.StatusForbidden, "this is a host-only route")
		return false
	}
	return true
}

// listProviders returns connected accounts. Each carries a keyring reference
// and never a credential, and there is deliberately no route that turns one
// back into a secret.
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	accounts, err := s.App.Store.ListProviderAccounts(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	if accounts == nil {
		accounts = []store.ProviderAccount{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (s *Server) connectProvider(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	var req providers.ConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	acct, err := s.App.Providers.Connect(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDuplicate):
			writeError(w, http.StatusConflict, err.Error())
		default:
			// Every remaining failure here is the user's to fix: a key for the
			// wrong provider, an unset variable, a CLI that is not logged in.
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

func (s *Server) disconnectProvider(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	if err := s.App.Providers.Disconnect(r.Context(), r.PathValue("account")); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// detectProviders reports what this host already offers, so connecting is
// usually one click rather than a trip to a browser. It names the variables
// that are set and never their values.
func (s *Server) detectProviders(w http.ResponseWriter, r *http.Request) {
	if !s.hostOnly(w, r) {
		return
	}
	found, err := s.App.Providers.Detect(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":  found,
		"keyring":    s.App.Secrets.Available(),
		"auth_kinds": []string{store.AuthAPIKey, store.AuthSubscription},
	})
}
