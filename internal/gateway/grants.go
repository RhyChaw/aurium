package gateway

import (
	"context"
	"fmt"
	"path"

	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/store"
)

// Modes (§10.4). Note these are deliberately a different type from the context
// engine's permissions: capabilities are allow/deny/approve, context is
// read/append/propose/write, and conflating them would be a security bug.
type Mode string

const (
	ModeAllow   Mode = "allow"
	ModeDeny    Mode = "deny"
	ModeApprove Mode = "approve"
)

// Subject is who is calling a capability.
type Subject struct {
	AgentID     string
	Role        string
	ContainerID string
	ProjectID   string
}

// Grant is a stored capability permission.
type Grant struct {
	ID             string `json:"id"`
	IntegrationID  string `json:"integration_id"`
	CapabilityGlob string `json:"capability_glob"`
	SubjectType    string `json:"subject_type"`
	SubjectID      string `json:"subject_id"`
	Mode           Mode   `json:"mode"`
	CreatedBy      string `json:"created_by"`
	CreatedAt      string `json:"created_at"`
}

// AddGrant stores a capability grant.
func (g *Gateway) AddGrant(ctx context.Context, gr Grant) (Grant, error) {
	if gr.Mode != ModeAllow && gr.Mode != ModeDeny && gr.Mode != ModeApprove {
		return Grant{}, fmt.Errorf("gateway: %q is not a mode (want allow, deny or approve)", gr.Mode)
	}
	if gr.ID == "" {
		gr.ID = ids.New(ids.Grant)
	}
	if gr.CreatedAt == "" {
		gr.CreatedAt = ids.Now()
	}
	if gr.CreatedBy == "" {
		gr.CreatedBy = "human"
	}

	_, err := g.Store.DB().ExecContext(ctx,
		`INSERT INTO grants (id, integration_id, capability_glob, subject_type, subject_id, mode, created_by, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		gr.ID, gr.IntegrationID, gr.CapabilityGlob, gr.SubjectType, gr.SubjectID,
		string(gr.Mode), gr.CreatedBy, gr.CreatedAt)
	if err != nil {
		return Grant{}, fmt.Errorf("gateway: add grant: %w", err)
	}
	return gr, nil
}

// Resolve returns the mode for a subject calling a capability (§10.4).
//
//	subjects, most specific first: agent -> role -> container -> project
//	within a level, the first matching glob wins; otherwise fall through
//	no match at any level: DENY
//
// Default deny matters more here than anywhere else in the system: a
// capability is somebody's real GitHub account, or their production database.
func (g *Gateway) Resolve(ctx context.Context, s Subject, integrationID, capability string) (Mode, string, error) {
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
		mode, glob, found, err := g.matchAtLevel(ctx, integrationID, capability, lvl.typ, lvl.id)
		if err != nil {
			return ModeDeny, "", err
		}
		if found {
			return mode, fmt.Sprintf("%s:%s matched %q", lvl.typ, lvl.id, glob), nil
		}
	}
	return ModeDeny, "no grant matched", nil
}

// matchAtLevel finds the first matching grant at one subject level.
//
// Within a level, deny wins over anything else. A user who writes an explicit
// deny alongside a broad allow means the deny.
func (g *Gateway) matchAtLevel(ctx context.Context, integrationID, capability, subjectType, subjectID string) (Mode, string, bool, error) {
	rows, err := g.Store.DB().QueryContext(ctx,
		`SELECT capability_glob, mode FROM grants
		 WHERE integration_id = ? AND subject_type = ? AND subject_id = ?
		 ORDER BY id`,
		integrationID, subjectType, subjectID)
	if err != nil {
		return ModeDeny, "", false, err
	}
	defer rows.Close()

	var (
		bestMode Mode
		bestGlob string
		found    bool
	)
	for rows.Next() {
		var glob, mode string
		if err := rows.Scan(&glob, &mode); err != nil {
			return ModeDeny, "", false, err
		}
		if !matchCapability(glob, capability) {
			continue
		}
		if !found {
			bestMode, bestGlob, found = Mode(mode), glob, true
		}
		// An explicit deny at this level is final.
		if Mode(mode) == ModeDeny {
			return ModeDeny, glob, true, nil
		}
		// Otherwise the more restrictive of the matches so far wins, so a
		// narrow `approve` is not overridden by a broad `allow`.
		if Mode(mode) == ModeApprove && bestMode == ModeAllow {
			bestMode, bestGlob = ModeApprove, glob
		}
	}
	return bestMode, bestGlob, found, rows.Err()
}

// matchCapability matches a capability name against a glob.
func matchCapability(glob, capability string) bool {
	if glob == "*" || glob == "**" {
		return true
	}
	ok, err := path.Match(glob, capability)
	return err == nil && ok
}

// SeedGrants writes the §10.4 defaults for a newly connected integration.
//
//	project: low -> allow, medium -> allow, high -> approve
//	worker:  medium -> approve, high -> deny
//
// The worker row is the important one. A delegated worker is running an
// unreviewed prompt from another agent, so it gets less trust than the agent
// a human started, not the same.
func (g *Gateway) SeedGrants(ctx context.Context, integrationID, projectID string, capabilities []Capability) error {
	for _, c := range capabilities {
		mode := ModeAllow
		if c.Risk == RiskHigh {
			mode = ModeApprove
		}
		if _, err := g.AddGrant(ctx, Grant{
			IntegrationID: integrationID, CapabilityGlob: c.Name,
			SubjectType: "project", SubjectID: projectID, Mode: mode,
			CreatedBy: "aurium (default)",
		}); err != nil {
			return err
		}
	}

	for _, seed := range []struct {
		risk Risk
		mode Mode
	}{{RiskMedium, ModeApprove}, {RiskHigh, ModeDeny}} {
		for _, c := range capabilities {
			if c.Risk != seed.risk {
				continue
			}
			if _, err := g.AddGrant(ctx, Grant{
				IntegrationID: integrationID, CapabilityGlob: c.Name,
				SubjectType: "role", SubjectID: store.RoleWorker, Mode: seed.mode,
				CreatedBy: "aurium (default)",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// RevokeForContainer denies an integration to one container, which is §62's
// "revoke C-185 without disconnecting GitHub".
func (g *Gateway) RevokeForContainer(ctx context.Context, integrationID, containerID, by string) error {
	_, err := g.AddGrant(ctx, Grant{
		IntegrationID: integrationID, CapabilityGlob: "*",
		SubjectType: "container", SubjectID: containerID, Mode: ModeDeny,
		CreatedBy: by,
	})
	return err
}

// ListGrants returns an integration's grants.
func (g *Gateway) ListGrants(ctx context.Context, integrationID string) ([]Grant, error) {
	rows, err := g.Store.DB().QueryContext(ctx,
		`SELECT id, integration_id, capability_glob, subject_type, subject_id, mode, created_by, created_at
		 FROM grants WHERE integration_id = ? ORDER BY id`, integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var gr Grant
		var mode string
		if err := rows.Scan(&gr.ID, &gr.IntegrationID, &gr.CapabilityGlob,
			&gr.SubjectType, &gr.SubjectID, &mode, &gr.CreatedBy, &gr.CreatedAt); err != nil {
			return nil, err
		}
		gr.Mode = Mode(mode)
		out = append(out, gr)
	}
	return out, rows.Err()
}
