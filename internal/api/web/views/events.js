// The event trail: the audit log, live.
//
// Every state transition in Aurium emits an event before the API call returns
// (D19), so this panel is not a debug view — it is the record of what happened
// and who caused it, which is the question "why did my branch move" needs.

import { el, mount } from "../lib/dom.js";
import { state, update, MAX_EVENTS } from "../lib/state.js";

export function renderEvents(host) {
  const filter = (state.eventFilter ?? "").toLowerCase();
  const matching = state.events.filter((e) => matches(e, filter)).slice(-MAX_EVENTS);

  const list = el("ol.events", { reversed: true },
    matching.map(row));

  const search = el("input.field", {
    type: "search",
    placeholder: "Filter by type, container or actor…",
    value: state.eventFilter ?? "",
    autocomplete: "off",
    oninput: (e) => update({ eventFilter: e.target.value }),
  });

  const follow = el("input", {
    type: "checkbox",
    checked: state.follow !== false,
    onchange: (e) => { state.follow = e.target.checked; },
  });

  mount(host,
    el("div.toolbar", search, el("label.check", follow, " Follow")),
    list);

  if (state.follow !== false) list.scrollTop = list.scrollHeight;
}

function matches(e, q) {
  if (!q) return true;
  return (e.type ?? "").toLowerCase().includes(q) ||
    (e.container_id ?? "").toLowerCase().includes(q) ||
    (e.agent_id ?? "").toLowerCase().includes(q) ||
    (e.actor ?? "").toLowerCase().includes(q);
}

function row(e) {
  const bits = [e.actor];
  if (e.container_id) bits.push(e.container_id);
  for (const [k, v] of Object.entries(e.payload ?? {})) {
    // A nested value stringifies to "[object Object]", which tells the reader
    // nothing. Compact JSON at least shows what is in it.
    bits.push(`${k}=${v !== null && typeof v === "object" ? JSON.stringify(v) : v}`);
  }
  return el("li",
    el("span.ev-ts", (e.ts ?? "").slice(11, 19)),
    el("span.ev-type", e.type),
    el("span.ev-rest", bits.join("  ")));
}
