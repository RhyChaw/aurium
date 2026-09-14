package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/secrets"
	"github.com/RhyChaw/aurium/internal/store"
)

// fakeKeyring stands in for the OS keyring so a test never touches the
// developer's real login keychain.
type fakeKeyring struct {
	m      map[string]string
	broken bool
}

func (f *fakeKeyring) Set(service, account, secret string) error {
	if f.broken {
		return errors.New("keyring locked")
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[service+"/"+account] = secret
	return nil
}

func (f *fakeKeyring) Get(service, account string) (string, error) {
	v, ok := f.m[service+"/"+account]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Delete(service, account string) error {
	delete(f.m, service+"/"+account)
	return nil
}

func newManager(t *testing.T) (*Manager, *store.Store, *fakeKeyring) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ring := &fakeKeyring{}
	sec := secrets.New(nil)
	sec.Keyring = ring

	m := New(st, sec, events.New(st))
	m.Home = t.TempDir() // never read the real home
	return m, st, ring
}

func TestConnectAPIKeyStoresOnlyAReference(t *testing.T) {
	m, st, ring := newManager(t)
	ctx := context.Background()

	acct, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey,
		Label: "work", Secret: "sk-ant-SECRETVALUE",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acct.SecretRef == "" || strings.Contains(acct.SecretRef, "SECRETVALUE") {
		t.Fatalf("the stored reference must name the secret, not contain it: %q", acct.SecretRef)
	}
	if acct.EnvVar != "ANTHROPIC_API_KEY" {
		t.Fatalf("env var = %q", acct.EnvVar)
	}

	// The credential must be nowhere in the database. This is the whole of
	// D17 for provider accounts, so it is asserted against the raw rows
	// rather than against the typed struct.
	var blob string
	if err := st.DB().QueryRow(
		`SELECT group_concat(id || label || env_var || secret_ref || source || status)
		 FROM provider_accounts`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blob, "SECRETVALUE") {
		t.Fatal("a credential reached the database")
	}

	name, value, err := m.Resolve(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ANTHROPIC_API_KEY" || value != "sk-ant-SECRETVALUE" {
		t.Fatalf("Resolve = %q=%q", name, value)
	}
	if len(ring.m) != 1 {
		t.Fatalf("the secret belongs in the keyring, found %d entries", len(ring.m))
	}
}

// A row claiming an account whose credential was never stored produces agents
// that fail with a 401 nobody can explain.
func TestConnectRollsBackWhenTheKeyringRefuses(t *testing.T) {
	m, st, ring := newManager(t)
	ring.broken = true
	ctx := context.Background()

	if _, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey, Secret: "sk-ant-x",
	}); err == nil {
		t.Fatal("a keyring failure must fail the connect")
	}

	accounts, err := st.ListProviderAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("a failed connect must leave no account behind, got %d", len(accounts))
	}
}

// Swapping the two providers' keys is the most common paste error and it
// otherwise fails much later and much less clearly.
func TestMismatchedKeysAreCaughtAtConnectTime(t *testing.T) {
	m, _, _ := newManager(t)
	ctx := context.Background()

	if _, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderOpenAI, AuthKind: store.AuthAPIKey, Secret: "sk-ant-oops",
	}); err == nil {
		t.Fatal("an Anthropic key must not be accepted as an OpenAI one")
	}
	if _, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey, Secret: "sk-proj-oops",
	}); err == nil {
		t.Fatal("an OpenAI key must not be accepted as an Anthropic one")
	}
}

func TestConnectFromHostEnv(t *testing.T) {
	m, _, _ := newManager(t)
	ctx := context.Background()

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-value")
	acct, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthSubscription, UseHostEnv: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acct.Source != store.SourceHostEnv {
		t.Fatalf("source = %q", acct.Source)
	}
	if acct.EnvVar != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("a subscription must arrive through the OAuth variable, got %q", acct.EnvVar)
	}

	_, value, err := m.Resolve(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value != "oauth-token-value" {
		t.Fatalf("resolved %q", value)
	}
}

