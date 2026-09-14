// Package secrets holds integration credentials on the host (§10.6).
//
// D17: the daemon is the only holder of secrets. Nothing here ever writes a
// credential into the database, an event, a container, or a log — the whole
// point of the MCP gateway is that an agent uses a capability without ever
// seeing the credential behind it.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keyring service name Aurium stores under.
const Service = "aurium"

// ErrNotFound is returned when no secret is stored for a reference.
var ErrNotFound = errors.New("secrets: not found")

// Keyring is the OS credential store. It is an interface with exactly one
// production implementation so that tests — and a future age-encrypted
// backend — can stand in without reaching the developer's real login keychain,
// which on macOS means a modal dialog in the middle of a test run.
type Keyring interface {
	Set(service, account, secret string) error
	Get(service, account string) (string, error)
	Delete(service, account string) error
}

// osKeyring is the real thing.
type osKeyring struct{}

func (osKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}
func (osKeyring) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}
func (osKeyring) Delete(service, account string) error {
	return keyring.Delete(service, account)
}

// ErrKeyringUnavailable is what a Keyring returns when there is none. It is
// matched against keyring.ErrNotFound's role in the fallback logic, so a stand
// -in implementation can express "nothing here" the same way.
var ErrKeyringUnavailable = errors.New("secrets: no keyring")

// Store reads and writes credentials.
type Store struct {
	// Fallback is used where no OS keyring exists (headless Linux, CI).
	Fallback Fallback
	// Keyring defaults to the OS keyring when nil.
	Keyring Keyring
}

func (s *Store) ring() Keyring {
	if s.Keyring != nil {
		return s.Keyring
	}
	return osKeyring{}
}

// Fallback is a secondary store for machines without a keyring.
type Fallback interface {
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
	Available() bool
}

// New returns a store using the OS keyring, with an optional fallback.
func New(fallback Fallback) *Store { return &Store{Fallback: fallback} }

// Ref is the opaque handle stored in the database in place of a credential.
// It names where the secret lives; it is never the secret.
func Ref(projectName, integrationName string) string {
	return "keyring:" + Service + "/" + projectName + "/" + integrationName
}

// accountFor turns a reference back into a keyring account.
func accountFor(ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, "keyring:"+Service+"/")
	if !ok {
		return "", fmt.Errorf("secrets: %q is not an aurium secret reference", ref)
	}
	if rest == "" {
		return "", fmt.Errorf("secrets: empty secret reference")
	}
	return rest, nil
}

// Set stores a credential and returns the reference to record instead.
func (s *Store) Set(projectName, integrationName, secret string) (string, error) {
	ref := Ref(projectName, integrationName)
	account, err := accountFor(ref)
	if err != nil {
		return "", err
	}

	if err := s.ring().Set(Service, account, secret); err != nil {
		if s.Fallback == nil || !s.Fallback.Available() {
			return "", fmt.Errorf("secrets: no OS keyring is available and no fallback is "+
				"configured; run `aurium doctor` for details: %w", err)
		}
		if ferr := s.Fallback.Set(account, secret); ferr != nil {
			return "", fmt.Errorf("secrets: storing in the fallback: %w", ferr)
		}
	}
	return ref, nil
}

// Get resolves a reference to a credential.
//
// Called only when spawning or connecting an upstream, and the value is passed
// straight into that process's environment. It is never returned through the
// API, so no caller can turn a reference back into a secret.
func (s *Store) Get(ref string) (string, error) {
	account, err := accountFor(ref)
	if err != nil {
		return "", err
	}

	secret, err := s.ring().Get(Service, account)
	if err == nil {
		return secret, nil
	}
	if isMissing(err) && (s.Fallback == nil || !s.Fallback.Available()) {
		return "", ErrNotFound
	}
	if s.Fallback != nil && s.Fallback.Available() {
		secret, ferr := s.Fallback.Get(account)
		if ferr == nil {
			return secret, nil
		}
		return "", ErrNotFound
	}
	return "", fmt.Errorf("secrets: reading %s: %w", ref, err)
}

// Delete removes a credential.
func (s *Store) Delete(ref string) error {
	account, err := accountFor(ref)
	if err != nil {
		return err
	}
	kerr := s.ring().Delete(Service, account)
	if s.Fallback != nil && s.Fallback.Available() {
		_ = s.Fallback.Delete(account)
	}
	if kerr != nil && !isMissing(kerr) {
		return kerr
	}
	return nil
}

// Available reports whether any secret store works on this machine.
func (s *Store) Available() bool {
	probe := "aurium-availability-probe"
	if err := s.ring().Set(Service, probe, "x"); err == nil {
		_ = s.ring().Delete(Service, probe)
		return true
	}
	return s.Fallback != nil && s.Fallback.Available()
}

// isMissing reports "there is no such secret", whichever backend said so.
func isMissing(err error) bool {
	return errors.Is(err, keyring.ErrNotFound) ||
		errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrKeyringUnavailable)
}

// FileFallback stores secrets in a file under ~/.aurium.
//
// It is a genuine downgrade from an OS keyring and says so: the file is 0600
// but not encrypted at rest by this build, so it exists for headless Linux and
// CI rather than as an equal alternative. `aurium doctor` reports when it is
// in use.
type FileFallback struct {
	Path string
}

func (f *FileFallback) Available() bool { return f.Path != "" }

func (f *FileFallback) load() (map[string]string, error) {
	out := map[string]string{}
	body, err := os.ReadFile(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			out[k] = v
		}
	}
	return out, nil
}

func (f *FileFallback) save(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k + "\t" + v + "\n")
	}
	// 0600, and written to a temp file first so a crash cannot leave a
	// truncated file that loses every other credential.
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

func (f *FileFallback) Get(account string) (string, error) {
	m, err := f.load()
	if err != nil {
		return "", err
	}
	v, ok := m[account]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *FileFallback) Set(account, secret string) error {
	m, err := f.load()
	if err != nil {
		return err
	}
	m[account] = secret
	return f.save(m)
}

func (f *FileFallback) Delete(account string) error {
	m, err := f.load()
	if err != nil {
		return err
	}
	delete(m, account)
	return f.save(m)
}
