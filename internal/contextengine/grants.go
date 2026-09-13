package contextengine

import (
	"context"
	"fmt"
	"path"

	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// Subject is who is asking. Resolution walks agent → role → container →
// project, most specific first (§8.3).
type Subject struct {
	AgentID     string
	Role        string
	ContainerID string
	ProjectID   string
	// Human short-circuits every check. A person at the CLI owns the machine
	// and the repository; making them fight the permission model would be
	// theatre, not security.
	Human bool
}

// Grant is a stored permission.
type Grant struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	ScopeID     string `json:"scope_id"`
	KeyGlob     string `json:"key_glob"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Perm        Perm   `json:"perm"`
}

// GrantPerm stores a permission.
func (e *Engine) GrantPerm(ctx context.Context, g Grant) (Grant, error) {
	if g.ID == "" {
		g.ID = ids.New(ids.Grant)
	}
	_, err := e.Store.DB().ExecContext(ctx,
		`INSERT INTO context_grants (id, scope, scope_id, key_glob, subject_type, subject_id, perm)
		 VALUES (?,?,?,?,?,?,?)`,
		g.ID, g.Scope, g.ScopeID, g.KeyGlob, g.SubjectType, g.SubjectID, string(g.Perm))
	if err != nil {
		return Grant{}, fmt.Errorf("context: grant %s on %s: %w", g.Perm, g.KeyGlob, err)
	}
	return g, nil
}

// ResolvePerm returns the strongest permission a subject holds on an item.
//
// Default deny: an item with no matching grant is unreadable. That is the
// right default for a system where agents can be adversarial, mistaken, or
// simply reading something they should not have been given.
func (e *Engine) ResolvePerm(ctx context.Context, s Subject, ref Ref) (Perm, error) {
	if s.Human {
		return PermWrite, nil
	}

	// Most specific first. The first subject level that matches at all decides,
	// so a container-level grant can narrow what a project-level grant opened.
	levels := []struct{ typ, id string }{
		{"agent", s.AgentID},
		{"role", s.Role},
		{"container", s.ContainerID},
		{"project", s.ProjectID},
	}

	for _, lvl := range levels {
		if lvl.id == "" {
			continue
		}
		best, found, err := e.bestGrant(ctx, ref, lvl.typ, lvl.id)
		if err != nil {
			return PermNone, err
		}
		if found {
			return best, nil
		}
	}

	// §8.1: a stacked child gets read on its parent chain's container context,
	// so B can see A's decisions without touching A's files. This is walked
	// rather than stored so it stays correct when a container is reparented.
	if ref.Scope == ScopeContainer && s.ContainerID != "" && ref.ScopeID != s.ContainerID {
		ancestor, err := e.isAncestorContainer(ctx, s.ContainerID, ref.ScopeID)
		if err != nil {
			return PermNone, err
		}
		if ancestor {
			return PermRead, nil
		}
	}

	// A container always owns its own context, and an agent its own.
	if ref.Scope == ScopeContainer && ref.ScopeID == s.ContainerID {
		return PermWrite, nil
	}
	if ref.Scope == ScopeAgent && ref.ScopeID == s.AgentID {
		return PermWrite, nil
	}
	return PermNone, nil
}

// bestGrant returns the strongest matching grant at one subject level.
func (e *Engine) bestGrant(ctx context.Context, ref Ref, subjectType, subjectID string) (Perm, bool, error) {
	rows, err := e.Store.DB().QueryContext(ctx,
		`SELECT key_glob, perm FROM context_grants
		 WHERE subject_type = ? AND subject_id = ? AND scope = ?
		   AND (scope_id = ? OR scope_id = '*')`,
		subjectType, subjectID, ref.Scope, ref.ScopeID)
	if err != nil {
		return PermNone, false, err
	}
	defer rows.Close()

	best, found := PermNone, false
	for rows.Next() {
		var glob, perm string
		if err := rows.Scan(&glob, &perm); err != nil {
			return PermNone, false, err
		}
		if !matchKey(glob, ref.Key) {
			continue
		}
		found = true
		if rank[Perm(perm)] > rank[best] {
			best = Perm(perm)
		}
	}
	return best, found, rows.Err()
}

// matchKey matches a key against a glob. `**` crosses path separators,
// `*` does not — so `decisions/*` covers `decisions/jwt` but not
// `decisions/auth/jwt`, which is what makes a narrow grant actually narrow.
func matchKey(glob, key string) bool {
	if glob == "**" || glob == "*" && !containsSlash(key) {
		return true
	}
	if suffix, ok := cutSuffix(glob, "/**"); ok {
		return key == suffix || hasPrefix(key, suffix+"/")
	}
	ok, err := path.Match(glob, key)
	return err == nil && ok
}

// isAncestorContainer reports whether ancestor appears in target's parent
// chain, with a depth cap so corrupt data cannot loop forever.
func (e *Engine) isAncestorContainer(ctx context.Context, child, ancestor string) (bool, error) {
	seen := map[string]bool{}
	current := child

	for depth := 0; depth < 64; depth++ {
		c, err := e.Store.GetContainer(ctx, current)
		if err != nil {
			return false, nil // a broken chain grants nothing
		}
		if c.ParentContainerID == "" {
			return false, nil
		}
		if c.ParentContainerID == ancestor {
			return true, nil
		}
		if seen[c.ParentContainerID] {
			return false, nil // cycle
		}
		seen[c.ParentContainerID] = true
		current = c.ParentContainerID
	}
	return false, nil
}

// SeedDefaults writes the §8.3 default grants for a project.
func (e *Engine) SeedDefaults(ctx context.Context, projectID string) error {
	defaults := []Grant{
		// primary and master: read everything, propose architecture and
		// conventions, append decisions. They may not silently rewrite the
		// project's shared understanding.
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "**", SubjectType: "role", SubjectID: store.RolePrimary, Perm: PermRead},
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "architecture/**", SubjectType: "role", SubjectID: store.RolePrimary, Perm: PermPropose},
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "conventions/**", SubjectType: "role", SubjectID: store.RolePrimary, Perm: PermPropose},
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "decisions/**", SubjectType: "role", SubjectID: store.RolePrimary, Perm: PermAppend},

		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "**", SubjectType: "role", SubjectID: store.RoleMaster, Perm: PermRead},
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "architecture/**", SubjectType: "role", SubjectID: store.RoleMaster, Perm: PermPropose},
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "conventions/**", SubjectType: "role", SubjectID: store.RoleMaster, Perm: PermPropose},
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "decisions/**", SubjectType: "role", SubjectID: store.RoleMaster, Perm: PermAppend},

		// workers: read only at project scope. A delegated worker is doing a
		// bounded subtask and has no business reshaping the project.
		{Scope: ScopeProject, ScopeID: projectID, KeyGlob: "**", SubjectType: "role", SubjectID: store.RoleWorker, Perm: PermRead},
	}

	for _, g := range defaults {
		if _, err := e.GrantPerm(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

// Require checks a permission and returns a typed error naming what was
// missing, so an agent is told how to proceed rather than just refused.
func (e *Engine) Require(ctx context.Context, s Subject, ref Ref, want Perm, action string) error {
	have, err := e.ResolvePerm(ctx, s, ref)
	if err != nil {
		return err
	}
	if !have.Allows(want) {
		return &ErrPermission{Ref: ref, Want: want, Have: have, Reason: action}
	}
	return nil
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func cutSuffix(s, suffix string) (string, bool) {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return s, false
}
