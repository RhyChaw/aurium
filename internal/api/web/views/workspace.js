// The workspace: rail, pulse, chat.
//
// Three panes because the three questions a human has are different shapes.
// "Where is everything" is a list you scan. "Is it alive" is a picture you
// glance at. "What is this one doing" is a conversation you read. Putting any
// two of them in one pane makes the other worse.

import { el, mount } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { renderRail } from "./rail.js";
import { renderHeartbeat } from "./heartbeat.js";
import { renderChat } from "./chat.js";
import { renderRepos } from "./repos.js";

let panes = null;

export function renderWorkspace(host) {
  // The panes are built once and re-rendered in place. Rebuilding the whole
  // grid on every event would lose the chat pane's scroll position and the
  // canvas's backing store several times a second.
  if (!panes || !host.contains(panes.root)) {
    panes = build();
    mount(host, panes.root);
  }

  renderRail(panes.rail);
  renderHeartbeat(panes.centre);
  renderRepos(panes.repos);
  renderChat(panes.chat);
}

function build() {
  const rail = el("div.rail");
  const centre = el("div.centre-inner");
  const repos = el("div.repos-strip");
  const chat = el("div.chat");

  const root = el("div.workspace",
    el("aside.rail-col",
      el("div.rail-head",
        el("h2", "Fleet"),
        el("button.mini", { onclick: () => update({ panel: "home" }) }, "+ project")),
      rail),
    el("section.centre-col", centre, repos),
    el("aside.chat-col", chat));

  return { root, rail, centre, repos, chat };
}
