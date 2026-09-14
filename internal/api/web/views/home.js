// Home: the projects that exist, and the door to making another.
//
// A project is a set of repositories worked on together (D22). The form below
// is the only place that fact is explained to a user, so it says it rather
// than assuming a reading of the descriptor format.

import { el, mount, shortID, ago } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
import { openProject, refreshProjectAgents } from "./rail.js";
import { choose } from "../lib/dialog.js";

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

  // Repositories that are not on this machine yet. They are cloned after the
  // project exists, so a failure to clone one leaves a project you can fix
  // rather than nothing at all.
  const pending = [];
  const pendingList = el("div.pending-repos");
  const renderPending = () => {
    mountPending(pendingList, pending, renderPending);
  };

  const name = el("input.field", { placeholder: "my-product", required: true });
  const description = el("input.field", { placeholder: "what this project is (optional)" });
  const root = el("input.field", {
    placeholder: "~/.aurium/projects/<name> (leave blank)",
  });
  const agent = el("select.field",
    el("option", { value: "claude" }, "claude"),
    el("option", { value: "codex" }, "codex"),
    el("option", { value: "shell" }, "shell"));
  // Built from what actually works here, not a fixed list. Offering "docker"
  // on a machine with Docker stopped is offering a choice that fails later.
  const driver = el("select.field");
  const driverNote = el("p.hint");
  loadDrivers(driver, driverNote);

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

        // Clone after the project exists, one at a time, reporting which one
        // is in flight: a large repository takes a while and a dialog that
        // says nothing for ninety seconds reads as hung.
        for (const r of pending) {
          status.textContent = `Cloning ${r.full_name}…`;
          await Aurium.githubClone(out.project.id, {
            full_name: r.full_name,
            clone_url: r.clone_url,
            base_branch: r.default_branch,
            driver: driver.value,
            agent: agent.value,
          });
        }

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
        pendingList,
        el("div.repo-row-actions",
          el("button.mini", {
            type: "button",
            onclick: (e) => { e.preventDefault(); addRow(); },
          }, "+ a path on this machine"),
          el("button.mini", {
            type: "button",
            onclick: async (e) => {
              e.preventDefault();
              const picked = await pickFromGitHub();
              if (!picked) return;
              if (!pending.some((r) => r.full_name === picked.full_name)) {
                pending.push(picked);
                renderPending();
              }
            },
          }, "+ from GitHub")))),
    el("details.advanced",
      el("summary", "Advanced"),
      field("Project root", root),
      el("label.field-row",
        el("span.field-label", "Sandbox"),
        driver,
        driverNote),
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

/**
 * loadDrivers fills the sandbox picker from what this machine can run.
 *
 * An unavailable driver stays in the list, disabled, with its reason — hiding
 * it would leave a user wondering where Docker went, and the reason is usually
 * "start Docker Desktop", which is a thing they can do.
 */
async function loadDrivers(select, note) {
  select.replaceChildren(el("option", { value: "" }, "checking…"));
  let drivers;
  try {
    ({ drivers } = await Aurium.drivers());
  } catch {
    select.replaceChildren(el("option", { value: "local" }, "local — no isolation"));
    return;
  }

  select.replaceChildren(...drivers.map((d) => el("option", {
    value: d.name,
    disabled: !d.available,
    selected: d.recommended,
  }, d.available
      ? (d.isolated ? d.name : `${d.name} — no isolation`)
      : `${d.name} — unavailable`)));

  const explain = () => {
    const chosen = drivers.find((d) => d.name === select.value);
    if (!chosen) return;
    if (!chosen.isolated) {
      note.className = "hint";
      note.textContent =
        "Agents run as processes on this machine with no isolation. Fine for " +
        "trying it out and on a repository you trust; it is not a sandbox.";
      const unavailable = drivers.filter((d) => !d.available && d.isolated);
      if (unavailable.length) {
        note.textContent += ` (${unavailable[0].reason})`;
      }
    } else {
      note.className = "hint";
      note.textContent = "Each agent gets its own container.";
    }
  };
  select.addEventListener("change", explain);
  explain();
}

/** mountPending draws the not-yet-cloned repositories. */
function mountPending(host, pending, rerender) {
  host.replaceChildren();
  for (const r of pending) {
    host.append(el("div.pending-repo",
      el("span.pending-name", r.full_name),
      el("span.pending-hint", r.private ? "private" : "public"),
      el("button.mini.ghost", {
        type: "button",
        title: "remove",
        onclick: (e) => {
          e.preventDefault();
          pending.splice(pending.indexOf(r), 1);
          rerender();
        },
      }, "×")));
  }
}

/** pickFromGitHub lists the user's repositories and returns the chosen one. */
async function pickFromGitHub() {
  let repos;
  try {
    const out = await Aurium.githubRepos(100);
    repos = out.repos ?? [];
  } catch (err) {
    update({ notice: err.message ?? String(err) });
    return null;
  }
  if (!repos.length) {
    update({ notice: "No repositories came back from GitHub." });
    return null;
  }

  // Archived repositories and forks sink to the bottom rather than being
  // hidden: they are rarely what you want and occasionally exactly what you
  // want, and a picker that silently omits a repo is a picker you stop trusting.
  const ordered = repos.slice().sort((a, b) =>
    (a.archived === b.archived ? 0 : a.archived ? 1 : -1) ||
    (a.fork === b.fork ? 0 : a.fork ? 1 : -1));

  const chosen = await choose("Add from GitHub", "It is cloned into the project root.",
    ordered.map((r) => ({
      value: r.full_name,
      label: r.full_name,
      hint: [r.private ? "private" : null, r.archived ? "archived" : null,
             r.fork ? "fork" : null, r.default_branch].filter(Boolean).join(" · "),
    })));
  return chosen ? ordered.find((r) => r.full_name === chosen) : null;
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
