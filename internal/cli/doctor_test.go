package cli

import (
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
