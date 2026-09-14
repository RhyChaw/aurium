// Light, dark, or follow the system.
//
// Three states rather than two. A toggle that only flips between light and
// dark has no way back to "whatever this machine is set to", which is the
// setting most people actually want and the one the page starts in.
//
// The choice is kept in localStorage: it is a per-browser convenience with no
// consequence if it comes back empty, which is exactly what that store is for.

const KEY = "aurium.theme";
const ORDER = ["system", "light", "dark"];

export function initTheme(button) {
  let mode = read();
  apply(mode);
  paint(button, mode);

  button?.addEventListener("click", () => {
    mode = ORDER[(ORDER.indexOf(mode) + 1) % ORDER.length];
    write(mode);
    apply(mode);
    paint(button, mode);
  });
}

function apply(mode) {
  const root = document.documentElement;
  if (mode === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", mode);
}

function paint(button, mode) {
  if (!button) return;
  button.textContent = mode;
  button.setAttribute("aria-label", `Theme: ${mode}. Click to change.`);
}

function read() {
  try {
    const v = localStorage.getItem(KEY);
    return ORDER.includes(v) ? v : "system";
  } catch {
    // Private windows and blocked site data throw on access, not on read. The
    // page must still render, in the system theme.
    return "system";
  }
}

function write(mode) {
  try {
    localStorage.setItem(KEY, mode);
  } catch {
    /* a preference that cannot be remembered is not an error */
  }
}
