// Editor state for one workspace: open files as Monaco models, the lazy
// directory tree, saves with conflict detection, and refresh-on-idle.
//
// Models live in a module-level cache keyed by workspace so unsaved buffers
// survive Agent↔Editor view switches (EditorView unmounts entirely); they are
// disposed on tab close, not on unmount.
import { useCallback, useEffect, useMemo, useState } from "react";
import { monaco, languageForPath, smartFormat } from "../lib/monaco";
import { filesAPI, ConflictError, type FileEntry } from "../lib/files";

interface CachedFile {
  model: monaco.editor.ITextModel;
  baseHash: string;
  savedVersionId: number;
  viewState: monaco.editor.ICodeEditorViewState | null;
}

interface WorkspaceCache {
  files: Map<string, CachedFile>;
  openPaths: string[];
  activePath: string | null;
}

const caches = new Map<string, WorkspaceCache>();

function cacheFor(ws: string): WorkspaceCache {
  let c = caches.get(ws);
  if (!c) {
    c = { files: new Map(), openPaths: [], activePath: null };
    caches.set(ws, c);
  }
  return c;
}

export interface TabInfo {
  path: string;
  dirty: boolean;
  stale: boolean; // changed on disk while the buffer was dirty
}

export interface SaveConflict {
  path: string;
  message: string;
  serverHash?: string;
  serverContent?: string;
}

