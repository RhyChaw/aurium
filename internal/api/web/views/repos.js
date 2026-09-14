// The strip under the pulse: the open project's repositories.
//
// A project that spans three repos is the whole point of D22, and a dashboard
// that never shows which three would be asserting it rather than demonstrating
// it. This is also the only place a repository can be added to a project
// without a terminal.

import { el, mount } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
import { refreshDetail } from "./rail.js";
import { prompt } from "../lib/dialog.js";

export function renderRepos(host) {
  const id = state.openProject;
  if (!id) {
    mount(host, el("p.empty", "Open a project to see its repositories."));
    return;
  }

  const detail = state.detail[id] ?? {};
  const repos = detail.repositories ?? [];
  const project = state.projects.find((p) => p.id === id);

  mount(host,
    el("div.repos-head",
      el("h3", "Repositories"),
      project?.descriptor
        ? el("code.ref", { title: project.descriptor }, "aurium.project.yaml")
        : el("span.pill", { title: "A single repository with no descriptor" }, "standalone"),
      el("button.mini", { onclick: () => addRepo(id) }, "+ add")),
    repos.length
      ? el("div.repo-chips", repos.map(chip))
      : el("p.empty", "None attached yet."));
}

function chip(r) {
  return el("div.repo-chip", { title: r.path },
    el("span.repo-name", basename(r.path)),
    el("span.repo-branch", r.base_branch),
    r.remote ? el("span.repo-remote", { title: r.remote }, "remote") : null);
}

function basename(p) {
  const parts = String(p ?? "").split("/").filter(Boolean);
  return parts[parts.length - 1] ?? p;
}

async function addRepo(projectID) {
  const path = await prompt("Add a repository",
    "It is given the guard hooks that enforce Invariant 1, and an aurium.yaml if it has none.",
    { placeholder: "~/code/api", submit: "Add" });
  if (!path) return;
  try {
    await Aurium.addRepo(projectID, { path });
    await refreshDetail(projectID);
  } catch (err) {
    update({ notice: err.message ?? String(err) });
  }
}
