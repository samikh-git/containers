// Client for the router's own REST API (workspace lifecycle, provider keys).
// The OpenCode SDK covers everything past /api/workspaces/{id}/opencode.

export interface WorkspaceStatus {
  ID: string;
  Generation: number;
  State: string;
  Container: string;
  CPUPercent?: number;
  MemoryUsedMB?: number;
  MemoryLimitMB?: number;
  DiskUsedMB?: number;
  DiskQuotaGB?: number;
  LastActivityAt?: string;
  AgentBusy?: boolean;
}

export interface RouteInfo {
  kind?: string;
  base_url?: string;
  allowed_models?: string[];
  key_set?: boolean;
  key_source?: string;
}

export interface ServeDefaults {
  image?: string;
  cpus?: number;
  memory_mb?: number;
  quota_gb?: number;
  profile_dir?: string;
  gateway_url?: string;
  route?: string;
  network?: string;
}

export interface UsageBucket {
  key: string;
  requests: number;
  forwarded: number;
  blocked: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
  req_bytes: number;
  resp_bytes: number;
  cost_usd: number;
  priced_requests: number;
}

export interface UsageSummary {
  window_start?: string;
  window_end?: string;
  ledger_path?: string;
  available: boolean;
  note?: string;
  total: UsageBucket;
  by_workspace: UsageBucket[];
  by_model: UsageBucket[];
  by_route: UsageBucket[];
}

export interface ProvisionSpec {
  id: string;
  image?: string;
  cpus?: number;
  memory_mb?: number;
  quota_gb?: number;
  profile_dir?: string;
  gateway_url?: string;
  route?: string;
  network?: string;
}

const TOKEN_KEY = "router-token";

// A token handed over in the URL fragment (…/#token=abc) is stored and then
// wiped from the address bar. The fragment — not a query parameter — because
// fragments are never sent to the server and so never reach a log or a
// Referer header. This is how the installer hands the operator their token.
function adoptTokenFromFragment() {
  const hash = location.hash.startsWith("#") ? location.hash.slice(1) : location.hash;
  if (!hash) return;
  const params = new URLSearchParams(hash);
  const token = params.get("token");
  if (!token) return;
  localStorage.setItem(TOKEN_KEY, token);
  params.delete("token");
  const rest = params.toString();
  history.replaceState(null, "", location.pathname + location.search + (rest ? `#${rest}` : ""));
}
adoptTokenFromFragment();

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token);
}

export function authHeaders(): Record<string, string> {
  const token = getToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

export class ApiError extends Error {
  status: number;
  data: Record<string, unknown>;
  constructor(message: string, status: number, data?: Record<string, unknown>) {
    super(message);
    this.status = status;
    this.data = data ?? {};
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 204) return undefined as T;
  const data = (await resp.json().catch(() => ({}))) as Record<string, unknown>;
  if (!resp.ok) {
    throw new ApiError(
      (data.error as string) ?? `${method} ${path} failed (${resp.status})`,
      resp.status,
      data,
    );
  }
  return data as T;
}

export const routerAPI = {
  listWorkspaces: (metrics = false) =>
    call<WorkspaceStatus[]>(
      "GET",
      metrics ? "/api/workspaces?metrics=1" : "/api/workspaces",
    ),
  createWorkspace: (spec: ProvisionSpec | string) => {
    const body = typeof spec === "string" ? { id: spec } : spec;
    return call<{ id: string; mount: string }>("POST", "/api/workspaces", body);
  },
  destroyWorkspace: (id: string) => call<void>("DELETE", `/api/workspaces/${id}`),
  stopWorkspace: (id: string) =>
    call<{ id: string }>("POST", `/api/workspaces/${id}/down`, {}),
  hibernateWorkspace: (id: string) =>
    call<{ id: string }>("POST", `/api/workspaces/${id}/hibernate`, {}),
  fanoutWorkspace: (id: string, n: number) =>
    call<{ branches: string[] }>("POST", `/api/workspaces/${id}/fanout`, { n }),
  keyStatus: () => call<Record<string, boolean>>("GET", "/api/keys"),
  setKey: (route: string, key: string) =>
    call<void>("PUT", `/api/keys/${encodeURIComponent(route)}`, { key }),
  routes: () => call<Record<string, RouteInfo>>("GET", "/api/routes"),
  defaults: () => call<ServeDefaults>("GET", "/api/defaults"),
  usage: (window = "24h") =>
    call<UsageSummary>("GET", `/api/usage?window=${encodeURIComponent(window)}`),
};