export function useEditor(ws: string, subscribeIdle: (fn: () => void) => () => void) {
  const cache = useMemo(() => cacheFor(ws), [ws]);
  const [openPaths, setOpenPaths] = useState<string[]>(cache.openPaths);
  const [activePath, setActivePathState] = useState<string | null>(cache.activePath);
  const [dirty, setDirty] = useState<Set<string>>(new Set());
  const [stale, setStale] = useState<Set<string>>(new Set());
  const [tree, setTree] = useState<Map<string, FileEntry[]>>(new Map());
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [conflict, setConflict] = useState<SaveConflict | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Recompute a file's dirty bit from its model version.
  const syncDirty = useCallback(
    (path: string) => {
      const f = cache.files.get(path);
      if (!f) return;
      const isDirty = f.model.getAlternativeVersionId() !== f.savedVersionId;
      setDirty((d) => {
        if (d.has(path) === isDirty) return d;
        const next = new Set(d);
        if (isDirty) next.add(path);
        else next.delete(path);
        return next;
      });
    },
    [cache],
  );

  // Rebuild dirty state after a remount (cache may hold dirty buffers).
  useEffect(() => {
    const d = new Set<string>();
    for (const [path, f] of cache.files) {
      if (f.model.getAlternativeVersionId() !== f.savedVersionId) d.add(path);
    }
    setDirty(d);
  }, [cache]);

  const setActivePath = useCallback(
    (path: string | null) => {
      cache.activePath = path;
      setActivePathState(path);
    },
    [cache],
  );

  const loadDir = useCallback(
    async (dir: string) => {
      try {
        const entries = await filesAPI.listDir(ws, dir);
        setTree((t) => new Map(t).set(dir, entries));
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : "could not list directory");
      }
    },
    [ws],
  );

  const toggleDir = useCallback(
    (dir: string) => {
      setExpanded((ex) => {
        const next = new Set(ex);
        if (next.has(dir)) next.delete(dir);
        else {
          next.add(dir);
          loadDir(dir);
        }
        return next;
      });
    },
    [loadDir],
  );

  useEffect(() => {
    loadDir("");
  }, [loadDir]);

  const openFile = useCallback(
    async (path: string) => {
      if (cache.files.has(path)) {
        setOpenPaths((p) => {
          const next = p.includes(path) ? p : [...p, path];
          cache.openPaths = next;
          return next;
        });
        setActivePath(path);
        return;
      }
      try {
        const res = await filesAPI.readFile(ws, path);
        if (res.type !== "file") {
          setError(res.type === "binary" ? `${path} is a binary file` : `${path} is not a file`);
          return;
        }
        const uri = monaco.Uri.file("/" + path);
        // Recognize the file type so it gets the right highlighting/folding, and
        // expand obviously-minified JSON so it doesn't render as one long line.
        const content = smartFormat(path, res.content ?? "");
        const model =
          monaco.editor.getModel(uri) ??
          monaco.editor.createModel(content, languageForPath(path), uri);
        const entry: CachedFile = {
          model,
          baseHash: res.hash ?? "",
          savedVersionId: model.getAlternativeVersionId(),
          viewState: null,
        };
        model.onDidChangeContent(() => syncDirty(path));
        cache.files.set(path, entry);
        setOpenPaths((p) => {
          const next = [...p, path];
          cache.openPaths = next;
          return next;
        });
        setActivePath(path);
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : `could not open ${path}`);
      }
    },
    [ws, cache, setActivePath, syncDirty],
  );

  const closeTab = useCallback(
    (path: string, opts?: { discard?: boolean }) => {
      const f = cache.files.get(path);
      if (f && !opts?.discard && f.model.getAlternativeVersionId() !== f.savedVersionId) {
        if (!window.confirm(`${path} has unsaved changes. Close and discard them?`)) return;
      }
      f?.model.dispose();
      cache.files.delete(path);
      setOpenPaths((p) => {
        const next = p.filter((x) => x !== path);
        cache.openPaths = next;
        if (cache.activePath === path) {
          const i = p.indexOf(path);
          const fallback = next[Math.min(i, next.length - 1)] ?? null;
          cache.activePath = fallback;
          setActivePathState(fallback);
        }
        return next;
      });
      setDirty((d) => {
        const next = new Set(d);
        next.delete(path);
        return next;
      });
      setStale((s) => {
        const next = new Set(s);
        next.delete(path);
        return next;
      });
    },
    [cache],
  );

  const save = useCallback(
    async (path?: string, force = false) => {
      const target = path ?? cache.activePath;
      if (!target) return;
      const f = cache.files.get(target);
      if (!f) return;
      const content = f.model.getValue();
      const versionAtSave = f.model.getAlternativeVersionId();
      try {
        const res = await filesAPI.writeFile(ws, target, content, f.baseHash, force);
        f.baseHash = res.hash;
        f.savedVersionId = versionAtSave;
        syncDirty(target);
        setStale((s) => {
          const next = new Set(s);
          next.delete(target);
          return next;
        });
        setConflict(null);
        setError(null);
      } catch (e) {
        if (e instanceof ConflictError) {
          setConflict({
            path: target,
            message: e.message,
            serverHash: e.serverHash,
            serverContent: e.serverContent,
          });
        } else {
          setError(e instanceof Error ? e.message : `could not save ${target}`);
        }
      }
    },
    [ws, cache, syncDirty],
  );

  const resolveConflict = useCallback(
    async (action: "overwrite" | "reload" | "cancel") => {
      if (!conflict) return;
      const f = cache.files.get(conflict.path);
      if (action === "overwrite") {
        await save(conflict.path, true);
        return;
      }
      if (action === "reload" && f) {
        // Take the server's version, dropping local edits.
        const res = await filesAPI.readFile(ws, conflict.path).catch(() => null);
        const content = res?.content ?? conflict.serverContent ?? "";
        f.model.setValue(content);
        f.baseHash = res?.hash ?? conflict.serverHash ?? "";
        f.savedVersionId = f.model.getAlternativeVersionId();
        syncDirty(conflict.path);
      }
      setConflict(null);
    },
    [conflict, cache, save, ws, syncDirty],
  );

  // Refresh: re-list every loaded directory; reload clean open buffers whose
  // content changed on disk; leave dirty buffers alone but flag them stale
  // (the save-time hash check still protects them).
  const refresh = useCallback(async () => {
    setExpanded((ex) => {
      loadDir("");
      ex.forEach((dir) => loadDir(dir));
      return ex;
    });
    for (const [path, f] of cache.files) {
      try {
        const res = await filesAPI.readFile(ws, path);
        if (res.type !== "file" || res.hash === f.baseHash) continue;
        if (f.model.getAlternativeVersionId() !== f.savedVersionId) {
          setStale((s) => new Set(s).add(path));
          continue;
        }
        f.model.setValue(smartFormat(path, res.content ?? ""));
        f.baseHash = res.hash ?? "";
        f.savedVersionId = f.model.getAlternativeVersionId();
        syncDirty(path);
      } catch {
        // File may have been deleted by the agent; save will surface it.
      }
    }
  }, [ws, cache, loadDir, syncDirty]);

  useEffect(() => subscribeIdle(refresh), [subscribeIdle, refresh]);

  // ---- file ops ----

  const parentDir = (path: string) => (path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "");

  const createFile = useCallback(
    async (path: string) => {
      try {
        await filesAPI.writeFile(ws, path, "", "");
        await loadDir(parentDir(path));
        await openFile(path);
      } catch (e) {
        setError(e instanceof Error ? e.message : `could not create ${path}`);
      }
    },
    [ws, loadDir, openFile],
  );

  const createDir = useCallback(
    async (path: string) => {
      try {
        await filesAPI.mkdir(ws, path);
        await loadDir(parentDir(path));
        setExpanded((ex) => new Set(ex).add(path));
        loadDir(path);
      } catch (e) {
        setError(e instanceof Error ? e.message : `could not create ${path}`);
      }
    },
    [ws, loadDir],
  );

  const renamePath = useCallback(
    async (from: string, to: string) => {
      try {
        await filesAPI.rename(ws, from, to);
      } catch (e) {
        setError(e instanceof Error ? e.message : `could not rename ${from}`);
        return;
      }
      // Re-key any open buffer (and buffers under a renamed directory),
      // preserving unsaved content by recreating the model at the new URI.
      for (const [path, f] of [...cache.files]) {
        const suffix = path === from ? "" : path.startsWith(from + "/") ? path.slice(from.length) : null;
        if (suffix === null) continue;
        const newPath = to + suffix;
        const content = f.model.getValue();
        const wasDirty = f.model.getAlternativeVersionId() !== f.savedVersionId;
        f.model.dispose();
        const model = monaco.editor.createModel(
          content,
          languageForPath(newPath),
          monaco.Uri.file("/" + newPath),
        );
        const entry: CachedFile = {
          model,
          baseHash: f.baseHash,
          savedVersionId: wasDirty ? -1 : model.getAlternativeVersionId(),
          viewState: f.viewState,
        };
        model.onDidChangeContent(() => syncDirty(newPath));
        cache.files.delete(path);
        cache.files.set(newPath, entry);
        setOpenPaths((p) => {
          const next = p.map((x) => (x === path ? newPath : x));
          cache.openPaths = next;
          return next;
        });
        if (cache.activePath === path) setActivePath(newPath);
        syncDirty(newPath);
      }
      await Promise.all([loadDir(parentDir(from)), loadDir(parentDir(to))]);
    },
    [ws, cache, loadDir, setActivePath, syncDirty],
  );

  const deletePath = useCallback(
    async (path: string, isDir: boolean) => {
      if (!window.confirm(`Delete ${path}${isDir ? " and everything in it" : ""}?`)) return;
      try {
        await filesAPI.remove(ws, path, isDir);
      } catch (e) {
        setError(e instanceof Error ? e.message : `could not delete ${path}`);
        return;
      }
      for (const open of [...cache.files.keys()]) {
        if (open === path || open.startsWith(path + "/")) closeTab(open, { discard: true });
      }
      await loadDir(parentDir(path));
    },
    [ws, cache, closeTab, loadDir],
  );

  const tabs: TabInfo[] = openPaths.map((path) => ({
    path,
    dirty: dirty.has(path),
    stale: stale.has(path),
  }));

  const anyDirty = dirty.size > 0;

  // Unsaved buffers shouldn't vanish on an accidental tab close.
  useEffect(() => {
    if (!anyDirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [anyDirty]);

  const getModel = useCallback((path: string) => cache.files.get(path) ?? null, [cache]);

  return {
    tabs,
    activePath,
    setActivePath,
    tree,
    expanded,
    toggleDir,
    loadDir,
    openFile,
    closeTab,
    save,
    conflict,
    resolveConflict,
    refresh,
    createFile,
    createDir,
    renamePath,
    deletePath,
    error,
    clearError: () => setError(null),
    getModel,
  };
}
