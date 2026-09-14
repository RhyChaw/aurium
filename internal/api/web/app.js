// Aurium dashboard v1 — the agent OS surface.
//
// The daemon serves this page with the host token injected, so everything here
// is same-origin and carries the credential the CLI uses. There is no build
// step: these are ES modules the browser loads directly, which is why `make
// build` still needs nothing but Go.
//
// Structure: lib/ is plumbing (DOM, API, state, stream), views/ are screens.
// One store, one subscribe; views re-render from state rather than reading the
// DOM to find out what they last drew.

import { $, el, mount } from "./lib/dom.js";
import { readToken } from "./lib/api.js";
import { state, update, subscribe } from "./lib/state.js";
import { connect } from "./lib/stream.js";
import { initTheme } from "./lib/theme.js";
import { Aurium } from "./lib/api.js";

import { renderHome, reloadProjects } from "./views/home.js";
import { renderWorkspace } from "./views/workspace.js";
import { renderApprovals, reloadApprovals } from "./views/approvals.js";
import { renderUsage, reloadUsage, reloadSpendStrip } from "./views/usage.js";
import { renderProviders, reloadProviders } from "./views/providers.js";
import { renderEvents } from "./views/events.js";
import { recordEvent, stopHeartbeat } from "./views/heartbeat.js";
import { refreshProjectAgents, refreshTranscript, refreshAllAgents } from "./views/rail.js";

const PANELS = {
  workspace: { label: "Workspace", render: renderWorkspace },
  home: { label: "Projects", render: renderHome },
  approvals: { label: "Approvals", render: renderApprovals },
  usage: { label: "Usage", render: renderUsage },
  providers: { label: "Providers", render: renderProviders },
  events: { label: "Events", render: renderEvents },
};

const ORDER = ["workspace", "home", "approvals", "usage", "providers", "events"];

// ---- rendering ----

function render() {
  renderChrome();

  const host = $("#panel");
  const panel = PANELS[state.panel] ?? PANELS.workspace;

  // Each panel owns its host element. Swapping the panel replaces the host so
  // one panel's leftover nodes can never appear under another.
  if (host.dataset.panel !== state.panel) {
    host.dataset.panel = state.panel;
    host.replaceChildren();
  }
  panel.render(host);
}

function renderChrome() {
  const pending = (state.approvals ?? []).filter((a) => (a.status ?? "pending") === "pending").length;

  mount($("#tabs"), ORDER.map((key) =>
    el("button.tab", {
      class: state.panel === key ? "is-active" : "",
      onclick: () => selectPanel(key),
    },
      PANELS[key].label,
      key === "approvals" && pending
        ? el("span.count", String(pending))
        : null)));

  const conn = $("#conn");
  conn.className = `conn conn-${state.conn}`;
  conn.textContent = state.conn === "live" ? "live"
    : state.conn === "connecting" ? "connecting" : "reconnecting";

  const project = state.projects.find((p) => p.id === state.openProject);
  $("#project-name").textContent = project?.name ?? (state.projects.length ? "—" : "no projects");

  const bar = $("#notice");
  bar.textContent = state.error ?? "";
  bar.hidden = !state.error;
}

function selectPanel(key) {
  // The notice reports the result of something the user just did on the panel
  // they were on. Carrying it to the next one would leave a stale sentence
  // over an unrelated screen.
  const leavingWorkspace = state.panel === "workspace" && key !== "workspace";
  update({ panel: key, error: null });
  // The pulse repaints ten times a second; there is no reason for it to do so
  // while nobody can see it.
  if (leavingWorkspace) stopHeartbeat();
  // Tabs load their own data on arrival rather than on every event: usage and
  // providers change on a human timescale, and refreshing them on a busy
  // stream would be constant work for a pane nobody is looking at.
  if (key === "usage") reloadUsage();
  if (key === "providers") reloadProviders();
  if (key === "approvals") reloadApprovals();
}

// ---- live updates ----
//
// Every event moves the pulse. Only the events that changed something the
// current view shows cause a reload, because a fleet of twenty agents produces
// a great many events and refetching everything on each one would spend the
// whole budget on work nobody sees.

