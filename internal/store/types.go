package store

import "errors"

// ErrNotFound is returned by every Get*/…By* method when no row matches.
var ErrNotFound = errors.New("store: not found")

// Task statuses (§7 tasks.status CHECK).
const (
	TaskCreated   = "created"
	TaskPlanning  = "planning"
	TaskRunning   = "running"
	TaskBlocked   = "blocked"
	TaskReview    = "review"
	TaskPRReady   = "pr_ready"
	TaskMerged    = "merged"
	TaskCompleted = "completed"
	TaskArchived  = "archived"
)

// TaskStatuses is the full set, in lifecycle order.
var TaskStatuses = []string{
	TaskCreated, TaskPlanning, TaskRunning, TaskBlocked,
	TaskReview, TaskPRReady, TaskMerged, TaskCompleted, TaskArchived,
}

// Container statuses (§7 containers.status CHECK).
const (
	ContainerCreating = "creating"
	ContainerQueued   = "queued"
	ContainerRunning  = "running"
	ContainerPaused   = "paused"
	ContainerStopped  = "stopped"
	ContainerStale    = "stale"
	ContainerConflict = "conflict"
	ContainerDrifted  = "drifted"
	ContainerError    = "error"
	ContainerArchived = "archived"
)

var ContainerStatuses = []string{
	ContainerCreating, ContainerQueued, ContainerRunning, ContainerPaused, ContainerStopped,
	ContainerStale, ContainerConflict, ContainerDrifted, ContainerError, ContainerArchived,
}

// How a container came into existence (§6.4).
const (
	OriginFresh   = "fresh"
	OriginFork    = "fork"
	OriginStack   = "stack"
	OriginRestore = "restore"
)

// Agent roles (§9.2).
const (
	RolePrimary = "primary"
	RoleMaster  = "master"
	RoleWorker  = "worker"
)

// Agent statuses (§7 agents.status CHECK).
const (
	AgentStarting = "starting"
	AgentRunning  = "running"
	AgentIdle     = "idle"
	AgentBlocked  = "blocked"
	AgentExited   = "exited"
	AgentError    = "error"
)

// Project is a set of repositories Aurium manages together (§D22).
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
	// Descriptor is the path of this project's aurium.project.yaml. It is
	// empty exactly for standalone projects — a single repository registered
	// by `aurium init` with no descriptor — so "is this a multi-repo project"
	// is a column read rather than a filesystem probe.
	Descriptor string `json:"descriptor,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// Repository is a git repository inside a project. A project may hold several
// (§D22); each keeps its own aurium.yaml for sandbox settings.
type Repository struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	Path       string `json:"path"`
	BaseBranch string `json:"base_branch"`
	Remote     string `json:"remote"`
}

// Task is a unit of work, optionally nested under a parent task.
type Task struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	ParentTaskID string `json:"parent_task_id"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// Container is the fundamental Aurium unit: a worktree, a rootfs, volumes,
// a branch, a recorded base, and the agents running inside it.
type Container struct {
	ID                string `json:"id"`
	ProjectID         string `json:"project_id"`
	TaskID            string `json:"task_id"`
	RepoID            string `json:"repo_id"`
	Branch            string `json:"branch"`
	Slug              string `json:"slug"`
	ParentContainerID string `json:"parent_container_id"`
	// ParentBranch is the git parent this container rebases onto; it may be
	// the repository base branch for a root container.
	ParentBranch     string `json:"parent_branch"`
	OriginSnapshotID string `json:"origin_snapshot_id"`
	OriginKind       string `json:"origin_kind"`
	// BaseSHA is the recorded base (D6): the parent commit this container was
	// last rebased onto. The whole sync engine is built on it.
	BaseSHA        string      `json:"base_sha"`
	PendingBaseSHA string      `json:"pending_base_sha"`
	HeadSHA        string      `json:"head_sha"`
	Driver         string      `json:"driver"`
	RuntimeID      string      `json:"runtime_id"`
	Image          string      `json:"image"`
	Worktree       string      `json:"worktree"`
	Ports          map[int]int `json:"ports"`
	Network        string      `json:"network"`
	Status         string      `json:"status"`
	LastError      string      `json:"last_error"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
}

// Snapshot is the §6.1 record: git tree + rootfs image + volume archives +
// context version, taken while the container was paused.
type Snapshot struct {
	ID             string `json:"id"`
	ContainerID    string `json:"container_id"`
	Seq            int    `json:"seq"`
	Label          string `json:"label"`
	Trigger        string `json:"trigger"`
	HeadSHA        string `json:"head_sha"`
	TreeRef        string `json:"tree_ref"`
	BaseSHA        string `json:"base_sha"`
	ImageRef       string `json:"image_ref"`
	ManifestPath   string `json:"manifest_path"`
	ContextVersion int    `json:"context_version"`
	Bytes          int64  `json:"bytes"`
	CreatedAt      string `json:"created_at"`
	// IncludesConversation is false when the agent ran under
	// agent_placement: host: its transcript lives in ~/.claude on the host
	// machine, outside the rootfs this snapshot captured, so restoring this
	// snapshot does not bring the conversation back. True for in-container
	// placement, where the transcript lives in $HOME inside the captured
	// rootfs and `--continue` resumes it. Source, rootfs and volumes are
	// captured identically either way; this field is the only place that
	// distinction is recorded, because Caps is per-driver and placement is
	// not a driver.
	IncludesConversation bool `json:"includes_conversation"`
	// Note explains IncludesConversation when it is false, so a restore
	// tells the user what they are getting instead of surprising them with
	// a fresh conversation they believed they had resumed.
	Note string `json:"note,omitempty"`
}

// Agent is one agent process in a container. D15: one interactive agent per
// container, so at most one row here has an interactive status.
type Agent struct {
	ID             string `json:"id"`
	ContainerID    string `json:"container_id"`
	Adapter        string `json:"adapter"`
	Role           string `json:"role"`
	ParentAgentID  string `json:"parent_agent_id"`
	TmuxSession    string `json:"tmux_session"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	StartedAt      string `json:"started_at"`
	LastActivityAt string `json:"last_activity_at"`
	// ProviderAccountID is which connected account this agent's credential
	// comes from (§D24). Empty for `shell`, which has no provider.
	ProviderAccountID string `json:"provider_account_id,omitempty"`
	// DisplayName is what the rail shows. Empty means "fall back to the
	// adapter name", so nothing had to be backfilled when it was added.
	DisplayName string `json:"display_name,omitempty"`
}
