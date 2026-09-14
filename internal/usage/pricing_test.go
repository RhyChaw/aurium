package usage

import (
	"context"
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

// "gpt-5" is a prefix of "gpt-5-mini". Taking the first match rather than the
// longest would price mini calls at five times what they cost.
func TestLongestPrefixWins(t *testing.T) {
	mini, ok := Lookup("gpt-5-mini-2026-01-01")
	if !ok {
		t.Fatal("a dated snapshot must match its family")
	}
	full, _ := Lookup("gpt-5")
	if mini.InputPMT >= full.InputPMT {
		t.Fatalf("gpt-5-mini priced as gpt-5: %v vs %v", mini, full)
	}
}

func TestUnknownModelIsNotFree(t *testing.T) {
	cost, priced := Cost("some-model-nobody-has-heard-of", 1000, 1000)
	if priced {
		t.Fatal("an unknown model must not report a price")
	}
	if cost != 0 {
		t.Fatalf("an unpriced call must carry no cost, got %v", cost)
	}
}

func TestCostArithmetic(t *testing.T) {
	// 1M input at $15 + 1M output at $75.
	cost, priced := Cost("claude-opus-5", 1_000_000, 1_000_000)
	if !priced || cost != 90 {
		t.Fatalf("Cost = %v (priced=%v), want 90", cost, priced)
	}
}

func TestProviderForAdapter(t *testing.T) {
	for adapter, want := range map[string]string{
		"claude": "anthropic", "codex": "openai", "shell": "", "custom": "",
	} {
		if got := ProviderFor(adapter); got != want {
			t.Errorf("ProviderFor(%q) = %q, want %q", adapter, got, want)
		}
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordSkipsWhatItCannotMeter(t *testing.T) {
	s := openStore(t)
	r := New(s, nil)
	ctx := context.Background()

	// No tokens reported: an adapter that does not report usage must not fill
	// the table with rows that say nothing.
	if _, err := r.Record(ctx, Call{Adapter: "claude", Model: "claude-opus-5"}); err != nil {
		t.Fatal(err)
	}
	// No provider: `shell` spends nothing Aurium can attribute.
	if _, err := r.Record(ctx, Call{Adapter: "shell", InputTokens: 10, OutputTokens: 10}); err != nil {
		t.Fatal(err)
	}

	totals, err := s.UsageTotals(ctx, store.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Calls != 0 {
		t.Fatalf("nothing meterable happened, got %d rows", totals.Calls)
	}
}

// A subscription seat has no per-token price. Pricing one anyway would invent
// a bill the user will never receive.
func TestSubscriptionTokensAreMeteredButNotCosted(t *testing.T) {
	s := openStore(t)
	r := New(s, nil)
	ctx := context.Background()

	acct, err := s.CreateProviderAccount(ctx, store.ProviderAccount{
		Provider: store.ProviderAnthropic, Label: "max", AuthKind: store.AuthSubscription,
		EnvVar: "CLAUDE_CODE_OAUTH_TOKEN", Source: store.SourcePasted,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.Record(ctx, Call{
		Adapter: "claude", Model: "claude-opus-5", AccountID: acct.ID,
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	totals, err := s.UsageTotals(ctx, store.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if totals.InputTokens != 1_000_000 {
		t.Fatalf("subscription tokens must still be metered, got %d", totals.InputTokens)
	}
	if totals.CostUSD != 0 {
		t.Fatalf("a subscription turn must carry no cost, got %v", totals.CostUSD)
	}
	if totals.UnpricedTokens != 2_000_000 {
		t.Fatalf("subscription tokens must be reported as unpriced, got %d", totals.UnpricedTokens)
	}
}

func TestAPIKeyCallIsCosted(t *testing.T) {
	s := openStore(t)
	r := New(s, nil)
	ctx := context.Background()

	acct, _ := s.CreateProviderAccount(ctx, store.ProviderAccount{
		Provider: store.ProviderAnthropic, Label: "payg", AuthKind: store.AuthAPIKey,
		EnvVar: "ANTHROPIC_API_KEY", Source: store.SourcePasted,
	})
	if _, err := r.Record(ctx, Call{
		Adapter: "claude", Model: "claude-opus-5", AccountID: acct.ID,
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	totals, _ := s.UsageTotals(ctx, store.UsageQuery{})
	if totals.CostUSD <= 0 {
		t.Fatalf("an API-key call on a priced model must cost something, got %v", totals.CostUSD)
	}
}
