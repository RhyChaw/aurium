package usage

import (
	"context"
	"fmt"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/store"
)

// Recorded is the event type a metered call emits, so the dashboard's usage
// figures move live rather than on a poll.
const Recorded = "usage.recorded"

// Call is one metered call, as the caller knows it.
type Call struct {
	ProjectID    string
	ContainerID  string
	AgentID      string
	Adapter      string
	Model        string
	Kind         string
	InputTokens  int64
	OutputTokens int64
	// AccountID is the provider account billed, when it is known.
	AccountID string
	// CostUSD is the provider's own figure for this call. It beats the price
	// table when HasCost is set, because the provider knows the account's
	// real rates — discounts, cache tiers, batch pricing — and Aurium does not.
	CostUSD float64
	HasCost bool
}

// Recorder writes metered calls and announces them.
type Recorder struct {
	Store  *store.Store
	Events *events.Bus
}

// New returns a recorder.
func New(s *store.Store, e *events.Bus) *Recorder { return &Recorder{Store: s, Events: e} }

// Record meters one call.
//
// A call reporting no tokens is dropped rather than stored as a zero: the
// adapters that do not report usage would otherwise fill the table with rows
// that say nothing, and "42 calls, 0 tokens" reads as a bug in the meter
// rather than as an absence of data.
func (r *Recorder) Record(ctx context.Context, c Call) (store.UsageEvent, error) {
	if r == nil || r.Store == nil {
		return store.UsageEvent{}, nil
	}
	if c.InputTokens <= 0 && c.OutputTokens <= 0 {
		return store.UsageEvent{}, nil
	}

	provider := ProviderFor(c.Adapter)
	if provider == "" {
		// An adapter with no provider spends nothing Aurium can attribute.
		return store.UsageEvent{}, nil
	}

	cost, priced := Cost(c.Model, c.InputTokens, c.OutputTokens)
	if c.HasCost {
		cost, priced = c.CostUSD, true
	}
	kind := c.Kind
	if kind == "" {
		kind = store.UsageExec
	}

	// A subscription seat has no per-token price, whatever the model is worth
	// on the API. Pricing it anyway would invent a bill the user will never
	// receive, so the tokens are recorded and the cost is not.
	if c.AccountID != "" {
		if acct, err := r.Store.GetProviderAccount(ctx, c.AccountID); err == nil &&
			acct.AuthKind == store.AuthSubscription {
			cost, priced = 0, false
		}
	}

	u, err := r.Store.RecordUsage(ctx, store.UsageEvent{
		ProjectID: c.ProjectID, ContainerID: c.ContainerID, AgentID: c.AgentID,
		Provider: provider, AccountID: c.AccountID, Model: c.Model, Kind: kind,
		InputTokens: c.InputTokens, OutputTokens: c.OutputTokens,
		CostUSD: cost, Priced: priced,
	})
	if err != nil {
		return store.UsageEvent{}, fmt.Errorf("usage: %w", err)
	}

	if r.Events != nil {
		// Best effort: a metered call is a record of something that already
		// happened, and failing to announce it must not fail the call.
		_ = r.Events.Emit(ctx, events.Event{
			Type: Recorded, Actor: events.ActorDaemon,
			ProjectID: c.ProjectID, ContainerID: c.ContainerID, AgentID: c.AgentID,
			Payload: map[string]any{
				"provider": provider, "model": c.Model, "kind": kind,
				"input_tokens": c.InputTokens, "output_tokens": c.OutputTokens,
				"cost_usd": cost, "priced": priced,
			},
		})
	}
	return u, nil
}
