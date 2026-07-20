// Two screens: the workspace launcher and the agent view. A hash route keeps
// refreshes and links working without a router dependency.
import { useEffect, useState } from "react";
import { Home } from "./Home";
import { Workspace } from "./Workspace";
import { getToken, setToken } from "./lib/router";
import { ThemeToggle } from "./ThemeToggle";

export type WorkspaceView = "agent" | "editor";

function currentWorkspace(): { id: string; view: WorkspaceView } | null {
  const m = location.hash.match(/^#\/ws\/([a-zA-Z0-9][a-zA-Z0-9_-]{0,63})(\/editor)?$/);
  return m ? { id: m[1], view: m[2] ? "editor" : "agent" } : null;
}

export default function App() {
  const [ws, setWs] = useState(currentWorkspace());
  const [needsToken, setNeedsToken] = useState(false);

  useEffect(() => {
    const onHash = () => setWs(currentWorkspace());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // One auth probe: if the router requires a token and we don't have a valid
  // one, ask for it before showing anything else.
  useEffect(() => {
    fetch("/api/workspaces", {
      headers: getToken() ? { Authorization: `Bearer ${getToken()}` } : {},
    }).then((r) => setNeedsToken(r.status === 401));
  }, []);

  if (needsToken) {
    return <TokenGate onDone={() => setNeedsToken(false)} />;
  }

  if (ws) {
    return (
      <Workspace
        id={ws.id}
        view={ws.view}
        onSetView={(v) => (location.hash = v === "editor" ? `#/ws/${ws.id}/editor` : `#/ws/${ws.id}`)}
        onBack={() => (location.hash = "")}
      />
    );
  }
  return <Home onOpen={(id) => (location.hash = `#/ws/${id}`)} />;
}

function TokenGate({ onDone }: { onDone: () => void }) {
  const [value, setValue] = useState("");
  const [err, setErr] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const resp = await fetch("/api/workspaces", {
      headers: { Authorization: `Bearer ${value}` },
    });
    if (resp.ok) {
      setToken(value);
      onDone();
    } else {
      setErr("That token was not accepted.");
    }
  };

  return (
    <div className="gate">
      <div className="gate-corner">
        <ThemeToggle />
      </div>
      <h1>Access token</h1>
      <p>This router requires an access token. Paste the ROUTER_API_TOKEN it was started with.</p>
      <form onSubmit={submit}>
        <input
          className="input"
          type="password"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="token"
          autoFocus
        />
        <button className="btn primary">Continue</button>
      </form>
      {err && <div className="error-note">{err}</div>}
    </div>
  );
}
