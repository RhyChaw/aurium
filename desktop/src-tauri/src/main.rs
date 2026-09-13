// Aurium desktop shell.
//
// The daemon is the product boundary (api/openapi.yaml), and it already serves
// the dashboard with the host token injected server-side. So this shell does
// not reimplement the UI: it waits for the daemon and then points the window
// at it.
//
// That choice is forced, not lazy. A UI bundled into the app would be a
// different origin from http://127.0.0.1:7770, and a cross-origin fetch
// carrying an Authorization header needs CORS headers the daemon does not send
// (correctly, for a loopback service). Loading the daemon's own page keeps
// everything same-origin, so fetch and EventSource work untouched and the
// token never leaves the daemon's response.
//
// What the shell adds over a browser tab: its own window and dock entry, no
// URL bar holding a credential, a splash that waits instead of showing a
// connection error, and native notifications when an agent is blocked on a
// human decision.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

const DEFAULT_DAEMON: &str = "http://127.0.0.1:7770";

/// Where the daemon is expected to be listening.
///
/// Honours AURIUM_DAEMON_URL so a non-default port does not need a rebuild.
/// Note that changing it also means adding that origin to the `remote` block in
/// capabilities/default.json, or notifications will not be granted there.
#[tauri::command]
fn daemon_url() -> String {
    std::env::var("AURIUM_DAEMON_URL").unwrap_or_else(|_| DEFAULT_DAEMON.to_string())
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        .invoke_handler(tauri::generate_handler![daemon_url])
        .run(tauri::generate_context!())
        .expect("aurium desktop failed to start");
}
