package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `version: 1

project:
  name: my-product
  description: API, web and infra, worked on together.

repos:
  - path: ./api
    base_branch: main
  - path: ./web
    base_branch: develop

defaults:
  driver: local
  agent: claude

context:
  - ./docs
`

func TestParseAndResolve(t *testing.T) {
	d, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if d.Project.Name != "my-product" || len(d.Repos) != 2 {
		t.Fatalf("parsed wrong: %+v", d)
	}
	if d.Repos[1].BaseBranch != "develop" {
		t.Fatalf("base_branch is per repo: %+v", d.Repos)
	}

	// Relative paths resolve against the descriptor, not the process working
	// directory, or the file would mean different things to the CLI and the
	// daemon.
	d.Path = "/projects/mine/" + Filename
	got, err := d.Resolve("./api")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/projects/mine/api" {
		t.Fatalf("relative resolve = %q", got)
	}
	if got, _ = d.Resolve("/elsewhere/infra"); got != "/elsewhere/infra" {
		t.Fatalf("absolute resolve = %q", got)
	}

	home, _ := os.UserHomeDir()
	if got, _ = d.Resolve("~/code/thing"); got != filepath.Join(home, "code/thing") {
		t.Fatalf("~ resolve = %q", got)
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	_, err := Parse([]byte("version: 1\nproject:\n  name: x\nrepoz: []\n"))
	if err == nil {
		t.Fatal("a mistyped key must fail loudly; silently ignoring it hides a missing repo")
	}
}

func TestValidationRules(t *testing.T) {
	cases := map[string]string{
		"wrong version":     "version: 2\nproject:\n  name: x\n",
		"no name":           "version: 1\nproject:\n  description: x\n",
		"repo with no path": "version: 1\nproject:\n  name: x\nrepos:\n  - base_branch: main\n",
		// Two rows for one worktree would make the guard hook's per-repository
		// "own branch only" rule ambiguous.
		"repo listed twice": "version: 1\nproject:\n  name: x\nrepos:\n  - path: ./a\n  - path: a\n",
	}
	for name, body := range cases {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRenderRoundTrips(t *testing.T) {
	d := &Descriptor{
		Version: 1,
		Project: Meta{Name: "my-product", Description: "one: with a colon"},
		Repos: []Repo{
			{Path: "./api", BaseBranch: "main"},
			{Path: "~/code/web"},
		},
		Defaults: Defaults{Driver: "local", Agent: "claude"},
		Context:  []string{"./docs"},
	}

	back, err := Parse(d.Render())
	if err != nil {
		t.Fatalf("a rendered descriptor must parse: %v\n%s", err, d.Render())
	}
	if back.Project.Description != "one: with a colon" {
		t.Fatalf("a value needing quotes lost its meaning: %q", back.Project.Description)
	}
	if len(back.Repos) != 2 || back.Repos[1].Path != "~/code/web" {
		t.Fatalf("repos did not survive: %+v", back.Repos)
	}
	if back.Defaults.Driver != "local" || back.Context[0] != "./docs" {
		t.Fatalf("defaults or context lost: %+v", back)
	}
	// The comments are the reason Render is hand-written rather than marshalled.
	if !strings.Contains(string(d.Render()), "relative to this file") {
		t.Fatal("the rendered file must explain how paths are resolved")
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "api", "src", "internal")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, Filename)
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Find(deep)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("Find = %q, want %q", got, path)
	}

	if _, err := Find(t.TempDir()); err == nil {
		t.Fatal("a directory in no project must report that, not guess")
	}
}

func TestWriteThenLoad(t *testing.T) {
	dir := t.TempDir()
	d := &Descriptor{Version: 1, Project: Meta{Name: "x"}, Repos: []Repo{{Path: "./a"}}}
	path := filepath.Join(dir, Filename)
	if err := d.Write(path); err != nil {
		t.Fatal(err)
	}
	if d.Path != path {
		t.Fatalf("Write must record where it wrote, got %q", d.Path)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := back.ResolvedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if repos[0].Path != filepath.Join(dir, "a") {
		t.Fatalf("ResolvedRepos = %q", repos[0].Path)
	}
}
