// The left rail: every project at once, each a container of thin agent tiles.
//
// Showing only the open project would defeat the point. The rail exists for
// peripheral vision — the question it answers is "is anything on fire",
// including in a project you are not looking at — so it draws them all and
// leans on colour to carry the answer from across the room.

import { el, mount, shortID, ago } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";

// The four colour bands. The daemon computes which one an agent is in
// (internal/api/projects.go), so the CLI and any future client agree with this
// page about what red means.
const STATE_LABEL = {
  starting: "spinning up",
  working: "working",
  done: "done",
  attention: "needs you",
};

export function renderRail(host) {
  const projects = state.projects ?? [];

  if (!projects.length) {
    mount(host,
      el("div.rail-empty",
        el("p", "No projects yet."),
        el("button.act.primary", { onclick: () => update({ panel: "home" }) },
          "Create one")));
    return;
  }

  mount(host, projects.map((p) => projectBlock(p)));
}

function projectBlock(p) {
  const agents = state.agentsByProject[p.id] ?? [];
  const isOpen = state.openProject === p.id;

  // The worst state in a project decides the header's accent, so a project
  // with one blocked agent among twelve still reads as needing attention
  // without the rail being scrolled.
  const worst = worstState(agents);

  // The root path goes on a child rather than the button: a title on the
  // button becomes its accessible name, and "open project" read aloud as a
  // 90-character filesystem path is useless.
  const header = el("button.project-head", {
    class: isOpen ? "is-open" : "",
    onclick: () => openProject(p.id),
    "aria-label": `Open project ${p.name || p.id}`,
    "aria-expanded": String(isOpen),
  },
    el("span.project-dot", {
      dataset: { state: worst },
      title: `worst agent state: ${STATE_LABEL[worst] ?? worst}`,
    }),
    el("span.project-name", { title: p.root }, p.name || shortID(p.id)),
    el("span.project-count", { title: `${agents.length} agents` }, String(agents.length)));

  const tiles = agents.length
    ? agents.map(agentTile)
    : [el("p.rail-none", "no agents")];

  return el("section.project-block", { class: isOpen ? "is-open" : "" }, header,
    el("div.tiles", tiles));
}

/**
 * One agent: a thin 5:1 box. Everything on it has to survive being 40 pixels
 * tall, so it carries a state bar, a name, a branch, and nothing else.
 */
function agentTile(tile) {
  const a = tile.agent ?? {};
  const name = a.display_name || a.adapter || "agent";
  const selected = state.openAgent === a.id;

  return el("button.tile", {
    class: [selected ? "is-selected" : "", tile.waiting ? "is-waiting" : ""]
      .filter(Boolean).join(" "),
    dataset: { state: tile.state ?? "done" },
    title: `${name} · ${a.status ?? "?"} · ${STATE_LABEL[tile.state] ?? ""}`,
    onclick: () => openAgent(a.id),
  },
    el("span.tile-bar"),
    el("span.tile-body",
      el("span.tile-top",
        el("span.tile-name", name),
        a.role && a.role !== "primary" ? el("span.tile-role", a.role) : null,
        tile.unread ? el("span.tile-unread", String(tile.unread)) : null),
      el("span.tile-sub",
        el("span.tile-branch", tile.container?.branch ?? "—"),
        el("span.tile-when", ago(a.last_activity_at)))));
}

function worstState(agents) {
  const order = ["attention", "starting", "working", "done"];
  for (const s of order) {
    if (agents.some((t) => t.state === s)) return s;
  }
  return "done";
}

/** openProject selects a project and loads what the other panes need. */
export async function openProject(id) {
  update({ openProject: id, panel: "workspace" });
  await Promise.all([refreshProjectAgents(id), refreshDetail(id)]);
}

export async function openAgent(id) {
  update({ openAgent: id, panel: "workspace", transcript: [], agentDetail: null });
  await refreshTranscript(id);
}

export async function refreshProjectAgents(id) {
  if (!id) return;
  try {
    const { agents } = await Aurium.projectAgents(id);
    state.agentsByProject[id] = agents ?? [];
    update({});
  } catch {
    // A rail that fails to refresh keeps its last good frame. Blanking it on
    // a transient error would be a worse answer than a slightly stale one.
  }
}

export async function refreshDetail(id) {
  if (!id) return;
  try {
    state.detail[id] = await Aurium.project(id);
    update({});
  } catch {
    /* as above */
  }
}

export async function refreshTranscript(id) {
  if (!id) return;
  try {
    const [{ messages }, detail] = await Promise.all([
      Aurium.agentMessages(id),
      Aurium.agent(id),
    ]);
    if (state.openAgent !== id) return; // selection moved while we were away
    update({ transcript: messages ?? [], transcriptAgent: id, agentDetail: detail });
  } catch (err) {
    if (state.openAgent === id) update({ transcript: [], agentDetail: null });
  }
}

/** refreshAllAgents reloads every project's rail, for the initial paint. */
export async function refreshAllAgents() {
  await Promise.all((state.projects ?? []).map((p) => refreshProjectAgents(p.id)));
}
