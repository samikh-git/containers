// Tab strip: dirty dot, stale marker, close with unsaved-changes confirm.
import type { ReactNode } from "react";
import type { useEditor } from "./useEditor";

export function EditorTabs({
  ed,
  right,
}: {
  ed: ReturnType<typeof useEditor>;
  right?: ReactNode;
}) {
  if (ed.tabs.length === 0) {
    return (
      <div className="tab-strip">
        <span className="spacer" />
        {right}
      </div>
    );
  }
  return (
    <div className="tab-strip" role="tablist">
      {ed.tabs.map((t) => {
        const name = t.path.split("/").pop();
        return (
          <div
            key={t.path}
            role="tab"
            aria-selected={t.path === ed.activePath}
            className={`edit-tab ${t.path === ed.activePath ? "active" : ""}`}
            title={t.path + (t.stale ? " — changed on disk since your edits" : "")}
            onClick={() => ed.setActivePath(t.path)}
            onAuxClick={(e) => {
              if (e.button === 1) ed.closeTab(t.path);
            }}
          >
            <span className="edit-tab-name">
              {name}
              {t.stale && <span className="stale-mark" aria-label="changed on disk">⚠</span>}
            </span>
            <button
              className={`edit-tab-close ${t.dirty ? "dirty" : ""}`}
              aria-label={t.dirty ? `Close ${name} (unsaved changes)` : `Close ${name}`}
              onClick={(e) => {
                e.stopPropagation();
                ed.closeTab(t.path);
              }}
            >
              <span className="x">×</span>
              <span className="dot">●</span>
            </button>
          </div>
        );
      })}
      <span className="spacer" />
      {right}
    </div>
  );
}
