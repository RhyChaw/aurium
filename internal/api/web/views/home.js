// Home: the projects that exist, and the door to making another.
//
// A project is a set of repositories worked on together (D22). The form below
// is the only place that fact is explained to a user, so it says it rather
// than assuming a reading of the descriptor format.

import { el, mount, shortID, ago } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
import { openProject, refreshProjectAgents } from "./rail.js";

export function renderHome(host) {
  mount(host,
    el("div.home",
      el("header.home-head",
        el("h2", "Projects"),
        el("p.lede",
          "A project is a set of repositories worked on together. Agents run in " +
          "containers inside them, and a project keeps their context, approvals " +
          "and spend in one place. A single repository works too — run ",
          el("code", "aurium init"),
          " in it and it appears here on its own.")),
      el("div.project-grid",
        (state.projects ?? []).map(card),
        newProjectCard())));
}

function card(p) {
  const detail = state.detail[p.id];
  const agents = state.agentsByProject[p.id] ?? [];
  const bands = countBands(agents);

  return el("article.project-card", { onclick: () => openProject(p.id) },
    el("div.card-head",
      el("h3", p.name || shortID(p.id)),
      p.descriptor
        ? null
        : el("span.pill", { title: "A single repository with no descriptor" }, "standalone")),
    el("p.card-root", { title: p.root }, p.root),
    el("div.card-stats",
      figure(detail?.repositories?.length ?? "—", "repos"),
      figure(detail?.containers ?? "—", "containers"),
      figure(agents.length, "agents")),
    el("div.card-bands",
      ["attention", "starting", "working", "done"]
        .filter((b) => bands[b])
        .map((b) => el("span.band", { dataset: { state: b } }, `${bands[b]} ${b}`)),
      Object.keys(bands).length === 0 ? el("span.muted", "no agents yet") : null),
    el("p.card-when", "created ", ago(p.created_at)));
}

function figure(value, label) {
  return el("div.figure", el("span.figure-value", String(value)), el("span.figure-label", label));
}

function countBands(agents) {
  const out = {};
  for (const t of agents) out[t.state] = (out[t.state] ?? 0) + 1;
  return out;
}

function newProjectCard() {
  return el("article.project-card.is-new",
    { onclick: (e) => { e.stopPropagation(); openCreate(); } },
    el("div.new-plus", "+"),
    el("h3", "New project"),
    el("p.muted", "Name it, then point it at one or more git repositories."));
}

// --- the create dialog ---
//
// A <dialog> rather than a route: creating a project is a detour from looking
// at the ones you have, and coming back to exactly where you were is the whole
// point of a modal.

function openCreate() {
  const rows = el("div.repo-rows");
  const addRow = (value = "") => rows.append(repoRow(rows, value));
  addRow();

  const name = el("input.field", { placeholder: "my-product", required: true });
  const description = el("input.field", { placeholder: "what this project is (optional)" });
  const root = el("input.field", {
    placeholder: "~/.aurium/projects/<name> (leave blank)",
  });
  const agent = el("select.field",
    el("option", { value: "claude" }, "claude"),
    el("option", { value: "codex" }, "codex"),
    el("option", { value: "shell" }, "shell"));
  const driver = el("select.field",
    el("option", { value: "docker" }, "docker"),
    el("option", { value: "local" }, "local — no isolation"),
    el("option", { value: "podman" }, "podman"));

  const status = el("p.dialog-status");
  const submit = el("button.act.primary", { type: "submit" }, "Create project");

  const form = el("form.dialog-body", {
    onsubmit: async (e) => {
      e.preventDefault();
      const repos = Array.from(rows.querySelectorAll("input"))
        .map((i) => i.value.trim())
        .filter(Boolean)
        .map((path) => ({ path }));

      submit.disabled = true;
      status.className = "dialog-status";
      status.textContent = "creating…";
      try {
        const out = await Aurium.createProject({
          name: name.value.trim(),
          description: description.value.trim(),
          root: root.value.trim(),
          driver: driver.value,
          agent: agent.value,
          repos,
        });
        dialog.close();
        await reloadProjects();
        await openProject(out.project.id);
      } catch (err) {
        // The daemon's message is the useful one — "that is not a git
        // repository", "already belongs to project X" — so it is shown as
        // written rather than replaced with a generic failure.
        status.className = "dialog-status is-error";
        status.textContent = err.message ?? String(err);
        submit.disabled = false;
      }
    },
  },
    field("Name", name),
    field("Description", description),
    field("Repositories",
      el("div",
        el("p.hint",
          "Absolute paths, or paths relative to the project root. Each one is " +
          "given guard hooks and an aurium.yaml if it has none."),
        rows,
        el("button.mini", {
          type: "button",
          onclick: (e) => { e.preventDefault(); addRow(); },
        }, "+ another repository"))),
    el("details.advanced",
      el("summary", "Advanced"),
      field("Project root", root),
      field("Default driver", driver),
      field("Default agent", agent)),
    status,
    el("div.dialog-actions",
      el("button.act", { type: "button", onclick: () => dialog.close() }, "Cancel"),
      submit));

  const dialog = el("dialog.dialog",
    el("h3.dialog-title", "New project"),
    form);

  dialog.addEventListener("close", () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
  name.focus();
}

function field(label, control) {
  return el("label.field-row", el("span.field-label", label), control);
}

function repoRow(rows, value) {
  const input = el("input.field", { value, placeholder: "~/code/api" });
  return el("div.repo-row", input,
    el("button.mini.ghost", {
      type: "button",
      title: "remove",
      onclick: (e) => {
        e.preventDefault();
        // Never remove the last row: an empty list with no way back is a dead
        // end, and a project with no repos is a legitimate thing to create.
        if (rows.children.length > 1) e.target.closest(".repo-row").remove();
        else input.value = "";
      },
    }, "×"));
}

export async function reloadProjects() {
  const { projects } = await Aurium.projects();
  update({ projects: projects ?? [] });
  await Promise.all((projects ?? []).map(async (p) => {
    await refreshProjectAgents(p.id);
    try {
      state.detail[p.id] = await Aurium.project(p.id);
    } catch {
      /* a card without counts is better than no card */
    }
  }));
  update({});
}
