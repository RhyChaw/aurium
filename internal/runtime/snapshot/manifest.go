// Package snapshot implements §6: snapshots, restore, fork and stack.
//
// D14 defines a snapshot as (git tree, rootfs image, volume archives, context
// version, agent config), taken while the container is paused. Process memory
// is deliberately excluded: CRIU is Linux-only, experimental, and absent from
// Docker Desktop and OrbStack, so a snapshot promising resumable processes
// could not be honoured. Agent *conversation* state survives regardless,
// because it lives in the container's $HOME, which the rootfs commit captures.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaVersion of manifest.json.
const SchemaVersion = 1

// Trigger records why a snapshot was taken (§6.1).
const (
	TriggerManual         = "manual"
	TriggerPreSync        = "pre_sync"
	TriggerPreRestore     = "pre_restore"
	TriggerTaskTransition = "task_transition"
	TriggerAuto           = "auto"
)

// Manifest is the on-disk description of a snapshot (§6.1).
type Manifest struct {
	Schema    int    `json:"schema"`
	ID        string `json:"id"`
	Container string `json:"container"`
	Seq       int    `json:"seq"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at"`
	Trigger   string `json:"trigger"`

	Git    GitPart    `json:"git"`
	Rootfs RootfsPart `json:"rootfs"`
	// Volumes are per-container volumes only; shared caches are mounted, not
	// captured, so restoring never rolls back another container's cache.
	Volumes []VolumePart `json:"volumes"`

	ContextVersion int         `json:"context_version"`
	Agents         []AgentPart `json:"agents"`
	// EnvHash is a digest of the sandbox section of aurium.yaml. Restoring
	// into a project whose environment declaration has changed is a warning,
	// not a silent surprise.
	EnvHash string         `json:"env_hash"`
	Ports   map[string]int `json:"ports,omitempty"`
}

// GitPart is the source half of a snapshot.
type GitPart struct {
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
	// TreeRef points at a commit whose tree is the full worktree state,
	// including untracked files — which HEAD does not capture.
	TreeRef string `json:"tree_ref"`
	BaseSHA string `json:"base_sha"`
	Parent  string `json:"parent"`
}

// RootfsPart is the committed container filesystem. Empty on drivers that
// cannot commit a rootfs; the manifest says so rather than pretending.
type RootfsPart struct {
	Image     string `json:"image,omitempty"`
	Digest    string `json:"digest,omitempty"`
	BaseImage string `json:"base_image,omitempty"`
	// Captured is false when the driver has no snapshot capability, which
	// makes restore's degraded behaviour explicit instead of surprising.
	Captured bool `json:"captured"`
}

// VolumePart is one archived per-container volume.
type VolumePart struct {
	Name    string `json:"name"`
	Mount   string `json:"mount"`
	Archive string `json:"archive"`
	Bytes   int64  `json:"bytes"`
}

// AgentPart records enough to bring an agent back (§6.3 step 6).
type AgentPart struct {
	Adapter    string `json:"adapter"`
	Role       string `json:"role"`
	Session    string `json:"session"`
	Model      string `json:"model,omitempty"`
	ResumeHint string `json:"resume_hint,omitempty"`
	CanResume  bool   `json:"can_resume"`
}

// Dir is the storage directory for a snapshot (§6.1).
func Dir(home, projectID, containerID string, seq int) string {
	return filepath.Join(home, "snapshots", projectID, containerID, fmt.Sprintf("%d", seq))
}

// TreeRef is the git ref a snapshot's worktree tree is stored under.
func TreeRef(containerID string, seq int) string {
	return fmt.Sprintf("refs/aurium/snap/%s/%d", containerID, seq)
}

// ImageRef is the docker tag a snapshot's rootfs is committed to.
func ImageRef(containerID string, seq int) string {
	return fmt.Sprintf("aurium-snap/%s:%d", containerID, seq)
}

// Write saves a manifest to dir/manifest.json.
func (m *Manifest) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: marshal manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(body, '\n'), 0o644)
}

// ReadManifest loads a manifest from a snapshot directory.
func ReadManifest(dir string) (*Manifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("snapshot: parse manifest in %s: %w", dir, err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("snapshot: manifest schema %d is not supported (this build reads %d)",
			m.Schema, SchemaVersion)
	}
	return &m, nil
}
