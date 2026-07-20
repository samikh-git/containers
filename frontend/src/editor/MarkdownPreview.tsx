// Live rendered preview for the active Markdown buffer. Reuses the same
// Streamdown renderer and `.md` styles as the chat transcript, and tracks the
// model's content so edits show up as you type (no save required).
import { useEffect, useState } from "react";
import { Streamdown } from "streamdown";
import { code } from "@streamdown/code";
import type { monaco } from "../lib/monaco";

export function MarkdownPreview({ model }: { model: monaco.editor.ITextModel }) {
  const [text, setText] = useState(() => model.getValue());
  useEffect(() => {
    setText(model.getValue());
    const sub = model.onDidChangeContent(() => setText(model.getValue()));
    return () => sub.dispose();
  }, [model]);
  return (
    <div className="editor-preview">
      <Streamdown className="md" plugins={{ code }} shikiTheme={["github-light", "github-dark"]}>
        {text}
      </Streamdown>
    </div>
  );
}
