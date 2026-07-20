import { useCallback, useState } from "react";
import { LayerCard } from "@cloudflare/kumo";
import { SectionTitle } from "./SectionTitle";

export type LogEntry = { t: string; msg: string; kind: "info" | "ok" | "err" };

const STORAGE_KEY = "admin-log";

function load(): LogEntry[] {
  try {
    return JSON.parse(sessionStorage.getItem(STORAGE_KEY) || "[]") as LogEntry[];
  } catch {
    return [];
  }
}

function save(entries: LogEntry[]) {
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(entries.slice(0, 40)));
  } catch {
    /* ignore */
  }
}

export function useActivityLog() {
  const [entries, setEntries] = useState<LogEntry[]>(load);

  const push = useCallback((msg: string, kind: LogEntry["kind"] = "info") => {
    const entry: LogEntry = {
      t: new Date().toLocaleTimeString(),
      msg,
      kind,
    };
    setEntries((prev) => {
      const next = [entry, ...prev].slice(0, 80);
      save(next);
      return next;
    });
  }, []);

  return { entries, push };
}

export function ActivityLog({ entries }: { entries: LogEntry[] }) {
  return (
    <LayerCard className="p-4">
      <SectionTitle>Activity</SectionTitle>
      <div className="max-h-40 overflow-y-auto font-mono text-xs text-kumo-subtle">
        {entries.length === 0 ? (
          <div>No activity yet.</div>
        ) : (
          entries.map((e, i) => (
            <div
              key={`${e.t}-${i}`}
              className={
                e.kind === "err"
                  ? "text-kumo-danger"
                  : e.kind === "ok"
                    ? "text-kumo-success"
                    : undefined
              }
            >
              {e.t}{"  "}{e.msg}
            </div>
          ))
        )}
      </div>
    </LayerCard>
  );
}
