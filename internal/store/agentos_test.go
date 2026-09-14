package store

import (
	"context"
	"errors"
	"testing"
)

func TestProviderAccountRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	a, err := s.CreateProviderAccount(ctx, ProviderAccount{
		Provider: ProviderAnthropic, Label: "work max", AuthKind: AuthSubscription,
		EnvVar: "CLAUDE_CODE_OAUTH_TOKEN", SecretRef: "keyring:aurium/provider/x",
		Source: SourcePasted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Status != AccountConnected || a.CreatedAt == "" {
		t.Fatalf("CreateProviderAccount must populate id, status and created_at: %+v", a)
	}

	got, err := s.GetProviderAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "work max" || got.SecretRef != "keyring:aurium/provider/x" {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	if _, err := s.GetProviderAccount(ctx, "pa_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account must be ErrNotFound, got %v", err)
	}
}

// Two accounts with the same label under the same provider are
// indistinguishable in a picker, so the database refuses them — and the caller
// must be able to tell that refusal apart from a daemon fault, because it is
// the user's to fix.
func TestDuplicateProviderLabelIsErrDuplicate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	mk := func() error {
		_, err := s.CreateProviderAccount(ctx, ProviderAccount{
			Provider: ProviderOpenAI, Label: "personal", AuthKind: AuthAPIKey,
			EnvVar: "OPENAI_API_KEY", Source: SourcePasted,
		})
		return err
	}
	if err := mk(); err != nil {
		t.Fatal(err)
	}
	if err := mk(); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a repeated (provider,label) must be ErrDuplicate, got %v", err)
	}
}

// The same label under a different provider is a different account; refusing
// it would make "personal" usable exactly once across the whole app.
func TestSameLabelUnderDifferentProvidersIsAllowed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	for _, p := range []string{ProviderAnthropic, ProviderOpenAI} {
		if _, err := s.CreateProviderAccount(ctx, ProviderAccount{
			Provider: p, Label: "personal", AuthKind: AuthAPIKey,
			EnvVar: "KEY", Source: SourcePasted,
		}); err != nil {
			t.Fatalf("%s/personal: %v", p, err)
		}
	}
}

