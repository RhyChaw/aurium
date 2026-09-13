// Aurium dashboard v0 (§11.3).
//
// Read-mostly by design. Attaching to an agent is a terminal operation, so the
// dashboard shows the `aurium attach` command with a copy button rather than
// embedding a terminal emulator.

const state = {
  token: null,
  project: null,
  lastEventID: null,
  events: [],
  filter: "",
  approvals: [],
  rows: [],
  selected: null,
  seenApprovals: new Set(),
  booted: false,
};

const MAX_EVENTS = 500;

const $ = (sel) => document.querySelector(sel);

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { Authorization: `Bearer ${state.token}`, ...(opts.headers || {}) },
  });
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return res.json();
}

// The dashboard is served by the daemon on loopback, and the daemon hands it
// the host token so the page can call the same API the CLI does.
function readToken() {
  const meta = document.querySelector('meta[name="aurium-token"]');
  if (meta) return meta.content;
  const fromHash = new URLSearchParams(location.hash.slice(1)).get("token");
  if (fromHash) {
    // Keep it out of the URL bar and out of any screenshot.
    history.replaceState(null, "", location.pathname);
    return fromHash;
  }
  return null;
}

function setConn(kind, label) {
  const el = $("#conn");
  el.className = `conn conn-${kind}`;
  el.textContent = label;
}

// ---- rendering ----

const SVG = "http://www.w3.org/2000/svg";
const LANE = 18;   // horizontal spacing between lineage lanes
const ROW = 44;    // row height, must match .node min-height

// Flattens the forest into rows, recording for each ancestor lane whether a
// sibling still follows below it. That is what decides if a lane draws a
// continuing vertical line or stops at an elbow.
function flatten(roots) {
  const rows = [];
  const walk = (node, depth, continues) => {
    rows.push({ node, depth, continues: continues.slice() });
    const kids = node.children ?? [];
    kids.forEach((child, i) => {
      const last = i === kids.length - 1;
      walk(child, depth + 1, continues.concat(!last));
    });
  };
  for (const r of roots) walk(r, 0, []);
  return rows;
}

function gutter(depth, continues, status) {
  const svg = document.createElementNS(SVG, "svg");
  const w = (depth + 1) * LANE + 8;
  svg.setAttribute("class", "gutter");
  svg.setAttribute("width", String(w));
  svg.setAttribute("height", String(ROW));
  const line = (x1, y1, x2, y2) => {
    const l = document.createElementNS(SVG, "line");
    l.setAttribute("x1", x1); l.setAttribute("y1", y1);
    l.setAttribute("x2", x2); l.setAttribute("y2", y2);
    svg.append(l);
  };
  // ancestor lanes that still have work below them
  for (let i = 0; i < depth; i++) {
    if (continues[i]) line(i * LANE + 9, 0, i * LANE + 9, ROW);
  }
  if (depth > 0) {
    const px = (depth - 1) * LANE + 9;
    const isLast = !continues[depth - 1];
    line(px, 0, px, isLast ? ROW / 2 : ROW);   // down from the parent lane
    line(px, ROW / 2, depth * LANE + 9, ROW / 2); // elbow into this node
  }
  const dot = document.createElementNS(SVG, "circle");
  dot.setAttribute("cx", String(depth * LANE + 9));
  dot.setAttribute("cy", String(ROW / 2));
  dot.setAttribute("r", "4.5");
  dot.setAttribute("class", `dot-${status}`);
  svg.append(dot);
  return svg;
}