function onEvent(e) {
  recordEvent(e);
  update({}); // the trail and the pulse both want a repaint

  const type = e.type ?? "";

  if (type.startsWith("approval.")) {
    reloadApprovals();
    // An approval changes an agent's colour band, and the rail is where that
    // is noticed.
    if (state.openProject) refreshProjectAgents(state.openProject);
    return;
  }

  if (type.startsWith("agent.") || type.startsWith("container.")) {
    if (e.project_id) refreshProjectAgents(e.project_id);
    else if (state.openProject) refreshProjectAgents(state.openProject);
  }

  if (type.startsWith("agent.message") && state.openAgent) {
    // Only when the message concerns the agent on screen.
    if (!e.agent_id || e.agent_id === state.openAgent) refreshTranscript(state.openAgent);
  }

  if (type.startsWith("project.")) reloadProjects();
  if (type === "usage.recorded" && state.panel === "usage") reloadUsage();
}

// The heartbeat's own vitals are a poll, not a stream: they are aggregates,
// and recomputing them on every event would be a query storm for numbers that
// only need to be roughly current.
async function pollHeartbeat() {
  try {
    update({ heartbeat: await Aurium.heartbeat(state.openProject) });
  } catch {
    /* the pane keeps its last good frame */
  }
}

// ---- keyboard ----
//
// This is a window someone leaves open. Reaching for the mouse to clear an
// approval queue is the wrong shape.

function initKeys() {
  window.addEventListener("keydown", (e) => {
    const active = document.activeElement;
    const typing =
      /^(INPUT|TEXTAREA|SELECT)$/.test(active?.tagName ?? "") ||
      active?.isContentEditable === true ||
      // A modal owns the keyboard while it is open; a stray "a" inside the new
      // project form must not reach the accelerators.
      document.querySelector("dialog[open]") !== null;
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

    if (e.key >= "1" && e.key <= String(ORDER.length)) {
      selectPanel(ORDER[Number(e.key) - 1]);
      return;
    }
    if (e.key === "r") {
      bootRefresh();
      return;
    }
    // a / x act on the first pending approval, which is the one on top — but
    // only while the Approvals panel is actually open.
    //
    // Approving executes a held upstream call inside the daemon (D17), so it
    // is not an action a stray keystroke may reach. Making it panel-local
    // means the approval you act on is one you were looking at.
    if ((e.key === "a" || e.key === "x") && state.panel === "approvals") {
      document.querySelector(`#panel .card ${e.key === "a" ? ".approve" : ".reject"}`)?.click();
      return;
    }
    if (e.key === "j" || e.key === "k") {
      moveSelection(e.key === "j" ? 1 : -1);
    }
  });
}

/** moveSelection walks the rail's tiles in the order they are drawn. */
function moveSelection(delta) {
  const tiles = Array.from(document.querySelectorAll("#panel .tile"));
  if (!tiles.length) return;
  const at = tiles.findIndex((t) => t.classList.contains("is-selected"));
  const next = tiles[Math.min(Math.max(at + delta, 0), tiles.length - 1)] ?? tiles[0];
  next.click();
  next.scrollIntoView({ block: "nearest" });
}

// ---- boot ----

async function bootRefresh() {
  await reloadProjects();
  await Promise.all([
    refreshAllAgents(), reloadApprovals(), pollHeartbeat(), reloadSpendStrip(),
  ]);
}

async function main() {
  initTheme($("#theme"));

  if (!readToken()) {
    update({ conn: "down", error: "No token in this page. Open the dashboard with `aurium dashboard`." });
    subscribe(render);
    render();
    return;
  }

  subscribe(render);
  initKeys();

  try {
    await bootRefresh();
  } catch (err) {
    update({ conn: "down", error: err.message ?? String(err) });
    return;
  }

  // Open the first project so the workspace is not empty on arrival, and land
  // on Projects when there are none — the only useful thing to do then is make
  // one.
  if (state.projects.length) {
    update({ openProject: state.projects[0].id, panel: "workspace" });
  } else {
    update({ panel: "home" });
  }

  state.booted = true;
  connect(onEvent);

  setInterval(pollHeartbeat, 5000);
  // The spend line is an aggregate over a day; refreshing it every few seconds
  // would be a query storm for a number that moves slowly.
  setInterval(reloadSpendStrip, 60000);
  // Approval cards show a relative expiry, so they must re-render while idle.
  setInterval(() => state.panel === "approvals" && update({}), 15000);
}

main();