func TestConnectFromHostEnvRefusesWhenUnset(t *testing.T) {
	m, _, _ := newManager(t)
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := m.Connect(context.Background(), ConnectRequest{
		Provider: store.ProviderOpenAI, AuthKind: store.AuthAPIKey, UseHostEnv: true,
	}); err == nil {
		t.Fatal("connecting an unset variable must fail rather than store an empty credential")
	}
}

// Copying a credential out of another program's config to store a second time
// would create a second thing to revoke. A CLI login is recorded and not held.
func TestCLILoginHoldsNoCredential(t *testing.T) {
	m, _, ring := newManager(t)
	ctx := context.Background()

	path := filepath.Join(m.Home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"whatever"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	acct, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderOpenAI, AuthKind: store.AuthSubscription, UseCLILogin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acct.Source != store.SourceCLILogin || acct.SecretRef != "" {
		t.Fatalf("a CLI login must hold nothing: %+v", acct)
	}
	if len(ring.m) != 0 {
		t.Fatal("nothing should have been written to the keyring")
	}

	name, value, err := m.Resolve(ctx, acct.ID)
	if err != nil || name != "" || value != "" {
		t.Fatalf("a CLI login resolves to nothing: %q=%q err=%v", name, value, err)
	}
}

func TestCLILoginRefusedWhenThereIsNone(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.Connect(context.Background(), ConnectRequest{
		Provider: store.ProviderOpenAI, AuthKind: store.AuthSubscription, UseCLILogin: true,
	}); err == nil {
		t.Fatal("claiming a login that is not there must fail")
	}
}

func TestConnectWithNothingAtAll(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.Connect(context.Background(), ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey,
	}); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
}

func TestDisconnectRemovesTheCredential(t *testing.T) {
	m, st, ring := newManager(t)
	ctx := context.Background()

	acct, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey, Secret: "sk-ant-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	if len(ring.m) != 0 {
		t.Fatal("disconnecting must delete the credential, not just the row")
	}
	if _, err := st.GetProviderAccount(ctx, acct.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the account must be gone, got %v", err)
	}
}

// Detect reports whether a credential exists. Reporting what it is would put
// it in an HTTP response.
func TestDetectNamesVariablesWithoutValues(t *testing.T) {
	m, _, _ := newManager(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-SECRETVALUE")
	t.Setenv("OPENAI_API_KEY", "")

	got, err := m.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Provider != store.ProviderAnthropic {
		t.Fatalf("Detect must report both providers in a stable order: %+v", got)
	}
	if len(got[0].HostEnv) != 1 || got[0].HostEnv[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("host env = %v", got[0].HostEnv)
	}
	if len(got[1].HostEnv) != 0 {
		t.Fatalf("an empty variable is not a credential: %v", got[1].HostEnv)
	}
	for _, d := range got {
		for _, field := range append([]string{d.CLILogin, d.SubscriptionCommand}, d.HostEnv...) {
			if strings.Contains(field, "SECRETVALUE") {
				t.Fatal("Detect leaked a credential")
			}
		}
	}
}

func TestResolveMarksABrokenAccount(t *testing.T) {
	m, st, ring := newManager(t)
	ctx := context.Background()

	acct, err := m.Connect(ctx, ConnectRequest{
		Provider: store.ProviderAnthropic, AuthKind: store.AuthAPIKey, Secret: "sk-ant-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The credential vanishes from under it — a revoked keychain entry, a
	// different login. The account must show as broken rather than the agent
	// merely failing to start.
	ring.m = map[string]string{}

	if _, _, err := m.Resolve(ctx, acct.ID); err == nil {
		t.Fatal("resolving a missing credential must fail")
	}
	got, err := st.GetProviderAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.AccountError || got.LastError == "" {
		t.Fatalf("the account must record why it broke: %+v", got)
	}
}
