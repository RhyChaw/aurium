# Aurium desktop

A native window for the Aurium daemon, built with [Tauri 2](https://tauri.app).

```
desktop/
  ui/                 the splash: waits for the daemon, then hands over
  src-tauri/          the Rust shell (one command, one plugin)
```

## What it is, and what it deliberately is not

The daemon is the product boundary. `api/openapi.yaml` says so, and it already
serves the dashboard at `/` with the host token injected server-side. So this
shell **does not reimplement the UI**. It waits for the daemon to answer
`/v1/health`, then navigates the window to it.

That is a forced choice, not a lazy one. A UI bundled inside the app would be a
different origin from `http://127.0.0.1:7770`, and a cross-origin `fetch`
carrying an `Authorization` header requires CORS headers the daemon does not
send — correctly, for a loopback-only service. Loading the daemon's own page
keeps everything same-origin, so `fetch` and `EventSource` work untouched and
the token never leaves the daemon's response.

What the window adds over a browser tab:

| | |
|---|---|
| Its own window and dock entry | the dashboard stops competing with 40 tabs |
| No URL bar | the page carries a credential; nothing to copy out or screenshot |
| A splash that waits | starting the app before the daemon shows "looking for the daemon", not a connection error |
| **Native notifications** | an agent blocked on an approval reaches you when the window is behind something else |
| A window sized for three panes | the dashboard's workspace is a rail, a pulse and a chat pane; below 900px they stack, so the window has a 940px minimum |

That last row is the reason to build this at all. A pending approval is an
agent sitting idle waiting for a human, and the only fix is getting a human's
attention.

## What the window shows

The daemon's dashboard, which as of 2026-09-14 is the workspace described in
`docs/superpowers/specs/2026-09-14-agent-os-dashboard-design.md`: every
project's agents in a left rail with a colour per state, a live pulse of the
fleet in the middle, and one agent's conversation on the right. Tabs alongside
it cover approvals, spend, and the provider accounts agents run on.

None of that lives in this app. It is the daemon's page, and a browser tab at
`http://127.0.0.1:7770` shows exactly the same thing.

## Approvals

`/v1/approvals` and `/v1/approvals/{id}/decide` existed in the API before this
app did, and nothing surfaced them. The dashboard now leads with an approvals
inbox (`internal/api/web/`), so it is available in a browser too; the desktop
shell adds the notification on top. Approving executes the held upstream call
inside the daemon and the result is delivered to the agent as a RESPONSE
message, which is why it cannot be done from the CLI process.

## Running it

Prerequisites: the [Tauri 2 prerequisites](https://tauri.app/start/prerequisites/)
(Rust plus the platform webview toolchain) and the Tauri CLI:

```bash
cargo install tauri-cli --version "^2"
```

Then, with the daemon running:

```bash
aurium daemon                       # writes ~/.aurium/token, listens on :7770
cd desktop && cargo tauri dev
```

To produce a bundle:

```bash
cd desktop && cargo tauri build      # .app/.dmg, .msi, .deb/.AppImage
```

`AURIUM_DAEMON_URL` overrides the daemon address. If you change it, add the new
origin to the `remote.urls` list in `src-tauri/capabilities/daemon.json` as
well, or notifications will not be granted on that origin.

## Status

The splash logic is verified against a mock daemon (both outcomes: daemon
present and absent).

The dashboard it hands over to was driven against a **real** daemon on
2026-09-14 — creating a project across two repositories, the rail's four colour
bands, opening an agent, sending it a message and seeing it land as IPC, the
usage aggregates, and connecting and dropping a provider account. Two defects
were found that way and fixed: `a` approved a held upstream call from any
panel, and the rail's project header read out as a filesystem path to a screen
reader.

**The Rust side has never been compiled.** It is ~30 lines with one command and
one plugin, but `cargo tauri dev` on a machine with the toolchain is the first
real test of it. `src-tauri/icons/` holds a single `icon.png`; run
`cargo tauri icon src-tauri/icons/icon.png` to generate the full platform set
before shipping a bundle.
