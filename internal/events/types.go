package events

// Event types (§11.1). These are the audit log, the dashboard feed and the
// plugin surface, so the names are a public contract.
const (
	ProjectCreated = "project.created"

	TaskCreated      = "task.created"
	TaskTransitioned = "task.transitioned"
	TaskCompleted    = "task.completed"

	ContainerCreated       = "container.created"
	ContainerStarted       = "container.started"
	ContainerPaused        = "container.paused"
	ContainerResumed       = "container.resumed"
	ContainerStopped       = "container.stopped"
	ContainerDestroyed     = "container.destroyed"
	ContainerSnapshotTaken = "container.snapshot.created"
	ContainerRestored      = "container.restored"
	ContainerForked        = "container.forked"
	ContainerStacked       = "container.stacked"
	ContainerSynced        = "container.synced"
	ContainerStale         = "container.stale"
	ContainerConflict      = "container.conflict"
	ContainerParentChanged = "container.parent_changed"
	ContainerDrifted       = "container.drifted"
	ContainerQueued        = "container.queued"

	AgentStarted          = "agent.started"
	AgentIdle             = "agent.idle"
	AgentActive           = "agent.active"
	AgentBlocked          = "agent.blocked"
	AgentExited           = "agent.exited"
	AgentMessageSent      = "agent.message.sent"
	AgentMessageDelivered = "agent.message.delivered"
	AgentMessageAcked     = "agent.message.acked"

	ContextUpdated         = "context.updated"
	ContextProposed        = "context.proposed"
	ContextProposalDecided = "context.proposal.decided"

	IntegrationConnected = "integration.connected"
	IntegrationRevoked   = "integration.revoked"
	IntegrationCall      = "integration.call"

	ApprovalRequested = "approval.requested"
	ApprovalDecided   = "approval.decided"
	ApprovalExpired   = "approval.expired"

	PRCreated = "pr.created"
	PRUpdated = "pr.updated"
	PRMerged  = "pr.merged"

	DaemonStarted    = "daemon.started"
	DaemonReconciled = "daemon.reconciled"
)

// Actors. An event always records who caused it, because "why did my branch
// move" is the question the audit log exists to answer.
const (
	ActorHuman  = "human"
	ActorDaemon = "daemon"
)

// ActorAgent formats an agent actor.
func ActorAgent(agentID string) string { return "agent:" + agentID }