function renderTree(data) {
  $("#project-name").textContent = data.project?.name ?? "—";
  const host = $("#tree");
  host.textContent = "";

  const roots = data.roots ?? [];
  $("#tree-empty").hidden = roots.length > 0;
  state.rows = flatten(roots);

  for (const { node, depth, continues } of state.rows) {
    const c = node.container ?? {};
    const sync = node.sync_status ?? "unknown";

    const row = document.createElement("div");
    row.className = "node" + (state.selected === c.id ? " is-selected" : "");
    row.tabIndex = 0;
    row.append(gutter(depth, continues, node.orphaned ? "error" : sync));

    const branch = document.createElement("span");
    branch.className = "node-branch";
    branch.textContent = c.branch ?? "(no branch)";
    row.append(branch);

    const badge = document.createElement("span");
    badge.className = `badge badge-${sync}`;
    badge.textContent = String(sync).replace(/_/g, " ");
    if (node.reason) badge.title = node.reason;
    row.append(badge);

    if (node.orphaned) {
      const orphan = document.createElement("span");
      orphan.className = "badge badge-error";
      orphan.textContent = "orphaned";
      orphan.title = "the parent container is gone";
      row.append(orphan);
    }

    for (const a of node.agents ?? []) {
      const chip = document.createElement("span");
      chip.className = "agent-chip" + (a.status === "running" ? " is-running" : "");
      chip.textContent = `${a.adapter ?? "agent"} ${a.role ?? ""}`.trim();
      chip.title = `${a.adapter ?? ""} ${a.role ?? ""} (${a.status ?? "?"})`;
      row.append(chip);
    }

    const spacer = document.createElement("span");
    spacer.className = "spacer";
    row.append(spacer);

    const id = document.createElement("span");
    id.className = "node-id";
    id.textContent = c.id ?? "";
    row.append(id);

    const open = () => selectContainer(c.id);
    row.addEventListener("click", open);
    row.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); }
    });
    host.append(row);
  }

  // Keep the detail pane in step with a tree that just moved under it.
  if (state.selected && !state.rows.some((r) => r.node.container?.id === state.selected)) {
    state.selected = null;
    renderDetail(null);
  }
}

// ---- container detail ----

function findRow(id) {
  return state.rows.find((r) => r.node.container?.id === id) ?? null;
}

async function selectContainer(id) {
  if (!id) return;
  state.selected = id;
  for (const el of document.querySelectorAll(".node")) el.classList.remove("is-selected");
  renderDetail(findRow(id), { loading: true });

  // The tree already carries agents; snapshots are a separate call, and a
  // failure there must still leave the rest of the pane usable.
  let snapshots = [];
  try {
    const data = await api(`/v1/containers/${encodeURIComponent(id)}/snapshots`);
    snapshots = Array.isArray(data) ? data : data?.snapshots ?? [];
  } catch {
    snapshots = null;
  }
  if (state.selected !== id) return; // selection moved while we were away
  renderDetail(findRow(id), { snapshots });
  renderTree.lastSelected = id;
  for (const el of document.querySelectorAll(".node")) {
    if (el.querySelector(".node-id")?.textContent === id) el.classList.add("is-selected");
  }
}

