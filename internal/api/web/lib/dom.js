// Element construction without a framework.
//
// The dashboard has no build step (a deviation recorded in the plan doc), so
// there is no JSX and no template compiler. What replaces them is this: one
// function that builds a DOM node from a tag, a props object and children.
//
// Everything is set through properties and textContent rather than innerHTML.
// Branch names, agent output and IPC message bodies all reach this page, and
// none of them are trusted markup.

/**
 * el("div.card", {onclick}, "text", child)
 *
 * The tag accepts a CSS-ish shorthand: "button.act.approve" is a button with
 * two classes. Anything after the first dot is a class name.
 */
export function el(spec, props = null, ...children) {
  const [tag, ...classes] = String(spec).split(".");
  const node = document.createElement(tag || "div");
  if (classes.length) node.className = classes.join(" ");

  if (props && typeof props === "object" && !(props instanceof Node) && !Array.isArray(props)) {
    for (const [key, value] of Object.entries(props)) {
      if (value === null || value === undefined || value === false) continue;
      if (key === "class") {
        node.className = node.className ? `${node.className} ${value}` : value;
      } else if (key === "style" && typeof value === "object") {
        Object.assign(node.style, value);
      } else if (key === "dataset") {
        Object.assign(node.dataset, value);
      } else if (key.startsWith("on") && typeof value === "function") {
        node.addEventListener(key.slice(2), value);
      } else if (key in node && key !== "list") {
        node[key] = value;
      } else {
        node.setAttribute(key, value);
      }
    }
  } else if (props !== null && props !== undefined) {
    children.unshift(props);
  }

  append(node, children);
  return node;
}

function append(node, children) {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (Array.isArray(child)) {
      append(node, child);
    } else if (child instanceof Node) {
      node.append(child);
    } else {
      node.append(document.createTextNode(String(child)));
    }
  }
}

/** svg() is el() for the SVG namespace, which createElement cannot produce. */
export function svg(spec, props = null, ...children) {
  const [tag, ...classes] = String(spec).split(".");
  const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
  if (classes.length) node.setAttribute("class", classes.join(" "));
  for (const [key, value] of Object.entries(props ?? {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key.startsWith("on") && typeof value === "function") {
      node.addEventListener(key.slice(2), value);
    } else {
      node.setAttribute(key, value);
    }
  }
  append(node, children);
  return node;
}

/** Replaces a host's children in one operation. */
export function mount(host, ...children) {
  host.replaceChildren();
  append(host, children);
  return host;
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/** A compact id for display: "a_01J9Z3K…" rather than 28 characters of ULID. */
export function shortID(id) {
  if (!id) return "";
  const i = id.indexOf("_");
  return i >= 0 && id.length > i + 7 ? id.slice(0, i + 7) + "…" : id;
}

/**
 * Relative time, because "3m ago" is legible at a glance and an ISO timestamp
 * is not. Falls back to the raw string rather than showing "Invalid Date".
 */
export function ago(iso) {
  if (!iso) return "";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  const secs = Math.round((Date.now() - then) / 1000);
  if (secs < 0) return "just now";
  if (secs < 45) return `${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

/** Thousands separators, so a seven-digit token count can be read. */
export function num(n) {
  return Number(n ?? 0).toLocaleString();
}

/**
 * Money, with enough precision to be useful at both ends: a fleet that has
 * spent $0.004 and one that has spent $412 are both worth reading exactly.
 */
export function usd(n) {
  const v = Number(n ?? 0);
  if (v === 0) return "$0.00";
  if (v < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

/** Compact token counts for tight spaces: 12.4k, 3.1M. */
export function compact(n) {
  const v = Number(n ?? 0);
  if (v >= 1e9) return `${(v / 1e9).toFixed(1)}B`;
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(1)}k`;
  return String(v);
}
