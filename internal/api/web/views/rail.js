// The left rail: every project at once, each a container of thin agent tiles.
//
// Showing only the open project would defeat the point. The rail exists for
// peripheral vision — the question it answers is "is anything on fire",
// including in a project you are not looking at — so it draws them all and
// leans on colour to carry the answer from across the room.

import { el, mount, shortID, ago } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
import { form } from "../lib/dialog.js";

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
    : [el("p.rail-none", "No agents yet.")];

  return el("section.project-block", { class: isOpen ? "is-open" : "" }, header,
    el("div.tiles", tiles,
      // The point of the button being here, inside the project, rather than in
      // a toolbar: an agent belongs to a project, and which project is the
      // first thing you would otherwise have to be asked.
      el("button.tile.tile-add", {
        onclick: (e) => { e.stopPropagation(); addAgent(p); },
        title: `Add an agent to ${p.name}`,
      },
        el("span.tile-bar"),
        el("span.tile-body",
          el("span.tile-add-label", "+  add agent")))));
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
        prBadge(tile.pr),
        el("span.tile-when", ago(a.last_activity_at)))));
}

/**
 * prBadge is what this agent's branch became on GitHub.
 *
 * Deliberately tiny and deliberately at the end: a container that is green in
 * CI and one with a failing build look different at a glance, which is the
 * whole point, but a branch with no PR is the ordinary case and must not be
 * decorated with an absence.
 */
function prBadge(pr) {
  if (!pr) return null;
  const state = pr.merged ? "merged"
    : pr.state === "closed" ? "closed"
    : pr.draft ? "draft"
    : pr.checks === "failure" ? "failing"
    : pr.checks === "pending" ? "building"
    : pr.checks === "success" ? "green"
    : "open";
  return el("span.pr", { dataset: { pr: state }, title: `#${pr.number} ${pr.title ?? ""} (${state})` },
    `#${pr.number}`);
}

function worstState(agents) {
  const order = ["attention", "starting", "working", "done"];
  for (const s of order) {
    if (agents.some((t) => t.state === s)) return s;
  }
  return "done";
}

// Adapters, and whether you can talk to one. `shell` is a real choice — it is
// a worktree and a prompt with no model behind it — but offering it without
// saying so produces an agent whose chat box does nothing.
const ADAPTERS = [
  { value: "claude", label: "Claude", chats: true },
  { value: "codex", label: "Codex", chats: true },
  { value: "shell", label: "Shell — no chat, attach in a terminal", chats: false },
];

/**
 * addAgent creates an agent and opens it.
 *
 * One dialog, because the two things worth deciding — which model, and which
 * repository — are one decision. Everything else has a defensible default: a
 * form asking for a branch name before you have said a word to the agent is a
 * form standing between you and the point.
 */
export async function addAgent(p) {
  const detail = state.detail[p.id] ?? {};
  const repos = detail.repositories ?? [];

  if (repos.length === 0) {
    update({ notice: `${p.name} has no repositories yet — add one first.` });
    return;
  }

  const fields = [{
    key: "adapter",
    label: "Agent",
    options: ADAPTERS.map((a) => ({
      value: a.value, label: a.label, selected: a.value === "claude",
    })),
  }];
  if (repos.length > 1) {
    fields.push({
      key: "repo_id",
      label: "Repository",
      hint: "It gets its own branch and worktree here.",
      options: repos.map((r) => ({ value: r.id, label: basename(r.path) })),
    });
  }

  const answer = await form("New agent",
    `In ${p.name}. It gets a branch of its own, so it cannot tread on anything else.`,
    fields, "Create agent");
  if (!answer) return;

  update({ notice: "Creating an agent…" });
  try {
    const out = await Aurium.spawnAgent(p.id, {
      repo_id: answer.repo_id ?? repos[0].id,
      adapter: answer.adapter,
    });
    update({ notice: null });
    await refreshProjectAgents(p.id);
    update({ openProject: p.id, notice: null });
    await openAgent(out.agent.id);
  } catch (err) {
    update({ notice: err.message ?? String(err) });
  }
}

function basename(path) {
  const parts = String(path ?? "").split("/").filter(Boolean);
  return parts[parts.length - 1] ?? path;
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
  } catch {
    // A transcript that fails to load leaves the pane empty rather than
    // showing another agent's messages under this agent's name.
    if (state.openAgent === id) update({ transcript: [], agentDetail: null });
  }
}

/** refreshAllAgents reloads every project's rail, for the initial paint. */
export async function refreshAllAgents() {
  await Promise.all((state.projects ?? []).map((p) => refreshProjectAgents(p.id)));
}
