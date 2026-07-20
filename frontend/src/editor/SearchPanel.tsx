// Project-wide text search (⌘⇧F), backed by OpenCode's ripgrep /find route.
import { useEffect, useRef, useState } from "react";
import type { OpencodeClient } from "@opencode-ai/sdk/client";

interface Match {
  path: { text: string };
  lines: { text: string };
  line_number: number;
  submatches: Array<{ start: number; end: number }>;
}

export function SearchPanel({
  client,
  agentOnline,
  onOpen,
  onClose,
}: {
  client: OpencodeClient;
  agentOnline: boolean;
  onOpen: (path: string, line: number) => void;
  onClose: () => void;
}) {
  const [pattern, setPattern] = useState("");
  const [matches, setMatches] = useState<Match[]>([]);
  const [searched, setSearched] = useState(false);
  const seq = useRef(0);

  useEffect(() => {
    if (!agentOnline || !pattern.trim()) {
      setMatches([]);
      setSearched(false);
      return;
    }
    const mine = ++seq.current;
    const t = setTimeout(async () => {
      try {
        const res = await client.find.text({ query: { pattern } });
        if (seq.current !== mine) return;
        setMatches(((res.data ?? []) as Match[]).slice(0, 200));
        setSearched(true);
      } catch {
        if (seq.current === mine) {
          setMatches([]);
          setSearched(true);
        }
      }
    }, 250);
    return () => clearTimeout(t);
  }, [pattern, client, agentOnline]);

  // Group by file, preserving result order.
  const groups: Array<{ path: string; items: Match[] }> = [];
  for (const m of matches) {
    const last = groups[groups.length - 1];
    if (last && last.path === m.path.text) last.items.push(m);
    else groups.push({ path: m.path.text, items: [m] });
  }

  return (
    <div className="search-panel">
      <div className="search-head">
        <span className="eyebrow">Search</span>
        <span className="spacer" />
        <button className="icon-btn" title="Close" onClick={onClose}>×</button>
      </div>
      <input
        className="search-input"
        autoFocus
        placeholder={agentOnline ? "Search in files…" : "Agent offline — search unavailable"}
        disabled={!agentOnline}
        value={pattern}
        onChange={(e) => setPattern(e.target.value)}
        onKeyDown={(e) => e.key === "Escape" && onClose()}
      />
      <div className="search-results">
        {groups.map((g) => (
          <div key={g.path} className="search-group">
            <div className="search-file">{g.path}</div>
            {g.items.map((m, i) => (
              <button
                key={`${m.line_number}-${i}`}
                className="search-hit"
                onClick={() => onOpen(g.path, m.line_number)}
              >
                <span className="search-line-no">{m.line_number}</span>
                <span className="search-line">{highlight(m)}</span>
              </button>
            ))}
          </div>
        ))}
        {searched && matches.length === 0 && <div className="quick-open-empty">No matches</div>}
      </div>
    </div>
  );
}

function highlight(m: Match) {
  const text = m.lines.text.replace(/\n$/, "");
  const sub = m.submatches[0];
  if (!sub) return text.trim();
  // Trim long lines around the first match.
  let start = 0;
  if (sub.start > 40) start = sub.start - 30;
  const slice = (s: number, e?: number) => text.slice(s, e);
  return (
    <>
      {start > 0 && "…"}
      {slice(start, sub.start)}
      <mark>{slice(sub.start, sub.end)}</mark>
      {slice(sub.end)}
    </>
  );
}
