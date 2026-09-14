// The approvals inbox.
//
// The only part of the dashboard that writes to the world. Approving executes
// the held upstream call inside the daemon (D17), so the button is a real
// action with a real consequence and is disabled while it is in flight.

import { el, mount, shortID } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";

export function renderApprovals(host) {
  const pending = (state.approvals ?? []).filter((a) => (a.status ?? "pending") === "pending");

  mount(host,
    el("p.lede",
      "An agent asked for a capability it does not hold. Approving executes the " +
      "held call inside the daemon and hands the agent the result."),
    pending.length
      ? el("ul.cards", pending.map(card))
      : el("p.empty", "Nothing waiting. No agent is blocked."));

  // The window title is the one signal that reaches you from the app switcher
  // and the dock tooltip when the window is behind something else.
  document.title = pending.length ? `Aurium (${pending.length})` : "Aurium";
}

function card(a) {
  const approve = el("button.act.approve", "Approve");
  const reject = el("button.act.reject", "Reject");

  const decide = async (decision) => {
    approve.disabled = reject.disabled = true;
    try {
      await Aurium.decide(a.id, decision);
      await reloadApprovals();
    } catch (err) {
      update({ error: err.message ?? String(err) });
      approve.disabled = reject.disabled = false;
    }
  };
  approve.addEventListener("click", () => decide("approved"));
  reject.addEventListener("click", () => decide("rejected"));

  const rel = relativeExpiry(a.expires_at);

  return el("li.card",
    el("div.card-top",
      el("span.cap", a.capability ?? "(unnamed capability)"),
      el("span.expiry", { class: expirySoon(rel) ? "soon" : "" }, rel)),
    el("div.card-sub",
      [a.container_id, a.agent_id, a.id].filter(Boolean).map(shortID).join(" · ")),
    a.reason ? el("p.card-reason", a.reason) : null,
    el("div.actions", approve, reject));
}

function expirySoon(rel) {
  return rel === "expired" || /in \d+s$/.test(rel);
}

function relativeExpiry(iso) {
  if (!iso) return "no expiry";
  const ms = new Date(iso).getTime() - Date.now();
  if (Number.isNaN(ms)) return "no expiry";
  if (ms <= 0) return "expired";
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `expires in ${secs}s`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `expires in ${mins}m`;
  return `expires in ${Math.round(mins / 60)}h`;
}

/** notify reaches a human who is not looking at the window. A blocked agent is
 *  an idle agent, and the only fix is a human's attention. */
async function notify(title, body) {
  try {
    const n = window.__TAURI__?.notification;
    if (!n) return;
    let granted = await n.isPermissionGranted();
    if (!granted) granted = (await n.requestPermission()) === "granted";
    if (granted) n.sendNotification({ title, body });
  } catch {
    /* a missing notification must never break the panel */
  }
}

export async function reloadApprovals() {
  try {
    const data = await Aurium.approvals();
    const list = Array.isArray(data) ? data : data?.approvals ?? [];

    // Only announce approvals this page has not seen, so a reconnect or a
    // periodic refresh does not re-announce the same blocked agent.
    const fresh = list.filter((a) => a.id && !state.seenApprovals.has(a.id));
    for (const a of list) if (a.id) state.seenApprovals.add(a.id);
    update({ approvals: list });

    if (state.booted && fresh.length === 1) {
      notify("Aurium: approval needed", fresh[0].capability ?? "An agent is waiting.");
    } else if (state.booted && fresh.length > 1) {
      notify("Aurium: approvals needed", `${fresh.length} agents are waiting on you.`);
    }
  } catch (err) {
    update({ error: err.message ?? String(err) });
  }
}