function renderDetail(row, opts = {}) {
  const host = $("#detail");
  host.textContent = "";
  if (!row) {
    const p = document.createElement("p");
    p.className = "empty";
    p.textContent = "Select a container to see its agents and snapshots.";
    host.append(p);
    return;
  }
  const c = row.node.container ?? {};

  const h = document.createElement("h3");
  h.textContent = c.branch ?? c.id ?? "container";
  host.append(h);

  const kv = document.createElement("dl");
  kv.className = "kv";
  const pair = (k, v) => {
    if (v === undefined || v === null || v === "") return;
    const dt = document.createElement("dt"); dt.textContent = k;
    const dd = document.createElement("dd"); dd.textContent = String(v);
    kv.append(dt, dd);
  };
  pair("id", c.id);
  pair("status", c.status);
  pair("sync", row.node.sync_status);
  pair("parent", c.parent_branch);
  pair("base", (c.base_sha ?? "").slice(0, 12));
  pair("worktree", c.worktree);
  const ports = Object.entries(c.ports ?? {});
  if (ports.length) pair("ports", ports.map(([k, v]) => `${k}:${v}`).join(" "));
  host.append(kv);

  const agents = row.node.agents ?? [];
  const ah = document.createElement("h4");
  ah.textContent = `agents (${agents.length})`;
  host.append(ah);
  if (agents.length) {
    const al = document.createElement("dl");
    al.className = "kv";
    for (const a of agents) {
      const dt = document.createElement("dt"); dt.textContent = a.adapter ?? "agent";
      const dd = document.createElement("dd"); dd.textContent = `${a.role ?? ""} ${a.status ?? ""}`.trim();
      al.append(dt, dd);
    }
    host.append(al);
  } else {
    const p = document.createElement("p"); p.className = "empty"; p.textContent = "none attached";
    host.append(p);
  }

  const sh = document.createElement("h4");
  sh.textContent = "snapshots";
  host.append(sh);
  if (opts.loading) {
    const p = document.createElement("p"); p.className = "empty"; p.textContent = "loading…";
    host.append(p);
  } else if (opts.snapshots === null) {
    const p = document.createElement("p"); p.className = "empty"; p.textContent = "could not load";
    host.append(p);
  } else if (!opts.snapshots?.length) {
    const p = document.createElement("p"); p.className = "empty"; p.textContent = "none yet";
    host.append(p);
  } else {
    for (const s of opts.snapshots.slice().reverse()) {
      const d = document.createElement("div");
      d.className = "snap";
      const seq = document.createElement("span"); seq.className = "snap-seq"; seq.textContent = `#${s.seq ?? "?"}`;
      const lab = document.createElement("span"); lab.className = "snap-label";
      lab.textContent = s.label || (s.head_sha ?? "").slice(0, 10) || s.id || "";
      const trg = document.createElement("span"); trg.className = "snap-trigger"; trg.textContent = s.trigger ?? "";
      d.append(seq, lab, trg);
      host.append(d);
    }
  }

  const actions = document.createElement("div");
  actions.className = "actions";
  const mini = (label, fn) => {
    const b = document.createElement("button");
    b.className = "mini";
    b.textContent = label;
    b.addEventListener("click", async (e) => {
      e.stopPropagation();
      b.disabled = true;
      try { await fn(); } finally { b.disabled = false; }
    });
    return b;
  };
  actions.append(
    mini("snapshot", async () => {
      await api(`/v1/containers/${encodeURIComponent(c.id)}/snapshot`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ label: "from dashboard" }),
      });
      await selectContainer(c.id);
    }),
    mini("sync", async () => {
      const out = await api(`/v1/containers/${encodeURIComponent(c.id)}/sync`, { method: "POST" });
      // A conflict is a normal outcome, not an error (openapi.yaml).
      if (out?.result === "conflict") {
        $("#project-name").textContent = `sync conflict: ${(out.files ?? []).join(", ")}`;
      }
      await refresh();
    }),
    // §11.3: no terminal emulator; hand the user the command instead.
    mini("copy attach", async () => {
      const cmd = `aurium attach ${c.id}`;
      try { await navigator.clipboard.writeText(cmd); } catch { window.prompt("attach with", cmd); }
    })
  );
  host.append(actions);
}

function renderTasks(tasks) {
  const body = $("#tasks");
  body.textContent = "";
  $("#tasks-empty").hidden = tasks.length > 0;

  for (const t of tasks) {
    const tr = document.createElement("tr");
    for (const value of [t.title, t.status, t.updated_at]) {
      const td = document.createElement("td");
      td.textContent = value ?? "";
      tr.append(td);
    }
    body.append(tr);
  }
}

function eventMatches(e) {
  if (!state.filter) return true;
  const q = state.filter.toLowerCase();
  return (
    e.type.toLowerCase().includes(q) ||
    (e.container_id ?? "").toLowerCase().includes(q) ||
    (e.actor ?? "").toLowerCase().includes(q)
  );
}

function renderEvents() {
  const list = $("#events");
  list.textContent = "";

  for (const e of state.events.filter(eventMatches).slice(-MAX_EVENTS)) {
    const li = document.createElement("li");

    const ts = document.createElement("span");
    ts.className = "ev-ts";
    ts.textContent = (e.ts ?? "").slice(11, 19);

    const type = document.createElement("span");
    type.className = "ev-type";
    type.textContent = e.type;

    const rest = document.createElement("span");
    rest.className = "ev-rest";
    const bits = [e.actor];
    if (e.container_id) bits.push(e.container_id);
    for (const [k, v] of Object.entries(e.payload ?? {})) {
      // Nested values stringify to "[object Object]", which tells the reader
      // nothing. Render them as compact JSON instead.
      const shown = v !== null && typeof v === "object" ? JSON.stringify(v) : v;
      bits.push(`${k}=${shown}`);
    }
    rest.textContent = bits.join("  ");

    li.append(ts, type, rest);
    list.append(li);
  }

  if ($("#autoscroll").checked) list.scrollTop = list.scrollHeight;
}

