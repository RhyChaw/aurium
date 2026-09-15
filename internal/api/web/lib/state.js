// One store, one subscribe.
//
// The dashboard redraws on an event stream that can deliver several frames in
// a tick, so views never read the DOM to find out what they showed last: they
// re-render from this object. Notifications are coalesced into a microtask so
// a burst of ten events costs one repaint, not ten.

const listeners = new Set();
let pending = false;

export const state = {
  // --- connection ---
  conn: "connecting", // connecting | live | down
  // notice is the single line under the tabs. It carries the result of
  // whatever the user just did — an error, or a plain fact like "queued to the
  // agent's inbox" — which is why it is not called `error`.
  notice: null,

  // --- what exists ---
  projects: [],
  /** agentsByProject: project id -> AgentTile[] (the rail's data). */
  agentsByProject: {},
  /** detail: project id -> {repositories, containers, agents, standalone}. */
  detail: {},

  // --- what is selected ---
  panel: "workspace",
  openProject: null,
  openAgent: null,

  // --- live ---
  heartbeat: null,
  events: [],
  eventFilter: "",
  /** follow pins the event list to its newest row. */
  follow: true,
  approvals: [],
  seenApprovals: new Set(),

  // --- chat ---
  transcript: [],
  transcriptAgent: null,
  agentDetail: null,
  sending: false,

  // --- other tabs ---
  usage: null,
  usageWindow: "24h",
  providers: null,
  detected: null,

  // --- setup wizard ---
  // setupDismissed lives only in this in-memory store: it is not persisted, so
  // a daemon that still has nothing in it opens back on the wizard next time
  // the page loads. That is what makes the wizard re-enterable rather than a
  // one-time gate.
  setupDismissed: false,
  preflight: null,
  preflightError: null,

  booted: false,
};

/** MAX_EVENTS bounds the in-memory trail; a window left open for a week must
 *  not grow without limit. */
export const MAX_EVENTS = 500;

export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** notify coalesces a burst of changes into one repaint. */
export function notify() {
  if (pending) return;
  pending = true;
  queueMicrotask(() => {
    pending = false;
    for (const fn of listeners) {
      try {
        fn(state);
      } catch (err) {
        // A throwing view must not take the others down with it, and must not
        // leave the page frozen on a stale frame.
        console.error("aurium: render failed", err);
      }
    }
  });
}

/** update applies a patch and schedules a repaint. */
export function update(patch) {
  Object.assign(state, patch);
  notify();
}