func TestDefaultProviderAccountIsTheOldestConnectedOne(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	first, _ := s.CreateProviderAccount(ctx, ProviderAccount{
		Provider: ProviderAnthropic, Label: "one", AuthKind: AuthAPIKey,
		EnvVar: "ANTHROPIC_API_KEY", Source: SourcePasted,
	})
	if _, err := s.CreateProviderAccount(ctx, ProviderAccount{
		Provider: ProviderAnthropic, Label: "two", AuthKind: AuthAPIKey,
		EnvVar: "ANTHROPIC_API_KEY", Source: SourcePasted,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.DefaultProviderAccount(ctx, ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("default must be stable (the oldest), got %s want %s", got.Label, first.Label)
	}

	// An account in error is not a default: pointing new agents at a
	// credential already known to be broken is worse than having none.
	if err := s.SetProviderAccountStatus(ctx, first.ID, AccountError, "401"); err != nil {
		t.Fatal(err)
	}
	got, err = s.DefaultProviderAccount(ctx, ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "two" {
		t.Fatalf("a broken account must not be the default, got %s", got.Label)
	}
}

// Disconnecting an account must not take running agents with it: their
// credential is already in their environment, and deleting the rows would make
// live work disappear from the dashboard.
func TestDeleteProviderAccountKeepsAgents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")
	c, err := s.CreateContainer(ctx, Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "feature", Slug: "feature",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: "/r/.aurium/wt/feature", Status: ContainerRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := s.CreateProviderAccount(ctx, ProviderAccount{
		Provider: ProviderAnthropic, Label: "work", AuthKind: AuthAPIKey,
		EnvVar: "ANTHROPIC_API_KEY", Source: SourcePasted,
	})
	a, err := s.CreateAgent(ctx, Agent{
		ContainerID: c.ID, Adapter: "claude", Role: RolePrimary,
		ProviderAccountID: acct.ID, DisplayName: "oauth work",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProviderAccount(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("the agent must survive its account being disconnected: %v", err)
	}
	if got.ProviderAccountID != "" {
		t.Fatalf("the link must be cleared, got %q", got.ProviderAccountID)
	}
	if got.DisplayName != "oauth work" {
		t.Fatalf("display name lost: %q", got.DisplayName)
	}
}

func TestListProjectAgentsSpansContainers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")
	other, _ := s.CreateProject(ctx, "other", "/o")
	otherRepo, _ := s.CreateRepository(ctx, other.ID, "/o", "main", "")

	mkAgent := func(projectID, repoID, branch string) {
		c, err := s.CreateContainer(ctx, Container{
			ProjectID: projectID, RepoID: repoID, Branch: branch, Slug: branch,
			ParentBranch: "main", BaseSHA: "abc", Driver: "local",
			Worktree: "/r/.aurium/wt/" + branch, Status: ContainerRunning,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateAgent(ctx, Agent{
			ContainerID: c.ID, Adapter: "claude", Role: RolePrimary,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkAgent(p.ID, repo.ID, "a")
	mkAgent(p.ID, repo.ID, "b")
	mkAgent(other.ID, otherRepo.ID, "c")

	got, err := s.ListProjectAgents(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want the project's 2 agents and no others, got %d", len(got))
	}
}

func TestUsageAggregatesAndBuckets(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	rows := []UsageEvent{
		{TS: "2026-09-14T10:00:00Z", Provider: ProviderAnthropic, Model: "claude-opus-5",
			Kind: UsageExec, InputTokens: 1000, OutputTokens: 100, CostUSD: 0.5, Priced: true},
		{TS: "2026-09-14T10:30:00Z", Provider: ProviderAnthropic, Model: "claude-opus-5",
			Kind: UsageExec, InputTokens: 2000, OutputTokens: 200, CostUSD: 1.0, Priced: true},
		// A subscription turn: real tokens, no price. It must show up in the
		// token totals and in unpriced, and must not move the cost.
		{TS: "2026-09-14T11:10:00Z", Provider: ProviderOpenAI, Model: "gpt-unknown",
			Kind: UsageExec, InputTokens: 500, OutputTokens: 50, CostUSD: 0, Priced: false},
	}
	for _, r := range rows {
		if _, err := s.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	totals, err := s.UsageTotals(ctx, UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Calls != 3 || totals.InputTokens != 3500 || totals.OutputTokens != 350 {
		t.Fatalf("totals wrong: %+v", totals)
	}
	if totals.CostUSD != 1.5 {
		t.Fatalf("cost must exclude unpriced calls, got %v", totals.CostUSD)
	}
	if totals.UnpricedTokens != 550 {
		t.Fatalf("unpriced tokens must be reported separately, got %d", totals.UnpricedTokens)
	}

	byProvider, err := s.AggregateUsage(ctx, UsageQuery{}, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if len(byProvider) != 2 || byProvider[0].Key != ProviderAnthropic {
		t.Fatalf("grouping wrong (most expensive first): %+v", byProvider)
	}

	// group_by is an allowlist, not a column name the caller chooses.
	if _, err := s.AggregateUsage(ctx, UsageQuery{}, "provider; --"); err == nil {
		t.Fatal("an unknown grouping must be refused, not interpolated into SQL")
	}

	// One-hour buckets put the first two calls together and the third alone.
	series, err := s.UsageSeries(ctx, UsageQuery{}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("want 2 hourly buckets, got %d: %+v", len(series), series)
	}
	if series[0].Calls != 2 || series[1].Calls != 1 {
		t.Fatalf("bucketing wrong: %+v", series)
	}
	if series[0].Start != "2026-09-14T10:00:00Z" {
		t.Fatalf("bucket start must be RFC 3339, got %q", series[0].Start)
	}

	// A window narrows the set rather than merely relabelling it.
	windowed, err := s.UsageTotals(ctx, UsageQuery{Since: "2026-09-14T10:15:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if windowed.Calls != 2 {
		t.Fatalf("since must exclude earlier calls, got %d", windowed.Calls)
	}
}
