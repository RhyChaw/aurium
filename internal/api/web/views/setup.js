// Setup: the first-run wizard.
//
// It renders the same check table `aurium doctor` prints, because a dashboard
// that develops a second opinion about what Aurium needs is worse than no
// dashboard at all. It is a screen, not a gate — reached only on a daemon
// with nothing in it, and left the moment the user says so.

import { el, mount } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
// A cycle with app.js, and a deliberate one: app.js owns panel navigation, and
// the steps below are navigation. Function declarations are hoisted at module
// instantiation, so selectPanel is bound long before any click can reach it.
import { selectPanel } from "../app.js";

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
    // A passing check can still have something to say — "the port is held, by
    // your own Aurium daemon" is the answer, not a complaint.
    el("span.check-detail", {}, c.ok ? c.detail ?? "ok" : c.error ?? "failed"),
    // The exact command, selectable: the whole point is that it can be copied.
    !c.ok && c.remedy ? el("code.check-fix", {}, c.remedy) : null);
}

export function renderSetup(host) {
  const checks = state.preflight ?? [];
  const blocking = checks.filter((c) => !c.ok && c.severity === "required");

  mount(host, el("div.setup", {}, [
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

    // Every step navigates through selectPanel, exactly as clicking the tab
    // does. Setting state.panel directly skipped the panel's load-on-arrival:
    // on first run nothing is cached, so steps 2 and 3 landed on an empty
    // provider grid and a GitHub card that said "checking…" forever.
    el("section.step", {},
      el("h3", {}, "2 · Connect a provider"),
      el("p", {}, "Agents run on your accounts."),
      el("button", { onclick: () => selectPanel("providers") }, "Open Providers")),

    el("section.step", {},
      el("h3", {}, "3 · Connect GitHub"),
      el("button", { onclick: () => selectPanel("providers") }, "Open Providers")),

    el("section.step", {},
      el("h3", {}, "4 · Your first project"),
      el("button", { onclick: () => selectPanel("home") }, "Create a project")),

    // A wizard you cannot leave is a trap, not a wizard. The dismissal is the
    // one piece of state selectPanel does not carry, so it is set first.
    el("button.mini", {
      onclick: () => {
        update({ setupDismissed: true });
        selectPanel("workspace");
      },
    }, "Skip for now"),
  ]));
}
