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

function renderTree(data) {
  $("#project-name").textContent = data.project?.name ?? "—";
  const host = $("#tree");
  host.textContent = "";

  const roots = data.roots ?? [];
  $("#tree-empty").hidden = roots.length > 0;

  const walk = (node) => {
    const row = document.createElement("div");
    row.className = "node";
    row.style.marginLeft = `${node.depth * 1.5}rem`;

    const branch = document.createElement("span");
    branch.className = "node-branch";
    branch.textContent = node.container.branch;
    row.append(branch);

    const badge = document.createElement("span");
    badge.className = `badge badge-${node.sync_status}`;
    badge.textContent = node.sync_status.replace(/_/g, " ");
    if (node.reason) badge.title = node.reason;
    row.append(badge);

    const id = document.createElement("span");
    id.className = "node-id";
    id.textContent = node.container.id;
    row.append(id);

    if (node.orphaned) {
      const orphan = document.createElement("span");
      orphan.className = "badge badge-error";
      orphan.textContent = "orphaned";
      orphan.title = "the parent container is gone";
      row.append(orphan);
    }

    const agents = document.createElement("span");
    agents.className = "node-agents";
    agents.textContent = (node.agents ?? [])
      .map((a) => `${a.adapter} ${a.role} (${a.status})`)
      .join(", ");
    row.append(agents);

    // §11.3: no terminal emulator; hand the user the command instead.
    const attach = document.createElement("button");
    attach.className = "attach";
    attach.textContent = "copy attach";
    attach.addEventListener("click", async () => {
      const cmd = `aurium attach ${node.container.id}`;
      try {
        await navigator.clipboard.writeText(cmd);
        attach.textContent = "copied";
      } catch {
        // Clipboard is blocked in some contexts; show the command so the
        // user can still copy it by hand rather than silently failing.
        attach.textContent = cmd;
      }
      setTimeout(() => (attach.textContent = "copy attach"), 2000);
    });
    row.append(attach);

    host.append(row);
    (node.children ?? []).forEach(walk);
  };
  roots.forEach(walk);
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
      // Anything that changes the world redraws the tree.
      if (!e.type.startsWith("agent.message")) refresh().catch(() => {});
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

async function main() {
  state.token = readToken();
  initTabs();

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
  connect();
}

main();