// ---- approvals ----
//
// The only part of the dashboard that writes. Approving executes the held
// upstream call inside the daemon (D17), so the button is a real action and is
// disabled while it is in flight.

// Tauri injects its notification API when the desktop shell loads this page;
// in a plain browser tab there is nothing to notify, and that is fine.
async function notify(title, body) {
  try {
    const n = window.__TAURI__?.notification;
    if (!n) return;
    let granted = await n.isPermissionGranted();
    if (!granted) granted = (await n.requestPermission()) === "granted";
    if (granted) n.sendNotification({ title, body });
  } catch {
    /* a missing notification must never break the panel */
  }
}

function relativeExpiry(iso) {
  if (!iso) return "no expiry";
  const ms = new Date(iso).getTime() - Date.now();
  if (Number.isNaN(ms)) return "no expiry";
  if (ms <= 0) return "expired";
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `expires in ${secs}s`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `expires in ${mins}m`;
  return `expires in ${Math.round(mins / 60)}h`;
}

function renderApprovals() {
  const host = $("#approvals");
  host.textContent = "";
  const pending = state.approvals.filter((a) => (a.status ?? "pending") === "pending");

  $("#approvals-empty").hidden = pending.length > 0;
  const badge = $("#approval-count");
  badge.hidden = pending.length === 0;
  badge.textContent = String(pending.length);
  // The desktop shell has no dock badge, but the window title shows up in the
  // app switcher and the dock tooltip, which is enough to notice from away.
  document.title = pending.length ? `Aurium (${pending.length})` : "Aurium";

  for (const a of pending) {
    const li = document.createElement("li");
    li.className = "card";

    const top = document.createElement("div");
    top.className = "card-top";
    const cap = document.createElement("span");
    cap.className = "cap";
    cap.textContent = a.capability ?? "(unnamed capability)";
    const exp = document.createElement("span");
    const rel = relativeExpiry(a.expires_at);
    exp.className = "expiry" + (rel === "expired" || /in \d+s$/.test(rel) ? " soon" : "");
    exp.textContent = rel;
    top.append(cap, exp);

    const sub = document.createElement("div");
    sub.className = "card-sub";
    sub.textContent = [a.container_id, a.agent_id, a.id].filter(Boolean).join(" \u00b7 ");

    const actions = document.createElement("div");
    actions.className = "actions";
    const approve = document.createElement("button");
    approve.className = "act approve";
    approve.textContent = "Approve";
    const reject = document.createElement("button");
    reject.className = "act reject";
    reject.textContent = "Reject";

    const decide = async (decision) => {
      approve.disabled = reject.disabled = true;
      try {
        await api(`/v1/approvals/${encodeURIComponent(a.id)}/decide`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ decision, by: "dashboard" }),
        });
        await refreshApprovals();
      } catch (err) {
        setConn("down", "error");
        $("#project-name").textContent = String(err.message ?? err);
        approve.disabled = reject.disabled = false;
      }
    };
    approve.addEventListener("click", () => decide("approved"));
    reject.addEventListener("click", () => decide("rejected"));
    actions.append(approve, reject);

    li.append(top, sub, actions);
    host.append(li);
  }
}

async function refreshApprovals() {
  const data = await api("/v1/approvals?status=pending");
  const list = Array.isArray(data) ? data : data?.approvals ?? [];

  // Notify only for approvals this page has not seen, so a reconnect or a
  // periodic refresh does not re-announce the same blocked agent.
  const fresh = list.filter((a) => a.id && !state.seenApprovals.has(a.id));
  for (const a of list) if (a.id) state.seenApprovals.add(a.id);
  state.approvals = list;
  renderApprovals();

  if (state.booted && fresh.length === 1) {
    notify("Aurium: approval needed", fresh[0].capability ?? "An agent is waiting.");
  } else if (state.booted && fresh.length > 1) {
    notify("Aurium: approvals needed", `${fresh.length} agents are waiting on you.`);
  }
}

// ---- data ----

async function refresh() {
  const { projects } = await api("/v1/projects");
  if (!projects?.length) {
    $("#project-name").textContent = "no project";
    return;
  }
  state.project = projects[0];

  const [tree, tasks] = await Promise.all([
    api(`/v1/projects/${state.project.id}/tree`),
    api(`/v1/projects/${state.project.id}/tasks`),
  ]);
  renderTree(tree);
  renderTasks(tasks.tasks ?? []);
  await refreshApprovals();
}

