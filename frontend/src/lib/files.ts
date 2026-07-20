// Client for the router's workspace file API (/api/workspaces/{id}/files/…) —
// the editor's read/write surface. Works with the sandbox asleep; only
// search (OpenCode /find) needs the agent awake.
import { authHeaders } from "./router";

export interface FileEntry {
  name: string;
  type: "file" | "dir" | "symlink" | "other";
  size?: number;
}

export interface ReadFileResult {
  type: "file" | "dir" | "binary";
  content?: string;
  hash?: string;
  entries?: FileEntry[];
  size: number;
  modified: string;
}

/** Save was rejected because the file changed (or vanished) underneath us. */
export class ConflictError extends Error {
  serverHash: string | undefined;
  serverContent: string | undefined;

  constructor(message: string, serverHash?: string, serverContent?: string) {
    super(message);
    this.name = "ConflictError";
    this.serverHash = serverHash;
    this.serverContent = serverContent;
  }
}

function fileURL(ws: string, path: string): string {
  const encoded = path.split("/").map(encodeURIComponent).join("/");
  return `/api/workspaces/${ws}/files/${encoded}`;
}

async function call<T>(method: string, url: string, body?: unknown): Promise<T> {
  const resp = await fetch(url, {
    method,
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 204) return undefined as T;
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 409) {
    const d = data as { error?: string; hash?: string; content?: string };
    throw new ConflictError(d.error ?? "conflict", d.hash, d.content);
  }
  if (!resp.ok) {
    throw new Error(
      (data as { error?: string }).error ?? `${method} ${url} failed (${resp.status})`,
    );
  }
  return data as T;
}

export const filesAPI = {
  listDir: (ws: string, path: string) =>
    call<ReadFileResult>("GET", fileURL(ws, path)).then((r) => r.entries ?? []),
  readFile: (ws: string, path: string) => call<ReadFileResult>("GET", fileURL(ws, path)),
  /** baseHash "" creates a new file (409 if it exists); force skips the check. */
  writeFile: (ws: string, path: string, content: string, baseHash: string, force = false) =>
    call<{ hash: string }>("PUT", fileURL(ws, path), { content, baseHash, force }),
  mkdir: (ws: string, path: string) => call<void>("POST", fileURL(ws, path), { op: "mkdir" }),
  rename: (ws: string, from: string, to: string) =>
    call<void>("POST", fileURL(ws, from), { op: "rename", to }),
  remove: (ws: string, path: string, recursive = false) =>
    call<void>("DELETE", fileURL(ws, path) + (recursive ? "?recursive=1" : "")),
};
