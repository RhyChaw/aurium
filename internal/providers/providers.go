// Package providers connects the model providers agents run on (§D24, §D25).
//
// Two things live here and nowhere else: what a provider is called in each of
// the four places it has a name (company, adapter, environment variable, CLI),
// and how a credential gets from the human to the keyring without ever passing
// through the database, an event, or a log.
//
// There is deliberately no function that turns a stored account back into a
// credential for a caller outside this package. Resolve exists for exactly one
// consumer — the runtime, building a container's environment — and returns the
// secret to it directly rather than through any serialisable type.
package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/secrets"
	"github.com/RhyChaw/aurium/internal/store"
)

// Event types for connecting and disconnecting an account.
const (
	Connected    = "provider.connected"
	Disconnected = "provider.disconnected"
)

// Spec describes one provider: every name it goes by, and how to log in.
type Spec struct {
	// Provider is the company, as stored.
	Provider string
	// Display is what the UI calls it.
	Display string
	// Adapter is the agent adapter that spends on it.
	Adapter string
	// APIKeyEnv is the environment variable carrying a pay-as-you-go key.
	APIKeyEnv string
	// SubscriptionEnv carries a seat credential, when the CLI has one. Empty
	// means the CLI keeps its login in a file instead (Codex), which is why
	// SubscriptionFile exists.
	SubscriptionEnv string
	// SubscriptionCommand is the command a human runs to produce a seat
	// credential. Aurium shows it; it does not run it, because both are
	// interactive browser flows that cannot be driven from a daemon.
	SubscriptionCommand string
	// SubscriptionFile is where the CLI stores its own login, relative to the
	// home directory. Detecting it is what makes "use the login I already
	// have" a single click.
	SubscriptionFile string
	// KeyPrefix is the shape a pasted API key has, used to catch the common
	// paste mistake of swapping the two providers' keys.
	KeyPrefix string
}

// Specs is the registry, keyed by provider.
var Specs = map[string]Spec{
	store.ProviderAnthropic: {
		Provider:            store.ProviderAnthropic,
		Display:             "Anthropic — Claude",
		Adapter:             "claude",
		APIKeyEnv:           "ANTHROPIC_API_KEY",
		SubscriptionEnv:     "CLAUDE_CODE_OAUTH_TOKEN",
		SubscriptionCommand: "claude setup-token",
		SubscriptionFile:    ".claude/.credentials.json",
		KeyPrefix:           "sk-ant-",
	},
	store.ProviderOpenAI: {
		Provider:            store.ProviderOpenAI,
		Display:             "OpenAI — Codex",
		Adapter:             "codex",
		APIKeyEnv:           "OPENAI_API_KEY",
		SubscriptionCommand: "codex login",
		SubscriptionFile:    ".codex/auth.json",
		KeyPrefix:           "sk-",
	},
}

// SpecFor returns the spec for a provider.
func SpecFor(provider string) (Spec, bool) {
	s, ok := Specs[strings.ToLower(strings.TrimSpace(provider))]
	return s, ok
}

// EnvVarFor returns the variable an account of this kind is delivered through.
func (s Spec) EnvVarFor(authKind string) string {
	if authKind == store.AuthSubscription && s.SubscriptionEnv != "" {
		return s.SubscriptionEnv
	}
	return s.APIKeyEnv
}

// Ref is the keyring reference for an account. Accounts live under a reserved
// "provider" namespace so they cannot collide with an integration whose name
// happens to be "anthropic".
func Ref(accountID string) string {
	return secrets.Ref("provider", accountID)
}

// Manager connects and disconnects provider accounts.
type Manager struct {
	Store   *store.Store
	Secrets *secrets.Store
	Events  *events.Bus
	// Home is the directory CLI logins are detected under. Defaults to the
	// user's home; overridable so tests do not read the developer's real one.
	Home string
}

// New returns a manager.
func New(s *store.Store, sec *secrets.Store, e *events.Bus) *Manager {
	return &Manager{Store: s, Secrets: sec, Events: e}
}

