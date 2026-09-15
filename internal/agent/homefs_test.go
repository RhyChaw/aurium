package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingHome is a HomeFS that keeps what was written instead of touching a
// disk, so a test can tell the difference between "Prepare wrote the file" and
// "Prepare wrote it somewhere nobody reads".
type recordingHome struct {
	files map[string]string
}

func newRecordingHome() *recordingHome { return &recordingHome{files: map[string]string{}} }

func (r *recordingHome) ReadFile(rel string) ([]byte, error) {
	body, ok := r.files[rel]
	if !ok {
		return nil, nil
	}
	return []byte(body), nil
}

func (r *recordingHome) WriteFile(rel, content string) error {
	r.files[rel] = content
	return nil
}

// Every adapter's Prepare must write through Projection.FS, never to
// Projection.Home with the os package.
//
// This is not a style preference. Home names a path inside the container; the
// daemon calling os.WriteFile on it writes to a same-named path on the host.
// On macOS that fails and no agent can be created at all
// ("mkdir /home/aurium: operation not supported"). On Linux it can quietly
// succeed, leaving the agent's instructions in the host's /home/aurium where
// the container will never look.
//
// The assertion that matters is the second one: Home is a real directory here,
// so an adapter that still writes through it would pass the first check and
// fail this one.
func TestPrepareWritesThroughTheProjectionFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter Adapter
		want    string
	}{
		{"claude", &Claude{}, ".claude/CLAUDE.md"},
		{"codex", &Codex{}, ".codex/AGENTS.md"},
		{"shell", &Shell{}, ".aurium_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			rec := newRecordingHome()

			if err := tc.adapter.Prepare(Projection{
				Home:        home,
				FS:          rec,
				ContextPath: "/work/repo/.aurium/CONTEXT.md",
				MCPCommand:  "aurium-mcp",
				AuriumURL:   "http://127.0.0.1:7770",
			}); err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			if _, ok := rec.files[tc.want]; !ok {
				t.Errorf("Prepare did not write %s through the projection filesystem; wrote %v",
					tc.want, keys(rec.files))
			}

			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("Prepare wrote %v to Home on this machine; Home is a path inside "+
					"the container, so those files are invisible to the agent",
					names)
			}
		})
	}
}

// A nil FS still means the host's filesystem rooted at Home, which is what the
// local driver and host placement rely on.
func TestPrepareFallsBackToTheHostFilesystemWhenFSIsNil(t *testing.T) {
	home := t.TempDir()
	if err := (&Claude{}).Prepare(Projection{
		Home:        home,
		ContextPath: "/work/repo/.aurium/CONTEXT.md",
		MCPCommand:  "aurium-mcp",
		AuriumURL:   "http://127.0.0.1:7770",
	}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); err != nil {
		t.Errorf("with a nil FS, Prepare should write under Home on this machine: %v", err)
	}
}

// ensureImport is additive: a home that already carries the import must not
// grow a second copy of it.
func TestEnsureImportDoesNotDuplicateThroughAProjectionFilesystem(t *testing.T) {
	rec := newRecordingHome()
	p := Projection{Home: "/home/aurium", FS: rec,
		ContextPath: "/work/repo/.aurium/CONTEXT.md",
		MCPCommand:  "aurium-mcp", AuriumURL: "http://127.0.0.1:7770"}

	for i := 0; i < 3; i++ {
		if err := (&Claude{}).Prepare(p); err != nil {
			t.Fatalf("Prepare %d: %v", i, err)
		}
	}
	if n := strings.Count(rec.files[".claude/CLAUDE.md"], "@/work/repo/.aurium/CONTEXT.md"); n != 1 {
		t.Errorf("import appears %d times after three Prepares, want 1:\n%s",
			n, rec.files[".claude/CLAUDE.md"])
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
