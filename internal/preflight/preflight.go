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
	// Detail explains a *passing* row. Not every true thing a probe learns is
	// a complaint: "the port is held, by your own Aurium daemon" is the answer
	// someone wants to read, and reporting it as a failure would report a
	// healthy machine as broken.
	Detail string `json:"detail,omitempty"`
}

// InfoError is what a probe returns when it found something worth saying that
// is nevertheless not a failure. Run turns it into a passing Result carrying
// Detail, so a probe keeps one return type and callers keep one verdict.
type InfoError struct{ Msg string }

func (e *InfoError) Error() string { return e.Msg }

// Info builds an InfoError. Probes return it instead of nil when the reason
// they passed is itself worth printing.
func Info(format string, a ...any) error {
	return &InfoError{Msg: fmt.Sprintf(format, a...)}
}

// RunTimeout bounds the whole table, not just each probe. Every probe already
// carries its own deadline, so this only ever trips when one escapes it — but
// Run is serial and three surfaces block on it (doctor's output, the
// /v1/preflight handler, the dashboard wizard's first paint), so "no single
// probe can hang everything" needs a backstop that does not depend on every
// probe being written correctly.
const RunTimeout = 30 * time.Second

// Run executes every check in order and never stops early: a contributor
// wants the whole list of what is wrong, not the first thing that failed.
func Run(ctx context.Context, checks []Check) []Result {
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()

	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := Result{Name: c.Name, Severity: string(c.Severity), OK: true}
		if err := ctx.Err(); err != nil {
			// Say so rather than reporting an unrun check as passing. A check
			// nobody ran is not a check that succeeded.
			r.OK = false
			r.Error = "not checked: the run exceeded its overall deadline"
			out = append(out, r)
			continue
		}
		if err := c.Probe(ctx); err != nil {
			var info *InfoError
			if errors.As(err, &info) {
				r.Detail = info.Msg
			} else {
				r.OK = false
				r.Error = err.Error()
				r.Remedy = c.Remedy.Command
				r.RemedyKind = string(c.Remedy.Kind)
			}
		}
		out = append(out, r)
	}
	return out
}

// Fix applies every Auto remedy whose check is failing, then re-probes. Manual
// remedies are never executed: they are the ones that need sudo, a GUI or a
// TTY, and a setup tool that escalates on your behalf is not one you should
// pipe from curl.
func Fix(ctx context.Context, checks []Check) []Result {
	for _, c := range checks {
		if c.Remedy.Kind != Auto || c.Remedy.Fix == nil {
			continue
		}
		if passed(c.Probe(ctx)) {
			continue
		}
		_ = c.Remedy.Fix(ctx) // a failed fix simply leaves the re-probe failing
	}
	return Run(ctx, checks)
}

// passed reports whether a probe's return value means the check is satisfied.
// nil obviously does; so does an InfoError, which is a pass with something to
// say — applying a remedy to one would be fixing a machine that is fine.
func passed(err error) bool {
	if err == nil {
		return true
	}
	var info *InfoError
	return errors.As(err, &info)
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
