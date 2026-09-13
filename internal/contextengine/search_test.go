package contextengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/store"
)

func TestQueryFindsItemsByContent(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	e := f.engine

	e.Write(ctx, Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "decisions/jwt"},
		0, "We chose asymmetric RS256 signing for tokens.", "human", "")
	e.Write(ctx, Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "decisions/db"},
		0, "Postgres with logical replication.", "human", "")

	hits, err := e.Query(ctx, Subject{
		Role: store.RolePrimary, ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}, "asymmetric signing", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("search found nothing")
	}
	if hits[0].Key != "decisions/jwt" {
		t.Fatalf("top hit = %q, want decisions/jwt", hits[0].Key)
	}
	if hits[0].Snippet == "" {
		t.Error("a hit needs a snippet or the agent must fetch every result to judge it")
	}
}

// Search is the easiest place to leak: a snippet from another container's
// context is a leak even if the item itself could not be opened.
func TestQueryNeverReturnsContextTheSubjectCannotRead(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	e := f.engine

	// A secret in a container that is unrelated to the searcher.
	e.Write(ctx, Ref{Scope: ScopeContainer, ScopeID: f.other.ID, Key: "plan"},
		0, "the pineapple deployment strategy", "human", "")
	// And something the searcher may legitimately see.
	e.Write(ctx, Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "notes"},
		0, "the pineapple is a fruit", "human", "")

	hits, err := e.Query(ctx, Subject{
		Role: store.RolePrimary, ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}, "pineapple", 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range hits {
		if strings.Contains(h.Snippet, "deployment strategy") {
			t.Fatalf("search leaked another container's context: %+v", h)
		}
	}
	found := false
	for _, h := range hits {
		if h.Key == "notes" {
			found = true
		}
	}
	if !found {
		t.Error("the readable item should still be returned")
	}
}

// A stacked child may read its parent's context, so search must reflect that.
func TestQueryIncludesTheParentChain(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()

	f.engine.Write(ctx, Ref{Scope: ScopeContainer, ScopeID: f.parent.ID, Key: "decisions/approach"},
		0, "we settled on the incremental migration approach", "human", "")

	hits, err := f.engine.Query(ctx, Subject{
		Role: store.RolePrimary, ContainerID: f.child.ID, ProjectID: f.project.ID,
	}, "incremental migration", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a stacked child should find its parent's decisions")
	}
}

// FTS5 has its own syntax; a stray quote or operator from an agent's prompt
// must be a search, not a syntax error.
func TestQueryToleratesFTSMetacharacters(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	f.engine.Write(ctx, Ref{Scope: ScopeProject, ScopeID: f.project.ID, Key: "notes"},
		0, "rate limiting uses a token bucket", "human", "")

	subject := Subject{Role: store.RolePrimary, ContainerID: f.parent.ID, ProjectID: f.project.ID}
	for _, q := range []string{
		`"unterminated`, `token AND (bucket`, `-bucket`, `NEAR/`, `*`, `token bucket`,
	} {
		if _, err := f.engine.Query(ctx, subject, q, 5); err != nil {
			t.Errorf("query %q returned an error instead of results: %v", q, err)
		}
	}
}

func TestSplitMarkdownKeepsSectionsWithTheirHeadings(t *testing.T) {
	secs := splitMarkdown(`# Title
intro text

## Rate limiting
we use a token bucket

## Storage
postgres
`)
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(secs), secs)
	}
	if secs[1].Heading != "Rate limiting" || !strings.Contains(secs[1].Body, "token bucket") {
		t.Fatalf("section = %+v", secs[1])
	}
}

// A `#` inside a fenced code block is a comment, not a heading.
func TestSplitMarkdownIgnoresHashesInsideCodeFences(t *testing.T) {
	secs := splitMarkdown("## Setup\n\n```sh\n# install the thing\nnpm ci\n```\n\n## Next\ndone\n")
	if len(secs) != 2 {
		t.Fatalf("a comment inside a fence was treated as a heading: %d sections", len(secs))
	}
	if !strings.Contains(secs[0].Body, "npm ci") {
		t.Errorf("the fenced block was split away from its section: %+v", secs[0])
	}
}

func TestIndexDocsIndexesMarkdownAndSkipsUnchangedFiles(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "architecture.md"),
		[]byte("# Architecture\n\n## Rate limiting\nWe use a token bucket keyed by tenant.\n"), 0o644)
	// Places that are large and never documentation must be skipped.
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "readme.md"),
		[]byte("# Some dependency\ntoken bucket\n"), 0o644)

	n, err := f.engine.IndexDocs(ctx, f.project.ID, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing was indexed")
	}

	// Re-indexing unchanged content must do no work.
	again, err := f.engine.IndexDocs(ctx, f.project.ID, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("re-indexing unchanged files did %d sections of work, want 0", again)
	}

	hits, err := f.engine.Query(ctx, Subject{
		Role: store.RolePrimary, ContainerID: f.parent.ID, ProjectID: f.project.ID,
	}, "token bucket", 10)
	if err != nil {
		t.Fatal(err)
	}

	var docHit *Hit
	for i := range hits {
		if hits[i].Kind == "doc" {
			docHit = &hits[i]
		}
		if strings.Contains(hits[i].Path, "node_modules") {
			t.Error("node_modules was indexed")
		}
	}
	if docHit == nil {
		t.Fatal("indexed documentation is not searchable")
	}
	if docHit.Heading != "Rate limiting" {
		t.Errorf("heading = %q — the section, not the whole file, should be the unit", docHit.Heading)
	}
}

func TestIndexDocsReindexesChangedFiles(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")

	os.WriteFile(path, []byte("# Notes\noriginal content\n"), 0o644)
	f.engine.IndexDocs(ctx, f.project.ID, []string{dir})

	os.WriteFile(path, []byte("# Notes\nrewritten content about zebras\n"), 0o644)
	if _, err := f.engine.IndexDocs(ctx, f.project.ID, []string{dir}); err != nil {
		t.Fatal(err)
	}

	subject := Subject{Role: store.RolePrimary, ContainerID: f.parent.ID, ProjectID: f.project.ID}
	hits, _ := f.engine.Query(ctx, subject, "zebras", 5)
	if len(hits) == 0 {
		t.Fatal("changed content was not re-indexed")
	}
	stale, _ := f.engine.Query(ctx, subject, "original", 5)
	for _, h := range stale {
		if h.Kind == "doc" {
			t.Fatal("the previous version of the file is still indexed")
		}
	}
}

func TestIndexDocsToleratesMissingPaths(t *testing.T) {
	f := newGrantFixture(t)
	if _, err := f.engine.IndexDocs(context.Background(), f.project.ID,
		[]string{"/definitely/not/a/real/path"}); err != nil {
		t.Fatalf("a configured path that does not exist should be skipped, not fail: %v", err)
	}
}
