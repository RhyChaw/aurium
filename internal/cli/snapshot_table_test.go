package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

// The note explaining a missing conversation is a sentence. A tabwriter column
// is as wide as its widest cell, so putting that sentence in the CONVERSATION
// column stretches the column to the length of the sentence and pushes every
// other column off the right of the terminal — at any width.
func TestSnapshotTableStaysLegibleWhenAConversationIsMissing(t *testing.T) {
	note := "the agent's conversation was not captured: under agent_placement: host " +
		"the transcript lives in ~/.claude on this machine, outside the container " +
		"filesystem this snapshot captured"
	snaps := []store.Snapshot{
		{Seq: 1, Trigger: "manual", HeadSHA: "abcdef1234567890", CreatedAt: "2026-09-15T10:00:00Z",
			IncludesConversation: true},
		{Seq: 2, Trigger: "manual", HeadSHA: "abcdef1234567890", CreatedAt: "2026-09-15T11:00:00Z",
			IncludesConversation: false, Note: note},
		{Seq: 3, Trigger: "pre_restore", HeadSHA: "abcdef1234567890", CreatedAt: "2026-09-15T12:00:00Z",
			IncludesConversation: false, Note: note},
	}

	var buf bytes.Buffer
	if err := renderSnapshotTable(&buf, snaps); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Every row of the table itself must fit a normal terminal. Rows are the
	// lines before the notes, which are printed after a blank line.
	table, _, _ := strings.Cut(out, "\nnote: ")
	for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") {
		if len(line) > 100 {
			t.Errorf("a table row is %d characters wide, which wrecks the table:\n%s", len(line), line)
		}
	}

	// The reason is still told — once, below the table, not once per row.
	if n := strings.Count(out, note); n != 1 {
		t.Errorf("the note should appear exactly once below the table, appeared %d times:\n%s", n, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), strings.TrimSpace("note: "+note)) {
		t.Errorf("the note must come after the table, not inside it:\n%s", out)
	}
	// And the column still answers the question it exists to answer.
	if !strings.Contains(table, "yes") || !strings.Contains(table, "no") {
		t.Errorf("the CONVERSATION column must still say yes/no:\n%s", table)
	}
}

// A table with nothing to explain prints no note at all.
func TestSnapshotTablePrintsNoNoteWhenEveryConversationWasCaptured(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSnapshotTable(&buf, []store.Snapshot{
		{Seq: 1, Trigger: "manual", CreatedAt: "2026-09-15T10:00:00Z", IncludesConversation: true},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "note:") {
		t.Errorf("nothing to explain, so nothing should be explained:\n%s", buf.String())
	}
}
