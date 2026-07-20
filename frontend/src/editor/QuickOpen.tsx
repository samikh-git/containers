// ⌘P fuzzy file finder, backed by OpenCode's /find/file (needs the sandbox
// awake — search is an interactive-session feature; file IO is not).
import { useEffect, useRef, useState } from "react";
import type { OpencodeClient } from "@opencode-ai/sdk/client";

export function QuickOpen({
  client,
  agentOnline,
  onOpen,
  onClose,
}: {
  client: OpencodeClient;
  agentOnline: boolean;
  onOpen: (path: string) => void;
  onClose: () => void;
}) {
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<string[]>([]);
  const [selected, setSelected] = useState(0);
  const seq = useRef(0);

  // Escape must work even when the input can't take focus (agent offline →
  // input disabled).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  useEffect(() => {
    if (!agentOnline || !query.trim()) {
      setResults([]);
      return;
    }
    const mine = ++seq.current;
    const t = setTimeout(async () => {
      try {
        const res = await client.find.files({ query: { query } });
        if (seq.current !== mine) return;
        setResults((res.data ?? []).slice(0, 30));
        setSelected(0);
      } catch {
        if (seq.current === mine) setResults([]);
      }
    }, 150);
    return () => clearTimeout(t);
  }, [query, client, agentOnline]);

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") onClose();
    else if (e.key === "ArrowDown") {
      e.preventDefault();
      setSelected((s) => Math.min(s + 1, results.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSelected((s) => Math.max(s - 1, 0));
    } else if (e.key === "Enter" && results[selected]) {
      onOpen(results[selected]);
    }
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="quick-open" onClick={(e) => e.stopPropagation()}>
        <input
          className="quick-open-input"
          autoFocus
          placeholder={agentOnline ? "Find a file…" : "Agent offline — file search unavailable"}
          disabled={!agentOnline}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKey}
        />
        <div className="quick-open-results">
          {results.map((p, i) => (
            <button
              key={p}
              className={`quick-open-item ${i === selected ? "selected" : ""}`}
              onMouseEnter={() => setSelected(i)}
              onClick={() => onOpen(p)}
            >
              <span className="qo-name">{p.split("/").pop()}</span>
              <span className="qo-path">{p}</span>
            </button>
          ))}
          {agentOnline && query.trim() && results.length === 0 && (
            <div className="quick-open-empty">No matches</div>
          )}
        </div>
      </div>
    </div>
  );
}
