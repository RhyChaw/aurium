// The right pane: one agent's conversation.
//
// It is not a terminal. §11.3 settled that, and the reasoning still holds —
// the dashboard hands you `aurium attach` for taking the wheel. What this pane
// is for is the question a terminal answers badly: what has this agent been
// saying, to whom, and is anything waiting on me.
//
// The transcript merges what the agent sent and what was sent to it. A message
// with no sending agent came from a human, which is how the two are told apart
// without a separate field.

import { el, mount, shortID, ago } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
import { refreshTranscript } from "./rail.js";

// Message types that mean something is wrong or waiting, so they can be
// coloured rather than read.
const URGENT = new Set(["BLOCKED", "APPROVAL_REQUIRED", "CONFLICT", "WARNING"]);

export function renderChat(host) {
  if (!state.openAgent) {
    mount(host, el("div.chat-empty",
      el("p", "Pick an agent on the left to see what it is doing.")));
    return;
  }

  const detail = state.agentDetail;
  const a = detail?.agent ?? {};
  const messages = state.transcript ?? [];

  mount(host,
    header(detail, a),
    el("div.chat-scroll", { id: "chat-scroll" },
      messages.length
        ? messages.map(bubble)
        : el("p.empty", "Nothing said yet. This agent has sent and received no messages.")),
    composer(a));

  // Pin to the newest message. A transcript that opens at the top means
  // scrolling past a week of history to find out what is happening now.
  const scroll = host.querySelector("#chat-scroll");
  if (scroll) scroll.scrollTop = scroll.scrollHeight;
}

function header(detail, a) {
  const name = a.display_name || a.adapter || "agent";
  const band = detail?.state ?? "done";
  const account = detail?.account;

  return el("div.chat-head",
    el("div.chat-id",
      el("span.chat-dot", { dataset: { state: band } }),
      el("span.chat-name", name),
      el("span.chat-status", a.status ?? "")),
    el("div.chat-meta",
      detail?.container ? el("span", detail.container.branch) : null,
      account
        ? el("span", { title: `${account.label} · ${account.auth_kind}` },
            account.provider)
        : el("span.muted", "no account"),
      el("span.muted", shortID(a.id))),
    el("div.chat-actions",
      el("button.mini", {
        title: "Take the wheel in a terminal",
        onclick: () => copyAttach(detail?.container?.id),
      }, "copy attach")));
}

function bubble(m) {
  // A message with no sending agent came from a human at this dashboard.
  const fromHuman = !m.from?.agent && !m.from?.container;
  const toHuman = m.to?.human;
  const urgent = URGENT.has(m.type);

  const who = fromHuman ? "you" : (m.from?.agent ? shortID(m.from.agent) : "agent");

  return el("article.msg", {
    class: [fromHuman ? "from-human" : "from-agent", urgent ? "is-urgent" : ""]
      .filter(Boolean).join(" "),
  },
    el("div.msg-head",
      el("span.msg-who", who),
      el("span.msg-type", m.type),
      m.priority === "high" ? el("span.msg-high", "high") : null,
      toHuman && !fromHuman ? el("span.msg-to", "→ you") : null,
      el("span.msg-when", { title: m.ts }, ago(m.ts)),
      // Status is on every bubble because at-least-once delivery makes it
      // meaningful: "queued" means the agent has not been handed this yet.
      el("span.msg-state", m.status)),
    el("div.msg-body", m.content ?? ""),
    refs(m.refs));
}

function refs(r) {
  if (!r) return null;
  const bits = [];
  if (r.task) bits.push(`task ${shortID(r.task)}`);
  if (r.branch) bits.push(r.branch);
  if (r.context_key) bits.push(r.context_key);
  for (const f of r.files ?? []) bits.push(f);
  if (!bits.length) return null;
  return el("div.msg-refs", bits.map((b) => el("code.ref", b)));
}

function composer(a) {
  const input = el("textarea.composer-input", {
    placeholder: "Say something to this agent…   (⌘/Ctrl + Enter to send)",
    rows: 2,
    disabled: state.sending,
  });

  const send = async () => {
    const content = input.value.trim();
    if (!content || state.sending) return;
    update({ sending: true });
    try {
      const out = await Aurium.sendToAgent(a.id, { content });
      input.value = "";
      // "typed" says the text went into the agent's live session; without it
      // the message is only in the inbox, and telling the user it was
      // delivered would be a claim nobody checked.
      update({ sending: false, notice: out?.typed ? null : "queued to the agent's inbox" });
      await refreshTranscript(a.id);
    } catch (err) {
      update({ sending: false, notice: err.message ?? String(err) });
    }
  };

  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      send();
    }
  });

  return el("div.composer", input,
    el("button.act.primary", { onclick: send, disabled: state.sending },
      state.sending ? "sending…" : "Send"));
}

async function copyAttach(containerID) {
  if (!containerID) return;
  const cmd = `aurium attach ${containerID}`;
  try {
    await navigator.clipboard.writeText(cmd);
    update({ notice: `copied: ${cmd}` });
  } catch {
    window.prompt("attach with", cmd);
  }
}
