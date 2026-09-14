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
  error: null,

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
