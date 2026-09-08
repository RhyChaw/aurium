package cli_test

import (
	"os"
	"strings"
	"testing"
)

// §73 demo, first half: snapshot a container, wreck it, restore it.
func TestSnapshotAndRestoreRecoversAWreckedWorktree(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")

	// The state worth protecting is mostly uncommitted.
	e.commitIn("A", "committed.go", "package main\n", "A: committed work")
	e.writeIn(e.wt("A"), "in-progress.go", "// half-written\n")

	out := e.aurium("snapshot", "A", "--label", "before-refactor")
	if !strings.Contains(out, "Snapshot 1") {
		t.Fatalf("unexpected snapshot output:\n%s", out)
	}

	// Now an agent wrecks the place, the way a bad run does.
	e.writeIn(e.wt("A"), "committed.go", "package main // CORRUPTED\n")
	os.Remove(e.wt("A") + "/in-progress.go")
	e.writeIn(e.wt("A"), "garbage.go", "// should not survive\n")

	e.aurium("restore", "A", "1")

	body, err := os.ReadFile(e.wt("A") + "/committed.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "CORRUPTED") {
		t.Error("restore did not revert the corrupted file")
	}
	if !e.exists("A", "in-progress.go") {
		t.Error("restore did not bring back uncommitted work — the state most worth protecting")
	}
	if e.exists("A", "garbage.go") {
		t.Error("a file created after the snapshot survived the restore")
	}
	e.git("fsck", "--no-progress")
}

func TestRestoreTakesABackupFirst(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.aurium("snapshot", "A")

	e.writeIn(e.wt("A"), "later.txt", "work done after the snapshot\n")
	e.aurium("restore", "A", "1")

	// The pre-restore snapshot means the restore itself is undoable.
	list := e.aurium("snapshot", "list", "A")
	if !strings.Contains(list, "pre_restore") {
		t.Fatalf("restore must snapshot the current state first:\n%s", list)
	}

	// And that backup really holds the state restore just discarded.
	e.aurium("restore", "A", "2")
	if !e.exists("A", "later.txt") {
		t.Error("the pre_restore snapshot did not capture the state it replaced")
	}
}

// The §6.4 distinction, end to end: a stacked child tracks its parent, a fork
// is a peer. Getting this wrong is silent until the first sync.
func TestStackTracksTheParentWhileForkIsAPeer(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.commitIn("A", "a.txt", "a", "A: work")

	e.aurium("stack", "B", "--on", "A")
	e.aurium("fork", "A", "--branch", "A-copy")

	out := e.aurium("tree")

	// B is a child of A: it appears nested under it.
	if !strings.Contains(out, "B") || !strings.Contains(out, "A-copy") {
		t.Fatalf("both containers should exist:\n%s", out)
	}

	// The stacked child inherits A's work.
	if !e.exists("B", "a.txt") {
		t.Error("a stacked child should start from the parent's state")
	}
	// So does the fork — but as its own commits, not inherited ones.
	if !e.exists("A-copy", "a.txt") {
		t.Error("a fork should start from the source's state")
	}

	// The real difference: after A moves, only the child is stale.
	e.commitIn("A", "a2.txt", "a2", "A: moves on")
	out = e.aurium("tree")

	lines := strings.Split(out, "\n")
	var bLine, forkLine string
	for _, l := range lines {
		if strings.Contains(l, " B ") {
			bLine = l
		}
		if strings.Contains(l, "A-copy") {
			forkLine = l
		}
	}
	if !strings.Contains(bLine, "stale") {
		t.Errorf("the stacked child must go stale when its parent moves:\n%s", bLine)
	}
	if strings.Contains(forkLine, "stale") {
		t.Errorf("a fork is a peer and must NOT track the source:\n%s", forkLine)
	}

	// And syncing the child brings A's new work in.
	e.aurium("sync", "B")
	if !e.exists("B", "a2.txt") {
		t.Error("syncing the stacked child did not bring in the parent's new commit")
	}
	e.git("fsck", "--no-progress")
}

func TestSnapshotListShowsLabelsAndTriggers(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.aurium("snapshot", "A", "--label", "milestone-one")
	e.aurium("snapshot", "A")

	out := e.aurium("snapshot", "list", "A")
	if !strings.Contains(out, "milestone-one") {
		t.Errorf("label missing from the listing:\n%s", out)
	}
	if !strings.Contains(out, "manual") {
		t.Errorf("trigger missing from the listing:\n%s", out)
	}
}

func TestSnapshotGCKeepsLabelledAndReferenced(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")

	e.aurium("snapshot", "A", "--label", "keep-me")
	for i := 0; i < 12; i++ {
		e.aurium("snapshot", "A")
	}

	out := e.aurium("snapshot", "gc")
	if !strings.Contains(out, "Removed") {
		t.Fatalf("gc should report what it removed:\n%s", out)
	}

	list := e.aurium("snapshot", "list", "A")
	if !strings.Contains(list, "keep-me") {
		t.Errorf("gc deleted a labelled snapshot:\n%s", list)
	}
}

func TestForkedContainersDoNotShareState(t *testing.T) {
	e := newEnv(t)
	e.aurium("container", "create", "A")
	e.commitIn("A", "shared.txt", "original\n", "A: original")
	e.aurium("fork", "A", "--branch", "A-copy")

	// Work in the fork must not reach the source.
	e.commitIn("A-copy", "fork-only.txt", "x", "fork: own work")
	if e.exists("A", "fork-only.txt") {
		t.Error("a fork's work leaked into the source container")
	}
	// And vice versa.
	e.commitIn("A", "source-only.txt", "y", "source: own work")
	if e.exists("A-copy", "source-only.txt") {
		t.Error("the source's later work leaked into the fork")
	}
	e.git("fsck", "--no-progress")
}
