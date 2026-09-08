package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a git repository with one commit on `main` and returns its
// root. Every git test builds on this rather than mocking git: the whole point
// of this package is that real git semantics are load-bearing (D1).
func initRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run(t, root, "git", "init", "-q", "-b", "main")
	run(t, root, "git", "config", "user.email", "test@aurium.dev")
	run(t, root, "git", "config", "user.name", "Aurium Test")
	run(t, root, "git", "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(root, "README.md"), "# fixture\n")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-qm", "initial")
	return root
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ctx() context.Context { return context.Background() }