func (m *Manager) home() string {
	if m.Home != "" {
		return m.Home
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// ConnectRequest is what the dashboard posts.
type ConnectRequest struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	AuthKind string `json:"auth_kind"`
	// Secret is the pasted key or token. It is never echoed back, never
	// logged, and never stored anywhere but the keyring.
	Secret string `json:"secret"`
	// UseHostEnv connects the credential this host already has in its
	// environment instead of a pasted one.
	UseHostEnv bool `json:"use_host_env"`
	// UseCLILogin records that the provider's own CLI is already logged in on
	// this host, with no credential for Aurium to hold at all.
	UseCLILogin bool `json:"use_cli_login"`
}

// ErrNoCredential is returned when a connect request names no way to
// authenticate.
var ErrNoCredential = errors.New("providers: no credential given")

// Connect stores a credential and records the account.
func (m *Manager) Connect(ctx context.Context, req ConnectRequest) (store.ProviderAccount, error) {
	spec, ok := SpecFor(req.Provider)
	if !ok {
		return store.ProviderAccount{}, fmt.Errorf("providers: %q is not a known provider", req.Provider)
	}
	if req.AuthKind != store.AuthAPIKey && req.AuthKind != store.AuthSubscription {
		return store.ProviderAccount{}, fmt.Errorf(
			"providers: auth_kind %q is neither %s nor %s", req.AuthKind, store.AuthAPIKey, store.AuthSubscription)
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = defaultLabel(spec, req.AuthKind)
	}
	envVar := spec.EnvVarFor(req.AuthKind)

	secret := strings.TrimSpace(req.Secret)
	source := store.SourcePasted

	switch {
	case secret != "":
		// Pasted: nothing more to do.
	case req.UseHostEnv:
		secret = strings.TrimSpace(os.Getenv(envVar))
		if secret == "" {
			return store.ProviderAccount{}, fmt.Errorf(
				"providers: $%s is not set on this host", envVar)
		}
		source = store.SourceHostEnv
	case req.UseCLILogin:
		// The CLI holds its own login in its own file. Aurium records that the
		// account exists and holds nothing: copying a credential out of
		// another program's config to store a second time would create a
		// second thing to revoke.
		path := m.cliLoginPath(spec)
		if path == "" {
			return store.ProviderAccount{}, fmt.Errorf(
				"providers: no %s login found on this host; run `%s` first",
				spec.Display, spec.SubscriptionCommand)
		}
		source = store.SourceCLILogin
	default:
		return store.ProviderAccount{}, ErrNoCredential
	}

	// A key for the wrong provider is the single most common paste error, and
	// it fails much later and much less clearly than it needs to.
	if source != store.SourceCLILogin && req.AuthKind == store.AuthAPIKey {
		if spec.Provider == store.ProviderOpenAI && strings.HasPrefix(secret, "sk-ant-") {
			return store.ProviderAccount{}, fmt.Errorf(
				"providers: that is an Anthropic key (sk-ant-…), not an OpenAI one")
		}
		if spec.Provider == store.ProviderAnthropic && !strings.HasPrefix(secret, "sk-ant-") {
			return store.ProviderAccount{}, fmt.Errorf(
				"providers: an Anthropic API key starts with sk-ant-; for a Claude subscription use auth_kind %q",
				store.AuthSubscription)
		}
	}

	acct := store.ProviderAccount{
		Provider: spec.Provider, Label: label, AuthKind: req.AuthKind,
		EnvVar: envVar, Source: source, Status: store.AccountConnected,
	}
	acct, err := m.Store.CreateProviderAccount(ctx, acct)
	if err != nil {
		return store.ProviderAccount{}, err
	}

	if source != store.SourceCLILogin {
		ref, err := m.Secrets.Set("provider", acct.ID, secret)
		if err != nil {
			// The row would otherwise claim an account whose credential was
			// never stored, and every agent started on it would fail with a
			// 401 nobody could explain.
			_ = m.Store.DeleteProviderAccount(ctx, acct.ID)
			return store.ProviderAccount{}, err
		}
		if _, err := m.Store.DB().ExecContext(ctx,
			`UPDATE provider_accounts SET secret_ref = ? WHERE id = ?`, ref, acct.ID); err != nil {
			_ = m.Secrets.Delete(ref)
			_ = m.Store.DeleteProviderAccount(ctx, acct.ID)
			return store.ProviderAccount{}, err
		}
		acct.SecretRef = ref
	}

	if m.Events != nil {
		// The payload names the account and how it was connected, and carries
		// nothing that could be replayed as a credential.
		_ = m.Events.Emit(ctx, events.Event{
			Type: Connected, Actor: events.ActorHuman,
			Payload: map[string]any{
				"account": acct.ID, "provider": acct.Provider,
				"label": acct.Label, "auth_kind": acct.AuthKind, "source": acct.Source,
			},
		})
	}
	return acct, nil
}

// Disconnect removes an account and its credential.
func (m *Manager) Disconnect(ctx context.Context, id string) error {
	acct, err := m.Store.GetProviderAccount(ctx, id)
	if err != nil {
		return err
	}
	if acct.SecretRef != "" {
		// Deleted before the row, so a failure here leaves an account that
		// still names its secret rather than a secret nothing points at.
		if err := m.Secrets.Delete(acct.SecretRef); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			return err
		}
	}
	if err := m.Store.DeleteProviderAccount(ctx, id); err != nil {
		return err
	}
	if m.Events != nil {
		_ = m.Events.Emit(ctx, events.Event{
			Type: Disconnected, Actor: events.ActorHuman,
			Payload: map[string]any{"account": id, "provider": acct.Provider, "label": acct.Label},
		})
	}
	return nil
}

