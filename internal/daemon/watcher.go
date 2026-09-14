package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/gitx"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/stack"
	"github.com/RhyChaw/aurium/internal/store"
)

// Intervals from §6.6. They are fields rather than constants so tests can
// drive the watcher without waiting real seconds.
type Intervals struct {
	RefPoll   time.Duration
	Reconcile time.Duration
	IdlePause time.Duration
}

// DefaultIntervals matches §6.6.
func DefaultIntervals() Intervals {
	return Intervals{
		RefPoll:   2 * time.Second,
		Reconcile: 30 * time.Second,
		IdlePause: 60 * time.Second,
	}
}

// Watcher keeps the database honest about the world (§6.6).
type Watcher struct {
	App       *app.App
	Log       *slog.Logger
	Intervals Intervals

	// seenStale remembers which containers have already been reported stale,
	// so a parent that moved once produces one notification rather than one
	// every two seconds.
	seenStale map[string]string
}

func NewWatcher(a *app.App, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{App: a, Log: log, Intervals: DefaultIntervals(), seenStale: map[string]string{}}
}

// Run polls until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	refs := time.NewTicker(w.Intervals.RefPoll)
	reconcile := time.NewTicker(w.Intervals.Reconcile)
	defer refs.Stop()
	defer reconcile.Stop()

	// Once, immediately. Reconcile is what hands forgotten containers back to
	// the local driver, and waiting for the first tick would leave every
	// container made before a restart unusable for thirty seconds — with no
	// sign of why, since the row and the worktree are both still there.
	w.Reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-refs.C:
			w.CheckParents(ctx)
		case <-reconcile.C:
			w.Reconcile(ctx)
		}
	}
}

// CheckParents marks children stale when their parent moves (§6.5).
//
// This path only reads git today. Anything here that starts to MUTATE a
// worktree — auto-sync is the obvious next one — must take the container's
// lease first (store.WithLease), because the CLI runs the same git operations
// in the same worktrees from a separate process.
func (w *Watcher) CheckParents(ctx context.Context) {
	projects, err := w.App.Store.ListProjects(ctx)
	if err != nil {
		return
	}
	for _, p := range projects {
		containers, err := w.App.Store.ListContainers(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, c := range containers {
			if c.Status == store.ContainerArchived || c.Worktree == "" {
				continue
			}
			// Skip a container somebody is actively rebasing: git state
			// observed mid-rebase is a snapshot of a half-finished operation,
			// and reporting it would flap the container's status.
			if _, err := w.App.Store.GetLease(ctx, c.ID); err == nil {
				continue
			}

			res, err := stack.CheckEligibility(ctx, gitx.New(c.Worktree), stack.Target{
				Branch: c.Branch, ParentBranch: c.ParentBranch, BaseSHA: c.BaseSHA,
			})
			if err != nil || res.Eligibility != stack.Eligible {
				if res.Eligibility == stack.UpToDate {
					delete(w.seenStale, c.ID)
				}
				continue
			}

			// Report once per distinct parent tip, not once per poll.
			if w.seenStale[c.ID] == res.ParentTip {
				continue
			}
			w.seenStale[c.ID] = res.ParentTip

			_ = w.App.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerStale,
				"parent "+c.ParentBranch+" moved")
			_ = w.App.Events.Emit(ctx, events.Event{
				Type: events.ContainerParentChanged, Actor: events.ActorDaemon,
				ProjectID: p.ID, ContainerID: c.ID,
				Payload: map[string]any{
					"parent": c.ParentBranch, "parent_tip": res.ParentTip,
					"commits": res.Behind,
				},
			})
		}
	}
}

// Reconcile brings the database back in line with the container runtime, which
// is what makes the daemon survive a crash or a `docker rm` behind its back.
func (w *Watcher) Reconcile(ctx context.Context) {
	projects, err := w.App.Store.ListProjects(ctx)
	if err != nil {
		return
	}
	for _, p := range projects {
		containers, err := w.App.Store.ListContainers(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, c := range containers {
			if c.RuntimeID == "" || c.Status == store.ContainerArchived {
				continue
			}
			drv, err := w.App.Manager.Drivers.Get(c.Driver)
			if err != nil {
				continue
			}
			// Hand back containers the driver cannot remember. The local
			// driver's registry lives in memory, so every container made
			// before a daemon restart became "runtime object not found" —
			// with the row still in the database and the worktree still on
			// disk. Adoption happens before Inspect, or reconcile would mark
			// them stopped and the user would watch a fleet die on restart.
			if adopter, ok := drv.(driver.Adopter); ok {
				adopter.Adopt(c.RuntimeID, driver.Spec{Workdir: c.Worktree})
			}

			st, err := drv.Inspect(ctx, c.RuntimeID)
			if err != nil {
				// The container is gone from the runtime. Say so rather than
				// leaving a row claiming it is running.
				if c.Status == store.ContainerRunning {
					_ = w.App.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerStopped,
						"container is no longer present in the runtime")
				}
				continue
			}
			switch {
			case st.Paused && c.Status != store.ContainerPaused:
				_ = w.App.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerPaused, "")
			case st.Running && c.Status == store.ContainerStopped:
				_ = w.App.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerRunning, "")
			case !st.Running && !st.Paused && c.Status == store.ContainerRunning:
				_ = w.App.Store.UpdateContainerStatus(ctx, c.ID, store.ContainerStopped, "")
			}
		}
	}
}
