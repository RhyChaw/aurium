package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/preflight"
)

func TestRenderDoctorMarksFailuresAndPrintsTheRemedy(t *testing.T) {
	var sb strings.Builder
	results := []preflight.Result{
		{Name: "go", OK: true, Severity: "required"},
		{Name: "git", OK: false, Severity: "required",
			Error:  "xcrun: error: You have not agreed to the Xcode license",
			Remedy: "sudo xcodebuild -license accept", RemedyKind: "manual"},
		{Name: "tmux", OK: false, Severity: "optional", Error: "not installed"},
	}

	ok := renderDoctor(&sb, results)
	out := sb.String()

	if ok {
		t.Error("a failed required check must make the run not-ok")
	}
	if !strings.Contains(out, "ok    go") {
		t.Errorf("passing check missing from output:\n%s", out)
	}
	if !strings.Contains(out, "Xcode license") {
		t.Errorf("the probe's own error must be shown:\n%s", out)
	}
	if !strings.Contains(out, "sudo xcodebuild -license accept") {
		t.Errorf("the remedy must be printed verbatim:\n%s", out)
	}
}

// An optional check failing is not a reason to fail the command.
func TestRenderDoctorOptionalFailureStaysOK(t *testing.T) {
	var sb strings.Builder
	ok := renderDoctor(&sb, []preflight.Result{
		{Name: "tmux", OK: false, Severity: "optional", Error: "not installed"},
	})
	if !ok {
		t.Error("an optional failure must not fail the command")
	}
}

// Regression test: doctor --json used to return nil (exit 0) unconditionally
// once encoding succeeded, without ever looking at the results — a required
// failure like `go` or `port` being down was invisible to the exit code,
// even though the human table correctly failed the command for the same
// results. CI's `doctor --json` step depends on this exit code to mean what
// it says.
func TestDoctorJSONFailsCommandOnRequiredFailure(t *testing.T) {
	var buf strings.Builder
	results := []preflight.Result{
		{Name: "go", OK: false, Severity: "required", Error: "not installed"},
		{Name: "tmux", OK: false, Severity: "optional", Error: "not installed"},
	}

	err := writeDoctorJSON(&buf, results)

	if !strings.Contains(buf.String(), `"name":"go"`) {
		t.Errorf("the JSON payload must still be written in full:\n%s", buf.String())
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("a required failure must return an *ExitError, got %v (%T)", err, err)
	}
}

// The companion case: an optional-only failure must still exit clean in
// --json mode, matching the human table's behavior.
func TestDoctorJSONOptionalFailureStaysOK(t *testing.T) {
	var buf strings.Builder
	results := []preflight.Result{
		{Name: "tmux", OK: false, Severity: "optional", Error: "not installed"},
	}

	err := writeDoctorJSON(&buf, results)

	if !strings.Contains(buf.String(), `"name":"tmux"`) {
		t.Errorf("the JSON payload must still be written:\n%s", buf.String())
	}
	if err != nil {
		t.Errorf("an optional failure must not fail --json mode: %v", err)
	}
}

// A passing row with something to say must say it. The port check reports
// "held by your own Aurium daemon" this way — a fact worth printing that is
// emphatically not a failure, and printing a bare "ok port" would throw away
// the only sentence that explains why a held port is fine.
func TestRenderDoctorPrintsDetailOnAPassingRow(t *testing.T) {
	var sb strings.Builder
	ok := renderDoctor(&sb, []preflight.Result{
		{Name: "port", OK: true, Severity: "required",
			Detail: "held by an Aurium daemon (version 0.1.0-dev, pid 99) — your own"},
	})

	if !ok {
		t.Error("a passing row must not fail the command")
	}
	if !strings.Contains(sb.String(), "ok    port") {
		t.Errorf("the row must still read as ok:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "your own") {
		t.Errorf("the detail must be printed:\n%s", sb.String())
	}
}
