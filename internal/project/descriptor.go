// Package project loads and writes aurium.project.yaml, the descriptor that
// says which repositories belong to one project (§D22).
//
// It is deliberately separate from internal/config. That package describes how
// to build a sandbox for *one* repository — driver, image, hooks, grants — and
// every repo keeps its own. This one describes membership, and nothing else.
// Merging them would mean every repo in a project carrying a copy of the
// project's shape, with no answer for which copy is right.
package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Filename is the descriptor Aurium looks for at a project root.
const Filename = "aurium.project.yaml"

// SchemaVersion is the only version this build understands.
const SchemaVersion = 1

// Descriptor is the whole of aurium.project.yaml.
type Descriptor struct {
	Version  int      `yaml:"version"`
	Project  Meta     `yaml:"project"`
	Repos    []Repo   `yaml:"repos"`
	Defaults Defaults `yaml:"defaults"`
	// Context lists directories indexed for aurium_context_query across the
	// whole project, in addition to whatever each repo's aurium.yaml names.
	Context []string `yaml:"context"`

	// Path is where this descriptor was loaded from; not part of the file.
	Path string `yaml:"-"`
}

// Meta names the project.
type Meta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// Repo is one member repository.
type Repo struct {
	// Path may be relative to the descriptor, absolute, or ~-rooted. It is
	// stored exactly as written so the file stays portable between machines;
	// Resolve turns it into a real path.
	Path string `yaml:"path"`
	// BaseBranch is this repo's trunk. It is per-repo because an API repo on
	// `main` and an infra repo on `master` is an ordinary situation, and a
	// single project-wide value would be wrong for one of them.
	BaseBranch string `yaml:"base_branch,omitempty"`
}

// Defaults seed a member repo's aurium.yaml when Aurium writes one. They are
// defaults, not policy: a repo that already has an aurium.yaml keeps it.
type Defaults struct {
	Driver string `yaml:"driver,omitempty"`
	Image  string `yaml:"image,omitempty"`
	Agent  string `yaml:"agent,omitempty"`
}

// Load reads and validates a descriptor.
func Load(path string) (*Descriptor, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	d, err := Parse(body)
	if err != nil {
		return nil, fmt.Errorf("project: %s: %w", path, err)
	}
	d.Path = path
	return d, nil
}

// Parse decodes and validates descriptor bytes.
func Parse(body []byte) (*Descriptor, error) {
	var d Descriptor
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	// Unknown keys are errors, matching internal/config and for the same
	// reason: a mistyped repo path that is silently ignored produces a project
	// quietly missing a third of itself.
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *Descriptor) validate() error {
	if d.Version != SchemaVersion {
		return fmt.Errorf("version %d is not supported (this build understands version %d)",
			d.Version, SchemaVersion)
	}
	if strings.TrimSpace(d.Project.Name) == "" {
		return fmt.Errorf("project.name is required")
	}

	seen := map[string]bool{}
	for i, r := range d.Repos {
		if strings.TrimSpace(r.Path) == "" {
			return fmt.Errorf("repos[%d] needs a path", i)
		}
		// Two entries for one directory would create two repository rows for
		// the same worktree, and the guard hook's "own branch only" rule is
		// enforced per repository. That is a correctness problem, not an
		// untidiness one.
		key := filepath.Clean(r.Path)
		if seen[key] {
			return fmt.Errorf("repos: %q is listed twice", r.Path)
		}
		seen[key] = true
	}
	return nil
}

// Dir is the project root: the directory holding the descriptor.
func (d *Descriptor) Dir() string {
	if d.Path == "" {
		return ""
	}
	return filepath.Dir(d.Path)
}

// Resolve turns a repo path as written into an absolute path.
//
// Relative paths are relative to the descriptor, not to the process's working
// directory: the file must mean the same thing whoever loads it and from
// wherever.
func (d *Descriptor) Resolve(repoPath string) (string, error) {
	return ResolveFrom(d.Dir(), repoPath)
}

