// The launcher: create a workspace or open one. Also surfaces the one piece
// of setup an end user may still owe — a provider key for the gateway route.
import { useCallback, useEffect, useState } from "react";
import { routerAPI, type WorkspaceStatus } from "./lib/router";
import { ThemeToggle } from "./ThemeToggle";

export function Home({ onOpen }: { onOpen: (id: string) => void }) {
  const [workspaces, setWorkspaces] = useState<WorkspaceStatus[]>([]);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [keyRoutes, setKeyRoutes] = useState<Record<string, boolean> | null>(null);

  const refresh = useCallback(async () => {
    try {
      setWorkspaces(await routerAPI.listWorkspaces());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "cannot reach the router");
    }
  }, []);

  useEffect(() => {
    refresh();
    routerAPI.keyStatus().then(setKeyRoutes).catch(() => setKeyRoutes(null));
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, [refresh]);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const id = name.trim();
    if (!id) return;
    setBusy(true);
    setError(null);
    try {
      await routerAPI.createWorkspace(id);
      onOpen(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : "workspace creation failed");
    } finally {
      setBusy(false);
    }
  };

  const destroy = async (id: string) => {
    if (!confirm(`Delete workspace "${id}"? Its files and history are removed.`)) return;
    try {
      await routerAPI.destroyWorkspace(id);
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "delete failed");
    }
  };

  const missingKeyRoutes = Object.entries(keyRoutes ?? {})
    .filter(([, ok]) => !ok)
    .map(([route]) => route);

  return (
    <div className="home">
      <header className="home-head">
        <h1>Workspaces</h1>
        <ThemeToggle />
      </header>
      <p className="sub">Each workspace is an isolated sandbox with its own agent and files.</p>

      {keyRoutes && missingKeyRoutes.length > 0 && (
        <KeyBanner routes={missingKeyRoutes} onSaved={() => routerAPI.keyStatus().then(setKeyRoutes)} />
      )}

      <form className="launcher" onSubmit={create}>
        <input
          className="input"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Name a new workspace, e.g. fix-login-bug"
          pattern="[a-zA-Z0-9][a-zA-Z0-9_\-]{0,63}"
          title="letters, digits, - and _"
          disabled={busy}
        />
        <button className="btn primary" disabled={busy || !name.trim()}>
          {busy ? "Creating…" : "Create"}
        </button>
      </form>
      <div className="hint">Letters, digits, dashes. The workspace starts in a few seconds.</div>

      <div className="ws-list">
        {workspaces.map((w) => (
          <div key={w.ID} className="ws-card" role="button" tabIndex={0}
            onClick={() => onOpen(w.ID)}
            onKeyDown={(e) => e.key === "Enter" && onOpen(w.ID)}>
            <span className="name">{w.ID}</span>
            <span className={`state ${w.State}`}>{w.State}</span>
            <button
              className="btn quiet danger destroy"
              onClick={(e) => {
                e.stopPropagation();
                destroy(w.ID);
              }}
            >
              Delete
            </button>
          </div>
        ))}
      </div>

      {error && <div className="error-note">{error}</div>}
    </div>
  );
}

function KeyBanner({ routes, onSaved }: { routes: string[]; onSaved: () => void }) {
  const [route, setRoute] = useState(routes[0]);
  const [key, setKey] = useState("");
  const [err, setErr] = useState<string | null>(null);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!key) return;
    try {
      await routerAPI.setKey(route, key);
      setKey("");
      setErr(null);
      onSaved();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "could not save the key");
    }
  };

  return (
    <div className="banner">
      <span className="eyebrow">Setup needed</span>
      <p>
        Agents can't reach a model yet. Paste a provider API key — it is stored
        only in the policy gateway, never in workspaces.
      </p>
      <form onSubmit={save}>
        {routes.length > 1 && (
          <select className="input" value={route} onChange={(e) => setRoute(e.target.value)}>
            {routes.map((r) => (
              <option key={r}>{r}</option>
            ))}
          </select>
        )}
        <input
          className="input"
          type="password"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder={`API key for ${route}`}
        />
        <button className="btn primary">Save key</button>
      </form>
      {err && <div className="error-note">{err}</div>}
    </div>
  );
}