function connect() {
  setConn("connecting", "connecting");

  // EventSource cannot set an Authorization header, so the token goes in the
  // query string. The stream is loopback-only, and the token is already in
  // this page.
  // since=0 on first connect replays the whole trail, so opening the
  // dashboard shows history rather than an empty pane until something happens.
  // On a reconnect it resumes from the last id seen, so nothing is missed.
  const params = new URLSearchParams({
    token: state.token,
    since: state.lastEventID ?? 0,
  });

  const es = new EventSource(`/v1/events?${params}`);

  es.onopen = () => setConn("live", "live");

  es.onmessage = (msg) => {
    try {
      const e = JSON.parse(msg.data);
      state.lastEventID = e.id;
      state.events.push(e);
      if (state.events.length > MAX_EVENTS * 2) {
        state.events = state.events.slice(-MAX_EVENTS);
      }
      renderEvents();
      // Anything that changes the world redraws the tree. An approval event
      // only touches the inbox, which is the cheaper and more urgent path.
      if (e.type.startsWith("approval")) refreshApprovals().catch(() => {});
      else if (!e.type.startsWith("agent.message")) refresh().catch(() => {});
    } catch {
      /* a malformed frame must not kill the stream */
    }
  };

  es.onerror = () => {
    setConn("down", "reconnecting");
    es.close();
    // Reconnect with `since`, so nothing is missed across the gap.
    setTimeout(connect, 2000);
  };
}

function initTabs() {
  for (const tab of document.querySelectorAll(".tab")) {
    tab.addEventListener("click", () => {
      for (const t of document.querySelectorAll(".tab")) t.classList.remove("is-active");
      for (const p of document.querySelectorAll(".panel")) p.classList.remove("is-active");
      tab.classList.add("is-active");
      $(`#panel-${tab.dataset.panel}`).classList.add("is-active");
    });
  }
  $("#event-filter").addEventListener("input", (e) => {
    state.filter = e.target.value;
    renderEvents();
  });
}

function showPanel(name) {
  for (const t of document.querySelectorAll(".tab")) {
    t.classList.toggle("is-active", t.dataset.panel === name);
  }
  for (const p of document.querySelectorAll(".panel")) {
    p.classList.toggle("is-active", p.id === `panel-${name}`);
  }
}

// Keyboard first: this is a window someone leaves open, and reaching for the
// mouse to clear an approval queue is the wrong shape.
function initKeys() {
  const PANELS = ["approvals", "tree", "tasks", "events"];
  window.addEventListener("keydown", (e) => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName ?? "");
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

    if (e.key >= "1" && e.key <= "4") {
      showPanel(PANELS[Number(e.key) - 1]);
      return;
    }
    if (e.key === "r") {
      refresh().catch(() => {});
      return;
    }
    // a / x act on the first pending approval, which is the one on top.
    if (e.key === "a" || e.key === "x") {
      const btn = document.querySelector(
        `#approvals .card ${e.key === "a" ? ".approve" : ".reject"}`
      );
      if (btn) { showPanel("approvals"); btn.click(); }
      return;
    }
    if (e.key === "j" || e.key === "k") {
      const ids = state.rows.map((r) => r.node.container?.id).filter(Boolean);
      if (!ids.length) return;
      const at = ids.indexOf(state.selected);
      const next = e.key === "j"
        ? ids[Math.min(at + 1, ids.length - 1)] ?? ids[0]
        : ids[Math.max(at - 1, 0)] ?? ids[0];
      showPanel("tree");
      selectContainer(next);
    }
  });
}

async function main() {
  state.token = readToken();
  initTabs();
  initKeys();

  if (!state.token) {
    setConn("down", "no token");
    $("#project-name").textContent = "open the dashboard with `aurium dashboard`";
    return;
  }

  try {
    await refresh();
  } catch (err) {
    setConn("down", "error");
    $("#project-name").textContent = String(err.message ?? err);
    return;
  }
  state.booted = true;
  connect();
  // Expiry is shown relative, so the cards must re-render even when idle.
  setInterval(renderApprovals, 15000);
}

main();
