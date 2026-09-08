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

// Project is a repository root Aurium manages.
type Project struct {
	ID        string
	Name      string
	Root      string
	CreatedAt string
}

// Repository is a git repository inside a project. The data model allows
// several per project (§2 non-goals: the runtime does not yet).
type Repository struct {
	ID         string
	ProjectID  string
	Path       string
	BaseBranch string
	Remote     string
}

// Task is a unit of work, optionally nested under a parent task.
type Task struct {
	ID           string
	ProjectID    string
	Title        string
	Status       string
	ParentTaskID string
	CreatedAt    string
	UpdatedAt    string
}

// Container is the fundamental Aurium unit: a worktree, a rootfs, volumes,
// a branch, a recorded base, and the agents running inside it.
type Container struct {
	ID                string
	ProjectID         string
	TaskID            string
	RepoID            string
	Branch            string
	Slug              string
	ParentContainerID string
	// ParentBranch is the git parent this container rebases onto; it may be
	// the repository base branch for a root container.
	ParentBranch     string
	OriginSnapshotID string
	OriginKind       string
	// BaseSHA is the recorded base (D6): the parent commit this container was
	// last rebased onto. The whole sync engine is built on it.
	BaseSHA        string
	PendingBaseSHA string
	HeadSHA        string
	Driver         string
	RuntimeID      string
	Image          string
	Worktree       string
	Ports          map[int]int
	Network        string
	Status         string
	LastError      string
	CreatedAt      string
	UpdatedAt      string
}

// Snapshot is the §6.1 record: git tree + rootfs image + volume archives +
// context version, taken while the container was paused.
type Snapshot struct {
	ID             string
	ContainerID    string
	Seq            int
	Label          string
	Trigger        string
	HeadSHA        string
	TreeRef        string
	BaseSHA        string
	ImageRef       string
	ManifestPath   string
	ContextVersion int
	Bytes          int64
	CreatedAt      string
}

// Agent is one agent process in a container. D15: one interactive agent per
// container, so at most one row here has an interactive status.
type Agent struct {
	ID             string
	ContainerID    string
	Adapter        string
	Role           string
	ParentAgentID  string
	TmuxSession    string
	Model          string
	Status         string
	StartedAt      string
	LastActivityAt string
}
