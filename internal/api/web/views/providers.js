// Providers: the accounts agents run on.
//
// Two truths this page has to tell without burying them. First, Aurium never
// shows a credential back to you — it holds a keyring reference, and there is
// no route that resolves one. Second, there is no OAuth client Aurium could
// legitimately drive for a Claude or ChatGPT subscription (D25), so a seat is
// connected through the provider CLI's own login. Pretending otherwise would
// be a worse experience than a clear two-step.

import { el, mount } from "../lib/dom.js";
import { state, update } from "../lib/state.js";
import { Aurium } from "../lib/api.js";
import { confirm } from "../lib/dialog.js";

export function renderProviders(host) {
  const detected = state.detected?.providers ?? [];
  const accounts = state.providers ?? [];

  mount(host,
    el("div.providers",
      el("p.lede",
        "Agents run on your accounts. Aurium stores the credential in your OS " +
        "keyring and keeps a reference — never the secret — in its database, and " +
        "no part of this API can hand one back."),
      state.detected && state.detected.keyring === false
        ? el("p.warn-bar",
            "No OS keyring is available on this machine, so credentials fall back to " +
            "a 0600 file under ~/.aurium. That file is not encrypted at rest.")
        : null,
      el("div.provider-grid", detected.map((d) => providerCard(d, accounts))),
      githubCard()));
}

/**
 * The GitHub card.
 *
 * Two different things live behind one connection and the card says so, because
 * they are not the same permission: Aurium listing your repositories is you
 * looking at your own account, and an agent calling github_* is something else
 * entirely — and only the second one can open a pull request while you are at
 * lunch.
 */
function githubCard() {
  const gh = state.github;

  return el("section.provider-card",
    el("header.provider-head",
      el("h3", "GitHub"),
      gh?.connected
        ? el("span.pill", gh.source === "gh_cli" ? "via gh CLI" : "stored token")
        : el("span.pill.ghost", "not connected")),

    gh === null || gh === undefined
      ? el("p.empty", "checking…")
      : gh.connected
        ? el("div",
            el("p.hint", "Connected as ", el("strong", gh.login),
              gh.name ? ` (${gh.name})` : "",
              ". Aurium borrows the token ", el("code", "gh"),
              " already holds rather than storing a second copy of it — one thing to revoke, ",
              "and nothing to go stale."),
            el("p.hint", "You can now pick repositories from GitHub when creating a project, ",
              "and a container's branch shows its pull request and build state in the rail."))
        : el("div",
            gh.error
              ? el("p.error-text", gh.error)
              : el("p.hint", "No GitHub connection."),
            el("p.hint", "Run ", el("code", "gh auth login"), " in a terminal. ",
              "Aurium reads the token from ", el("code", "gh"),
              " and never keeps a copy.")),

    el("div.connect-block",
      el("h4", "Agent tools"),
      el("p.hint",
        "Separate from the above. This spawns the GitHub MCP server behind the ",
        "gateway so agents can search repositories, read files and open pull ",
        "requests as ", el("code", "github_*"), " tools — under per-container ",
        "grants, with an approval held for anything dangerous. The agents never ",
        "see the token."),
      state.openProject
        ? el("button.act", {
            disabled: !gh?.connected || state.githubToolsBusy,
            onclick: () => enableTools(state.openProject),
          }, state.githubToolsBusy ? "connecting…" : "Enable for the open project")
        : el("p.hint", "Open a project first — tools are granted per project."),
      state.githubTools
        ? el("p.dialog-status", state.githubTools)
        : null));
}

async function enableTools(projectID) {
  update({ githubToolsBusy: true, githubTools: null });
  try {
    const out = await Aurium.githubTools(projectID);
    update({
      githubToolsBusy: false,
      githubTools: out.status === "connected"
        ? `Connected — ${out.capabilities} capabilities registered. Grant them per container with \`aurium integration grant github\`.`
        : `The server reported: ${out.last_error || out.status}`,
    });
  } catch (err) {
    update({ githubToolsBusy: false, githubTools: err.message ?? String(err) });
  }
}

function providerCard(d, accounts) {
  const mine = accounts.filter((a) => a.provider === d.provider);

  return el("section.provider-card",
    el("header.provider-head",
      el("h3", d.display),
      el("span.muted", `adapter: ${d.adapter}`)),

    mine.length
      ? el("ul.account-list", mine.map(accountRow))
      : el("p.empty", "No account connected. Agents fall back to whatever this " +
          "terminal exported, which is not a choice anybody made."),

    el("div.connect-block",
      el("h4", "Connect"),
      apiKeyForm(d),
      subscriptionForm(d)));
}

