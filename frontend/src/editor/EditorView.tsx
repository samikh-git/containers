// The editor view: file tree, tab strip, and a single Monaco instance that
// swaps models on tab switch (view state saved/restored per tab). Reads and
// writes go through the router files API; search goes through OpenCode.
import { useCallback, useEffect, useRef, useState } from "react";
import type { OpencodeClient } from "@opencode-ai/sdk/client";
import { monaco, monacoThemeFor, isMarkdownPath, wordWrapForPath } from "../lib/monaco";
import { useThemeAttr } from "../lib/theme";
import { useEditor } from "./useEditor";
import { FileTree } from "./FileTree";
import { EditorTabs } from "./EditorTabs";
import { QuickOpen } from "./QuickOpen";
import { SearchPanel } from "./SearchPanel";
import { MarkdownPreview } from "./MarkdownPreview";
import { TerminalPanel } from "./TerminalPanel";

export default function EditorView({
  workspaceID,
  client,
  agentOnline,
  subscribeIdle,
}: {
  workspaceID: string;
  client: OpencodeClient;
  agentOnline: boolean;
  subscribeIdle: (fn: () => void) => () => void;
}) {
  const ed = useEditor(workspaceID, subscribeIdle);
  const theme = useThemeAttr();
  const containerRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);
  const [quickOpen, setQuickOpen] = useState(false);
  const [searchOpen, setSearchOpen] = useState(false);
  const [previewOpen, setPreviewOpen] = useState(false);
  // The terminal stays mounted once opened — closing just hides it, so the
  // shell (and its scrollback) survives toggling. It dies with the view.
  const [termOpen, setTermOpen] = useState(false);
  const [termMounted, setTermMounted] = useState(false);
  const [termHeight, setTermHeight] = useState(240);

  const toggleTerminal = useCallback(() => {
    setTermOpen((v) => !v);
    setTermMounted(true);
  }, []);

  // Drag the divider above the panel to resize it.
  const dragTerminal = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      const startY = e.clientY;
      const startHeight = termHeight;
      const onMove = (ev: MouseEvent) => {
        const h = startHeight + (startY - ev.clientY);
        setTermHeight(Math.min(600, Math.max(120, h)));
      };
      const onUp = () => {
        window.removeEventListener("mousemove", onMove);
        window.removeEventListener("mouseup", onUp);
      };
      window.addEventListener("mousemove", onMove);
      window.addEventListener("mouseup", onUp);
    },
    [termHeight],
  );

  const activeIsMarkdown = ed.activePath ? isMarkdownPath(ed.activePath) : false;
  const showPreview = previewOpen && activeIsMarkdown;
  const activeEntry = ed.activePath ? ed.getModel(ed.activePath) : null;

  // Refs so editor commands and window listeners see current state without
  // re-registering.
  const edRef = useRef(ed);
  edRef.current = ed;

  useEffect(() => {
    if (!containerRef.current) return;
    const editor = monaco.editor.create(containerRef.current, {
      model: null,
      theme: monacoThemeFor(theme),
      automaticLayout: true,
      fontSize: 13,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      padding: { top: 8 },
    });
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => {
      edRef.current.save();
    });
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyP, () => setQuickOpen(true));
    editor.addCommand(monaco.KeyMod.WinCtrl | monaco.KeyCode.Backquote, () => {
      setTermOpen((v) => !v);
      setTermMounted(true);
    });
    editorRef.current = editor;
    return () => {
      editorRef.current = null;
      editor.dispose(); // models are cached in useEditor, not owned by the editor
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    monaco.editor.setTheme(monacoThemeFor(theme));
  }, [theme]);

  // Swap the visible model when the active tab changes.
  const prevPath = useRef<string | null>(null);
  useEffect(() => {
    const editor = editorRef.current;
    if (!editor) return;
    if (prevPath.current && prevPath.current !== ed.activePath) {
      const prev = ed.getModel(prevPath.current);
      if (prev) prev.viewState = editor.saveViewState();
    }
    const entry = ed.activePath ? ed.getModel(ed.activePath) : null;
    editor.setModel(entry?.model ?? null);
    editor.updateOptions({ wordWrap: ed.activePath ? wordWrapForPath(ed.activePath) : "off" });
    if (entry?.viewState) editor.restoreViewState(entry.viewState);
    if (entry) editor.focus();
    prevPath.current = ed.activePath;
  }, [ed.activePath, ed]);

  // Global shortcuts (work when focus is outside Monaco too).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (!mod) return;
      if (e.key === "s" && !e.shiftKey) {
        e.preventDefault();
        edRef.current.save();
      } else if (e.key === "p" && !e.shiftKey) {
        e.preventDefault();
        setQuickOpen(true);
      } else if ((e.key === "f" || e.key === "F") && e.shiftKey) {
        e.preventDefault();
        setSearchOpen((v) => !v);
      } else if ((e.key === "v" || e.key === "V") && e.shiftKey) {
        e.preventDefault();
        setPreviewOpen((v) => !v);
      } else if (e.key === "`" && e.ctrlKey) {
        e.preventDefault();
        setTermOpen((v) => !v);
        setTermMounted(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const openAtLine = useCallback(
    async (path: string, line?: number) => {
      await edRef.current.openFile(path);
      if (line !== undefined) {
        // The model swap effect runs after state settles; queue the reveal.
        requestAnimationFrame(() => {
          const editor = editorRef.current;
          if (!editor) return;
          editor.revealLineInCenter(line);
          editor.setSelection(new monaco.Selection(line, 1, line, 1));
          editor.focus();
        });
      }
    },
    [],
  );

  return (
    <div className="editor-shell">
      <aside className="file-pane">
        <FileTree ed={ed} onSearch={() => setSearchOpen(true)} />
      </aside>
      <div className="editor-main">
        <EditorTabs
          ed={ed}
          right={
            <button
              className="icon-btn term-toggle"
              aria-pressed={termOpen}
              onClick={toggleTerminal}
              title="Toggle terminal (⌃`)"
            >
              {">_"}
            </button>
          }
        />
        <div className="editor-surface">
          <div className="editor-pane">
            <div ref={containerRef} className="monaco-host" />
            {ed.activePath === null && (
              <div className="editor-empty">
                <p>Open a file from the tree, or</p>
                <p>
                  <kbd>⌘P</kbd> to find a file · <kbd>⌘⇧F</kbd> to search
                </p>
              </div>
            )}
            {activeIsMarkdown && (
              <button
                className="preview-toggle"
                aria-pressed={showPreview}
                onClick={() => setPreviewOpen((v) => !v)}
                title="Toggle Markdown preview (⌘⇧V)"
              >
                {showPreview ? "Edit" : "Preview"}
              </button>
            )}
          </div>
          {showPreview && activeEntry && <MarkdownPreview model={activeEntry.model} />}
        </div>
        {termMounted && (
          <>
            {termOpen && (
              <div
                className="term-resize"
                role="separator"
                aria-orientation="horizontal"
                aria-label="Resize terminal"
                onMouseDown={dragTerminal}
              />
            )}
            <TerminalPanel
              workspaceID={workspaceID}
              visible={termOpen}
              height={termHeight}
              onClose={() => setTermOpen(false)}
            />
          </>
        )}
        {ed.error && (
          <div className="editor-error" role="alert">
            {ed.error}
            <button className="btn small" onClick={ed.clearError}>Dismiss</button>
          </div>
        )}
      </div>
      {searchOpen && (
        <aside className="search-pane">
          <SearchPanel
            client={client}
            agentOnline={agentOnline}
            onOpen={openAtLine}
            onClose={() => setSearchOpen(false)}
          />
        </aside>
      )}
      {quickOpen && (
        <QuickOpen
          client={client}
          agentOnline={agentOnline}
          onOpen={(p) => {
            setQuickOpen(false);
            openAtLine(p);
          }}
          onClose={() => setQuickOpen(false)}
        />
      )}
      {ed.conflict && (
        <div className="modal-backdrop">
          <div className="modal" role="dialog" aria-label="Save conflict">
            <h3>File changed on disk</h3>
            <p>
              <code>{ed.conflict.path}</code> was modified (likely by the agent) since you opened
              it. {ed.conflict.message === "file deleted on disk" && "It has been deleted on disk."}
            </p>
            <div className="modal-actions">
              <button className="btn" onClick={() => ed.resolveConflict("cancel")}>Cancel</button>
              <button className="btn" onClick={() => ed.resolveConflict("reload")}>
                Reload from disk
              </button>
              <button className="btn danger" onClick={() => ed.resolveConflict("overwrite")}>
                Overwrite
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
