// Lazy-loading directory tree with inline create/rename rows, VS Code style.
import { useState } from "react";
import type { FileEntry } from "../lib/files";
import type { useEditor } from "./useEditor";

type Ed = ReturnType<typeof useEditor>;

// An in-progress inline input: creating under a dir, or renaming a path.
interface Pending {
  kind: "new-file" | "new-dir" | "rename";
  dir: string; // parent dir ("" = root)
  path?: string; // rename target
  initial: string;
}

export function FileTree({ ed, onSearch }: { ed: Ed; onSearch: () => void }) {
  const [pending, setPending] = useState<Pending | null>(null);

  const submit = (name: string) => {
    const p = pending;
    setPending(null);
    if (!p || !name.trim()) return;
    const full = p.dir ? `${p.dir}/${name}` : name;
    if (p.kind === "new-file") ed.createFile(full);
    else if (p.kind === "new-dir") ed.createDir(full);
    else if (p.kind === "rename" && p.path) {
      const parent = p.path.includes("/") ? p.path.slice(0, p.path.lastIndexOf("/")) : "";
      ed.renamePath(p.path, parent ? `${parent}/${name}` : name);
    }
  };

  return (
    <div className="file-tree">
      <div className="file-tree-head">
        <span className="eyebrow">Files</span>
        <span className="spacer" />
        <button className="icon-btn" title="New file" onClick={() => setPending({ kind: "new-file", dir: "", initial: "" })}>＋</button>
        <button className="icon-btn" title="New folder" onClick={() => setPending({ kind: "new-dir", dir: "", initial: "" })}>⊞</button>
        <button className="icon-btn" title="Search in files (⌘⇧F)" onClick={onSearch}>⌕</button>
        <button className="icon-btn" title="Refresh" onClick={() => ed.refresh()}>↻</button>
      </div>
      <div className="file-tree-body">
        <DirChildren ed={ed} dir="" depth={0} pending={pending} setPending={setPending} submit={submit} />
      </div>
    </div>
  );
}

function DirChildren({
  ed,
  dir,
  depth,
  pending,
  setPending,
  submit,
}: {
  ed: Ed;
  dir: string;
  depth: number;
  pending: Pending | null;
  setPending: (p: Pending | null) => void;
  submit: (name: string) => void;
}) {
  const entries = ed.tree.get(dir);
  return (
    <>
      {pending && pending.kind !== "rename" && pending.dir === dir && (
        <InlineInput depth={depth} initial={pending.initial} onDone={submit} onCancel={() => setPending(null)} />
      )}
      {entries?.map((e) => (
        <TreeRow
          key={e.name}
          ed={ed}
          entry={e}
          dir={dir}
          depth={depth}
          pending={pending}
          setPending={setPending}
          submit={submit}
        />
      ))}
      {entries && entries.length === 0 && depth === 0 && !pending && (
        <div className="tree-empty">Empty workspace</div>
      )}
    </>
  );
}

function TreeRow({
  ed,
  entry,
  dir,
  depth,
  pending,
  setPending,
  submit,
}: {
  ed: Ed;
  entry: FileEntry;
  dir: string;
  depth: number;
  pending: Pending | null;
  setPending: (p: Pending | null) => void;
  submit: (name: string) => void;
}) {
  const path = dir ? `${dir}/${entry.name}` : entry.name;
  const isDir = entry.type === "dir";
  const open = ed.expanded.has(path);
  const active = ed.activePath === path;

  if (pending?.kind === "rename" && pending.path === path) {
    return <InlineInput depth={depth} initial={entry.name} onDone={submit} onCancel={() => setPending(null)} />;
  }

  return (
    <>
      <div
        className={`tree-row ${active ? "active" : ""}`}
        style={{ paddingLeft: 8 + depth * 14 }}
        onClick={() => (isDir ? ed.toggleDir(path) : ed.openFile(path))}
      >
        <span className="tree-caret">{isDir ? (open ? "▾" : "▸") : ""}</span>
        <span className={`tree-name ${entry.type}`}>{entry.name}</span>
        <span className="tree-actions" onClick={(e) => e.stopPropagation()}>
          {isDir && (
            <>
              <button className="icon-btn" title="New file here"
                onClick={() => { if (!open) ed.toggleDir(path); setPending({ kind: "new-file", dir: path, initial: "" }); }}>＋</button>
            </>
          )}
          <button className="icon-btn" title="Rename"
            onClick={() => setPending({ kind: "rename", dir, path, initial: entry.name })}>✎</button>
          <button className="icon-btn" title="Delete" onClick={() => ed.deletePath(path, isDir)}>🗑</button>
        </span>
      </div>
      {isDir && open && (
        <DirChildren ed={ed} dir={path} depth={depth + 1} pending={pending} setPending={setPending} submit={submit} />
      )}
    </>
  );
}

function InlineInput({
  depth,
  initial,
  onDone,
  onCancel,
}: {
  depth: number;
  initial: string;
  onDone: (name: string) => void;
  onCancel: () => void;
}) {
  const [value, setValue] = useState(initial);
  return (
    <div className="tree-row" style={{ paddingLeft: 8 + depth * 14 }}>
      <input
        className="tree-input"
        autoFocus
        value={value}
        onChange={(e) => setValue(e.target.value)}
        onBlur={onCancel}
        onKeyDown={(e) => {
          if (e.key === "Enter") onDone(value);
          else if (e.key === "Escape") onCancel();
        }}
      />
    </div>
  );
}