function accountRow(a) {
  return el("li.account", { dataset: { status: a.status } },
    el("div.account-main",
      el("span.account-label", a.label),
      el("span.pill", a.auth_kind === "subscription" ? "subscription" : "API key"),
      el("span.pill.ghost", sourceLabel(a.source))),
    el("div.account-meta",
      el("code.ref", a.secret_ref || "held by the provider CLI"),
      a.status === "error" ? el("span.error-text", a.last_error || "not working") : null),
    el("button.mini.danger", {
      onclick: async () => {
        // Disconnecting deletes the credential as well as the row, which is
        // not obvious from the word, so it is said before it happens.
        const sure = await confirm(`Disconnect "${a.label}"?`,
          "This deletes the stored credential. Agents already running keep the copy " +
          "in their environment and carry on.",
          { confirmLabel: "Disconnect", danger: true });
        if (!sure) return;
        try {
          await Aurium.disconnectProvider(a.id);
          await reloadProviders();
        } catch (err) {
          update({ notice: err.message ?? String(err) });
        }
      },
    }, "disconnect"));
}

function sourceLabel(source) {
  switch (source) {
    case "host_env": return "from this host's environment";
    case "cli_login": return "the CLI's own login";
    default: return "pasted";
  }
}

function apiKeyForm(d) {
  const label = el("input.field", { placeholder: `${d.adapter} api key` });
  const secret = el("input.field", { type: "password", placeholder: "sk-…", autocomplete: "off" });
  const status = el("p.dialog-status");

  const hasHostKey = (d.host_env ?? []).includes(d.api_key_env);

  const connect = async (body) => {
    status.className = "dialog-status";
    status.textContent = "connecting…";
    try {
      await Aurium.connectProvider({
        provider: d.provider, auth_kind: "api_key",
        label: label.value.trim(), ...body,
      });
      secret.value = "";
      label.value = "";
      status.textContent = "";
      await reloadProviders();
    } catch (err) {
      status.className = "dialog-status is-error";
      status.textContent = err.message ?? String(err);
    }
  };

  return el("div.connect-option",
    el("h5", "Pay as you go — an API key"),
    el("div.connect-row", label, secret,
      el("button.act", { onclick: () => connect({ secret: secret.value }) }, "Connect")),
    hasHostKey
      ? el("p.hint",
          el("button.mini", { onclick: () => connect({ use_host_env: true }) },
            `Use $${d.api_key_env}`),
          " — this host already exports it.")
      : el("p.hint", `Nothing in $${d.api_key_env} on this host.`),
    status);
}

function subscriptionForm(d) {
  const label = el("input.field", { placeholder: `${d.adapter} subscription` });
  const secret = el("input.field", { type: "password", placeholder: "paste the token", autocomplete: "off" });
  const status = el("p.dialog-status");

  const hasHostToken = d.subscription_env && (d.host_env ?? []).includes(d.subscription_env);

  const connect = async (body) => {
    status.className = "dialog-status";
    status.textContent = "connecting…";
    try {
      await Aurium.connectProvider({
        provider: d.provider, auth_kind: "subscription",
        label: label.value.trim(), ...body,
      });
      secret.value = "";
      label.value = "";
      status.textContent = "";
      await reloadProviders();
    } catch (err) {
      status.className = "dialog-status is-error";
      status.textContent = err.message ?? String(err);
    }
  };

  return el("div.connect-option",
    el("h5", "Subscription — your existing plan"),
    el("p.hint",
      "There is no public OAuth client Aurium can drive for a subscription, so " +
      "this goes through the provider's own CLI. Run ",
      el("code", d.subscription_command),
      d.subscription_env
        ? el("span", " and paste the token it prints.")
        : el("span", " once, then use the login it leaves on this machine.")),

    d.cli_login
      ? el("div.connect-row",
          label,
          el("button.act.primary", { onclick: () => connect({ use_cli_login: true }) },
            "Use the login on this machine"),
          el("span.hint", { title: d.cli_login }, "found"))
      : null,

    d.subscription_env
      ? el("div.connect-row", label, secret,
          el("button.act", { onclick: () => connect({ secret: secret.value }) }, "Connect"))
      : null,

    hasHostToken
      ? el("p.hint",
          el("button.mini", { onclick: () => connect({ use_host_env: true }) },
            `Use $${d.subscription_env}`),
          " — this host already exports it.")
      : null,
    status);
}

export async function reloadProviders() {
  try {
    const [{ accounts }, detected, gh] = await Promise.all([
      Aurium.providers(),
      Aurium.detectProviders(),
      // A GitHub check is a network round trip, so it must not be able to stop
      // the rest of the page rendering.
      Aurium.github().catch(() => ({ connected: false, error: "could not be checked" })),
    ]);
    update({ providers: accounts ?? [], detected, github: gh });
  } catch (err) {
    update({ notice: err.message ?? String(err) });
  }
}