// Resolve returns the environment variable and value an agent on this account
// needs. It is the only path from an account to a credential, and it hands the
// value to the runtime rather than to any serialisable type.
//
// An account connected as cli_login resolves to nothing, which is correct: the
// agent's own CLI finds its own login, and Aurium holds no copy.
func (m *Manager) Resolve(ctx context.Context, accountID string) (name, value string, err error) {
	acct, err := m.Store.GetProviderAccount(ctx, accountID)
	if err != nil {
		return "", "", err
	}
	if acct.SecretRef == "" {
		return "", "", nil
	}
	secret, err := m.Secrets.Get(acct.SecretRef)
	if err != nil {
		// Record why, so the dashboard shows a broken account rather than an
		// agent that merely fails to start.
		_ = m.Store.SetProviderAccountStatus(ctx, accountID, store.AccountError, err.Error())
		return "", "", fmt.Errorf("providers: %s (%s): %w", acct.Label, acct.Provider, err)
	}
	return acct.EnvVar, secret, nil
}

// Detected is what this host already offers, so connecting is usually one
// click rather than a trip to a browser.
type Detected struct {
	Provider string `json:"provider"`
	Display  string `json:"display"`
	Adapter  string `json:"adapter"`
	// HostEnv names the environment variables that are set, without their
	// values. Reporting whether a credential exists is useful; reporting what
	// it is would put it in an HTTP response.
	HostEnv []string `json:"host_env"`
	// CLILogin is the provider CLI's own login file, if present.
	CLILogin string `json:"cli_login,omitempty"`
	// SubscriptionCommand is what to run when nothing was found.
	SubscriptionCommand string `json:"subscription_command,omitempty"`
	APIKeyEnv           string `json:"api_key_env"`
	SubscriptionEnv     string `json:"subscription_env,omitempty"`
	// Connected counts accounts already connected for this provider.
	Connected int `json:"connected"`
}

// Detect reports what each provider has available on this host.
func (m *Manager) Detect(ctx context.Context) ([]Detected, error) {
	accounts, err := m.Store.ListProviderAccounts(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, a := range accounts {
		counts[a.Provider]++
	}

	// Iterated in a fixed order so the page does not reshuffle between loads.
	var out []Detected
	for _, provider := range []string{store.ProviderAnthropic, store.ProviderOpenAI} {
		spec := Specs[provider]
		d := Detected{
			Provider: spec.Provider, Display: spec.Display, Adapter: spec.Adapter,
			SubscriptionCommand: spec.SubscriptionCommand,
			APIKeyEnv:           spec.APIKeyEnv, SubscriptionEnv: spec.SubscriptionEnv,
			Connected: counts[provider],
		}
		for _, name := range []string{spec.APIKeyEnv, spec.SubscriptionEnv} {
			if name != "" && strings.TrimSpace(os.Getenv(name)) != "" {
				d.HostEnv = append(d.HostEnv, name)
			}
		}
		d.CLILogin = m.cliLoginPath(spec)
		out = append(out, d)
	}
	return out, nil
}

// cliLoginPath returns the provider CLI's login file if it exists on this host.
func (m *Manager) cliLoginPath(spec Spec) string {
	if spec.SubscriptionFile == "" {
		return ""
	}
	home := m.home()
	if home == "" {
		return ""
	}
	path := filepath.Join(home, spec.SubscriptionFile)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func defaultLabel(spec Spec, authKind string) string {
	if authKind == store.AuthSubscription {
		return spec.Adapter + " subscription"
	}
	return spec.Adapter + " api key"
}
