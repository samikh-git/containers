// Monaco wiring: bundle the editor into our own build (never the default CDN
// loader — the router binary must stay self-contained) and define app themes.
// This module is only reachable through the lazy-loaded editor chunk, so the
// agent-only path never downloads Monaco.
import * as monaco from "monaco-editor";
import { loader } from "@monaco-editor/react";
import editorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import tsWorker from "monaco-editor/esm/vs/language/typescript/ts.worker?worker";
import jsonWorker from "monaco-editor/esm/vs/language/json/json.worker?worker";
import cssWorker from "monaco-editor/esm/vs/language/css/css.worker?worker";
import htmlWorker from "monaco-editor/esm/vs/language/html/html.worker?worker";
import type { Theme } from "./theme";

self.MonacoEnvironment = {
  getWorker(_workerId: string, label: string): Worker {
    switch (label) {
      case "typescript":
      case "javascript":
        return new tsWorker();
      case "json":
        return new jsonWorker();
      case "css":
      case "scss":
      case "less":
        return new cssWorker();
      case "html":
      case "handlebars":
      case "razor":
        return new htmlWorker();
      default:
        return new editorWorker();
    }
  },
};

loader.config({ monaco });

// Hexes mirror the CSS vars in index.css (:root / :root[data-theme="dark"]);
// Monaco themes cannot read CSS custom properties. Token colors approximate
// the github-light/github-dark Shiki themes used for chat code blocks.
monaco.editor.defineTheme("app-light", {
  base: "vs",
  inherit: true,
  rules: [
    { token: "comment", foreground: "6e7781" },
    { token: "keyword", foreground: "cf222e" },
    { token: "string", foreground: "0a3069" },
    { token: "number", foreground: "0550ae" },
    { token: "type", foreground: "953800" },
    { token: "function", foreground: "8250df" },
    { token: "variable", foreground: "0d0d0d" },
  ],
  colors: {
    "editor.background": "#ffffff",
    "editor.foreground": "#0d0d0d",
    "editorLineNumber.foreground": "#9b9b9b",
    "editorLineNumber.activeForeground": "#0d0d0d",
    "editor.lineHighlightBackground": "#f7f7f7",
    "editorCursor.foreground": "#0d0d0d",
    "editorWidget.background": "#ffffff",
    "editorWidget.border": "#e6e6e6",
    "editorIndentGuide.background1": "#efefef",
    "focusBorder": "#0969da",
  },
});

monaco.editor.defineTheme("app-dark", {
  base: "vs-dark",
  inherit: true,
  rules: [
    { token: "comment", foreground: "8b949e" },
    { token: "keyword", foreground: "ff7b72" },
    { token: "string", foreground: "a5d6ff" },
    { token: "number", foreground: "79c0ff" },
    { token: "type", foreground: "ffa657" },
    { token: "function", foreground: "d2a8ff" },
    { token: "variable", foreground: "ececec" },
  ],
  colors: {
    "editor.background": "#212121",
    "editor.foreground": "#ececec",
    "editorLineNumber.foreground": "#7a7a7a",
    "editorLineNumber.activeForeground": "#ececec",
    "editor.lineHighlightBackground": "#2a2a2a",
    "editorCursor.foreground": "#ececec",
    "editorWidget.background": "#2a2a2a",
    "editorWidget.border": "#3a3a3a",
    "editorIndentGuide.background1": "#2e2e2e",
    "focusBorder": "#6cb0ff",
  },
});

export function monacoThemeFor(theme: Theme): string {
  return theme === "dark" ? "app-dark" : "app-light";
}

/** Language id for a file path, from Monaco's own extension registry. */
export function languageForPath(path: string): string | undefined {
  const name = path.split("/").pop() ?? path;
  const ext = name.includes(".") ? name.slice(name.lastIndexOf(".")) : "";
  for (const lang of monaco.languages.getLanguages()) {
    if (ext && lang.extensions?.includes(ext)) return lang.id;
    if (lang.filenames?.includes(name)) return lang.id;
  }
  return undefined;
}

/** True when the file is Markdown (drives word wrap and the preview toggle). */
export function isMarkdownPath(path: string): boolean {
  return languageForPath(path) === "markdown";
}

/**
 * Editor word-wrap setting for a file type. Prose (Markdown, plain text, and
 * unrecognized files that render as plaintext) reflows so a long paragraph
 * spreads across the viewport instead of scrolling off to the right; code keeps
 * hard lines so structure stays intact.
 */
export function wordWrapForPath(path: string): "on" | "off" {
  const lang = languageForPath(path);
  return lang === undefined || lang === "markdown" || lang === "plaintext" ? "on" : "off";
}

/**
 * Pretty-print obviously-minified structured content for display. Only touches
 * files whose type has a safe formatter (currently JSON) and that actually look
 * minified — a very long line — so already-formatted files aren't reflowed into
 * a different indentation style. Returns the content unchanged otherwise.
 */
export function smartFormat(path: string, content: string): string {
  if (languageForPath(path) !== "json") return content;
  const longestLine = content.split("\n").reduce((m, l) => Math.max(m, l.length), 0);
  if (longestLine <= 200) return content;
  try {
    return JSON.stringify(JSON.parse(content), null, 2);
  } catch {
    return content; // not strict JSON (comments, trailing commas, …); leave as-is
  }
}

export { monaco };
