// Setup: the first-run wizard.
//
// It renders the same check table `aurium doctor` prints, because a dashboard
// that develops a second opinion about what Aurium needs is worse than no
// dashboard at all. It is a screen, not a gate — reached only on a daemon
// with nothing in it, and left the moment the user says so.

import { el, mount } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";

export async function reloadPreflight() {
  try {
    const { checks } = await Aurium.preflight();
    update({ preflight: checks, preflightError: null });
  } catch (err) {
    update({ preflight: [], preflightError: String(err) });
  }
}

function checkRow(c) {
  const cls = c.ok ? "is-ok" : c.severity === "optional" ? "is-warn" : "is-fail";
  return el(`li.check.${cls}`, {},
    el("span.check-name", {}, c.name),
    el("span.check-detail", {}, c.ok ? "ok" : c.error ?? "failed"),
    // The exact command, selectable: the whole point is that it can be copied.
    !c.ok && c.remedy ? el("code.check-fix", {}, c.remedy) : null);
}

export function renderSetup(host) {
  const checks = state.preflight ?? [];
  const blocking = checks.filter((c) => !c.ok && c.severity === "required");

  mount(host, [
    el("h2", {}, "Set up Aurium"),
    el("p.lede", {}, "Four steps. The first one is your machine."),

    el("section.step", {},
      el("h3", {}, "1 · Environment"),
      state.preflightError
        ? el("p.notice", {}, `Could not check this machine: ${state.preflightError}`)
        : blocking.length
          ? el("p.notice", {}, `${blocking.length} thing(s) need fixing before agents can run.`)
          : el("p.notice.is-ok", {}, "This machine is ready."),
      el("ul.checks", {}, checks.map(checkRow)),
      el("button.mini", { onclick: reloadPreflight }, "Re-check")),

    el("section.step", {},
      el("h3", {}, "2 · Connect a provider"),
      el("p", {}, "Agents run on your accounts."),
      el("button", { onclick: () => update({ panel: "providers" }) }, "Open Providers")),

    el("section.step", {},
      el("h3", {}, "3 · Connect GitHub"),
      el("button", { onclick: () => update({ panel: "providers" }) }, "Open Providers")),

    el("section.step", {},
      el("h3", {}, "4 · Your first project"),
      el("button", { onclick: () => update({ panel: "home" }) }, "Create a project")),

    // A wizard you cannot leave is a trap, not a wizard.
    el("button.mini", { onclick: () => update({ panel: "workspace", setupDismissed: true }) },
      "Skip for now"),
  ]);
}