// ResolveFrom expands ~ and resolves a possibly-relative path against base.
func ResolveFrom(base, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("project: empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("project: expanding %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if base == "" {
		abs, err := filepath.Abs(p)
		return abs, err
	}
	return filepath.Clean(filepath.Join(base, p)), nil
}

// ResolvedRepos returns each member repo with its path made absolute.
func (d *Descriptor) ResolvedRepos() ([]Repo, error) {
	out := make([]Repo, 0, len(d.Repos))
	for _, r := range d.Repos {
		abs, err := d.Resolve(r.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, Repo{Path: abs, BaseBranch: r.BaseBranch})
	}
	return out, nil
}

// Find locates a descriptor by walking up from dir, so a command run deep
// inside a member repository still finds the project it belongs to.
func Find(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(abs, Filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("project: no %s found in %s or any parent directory", Filename, dir)
		}
		abs = parent
	}
}

// Render writes the descriptor as the YAML a human will later edit by hand.
//
// It is written by hand rather than marshalled so the comments survive. A
// generated file with no explanation of what `defaults` seeds, or why paths are
// relative to the file, is a file people are afraid to edit.
func (d *Descriptor) Render() []byte {
	var b strings.Builder
	b.WriteString("version: 1\n\n")
	b.WriteString("project:\n")
	fmt.Fprintf(&b, "  name: %s\n", yamlScalar(d.Project.Name))
	if d.Project.Description != "" {
		fmt.Fprintf(&b, "  description: %s\n", yamlScalar(d.Project.Description))
	}

	b.WriteString("\n# The repositories worked on together. Paths are relative to this file\n")
	b.WriteString("# (or absolute, or ~-rooted), so the descriptor travels with the project.\n")
	b.WriteString("# Each repo keeps its own aurium.yaml for sandbox settings.\n")
	if len(d.Repos) == 0 {
		b.WriteString("repos: []\n")
	} else {
		b.WriteString("repos:\n")
		for _, r := range d.Repos {
			fmt.Fprintf(&b, "  - path: %s\n", yamlScalar(r.Path))
			if r.BaseBranch != "" {
				fmt.Fprintf(&b, "    base_branch: %s\n", yamlScalar(r.BaseBranch))
			}
		}
	}

	if d.Defaults != (Defaults{}) {
		b.WriteString("\n# Seeds a member repo's aurium.yaml when Aurium writes one. A repo that\n")
		b.WriteString("# already has one keeps it.\n")
		b.WriteString("defaults:\n")
		if d.Defaults.Driver != "" {
			fmt.Fprintf(&b, "  driver: %s\n", yamlScalar(d.Defaults.Driver))
		}
		if d.Defaults.Image != "" {
			fmt.Fprintf(&b, "  image: %s\n", yamlScalar(d.Defaults.Image))
		}
		if d.Defaults.Agent != "" {
			fmt.Fprintf(&b, "  agent: %s\n", yamlScalar(d.Defaults.Agent))
		}
	}

	if len(d.Context) > 0 {
		b.WriteString("\n# Indexed for aurium_context_query across the whole project.\n")
		b.WriteString("context:\n")
		for _, c := range d.Context {
			fmt.Fprintf(&b, "  - %s\n", yamlScalar(c))
		}
	}
	return []byte(b.String())
}

// Write renders the descriptor to path.
func (d *Descriptor) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, d.Render(), 0o644); err != nil {
		return fmt.Errorf("project: write %s: %w", path, err)
	}
	d.Path = path
	return nil
}

// yamlScalar quotes a value when leaving it bare would change its meaning.
// A project named "yes", a path with a colon in it, or a leading "*" are all
// valid inputs that unquoted YAML would read as something else.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#{}[],&*?|<>=!%@`\"'\n\t") || strings.TrimSpace(s) != s {
		return fmt.Sprintf("%q", s)
	}
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return fmt.Sprintf("%q", s)
	}
	if s[0] == '-' {
		return fmt.Sprintf("%q", s)
	}
	return s
}
