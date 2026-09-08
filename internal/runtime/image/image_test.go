package image

import (
	"strings"
	"testing"
)

func spec() Spec {
	return Spec{
		BaseImage:  "node:20-alpine",
		AgentLayer: "RUN npm install -g @anthropic-ai/claude-code",
		UID:        501,
		GID:        20,
	}
}

func TestTagIsDeterministic(t *testing.T) {
	a, b := Tag(spec()), Tag(spec())
	if a != b {
		t.Fatalf("Tag must be deterministic: %s != %s", a, b)
	}
	if !strings.HasPrefix(a, "aurium-local/") {
		t.Fatalf("tag %q should be namespaced under aurium-local/", a)
	}
}

// The tag is the cache key. If any input that changes the image's contents
// does not change the tag, containers silently run a stale image — with the
// wrong uid, or without the agent installed.
func TestTagChangesWithEveryInputThatChangesTheImage(t *testing.T) {
	base := Tag(spec())

	cases := map[string]func(*Spec){
		"base image": func(s *Spec) { s.BaseImage = "node:22-alpine" },
		"agent layer": func(s *Spec) {
			s.AgentLayer = "RUN npm install -g @openai/codex"
		},
		"uid": func(s *Spec) { s.UID = 502 },
		"gid": func(s *Spec) { s.GID = 21 },
	}
	for name, mutate := range cases {
		s := spec()
		mutate(&s)
		if Tag(s) == base {
			t.Errorf("changing the %s must change the image tag", name)
		}
	}
}

func TestTagIsAValidDockerReference(t *testing.T) {
	got := Tag(Spec{BaseImage: "ghcr.io/org/My_Image:v1.2", UID: 501, GID: 20})
	// Docker rejects uppercase in a repository name and only allows
	// [a-zA-Z0-9_.-] in a tag.
	repo, tag, ok := strings.Cut(got, ":")
	if !ok {
		t.Fatalf("tag %q has no :tag component", got)
	}
	if repo != strings.ToLower(repo) {
		t.Errorf("repository %q must be lowercase", repo)
	}
	if strings.ContainsAny(tag, "/:@ ") {
		t.Errorf("tag component %q contains characters docker rejects", tag)
	}
}

func TestRenderDockerfileIncludesEverythingAContainerNeeds(t *testing.T) {
	out, err := RenderDockerfile(spec())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM node:20-alpine",
		"@anthropic-ai/claude-code",
		"mkuser.sh 501 20 aurium",
		"aurium-mcp",
		"tmux",
		"zstd",
		"USER aurium",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Dockerfile missing %q\n---\n%s", want, out)
		}
	}
}

// Alpine and Debian have different package managers; picking the wrong one
// fails the build with a confusing error deep in a layer.
func TestPackageInstallMatchesTheBaseDistro(t *testing.T) {
	alpine, _ := RenderDockerfile(Spec{BaseImage: "node:20-alpine", UID: 1, GID: 1})
	if !strings.Contains(alpine, "apk add") {
		t.Errorf("alpine base should use apk:\n%s", alpine)
	}
	debian, _ := RenderDockerfile(Spec{BaseImage: "node:20-bookworm", UID: 1, GID: 1})
	if !strings.Contains(debian, "apt-get") {
		t.Errorf("debian base should use apt-get:\n%s", debian)
	}
}

func TestRenderIsStableAcrossCalls(t *testing.T) {
	a, _ := RenderDockerfile(spec())
	b, _ := RenderDockerfile(spec())
	if a != b {
		t.Fatal("Dockerfile rendering must be deterministic or the cache key lies")
	}
}
