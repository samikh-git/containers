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

const TOKEN_KEY = "router-token";

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

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 204) return undefined as T;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    throw new Error(
      (data as { error?: string }).error ?? `${method} ${path} failed (${resp.status})`,
    );
  }
  return data as T;
}

export const routerAPI = {
  listWorkspaces: () => call<WorkspaceStatus[]>("GET", "/api/workspaces"),
  createWorkspace: (id: string) =>
    call<{ id: string; mount: string }>("POST", "/api/workspaces", { id }),
  destroyWorkspace: (id: string) => call<void>("DELETE", `/api/workspaces/${id}`),
  stopWorkspace: (id: string) => call<{ id: string }>("POST", `/api/workspaces/${id}/down`, {}),
  keyStatus: () => call<Record<string, boolean>>("GET", "/api/keys"),
  setKey: (route: string, key: string) =>
    call<void>("PUT", `/api/keys/${encodeURIComponent(route)}`, { key }),
};
