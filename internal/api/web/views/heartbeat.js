// The centre pane: the fleet's pulse.
//
// It is decorative and diagnostic at once, which is the only reason it earns
// the middle of the screen. A flat line while four agents claim to be running
// is a real signal, and it is one a table of rows does not give you: the eye
// notices an absence of movement long before it notices a timestamp that has
// stopped advancing.
//
// That is also why the lanes are drawn at rest rather than left blank. An
// empty box reads as a broken canvas; a still line reads as a quiet fleet,
// which is the true thing.
//
// The trace is a canvas because it repaints many times a second; everything
// around it is DOM, because it changes rarely and must be selectable.

import { el, mount, num, usd, compact, svg } from "../lib/dom.js";
import { state } from "../lib/state.js";

// Lanes. Each event type contributes a blip on its own row, so a burst of
// container churn is visually distinct from a burst of agent chatter.
const LANES = [
  { key: "agent", label: "agents", test: (t) => t.startsWith("agent.") },
  { key: "container", label: "containers", test: (t) => t.startsWith("container.") },
  { key: "context", label: "context", test: (t) => t.startsWith("context.") },
  { key: "gateway", label: "tools", test: (t) => t.startsWith("integration.") || t.startsWith("usage.") },
  { key: "approval", label: "approvals", test: (t) => t.startsWith("approval.") },
];

const SLOTS = 300;   // columns of history held on screen
const TICK_MS = 100; // how often a column is committed

const ring = Array.from({ length: SLOTS }, () => emptyColumn());
let building = emptyColumn();
let timer = null;
let canvas = null;
let ctx = null;
let phase = 0;

function emptyColumn() {
  const c = {};
  for (const lane of LANES) c[lane.key] = 0;
  return c;
}

/** recordEvent is called for every frame on the stream. */
export function recordEvent(e) {
  const type = e?.type ?? "";
  for (const lane of LANES) {
    if (lane.test(type)) {
      building[lane.key] += 1;
      return;
    }
  }
  // An event the UI does not recognise is still the fleet doing something. A
  // pulse that ignored it would go quiet exactly when a new event type is
  // introduced, which is the worst possible time to look dead.
  building.agent += 1;
}

export function renderHeartbeat(host) {
  const hb = state.heartbeat;

  if (!canvas) {
    canvas = el("canvas.pulse-canvas");
    ctx = canvas.getContext("2d");
  }

  mount(host,
    el("div.pulse",
      el("div.pulse-head",
        el("h2.pulse-title", state.openProject
          ? (state.projects.find((p) => p.id === state.openProject)?.name ?? "the fleet")
          : "the fleet"),
        el("div.pulse-legend", LANES.map((lane) =>
          el("span.legend-item", { dataset: { lane: lane.key } },
            el("i.legend-swatch"), lane.label))),
        el("span.pulse-rate",
          hb ? `${(hb.events_per_minute ?? 0).toFixed(1)} events/min` : "—")),
      el("div.pulse-frame", canvas)),
    vitals(hb),
    spend());

  sizeCanvas();
  start();
}

function vitals(hb) {
  if (!hb) {
    return el("div.vitals", el("p.empty", "waiting for the daemon…"));
  }
  const agents = hb.agents ?? {};
  const live = (agents.working ?? 0) + (agents.starting ?? 0);

  return el("div.vitals",
    stat(String(live), "agents working",
      agents.attention ? "attention" : "working",
      `${num(agents.done ?? 0)} done · ${num(agents.attention ?? 0)} need you`),
    stat(String(hb.approvals ?? 0), "waiting on you",
      hb.approvals ? "attention" : "done",
      hb.approvals ? "an agent is blocked" : "nothing is blocked"),
    stat(String(hb.unread ?? 0), "unread messages", "working", "across every inbox"),
    stat(compact(hb.tokens_last_hour ?? 0), "tokens · last hour", "working",
      `${usd(hb.cost_last_hour ?? 0)} priced`));
}

function stat(value, label, band, sub) {
  return el("div.stat", { dataset: { state: band } },
    el("span.stat-value", value),
    el("span.stat-label", label),
    el("span.stat-sub", sub));
}

/**
 * The spend strip. It sits under the vitals rather than only on the Usage tab
 * because cost is a thing you want to notice, not a thing you want to go and
 * look for — and a fleet that quietly triples its burn overnight is exactly
 * what this window is left open to catch.
 */
