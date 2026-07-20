// The integrated terminal: xterm.js over the router's terminal WebSocket
// (/api/workspaces/{id}/terminal → sandbox termbridge). Input rides binary
// frames; resize is a JSON text frame. The router counts client→server bytes
// as idle-check activity, so typing keeps the workspace awake — walking away
// lets it hibernate, which shows up here as a disconnect overlay whose
// reconnect path also wakes a sleeping workspace (shells don't survive
// scale-to-zero; /workspace does).
import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { getToken, routerAPI } from "../lib/router";
import { useThemeAttr, type Theme } from "../lib/theme";

// termbridge close codes (sandbox-image/termbridge/main.go).
const CLOSE_SHELL_EXITED = 4000;
const CLOSE_STOPPING = 4002;

// Matches the app tokens in index.css (:root / :root[data-theme="dark"]).
const xtermThemes: Record<Theme, object> = {
  light: {
    background: "#ffffff",
    foreground: "#0d0d0d",
    cursor: "#0d0d0d",
    selectionBackground: "rgba(13, 13, 13, 0.18)",
  },
  dark: {
    background: "#212121",
    foreground: "#ececec",
    cursor: "#ececec",
    selectionBackground: "rgba(236, 236, 236, 0.25)",
  },
};

type ConnState =
  | { kind: "connecting" }
  | { kind: "open" }
  | { kind: "closed"; reason: string; canWake: boolean };

function socketURL(workspaceID: string): string {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const token = getToken();
  return (
    `${proto}://${location.host}/api/workspaces/${workspaceID}/terminal` +
    (token ? `?token=${encodeURIComponent(token)}` : "")
  );
}

export function TerminalPanel({
  workspaceID,
  visible,
  height,
  onClose,
}: {
  workspaceID: string;
  visible: boolean;
  height: number;
  onClose: () => void;
}) {
  const theme = useThemeAttr();
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const [conn, setConn] = useState<ConnState>({ kind: "connecting" });

  const connect = useCallback(() => {
    wsRef.current?.close();
    setConn({ kind: "connecting" });
    const ws = new WebSocket(socketURL(workspaceID));
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;

    const sendSize = () => {
      const term = termRef.current;
      if (term && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      }
    };
    ws.onopen = () => {
      setConn({ kind: "open" });
      fitRef.current?.fit();
      sendSize();
      termRef.current?.focus();
    };
    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        termRef.current?.write(new Uint8Array(ev.data));
      }
    };
    ws.onclose = async (ev) => {
      if (wsRef.current !== ws) return; // superseded by a reconnect
      if (ev.code === CLOSE_SHELL_EXITED) {
        setConn({ kind: "closed", reason: "The shell exited.", canWake: false });
        return;
      }
      // Anything else — hibernation close, proxy 502 during handshake, a
      // dropped socket — may mean the workspace went to sleep. Ask.
      let asleep = false;
      try {
        const all = await routerAPI.listWorkspaces();
        asleep = all.find((w) => w.ID === workspaceID)?.State !== "running";
      } catch {
        // Router unreachable: report the disconnect as-is.
      }
      setConn({
        kind: "closed",
        reason:
          ev.code === CLOSE_STOPPING || asleep
            ? "The workspace is asleep — everything in /workspace is safe, but shells don't survive hibernation."
            : "The connection dropped.",
        canWake: asleep,
      });
    };
  }, [workspaceID]);

  // Reconnect, waking the workspace first when it isn't running. Up is
  // idempotent — on a hibernated workspace it re-provisions onto the
  // preserved volume.
  const reconnect = useCallback(async () => {
    setConn({ kind: "connecting" });
    try {
      const all = await routerAPI.listWorkspaces();
      if (all.find((w) => w.ID === workspaceID)?.State !== "running") {
        await routerAPI.createWorkspace(workspaceID);
      }
      connect();
    } catch (e) {
      setConn({
        kind: "closed",
        reason: e instanceof Error ? e.message : "could not wake the workspace",
        canWake: true,
      });
    }
  }, [workspaceID, connect]);

  // One Terminal per mounted panel; the shell's lifetime is the socket's.
  useEffect(() => {
    if (!hostRef.current) return;
    const term = new Terminal({
      fontSize: 13,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      cursorBlink: true,
      theme: xtermThemes[theme],
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(hostRef.current);
    termRef.current = term;
    fitRef.current = fit;

    const encoder = new TextEncoder();
    const data = term.onData((s) => {
      const ws = wsRef.current;
      if (ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(s));
    });
    connect();
    return () => {
      data.dispose();
      wsRef.current?.close();
      wsRef.current = null;
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (termRef.current) termRef.current.options.theme = xtermThemes[theme];
  }, [theme]);

  // Refit whenever our box changes size (drag, window resize) or we become
  // visible again (fit inside display:none yields nonsense dimensions).
  useEffect(() => {
    if (!visible) return;
    const refit = () => {
      fitRef.current?.fit();
      const term = termRef.current;
      const ws = wsRef.current;
      if (term && ws?.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      }
    };
    refit();
    const observer = new ResizeObserver(refit);
    if (hostRef.current) observer.observe(hostRef.current);
    return () => observer.disconnect();
  }, [visible, height]);

  return (
    <div className="term-pane" style={visible ? { height } : { display: "none" }}>
      <div className="term-head">
        <span className="term-title">Terminal</span>
        <span className="spacer" />
        <button className="icon-btn" onClick={onClose} title="Hide terminal (⌃`)" aria-label="Hide terminal">
          ×
        </button>
      </div>
      <div className="term-body">
        <div ref={hostRef} className="term-host" />
        {conn.kind !== "open" && (
          <div className="term-overlay">
            {conn.kind === "connecting" ? (
              <span>Connecting…</span>
            ) : (
              <>
                <span>{conn.reason}</span>
                <button className="btn small" onClick={reconnect}>
                  {conn.canWake ? "Wake & reconnect" : "New shell"}
                </button>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
