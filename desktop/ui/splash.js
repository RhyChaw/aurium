// Waits for the daemon, then hands the window over to it.
//
// /v1/health is deliberately unauthenticated (openapi.yaml) but this page is a
// different origin, so the response is unreadable without CORS. It does not
// need to be readable: with mode "no-cors" the request resolves when something
// answered and rejects when nothing is listening, which is exactly the
// liveness signal required. Once it answers, the window navigates to the
// daemon and everything after that point is same-origin.

const $ = (sel) => document.querySelector(sel);
const POLL_MS = 900;
const PATIENCE_MS = 6000;

let url = "http://127.0.0.1:7770";
let started = Date.now();
let timer = null;

async function alive() {
  try {
    await fetch(`${url}/v1/health`, { mode: "no-cors", cache: "no-store" });
    return true;
  } catch {
    return false;
  }
}

function giveUp() {
  clearTimeout(timer);
  $("#status").textContent = `No daemon on ${url}. Start it, then try again.`;
  $("#hint").hidden = false;
  $("#retry").hidden = false;
}

async function poll() {
  if (await alive()) {
    $("#status").textContent = "connected, opening…";
    location.replace(url + "/");
    return;
  }
  if (Date.now() - started > PATIENCE_MS) return giveUp();
  timer = setTimeout(poll, POLL_MS);
}

$("#retry").addEventListener("click", () => {
  started = Date.now();
  $("#hint").hidden = true;
  $("#retry").hidden = true;
  $("#status").textContent = "looking for the daemon…";
  poll();
});

(async () => {
  try {
    const invoke = window.__TAURI_INTERNALS__?.invoke;
    if (invoke) url = (await invoke("daemon_url", {})).replace(/\/$/, "");
  } catch {
    /* fall back to the loopback default */
  }
  poll();
})();
