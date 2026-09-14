package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/ids"
)

// How a metered call arose.
const (
	// UsageExec is a headless one-shot run (Adapter.Execute).
	UsageExec = "exec"
	// UsageDelegation is a serial worker run inside a master's container.
	UsageDelegation = "delegation"
	// UsageManual is a figure the user entered themselves, so a subscription
	// agent's spend can be tracked even though no API reports it.
	UsageManual = "manual"
)

// UsageEvent is one metered call (§D26).
//
// Priced separates "this model has no price in the table" from "this call cost
// nothing". Folding the first into $0.00 would make the Usage view quietly
// under-report spend, which is the one thing a cost view must never do.
type UsageEvent struct {
	ID           string  `json:"id"`
	TS           string  `json:"ts"`
	ProjectID    string  `json:"project_id,omitempty"`
	ContainerID  string  `json:"container_id,omitempty"`
	AgentID      string  `json:"agent_id,omitempty"`
	Provider     string  `json:"provider"`
	AccountID    string  `json:"provider_account_id,omitempty"`
	Model        string  `json:"model,omitempty"`
	Kind         string  `json:"kind"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Priced       bool    `json:"priced"`
}

// RecordUsage writes one metered call.
func (s *Store) RecordUsage(ctx context.Context, u UsageEvent) (UsageEvent, error) {
	if u.ID == "" {
		u.ID = ids.New(ids.Usage)
	}
	if u.TS == "" {
		u.TS = ids.Now()
	}
	if u.Kind == "" {
		u.Kind = UsageExec
	}
	if u.Provider == "" {
		return UsageEvent{}, fmt.Errorf("store: usage needs a provider")
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_events
		   (id, ts, project_id, container_id, agent_id, provider, provider_account_id,
		    model, kind, input_tokens, output_tokens, cost_usd, priced)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.TS, nullable(u.ProjectID), nullable(u.ContainerID), nullable(u.AgentID),
		u.Provider, nullable(u.AccountID), u.Model, u.Kind,
		u.InputTokens, u.OutputTokens, u.CostUSD, boolInt(u.Priced))
	if err != nil {
		return UsageEvent{}, fmt.Errorf("store: record usage: %w", err)
	}
	return u, nil
}

// UsageQuery selects and groups metered calls.
type UsageQuery struct {
	// Since and Until are RFC 3339; empty means unbounded. Timestamps are
	// stored as text in a format that sorts chronologically, so a string
	// comparison is a time comparison.
	Since string
	Until string
	// ProjectID, AgentID and Provider narrow the set.
	ProjectID string
	AgentID   string
	Provider  string
}

// UsageGroup is one row of an aggregate.
type UsageGroup struct {
	// Key is the grouping value: a provider, an agent id, a model, a project.
	Key string `json:"key"`
	// Label is a human-readable rendering of Key, filled in by the API layer
	// where it has the names to do it.
	Label        string  `json:"label,omitempty"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	// UnpricedTokens is what the cost figure does NOT cover: tokens spent on
	// a model with no price, or on a subscription where per-token pricing does
	// not exist. Shown beside the cost rather than folded into it.
	UnpricedTokens int64 `json:"unpriced_tokens"`
}

// usageGroupColumns maps a caller-facing grouping name onto a column. The map
// is the allowlist: a grouping the caller invents is rejected rather than
// interpolated into SQL.
var usageGroupColumns = map[string]string{
	"provider":  "provider",
	"agent":     "COALESCE(agent_id,'')",
	"account":   "COALESCE(provider_account_id,'')",
	"model":     "COALESCE(model,'')",
	"project":   "COALESCE(project_id,'')",
	"container": "COALESCE(container_id,'')",
	"kind":      "kind",
}

// UsageGroupings lists the accepted values of group_by.
func UsageGroupings() []string {
	out := make([]string, 0, len(usageGroupColumns))
	for k := range usageGroupColumns {
		out = append(out, k)
	}
	return out
}

