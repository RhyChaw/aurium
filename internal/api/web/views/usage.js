// Usage: what the fleet is spending, and — just as prominently — what this
// page cannot see.
//
// A cost view whose coverage is invisible invites more trust than it has
// earned. Aurium meters what a provider actually reports, which today means
// headless runs; interactive REPL turns surface nothing the daemon can read.
// So the gap is on the page, above the numbers, not in a footnote.

import { el, mount, num, usd, compact, svg } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";

const WINDOWS = [
  ["1h", "last hour"],
  ["24h", "last day"],
  ["168h", "last week"],
  ["720h", "last month"],
];

export function renderUsage(host) {
  const u = state.usage;
  if (!u) {
    mount(host, el("p.empty", "loading usage…"));
    return;
  }
  const t = u.totals ?? {};

  mount(host,
    el("div.usage",
      el("div.usage-head",
        el("h2", "Usage"),
        el("div.window-picker", WINDOWS.map(([value, label]) =>
          el("button.chip", {
            class: state.usageWindow === value ? "is-active" : "",
            onclick: () => setWindow(value),
          }, label)))),

      el("p.coverage", u.coverage),

      el("div.usage-totals",
        total(usd(t.cost_usd), "priced spend", u.pricing?.note),
        total(compact((t.input_tokens ?? 0) + (t.output_tokens ?? 0)), "tokens",
          `${num(t.input_tokens)} in · ${num(t.output_tokens)} out`),
        total(compact(t.unpriced_tokens), "unpriced tokens",
          "on a subscription seat, or a model with no price in the table"),
        total(num(t.calls), "metered calls", "only where a provider reported usage")),

      sparkline(),

      el("div.usage-groups",
        group("By provider", u.groups?.provider, "which company"),
        group("By agent", u.groups?.agent, "which agent"),
        group("By model", u.groups?.model),
        group("By project", u.groups?.project)),

      el("p.hint",
        "List prices, checked ", el("strong", u.pricing?.verified ?? "—"),
        ". Priced models: ", (u.pricing?.models ?? []).join(", "),
        ". Anything else is counted and shown as unpriced.")));
}

function total(value, label, sub) {
  return el("div.total", { title: sub ?? "" },
    el("span.total-value", value),
    el("span.total-label", label),
    sub ? el("span.total-sub", sub) : null);
}

function group(title, rows, note) {
  const list = rows ?? [];
  if (!list.length) {
    return el("section.group", el("h3", title), el("p.empty", "nothing yet"));
  }
  const max = Math.max(...list.map((r) => (r.input_tokens ?? 0) + (r.output_tokens ?? 0)), 1);

  return el("section.group",
    el("h3", title, note ? el("span.muted", ` — ${note}`) : null),
    el("table.usage-table",
      el("tbody", list.map((r) => {
        const tokens = (r.input_tokens ?? 0) + (r.output_tokens ?? 0);
        return el("tr",
          el("td.g-key", r.label || r.key || "—"),
          el("td.g-bar",
            el("span.bar", { style: { width: `${Math.round((tokens / max) * 100)}%` } })),
          el("td.g-tokens", compact(tokens)),
          el("td.g-cost",
            r.unpriced_tokens > 0 && r.cost_usd === 0
              ? el("span.muted", { title: "unpriced" }, "—")
              : usd(r.cost_usd)));
      }))));
}

/**
 * The sparkline is drawn as an SVG path rather than a canvas: it changes once
 * per load, and an SVG scales with the pane and prints.
 */
function sparkline() {
  const series = state.usageSeriesData ?? [];
  if (series.length < 2) {
    return el("div.spark", el("p.empty", "not enough history to draw a line yet"));
  }

  const w = 720;
  const h = 90;
  const values = series.map((b) => (b.input_tokens ?? 0) + (b.output_tokens ?? 0));
  const max = Math.max(...values, 1);
  const step = w / (values.length - 1);

  const points = values.map((v, i) => [i * step, h - (v / max) * (h - 8) - 4]);
  const line = points.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`).join("");
  const area = `${line}L${w},${h}L0,${h}Z`;

  return el("div.spark",
    el("div.spark-head",
      el("span", "tokens over the window"),
      el("span.muted", `${compact(max)} peak`)),
    svg("svg", { viewBox: `0 0 ${w} ${h}`, preserveAspectRatio: "none", class: "spark-svg" },
      svg("path.spark-area", { d: area }),
      svg("path.spark-line", { d: line })));
}

function setWindow(value) {
  update({ usageWindow: value });
  reloadUsage();
}

/**
 * reloadSpendStrip feeds the workspace's spend line. It is separate from
 * reloadUsage because the workspace wants one cheap series on a slow timer,
 * and the Usage tab wants four aggregates the moment you open it.
 */
export async function reloadSpendStrip() {
  try {
    const series = await Aurium.usageSeries("24h", state.openProject);
    state.usageSeriesData = series?.series ?? [];
    update({});
  } catch {
    /* the strip keeps its last good frame */
  }
}

export async function reloadUsage() {
  try {
    const [report, series] = await Promise.all([
      Aurium.usage(state.usageWindow, state.openProject),
      Aurium.usageSeries(state.usageWindow, state.openProject),
    ]);
    state.usageSeriesData = series?.series ?? [];
    update({ usage: report });
  } catch (err) {
    update({ error: err.message ?? String(err) });
  }
}
