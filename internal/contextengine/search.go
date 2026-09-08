package contextengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/RhyChaw/aurium/internal/ids"
)

// Hit is one search result.
type Hit struct {
	// Kind is "item" for context items, "doc" for indexed documentation.
	Kind    string  `json:"kind"`
	Key     string  `json:"key"`
	Path    string  `json:"path,omitempty"`
	Heading string  `json:"heading,omitempty"`
	Scope   string  `json:"scope,omitempty"`
	Version int     `json:"version,omitempty"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

// Query searches context items and indexed docs (§8.6).
//
// Results are restricted to what the subject may actually read. Search is the
// easiest place to leak: a snippet from another container's context is still a
// leak, even if the caller could not open the item directly. So the permission
// check runs per hit, not on the query.
func (e *Engine) Query(ctx context.Context, s Subject, q string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 10
	}
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("context: empty query")
	}
	match := ftsQuery(q)

	var hits []Hit

	// Over-fetch, because permission filtering below will discard some and we
	// still want k results where they exist.
	rows, err := e.Store.DB().QueryContext(ctx,
		`SELECT i.scope, i.scope_id, i.key, i.version,
		        snippet(context_fts, 1, '', '', ' … ', 24) AS snip,
		        bm25(context_fts) AS score
		 FROM context_fts
		 JOIN context_items i ON i.rowid = context_fts.rowid
		 WHERE context_fts MATCH ?
		 ORDER BY score LIMIT ?`, match, k*5)
	if err != nil {
		return nil, fmt.Errorf("context: search items: %w", err)
	}

	type candidate struct {
		ref  Ref
		hit  Hit
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var scope, scopeID, key string
		var version int
		var snip string
		var score float64
		if err := rows.Scan(&scope, &scopeID, &key, &version, &snip, &score); err != nil {
			rows.Close()
			return nil, err
		}
		c.ref = Ref{Scope: scope, ScopeID: scopeID, Key: key}
		c.hit = Hit{
			Kind: "item", Key: key, Scope: scope, Version: version,
			Snippet: strings.TrimSpace(snip), Score: -score, // bm25 is negative-better
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Permission-filter after closing the cursor: ResolvePerm queries.
	for _, c := range candidates {
		if len(hits) >= k {
			break
		}
		perm, err := e.ResolvePerm(ctx, s, c.ref)
		if err != nil {
			return nil, err
		}
		if perm.Allows(PermRead) {
			hits = append(hits, c.hit)
		}
	}

	// Project documentation is readable by anyone who can read the project.
	docRows, err := e.Store.DB().QueryContext(ctx,
		`SELECT d.path, COALESCE(d.heading,''),
		        snippet(docs_fts, 2, '', '', ' … ', 24) AS snip,
		        bm25(docs_fts) AS score
		 FROM docs_fts
		 JOIN context_docs d ON d.rowid = docs_fts.rowid
		 WHERE docs_fts MATCH ? AND d.project_id = ?
		 ORDER BY score LIMIT ?`, match, s.ProjectID, k)
	if err == nil {
		for docRows.Next() {
			var path, heading, snip string
			var score float64
			if err := docRows.Scan(&path, &heading, &snip, &score); err != nil {
				break
			}
			hits = append(hits, Hit{
				Kind: "doc", Path: path, Heading: heading,
				Snippet: strings.TrimSpace(snip), Score: -score,
			})
		}
		docRows.Close()
	}
	return hits, nil
}

// ftsQuery turns a user phrase into an FTS5 MATCH expression.
//
// FTS5 has its own syntax, and a stray quote or a bare `-` from an agent's
// prompt is a syntax error rather than a search. Every term is quoted, which
// makes any input a literal term search.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, "")
		if f == "" {
			continue
		}
		quoted = append(quoted, `"`+f+`"`)
	}
	if len(quoted) == 0 {
		return `""`
	}
	return strings.Join(quoted, " OR ")
}

// ---- documentation indexing ----

// IndexDocs walks a project's declared context directories and indexes their
// Markdown, split on headings (§8.6).
//
// Splitting on headings rather than storing whole files matters: an agent
// searching for "rate limiting" wants the section about it, not a 4000-line
// architecture document it will then have to read in full.
func (e *Engine) IndexDocs(ctx context.Context, projectID string, roots []string) (int, error) {
	indexed := 0

	for _, root := range roots {
		expanded := expandHome(root)
		info, err := os.Stat(expanded)
		if err != nil {
			continue // a configured path that does not exist is not an error
		}
		if !info.IsDir() {
			n, err := e.indexFile(ctx, projectID, expanded, expanded)
			if err != nil {
				return indexed, err
			}
			indexed += n
			continue
		}

		err = filepath.WalkDir(expanded, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // an unreadable subtree should not fail the whole index
			}
			if d.IsDir() {
				// Skip the places that are large and never documentation.
				switch d.Name() {
				case ".git", "node_modules", ".aurium", "vendor", "target", "dist":
					return fs.SkipDir
				}
				return nil
			}
			if ext := strings.ToLower(filepath.Ext(path)); ext != ".md" && ext != ".markdown" {
				return nil
			}
			n, err := e.indexFile(ctx, projectID, expanded, path)
			if err != nil {
				return err
			}
			indexed += n
			return nil
		})
		if err != nil {
			return indexed, err
		}
	}
	return indexed, nil
}

// indexFile splits one Markdown file into heading sections and stores them,
// skipping the work entirely when the content hash is unchanged.
func (e *Engine) indexFile(ctx context.Context, projectID, source, path string) (int, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, nil
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	var existing string
	e.Store.DB().QueryRowContext(ctx,
		`SELECT hash FROM context_docs WHERE project_id = ? AND path = ? LIMIT 1`,
		projectID, path).Scan(&existing)
	if existing == hash {
		return 0, nil // unchanged
	}

	if _, err := e.Store.DB().ExecContext(ctx,
		`DELETE FROM context_docs WHERE project_id = ? AND path = ?`, projectID, path); err != nil {
		return 0, err
	}

	now := ids.Now()
	count := 0
	for _, sec := range splitMarkdown(string(body)) {
		if strings.TrimSpace(sec.Body) == "" {
			continue
		}
		if _, err := e.Store.DB().ExecContext(ctx,
			`INSERT INTO context_docs (id, project_id, source, path, heading, content, hash, indexed_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			ids.New("doc"), projectID, source, path, nullIfEmpty(sec.Heading), sec.Body, hash, now); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Section is one heading-delimited chunk of a Markdown file.
type Section struct {
	Heading string
	Body    string
}

// splitMarkdown breaks a document at ATX headings, keeping the heading with
// the text beneath it.
func splitMarkdown(body string) []Section {
	lines := strings.Split(body, "\n")

	var (
		out     []Section
		heading string
		buf     []string
	)
	flush := func() {
		if len(buf) > 0 || heading != "" {
			out = append(out, Section{Heading: heading, Body: strings.TrimSpace(strings.Join(buf, "\n"))})
		}
		buf = nil
	}

	inFence := false
	for _, line := range lines {
		// A `#` inside a fenced code block is not a heading.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "#") {
			flush()
			heading = strings.TrimSpace(strings.TrimLeft(line, "# "))
			buf = append(buf, line)
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
