// Monaco side-by-side review of the agent's changes. The original content is
// reconstructed by reverse-applying the git patch onto the current file (read
// via the router files API, so this works with the sandbox asleep). Files
// whose patches can't be applied fall back to the hunk-table renderer.
import { useEffect, useState } from "react";
import { applyPatch, parsePatch, reversePatch } from "diff";
import { DiffEditor } from "@monaco-editor/react";
import { languageForPath, monacoThemeFor } from "../lib/monaco";
import { useThemeAttr } from "../lib/theme";
import { filesAPI } from "../lib/files";
import { DiffPanel } from "../DiffPanel";
import type { VcsChange } from "../lib/useAgent";

interface Sides {
  original: string;
  modified: string;
}

export default function DiffView({
  workspaceID,
  diffs,
}: {
  workspaceID: string;
  diffs: VcsChange[];
}) {
  const theme = useThemeAttr();
  const [selected, setSelected] = useState(diffs[0]?.file ?? null);
  const [sides, setSides] = useState<Sides | null>(null);
  const [fallback, setFallback] = useState(false);

  const change = diffs.find((d) => d.file === selected) ?? null;

  useEffect(() => {
    if (!change) return;
    let cancelled = false;
    setSides(null);
    setFallback(false);
    (async () => {
      const current =
        change.status === "deleted"
          ? ""
          : await filesAPI
              .readFile(workspaceID, change.file)
              .then((r) => (r.type === "file" ? (r.content ?? "") : null))
              .catch(() => null);
      if (cancelled) return;
      if (current === null) {
        setFallback(true); // binary or unreadable
        return;
      }
      if (change.status === "added") {
        setSides({ original: "", modified: current });
        return;
      }
      try {
        const parsed = parsePatch(change.patch)[0];
        const original = applyPatch(current, reversePatch(parsed));
        if (original === false) throw new Error("patch does not apply");
        setSides({ original, modified: current });
      } catch {
        setFallback(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [change, workspaceID]);

  if (diffs.length === 0) return null;

  return (
    <div className="diff-view">
      <aside className="diff-files">
        {diffs.map((d) => (
          <button
            key={d.file}
            className={`diff-file-item ${d.file === selected ? "active" : ""}`}
            onClick={() => setSelected(d.file)}
            title={d.file}
          >
            <span className="fname">
              {d.file}
              {d.status !== "modified" ? ` · ${d.status}` : ""}
            </span>
            <span className="plus">+{d.additions}</span>
            <span className="minus">−{d.deletions}</span>
          </button>
        ))}
      </aside>
      <div className="diff-editor-host">
        {change && fallback && <DiffPanel diffs={[change]} />}
        {change && !fallback && sides && (
          <DiffEditor
            original={sides.original}
            modified={sides.modified}
            language={languageForPath(change.file)}
            theme={monacoThemeFor(theme)}
            options={{
              readOnly: true,
              renderSideBySide: true,
              minimap: { enabled: false },
              scrollBeyondLastLine: false,
              automaticLayout: true,
              fontSize: 12.5,
            }}
          />
        )}
        {change && !fallback && !sides && <div className="none">Loading diff…</div>}
      </div>
    </div>
  );
}