// AggregateUsage totals metered calls, grouped.
func (s *Store) AggregateUsage(ctx context.Context, q UsageQuery, groupBy string) ([]UsageGroup, error) {
	col, ok := usageGroupColumns[groupBy]
	if !ok {
		return nil, fmt.Errorf("store: %q is not a usage grouping (want one of %v)",
			groupBy, UsageGroupings())
	}
	where, args := q.clause()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+col+` AS k,
		        count(*),
		        COALESCE(sum(input_tokens),0),
		        COALESCE(sum(output_tokens),0),
		        COALESCE(sum(cost_usd),0),
		        COALESCE(sum(CASE WHEN priced = 0 THEN input_tokens + output_tokens ELSE 0 END),0)
		 FROM usage_events `+where+`
		 GROUP BY k
		 ORDER BY sum(cost_usd) DESC, sum(input_tokens + output_tokens) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageGroup
	for rows.Next() {
		var g UsageGroup
		if err := rows.Scan(&g.Key, &g.Calls, &g.InputTokens, &g.OutputTokens,
			&g.CostUSD, &g.UnpricedTokens); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UsageBucket is one point on the usage sparkline.
type UsageBucket struct {
	// Start is the RFC 3339 start of the bucket.
	Start        string  `json:"start"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// UsageSeries buckets metered calls into fixed-width intervals.
//
// Bucketing happens in SQLite via strftime so a long window does not stream
// every row into the daemon to be counted in Go.
func (s *Store) UsageSeries(ctx context.Context, q UsageQuery, bucketMinutes int) ([]UsageBucket, error) {
	if bucketMinutes <= 0 {
		bucketMinutes = 60
	}
	where, args := q.clause()

	// Round each timestamp down to the bucket by converting to a unix second
	// count, dividing, and converting back. datetime() returns
	// "YYYY-MM-DD HH:MM:SS"; the space becomes a T so the result is RFC 3339
	// like every other timestamp Aurium emits.
	secs := bucketMinutes * 60
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT replace(datetime((strftime('%%s', ts) / %d) * %d, 'unixepoch'), ' ', 'T') || 'Z' AS b,
		        count(*),
		        COALESCE(sum(input_tokens),0),
		        COALESCE(sum(output_tokens),0),
		        COALESCE(sum(cost_usd),0)
		 FROM usage_events %s
		 GROUP BY b ORDER BY b`, secs, secs, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageBucket
	for rows.Next() {
		var b UsageBucket
		if err := rows.Scan(&b.Start, &b.Calls, &b.InputTokens, &b.OutputTokens, &b.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UsageTotals is the headline figure.
func (s *Store) UsageTotals(ctx context.Context, q UsageQuery) (UsageGroup, error) {
	where, args := q.clause()
	var g UsageGroup
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*),
		        COALESCE(sum(input_tokens),0),
		        COALESCE(sum(output_tokens),0),
		        COALESCE(sum(cost_usd),0),
		        COALESCE(sum(CASE WHEN priced = 0 THEN input_tokens + output_tokens ELSE 0 END),0)
		 FROM usage_events `+where, args...).
		Scan(&g.Calls, &g.InputTokens, &g.OutputTokens, &g.CostUSD, &g.UnpricedTokens)
	return g, err
}

func (q UsageQuery) clause() (string, []any) {
	var conds []string
	var args []any
	if q.Since != "" {
		conds = append(conds, "ts >= ?")
		args = append(args, q.Since)
	}
	if q.Until != "" {
		conds = append(conds, "ts < ?")
		args = append(args, q.Until)
	}
	if q.ProjectID != "" {
		conds = append(conds, "project_id = ?")
		args = append(args, q.ProjectID)
	}
	if q.AgentID != "" {
		conds = append(conds, "agent_id = ?")
		args = append(args, q.AgentID)
	}
	if q.Provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, q.Provider)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
