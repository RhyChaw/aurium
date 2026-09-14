// The daemon's HTTP API, from the browser.
//
// The daemon serves this page and injects the host token into it, so every
// call below is same-origin and carries the same credential the CLI uses.
// That is why there is no CORS story here and no login screen: anything that
// can load this page could already read ~/.aurium/token.

let token = null;

/** readToken finds the credential the daemon injected into the page. */
export function readToken() {
  const meta = document.querySelector('meta[name="aurium-token"]');
  if (meta?.content) {
    token = meta.content;
    return token;
  }
  // A token in the fragment is a fallback for opening the page by hand. It is
  // stripped from the URL immediately: a credential in the address bar is a
  // credential in browser history and in every screenshot.
  const fromHash = new URLSearchParams(location.hash.slice(1)).get("token");
  if (fromHash) {
    history.replaceState(null, "", location.pathname);
    token = fromHash;
  }
  return token;
}

export function currentToken() {
  return token;
}

/** ApiError carries the status so callers can tell 409 from 500. */
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function request(method, path, body) {
  const opts = {
    method,
    headers: { Authorization: `Bearer ${token}` },
  };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }

  const res = await fetch(path, opts);
  if (res.status === 204) return null;

  const text = await res.text();
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = null;
  }
  if (!res.ok) {
    // The daemon answers errors as {"error": "..."}. Preferring that to the
    // raw body is what turns "400 Bad Request" into a sentence the user can
    // act on.
    throw new ApiError(res.status, parsed?.error ?? text ?? res.statusText);
  }
  return parsed;
}

export const api = {
  get: (path) => request("GET", path),
  post: (path, body) => request("POST", path, body ?? {}),
  patch: (path, body) => request("PATCH", path, body ?? {}),
  del: (path) => request("DELETE", path),
};

// --- typed helpers, so paths are spelled once ---

const enc = encodeURIComponent;

export const Aurium = {
  projects: () => api.get("/v1/projects"),
  project: (id) => api.get(`/v1/projects/${enc(id)}`),
  createProject: (body) => api.post("/v1/projects", body),
  addRepo: (id, body) => api.post(`/v1/projects/${enc(id)}/repos`, body),
  projectAgents: (id) => api.get(`/v1/projects/${enc(id)}/agents`),
  spawnAgent: (id, body) => api.post(`/v1/projects/${enc(id)}/agents`, body),
  tree: (id) => api.get(`/v1/projects/${enc(id)}/tree`),
  tasks: (id) => api.get(`/v1/projects/${enc(id)}/tasks`),

  agent: (id) => api.get(`/v1/agents/${enc(id)}`),
  agentMessages: (id, limit = 200) => api.get(`/v1/agents/${enc(id)}/messages?limit=${limit}`),
  sendToAgent: (id, body) => api.post(`/v1/agents/${enc(id)}/message`, body),

  snapshots: (id) => api.get(`/v1/containers/${enc(id)}/snapshots`),
  snapshot: (id, label) => api.post(`/v1/containers/${enc(id)}/snapshot`, { label }),
  sync: (id) => api.post(`/v1/containers/${enc(id)}/sync`),

  approvals: () => api.get("/v1/approvals?status=pending"),
  decide: (id, decision) =>
    api.post(`/v1/approvals/${enc(id)}/decide`, { decision, by: "dashboard" }),

  providers: () => api.get("/v1/providers"),
  detectProviders: () => api.get("/v1/providers/detect"),
  connectProvider: (body) => api.post("/v1/providers", body),
  disconnectProvider: (id) => api.del(`/v1/providers/${enc(id)}`),

  drivers: () => api.get("/v1/drivers"),
  github: () => api.get("/v1/github"),
  githubRepos: (limit = 100) => api.get(`/v1/github/repos?limit=${limit}`),
  githubClone: (project, body) => api.post(`/v1/projects/${enc(project)}/github/clone`, body),
  githubTools: (project) => api.post(`/v1/projects/${enc(project)}/github/tools`, {}),

  usage: (window, project) =>
    api.get(`/v1/usage?window=${enc(window)}${project ? `&project=${enc(project)}` : ""}`),
  usageSeries: (window, project) =>
    api.get(`/v1/usage/series?window=${enc(window)}${project ? `&project=${enc(project)}` : ""}`),
  heartbeat: (project) =>
    api.get(`/v1/heartbeat${project ? `?project=${enc(project)}` : ""}`),
};
