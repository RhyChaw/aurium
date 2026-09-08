// Package image builds Aurium's derived container image (§5.2 layer L1).
//
// The image is a pure function of (base image, agent layer, host uid/gid), so
// its tag is a hash of exactly those inputs. That makes the tag the cache key:
// if it matches, the image is right, and the build is skipped.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/RhyChaw/aurium/assets"
)

// Spec describes the image to build.
type Spec struct {
	// BaseImage is what the project declared, e.g. node:20-alpine.
	BaseImage string
	// AgentLayer is the adapter's Dockerfile RUN fragment.
	AgentLayer string
	// UID and GID are the HOST user's, baked in so files written to a
	// bind-mounted worktree are owned correctly (§5.4).
	UID int
	GID int
	// ExtraPackages are appended to the distro package install.
	ExtraPackages []string
}

// basePackages are needed by every container regardless of language.
var basePackages = []string{"git", "tmux", "bash", "zstd", "ca-certificates"}

// Tag returns the deterministic image reference for a spec.
//
// Every input that changes the image's contents must change this string, or a
// container silently runs a stale image — with the wrong uid, or without the
// agent CLI installed.
func Tag(s Spec) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%d\x00%d\x00%s",
		s.BaseImage, s.AgentLayer, s.UID, s.GID, strings.Join(s.ExtraPackages, ","))
	digest := hex.EncodeToString(h.Sum(nil))[:12]

	return fmt.Sprintf("aurium-local/%s:%s", sanitiseRepo(s.BaseImage), digest)
}

var repoUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// sanitiseRepo turns a base image reference into a legal docker repository
// path component: lowercase, no slashes, no colons.
func sanitiseRepo(base string) string {
	s := strings.ToLower(base)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = repoUnsafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "base"
	}
	return s
}

type templateData struct {
	BaseImage      string
	AgentLayer     string
	PackageInstall string
	UID            int
	GID            int
}

// RenderDockerfile produces the Dockerfile for a spec.
func RenderDockerfile(s Spec) (string, error) {
	raw, err := assets.Templates.ReadFile("templates/Dockerfile.tmpl")
	if err != nil {
		return "", fmt.Errorf("image: read template: %w", err)
	}
	tmpl, err := template.New("dockerfile").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("image: parse template: %w", err)
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, templateData{
		BaseImage:      s.BaseImage,
		AgentLayer:     s.AgentLayer,
		PackageInstall: packageInstall(s),
		UID:            s.UID,
		GID:            s.GID,
	})
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// packageInstall picks the right package manager for the base image. Guessing
// wrong fails the build deep inside a layer with a confusing message, so the
// detection is explicit and the fallback tries both.
func packageInstall(s Spec) string {
	pkgs := append(append([]string{}, basePackages...), s.ExtraPackages...)
	list := strings.Join(pkgs, " ")

	base := strings.ToLower(s.BaseImage)
	switch {
	case strings.Contains(base, "alpine"):
		return "RUN apk add --no-cache " + list
	case strings.Contains(base, "debian"), strings.Contains(base, "ubuntu"),
		strings.Contains(base, "bookworm"), strings.Contains(base, "bullseye"),
		strings.Contains(base, "jammy"), strings.Contains(base, "noble"),
		strings.Contains(base, "slim"):
		return "RUN apt-get update && apt-get install -y --no-install-recommends " +
			list + " && rm -rf /var/lib/apt/lists/*"
	default:
		// Unknown distro: try each manager and keep going. Better a working
		// container missing tmux than a build that cannot start at all.
		return "RUN (apk add --no-cache " + list + ") || " +
			"(apt-get update && apt-get install -y --no-install-recommends " + list +
			" && rm -rf /var/lib/apt/lists/*) || " +
			"(dnf install -y " + list + ") || " +
			"echo 'aurium: could not install base packages; tmux/zstd may be missing' >&2"
	}
}

// Builder builds and caches derived images.
type Builder struct {
	// Bin is the container CLI ("docker" or "podman").
	Bin string
	// MCPBinary is the host path to the linux aurium-mcp binary copied into
	// the image.
	MCPBinary string
	Verbose   bool
}

// Ensure returns the image tag for a spec, building it if it is not present.
func (b *Builder) Ensure(ctx context.Context, s Spec) (string, error) {
	tag := Tag(s)

	// The tag encodes every input, so its presence means the image is correct.
	if err := b.run(ctx, "image", "inspect", tag); err == nil {
		return tag, nil
	}

	dir, err := os.MkdirTemp("", "aurium-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	dockerfile, err := RenderDockerfile(s)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}

	mkuser, err := assets.Templates.ReadFile("templates/mkuser.sh")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "mkuser.sh"), mkuser, 0o755); err != nil {
		return "", err
	}

	// The shim must exist in the build context even when it was not built for
	// this platform yet, or COPY fails with an unhelpful message.
	if err := b.stageMCPBinary(dir); err != nil {
		return "", err
	}

	if err := b.run(ctx, "build", "-t", tag, dir); err != nil {
		return "", fmt.Errorf("image: build %s: %w", tag, err)
	}
	return tag, nil
}

func (b *Builder) stageMCPBinary(dir string) error {
	dst := filepath.Join(dir, "aurium-mcp")
	if b.MCPBinary == "" {
		return fmt.Errorf("image: no aurium-mcp binary configured; " +
			"build it for linux and set Builder.MCPBinary")
	}
	src, err := os.ReadFile(b.MCPBinary)
	if err != nil {
		return fmt.Errorf("image: read aurium-mcp from %s: %w", b.MCPBinary, err)
	}
	return os.WriteFile(dst, src, 0o755)
}

func (b *Builder) run(ctx context.Context, args ...string) error {
	bin := b.Bin
	if bin == "" {
		bin = "docker"
	}
	if b.Verbose {
		fmt.Fprintf(os.Stderr, "+ %s %s\n", bin, strings.Join(args, " "))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if b.Verbose {
		cmd.Stdout = os.Stderr
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %s: %w", bin, strings.Join(args, " "),
			strings.TrimSpace(stderr.String()), err)
	}
	return nil
}
