package contextengine

import (
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

// TestShowProjection prints a full projection so its prose can be reviewed by
// a human. This file is read by a language model on every turn, so the wording
// is part of the product, not an implementation detail.
// Run: go test ./internal/contextengine/ -run TestShowProjection -v
func TestShowProjection(t *testing.T) {
	t.Log("\n" + string(Render(ProjectionData{
		Container: store.Container{
			ID: "c_01J9Z3KQ7V", Branch: "implement-oauth",
			ParentBranch: "implement-auth", BaseSHA: "9f8e7d6c5b4a3210",
		},
		Task:        store.Task{Title: "Implement OAuth"},
		ParentDesc:  "`implement-auth` (c_01H8Y2)",
		Stale:       true,
		SyncStatus:  "stale — parent is 3 commit(s) ahead",
		Objective:   "Add OAuth 2.0 with PKCE to the login flow, behind a feature flag.",
		Constraints: "- No new runtime dependencies.\n- The existing session cookie must keep working.",
		Decisions: []Item{{
			Key: "decisions/2026-09-07-jwt", Version: 3,
			Content: "Asymmetric signing (RS256). Symmetric was rejected because the mobile client cannot hold the secret.",
		}},
		Discoveries: []Item{{
			Key: "discoveries/auth-flow", Version: 1,
			Content: "The legacy flow double-encodes `state`; anything new must not inherit that.",
		}},
		UnreadMessages: 2,
		Tools: []string{
			"aurium_context_get", "aurium_context_write", "aurium_ipc_inbox",
			"aurium_task_status", "github_create_pull_request",
		},
	})))
}
