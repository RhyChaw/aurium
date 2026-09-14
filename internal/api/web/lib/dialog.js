// In-page dialogs, not native ones.
//
// `window.prompt` and `window.confirm` are not merely ugly here. They freeze
// the renderer — nothing else on the page runs, the event stream stops being
// drained, and the pulse stops — until someone clicks the button. In a window
// that is meant to be left open watching a fleet, a modal that halts the whole
// surface to ask "which repository?" is the wrong trade.
//
// `<dialog>` gives the same focus trap and Escape handling natively, without
// stopping the page behind it.

import { el } from "./dom.js";

/**
 * ask renders a modal and resolves with the caller's chosen value, or null if
 * the user backed out. `build` receives a `done` callback and returns the body.
 */
function ask(title, build) {
  return new Promise((resolve) => {
    let settled = false;
    const done = (value) => {
      if (settled) return;
      settled = true;
      dialog.close();
      resolve(value);
    };

    const dialog = el("dialog.dialog",
      el("h3.dialog-title", title),
      build(done));

    // Escape and the backdrop both count as backing out, and both must resolve
    // rather than leaving the caller waiting forever on a promise.
    dialog.addEventListener("close", () => {
      dialog.remove();
      if (!settled) {
        settled = true;
        resolve(null);
      }
    });
    dialog.addEventListener("click", (e) => {
      if (e.target === dialog) done(null);
    });

    document.body.append(dialog);
    dialog.showModal();
    dialog.querySelector("input, select, button")?.focus();
  });
}

/** choose presents options and resolves with the chosen one's value. */
export function choose(title, lede, options) {
  return ask(title, (done) =>
    el("div.dialog-body",
      lede ? el("p.hint", lede) : null,
      el("div.choice-list", options.map((o) =>
        el("button.choice", { type: "button", onclick: () => done(o.value) },
          el("span.choice-label", o.label),
          o.hint ? el("span.choice-hint", o.hint) : null))),
      el("div.dialog-actions",
        el("button.act", { type: "button", onclick: () => done(null) }, "Cancel"))));
}

/** prompt asks for one line of text. */
export function prompt(title, lede, { placeholder = "", value = "", submit = "OK" } = {}) {
  return ask(title, (done) => {
    const input = el("input.field", { placeholder, value });
    const form = el("form.dialog-body", {
      onsubmit: (e) => {
        e.preventDefault();
        done(input.value.trim() || null);
      },
    },
      lede ? el("p.hint", lede) : null,
      input,
      el("div.dialog-actions",
        el("button.act", { type: "button", onclick: () => done(null) }, "Cancel"),
        el("button.act.primary", { type: "submit" }, submit)));
    return form;
  });
}

/**
 * form presents several choices at once and resolves with an object of values.
 *
 * Chaining two dialogs to ask two questions makes the second one feel like a
 * consequence of the first rather than part of the same decision, and gives
 * the user nowhere to change their mind about the first.
 *
 * `fields` is [{ key, label, hint, options: [{value,label,hint}] }].
 */
export function form(title, lede, fields, submitLabel = "Create") {
  return ask(title, (done) => {
    const controls = {};
    const rows = fields.map((f) => {
      const select = el("select.field",
        f.options.map((o) =>
          el("option", { value: o.value, selected: o.selected }, o.label)));
      controls[f.key] = select;
      return el("label.field-row",
        el("span.field-label", f.label),
        select,
        f.hint ? el("span.field-hint", f.hint) : null);
    });

    return el("form.dialog-body", {
      onsubmit: (e) => {
        e.preventDefault();
        const out = {};
        for (const [k, c] of Object.entries(controls)) out[k] = c.value;
        done(out);
      },
    },
      lede ? el("p.hint", lede) : null,
      rows,
      el("div.dialog-actions",
        el("button.act", { type: "button", onclick: () => done(null) }, "Cancel"),
        el("button.act.primary", { type: "submit" }, submitLabel)));
  });
}

/** confirm asks a yes/no question. `danger` colours the confirming button. */
export function confirm(title, lede, { confirmLabel = "Confirm", danger = false } = {}) {
  return ask(title, (done) =>
    el("div.dialog-body",
      lede ? el("p.hint", lede) : null,
      el("div.dialog-actions",
        el("button.act", { type: "button", onclick: () => done(false) }, "Cancel"),
        el("button.act", {
          type: "button",
          class: danger ? "danger-act" : "primary",
          onclick: () => done(true),
        }, confirmLabel)))).then((v) => v === true);
}