function spend() {
  const series = state.usageSeriesData ?? [];
  const values = series.map((b) => (b.input_tokens ?? 0) + (b.output_tokens ?? 0));

  if (values.length < 2) {
    return el("div.spend",
      el("div.spend-head", el("span", "spend"), el("span.muted", "last 24 hours")),
      el("p.empty", "Nothing metered yet. Usage is recorded where a provider reports it."));
  }

  const total = series.reduce((sum, b) => sum + (b.cost_usd ?? 0), 0);
  const tokens = values.reduce((a, b) => a + b, 0);

  const w = 600;
  const h = 44;
  const max = Math.max(...values, 1);
  const step = w / (values.length - 1);
  const points = values.map((v, i) => [i * step, h - (v / max) * (h - 6) - 3]);
  const line = points.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`).join("");

  return el("div.spend",
    el("div.spend-head",
      el("span", "spend"),
      el("span.muted", `${compact(tokens)} tokens · ${usd(total)} · last 24 hours`)),
    svg("svg", { viewBox: `0 0 ${w} ${h}`, preserveAspectRatio: "none", class: "spend-svg" },
      svg("path.spark-area", { d: `${line}L${w},${h}L0,${h}Z` }),
      svg("path.spark-line", { d: line })));
}

function sizeCanvas() {
  if (!canvas?.parentElement) return;
  const rect = canvas.parentElement.getBoundingClientRect();
  // devicePixelRatio matters: a 1px trace drawn at CSS resolution on a retina
  // display is a blurry 2px smear.
  const dpr = window.devicePixelRatio || 1;
  const w = Math.max(1, Math.floor(rect.width));
  const h = Math.max(1, Math.floor(rect.height));
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
    canvas.style.width = `${w}px`;
    canvas.style.height = `${h}px`;
  }
}

function start() {
  if (timer) return;
  timer = setInterval(() => {
    ring.push(building);
    ring.shift();
    building = emptyColumn();
    phase += 1;
    draw();
  }, TICK_MS);
  draw();
}

/** stopHeartbeat releases the timer when the pane leaves the screen. */
export function stopHeartbeat() {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

function draw() {
  if (!ctx || !canvas.isConnected) return;
  sizeCanvas();

  const dpr = window.devicePixelRatio || 1;
  const w = canvas.width;
  const h = canvas.height;
  ctx.clearRect(0, 0, w, h);

  const styles = getComputedStyle(document.documentElement);
  const token = (name, fallback) => styles.getPropertyValue(name).trim() || fallback;
  const grid = token("--border-strong", "#ccc");
  const muted = token("--muted", "#888");

  const padL = 78 * dpr; // room for lane labels inside the canvas
  const padR = 10 * dpr;
  const plotW = Math.max(1, w - padL - padR);
  const laneH = h / LANES.length;
  const colW = plotW / SLOTS;

  // Vertical time gridlines, one per ~30 columns (three seconds). They give
  // the trace a sense of scale that a bare line has no way to convey.
  ctx.save();
  ctx.strokeStyle = grid;
  ctx.globalAlpha = 0.16;
  ctx.lineWidth = dpr;
  for (let s = SLOTS - ((phase % 30) || 30); s > 0; s -= 30) {
    const x = padL + s * colW;
    ctx.beginPath();
    ctx.moveTo(x, 0);
    ctx.lineTo(x, h);
    ctx.stroke();
  }
  ctx.restore();

  LANES.forEach((lane, i) => {
    const base = laneH * (i + 0.5);
    const max = laneH * 0.40;
    const color = token(`--lane-${lane.key}`, "#888");

    // The lane at rest. Drawn always, so a quiet fleet reads as a still line
    // rather than as an empty box that looks broken.
    ctx.save();
    ctx.strokeStyle = color;
    ctx.globalAlpha = 0.22;
    ctx.lineWidth = dpr;
    ctx.beginPath();
    ctx.moveTo(padL, base);
    ctx.lineTo(w - padR, base);
    ctx.stroke();
    ctx.restore();

    // The label, inside the canvas so it scrolls with nothing and always lines
    // up with its lane however the pane is resized.
    ctx.save();
    ctx.fillStyle = muted;
    ctx.globalAlpha = 0.75;
    ctx.font = `${10 * dpr}px ui-monospace, SFMono-Regular, Menlo, monospace`;
    ctx.textBaseline = "middle";
    ctx.fillText(lane.label, 8 * dpr, base);
    ctx.restore();

    // The activity itself.
    ctx.save();
    ctx.strokeStyle = color;
    ctx.fillStyle = color;
    ctx.lineWidth = Math.max(1, dpr * 1.2);
    ctx.beginPath();
    for (let s = 0; s < SLOTS; s++) {
      const n = ring[s][lane.key];
      if (n === 0) continue;
      // Log-compressed: one very busy tick must not flatten every other lane
      // into invisibility.
      const amp = Math.min(1, Math.log2(n + 1) / 4) * max;
      const x = padL + s * colW;
      ctx.moveTo(x, base - amp);
      ctx.lineTo(x, base + amp);
    }
    ctx.stroke();
    ctx.restore();
  });

  // The playhead: a soft leading edge so "now" is unambiguous even at rest.
  const x = w - padR;
  const fade = ctx.createLinearGradient(x - 60 * dpr, 0, x, 0);
  fade.addColorStop(0, "rgba(0,0,0,0)");
  fade.addColorStop(1, token("--accent", "#888"));
  ctx.save();
  ctx.globalAlpha = 0.35;
  ctx.strokeStyle = fade;
  ctx.lineWidth = dpr;
  ctx.beginPath();
  ctx.moveTo(x - 60 * dpr, h / 2);
  ctx.lineTo(x, h / 2);
  ctx.stroke();
  ctx.globalAlpha = 0.9;
  ctx.strokeStyle = token("--accent", "#888");
  ctx.beginPath();
  ctx.moveTo(x, 0);
  ctx.lineTo(x, h);
  ctx.stroke();
  ctx.restore();
}

// The canvas must resurvey itself when the window changes, or it stretches.
window.addEventListener("resize", () => {
  sizeCanvas();
  draw();
});
