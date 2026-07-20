// File-change review: each changed file arrives as a unified git patch from
// the workspace repo; parse it into hunks and render line by line.
import { parsePatch } from "diff";
import type { VcsChange } from "./lib/useAgent";

export function DiffPanel({ diffs }: { diffs: VcsChange[] }) {
  return (
    <div className="diff-panel">
      {diffs.map((d) => (
        <FileView key={d.file} change={d} />
      ))}
    </div>
  );
}

function FileView({ change }: { change: VcsChange }) {
  let hunks: Array<{
    oldStart: number;
    oldLines: number;
    newStart: number;
    newLines: number;
    lines: string[];
  }> = [];
  try {
    hunks = parsePatch(change.patch)[0]?.hunks ?? [];
  } catch {
    // A patch we can't parse still gets its header row below.
  }
  // Auto-expand small diffs; keep big ones collapsed behind their summary.
  const open = change.additions + change.deletions <= 120;
  return (
    <details className="diff-file" open={open}>
      <summary>
        <span className="fname">
          {change.file}
          {change.status !== "modified" ? ` · ${change.status}` : ""}
        </span>
        <span className="plus">+{change.additions}</span>
        <span className="minus">−{change.deletions}</span>
      </summary>
      <div className="diff-hunks">
        <table>
          <tbody>
            {hunks.flatMap((h, hi) => {
              let oldLine = h.oldStart;
              let newLine = h.newStart;
              const rows = [
                <tr key={`h${hi}`} className="hunk-header">
                  <td className="lineno" />
                  <td className="lineno" />
                  <td>{`@@ -${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@`}</td>
                </tr>,
              ];
              for (const [li, line] of h.lines.entries()) {
                const kind = line[0] === "+" ? "add" : line[0] === "-" ? "del" : "ctx";
                rows.push(
                  <tr key={`h${hi}l${li}`} className={kind}>
                    <td className="lineno">{kind === "add" ? "" : oldLine++}</td>
                    <td className="lineno">{kind === "del" ? "" : newLine++}</td>
                    <td>{line}</td>
                  </tr>,
                );
              }
              return rows;
            })}
          </tbody>
        </table>
      </div>
    </details>
  );
}
