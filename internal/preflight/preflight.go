// Package preflight answers one question — can this machine run Aurium — as
// data rather than as printed output, so the CLI, the API and the dashboard
// all render the same answer instead of keeping three opinions.
package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Severity string

const (
	Required Severity = "required"
	Optional Severity = "optional"
)

type RemedyKind string

const (
	NoRemedy RemedyKind = ""
	// Auto is unprivileged and idempotent. Anything needing sudo, a GUI
	// installer or a TTY is Manual, permanently.
	Auto   RemedyKind = "auto"
	Manual RemedyKind = "manual"
)

type Remedy struct {
	Kind    RemedyKind
	Command string                      // exact copy-paste command, Manual only
	Note    string                      // why it can't be automated
	Fix     func(context.Context) error // non-nil only when Kind == Auto
}

type Check struct {
	Name     string
	Severity Severity
	Probe    func(context.Context) error
	Remedy   Remedy
}

type Result struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Severity   string `json:"severity"`
	Error      string `json:"error,omitempty"`
	Remedy     string `json:"remedy,omitempty"`
	RemedyKind string `json:"remedy_kind,omitempty"`
}

// Run executes every check in order and never stops early: a contributor
// wants the whole list of what is wrong, not the first thing that failed.
func Run(ctx context.Context, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := Result{Name: c.Name, Severity: string(c.Severity), OK: true}
		if err := c.Probe(ctx); err != nil {
			r.OK = false
			r.Error = err.Error()
			r.Remedy = c.Remedy.Command
			r.RemedyKind = string(c.Remedy.Kind)
		}
		out = append(out, r)
	}
	return out
}

// BinaryWorks reports whether name is on PATH *and* actually runs.
//
// exec.LookPath answers "is there a file with this name", which is a different
// question. macOS answers yes for /usr/bin/git on a machine where the Xcode
// licence has never been accepted and no git command can run; doctor printed
// `ok git` for exactly that machine while every git invocation failed.
func BinaryWorks(ctx context.Context, name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return errors.New("not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The binary's own first line of stderr is almost always the real
		// explanation, and is far more useful than "exit status 69".
		if line := firstLine(stderr.String()); line != "" {
			return errors.New(line)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
