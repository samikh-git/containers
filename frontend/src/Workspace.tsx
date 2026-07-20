// The workspace screen: an Agent view (session sidebar, chat thread with
// live streaming and permission fences, model picker, Changes tab) and an
// Editor view (Monaco, lazy-loaded so the agent path never downloads it).
// useAgent stays mounted here so the SSE stream survives view switches.
import { Suspense, lazy, useEffect, useRef, useState } from "react";
import type { Part } from "@opencode-ai/sdk/client";
import { Streamdown } from "streamdown";
import { code } from "@streamdown/code";
import {
  useAgent,
  type ChatMessage,
  type ModelChoice,
  type PermissionAsk,
  type VcsChange,
} from "./lib/useAgent";
import { ThemeToggle } from "./ThemeToggle";
import type { WorkspaceView } from "./App";

const EditorView = lazy(() => import("./editor/EditorView"));
// Shares the Monaco chunk with EditorView; the chat-only path loads neither.
const DiffView = lazy(() => import("./editor/DiffView"));

export function Workspace({
  id,
  view,
  onSetView,
  onBack,
}: {
  id: string;
  view: WorkspaceView;
  onSetView: (v: WorkspaceView) => void;
  onBack: () => void;
}) {
  const agent = useAgent(id);
  const [tab, setTab] = useState<"chat" | "changes">("chat");
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem("sidebar-collapsed") === "1",
  );

  const toggleSidebar = () =>
    setCollapsed((c) => {
      localStorage.setItem("sidebar-collapsed", c ? "0" : "1");
      return !c;
    });

  // ⌘B / Ctrl+B toggles the sessions sidebar, like most editors.
  useEffect(() => {
    if (view !== "agent") return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.shiftKey && e.key === "b") {
        e.preventDefault();
        toggleSidebar();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [view]);

  const switcher = (
    <div className="view-switch" role="tablist" aria-label="Workspace view">
      <button role="tab" aria-selected={view === "agent"}
        className={`view-switch-btn ${view === "agent" ? "active" : ""}`}
        onClick={() => onSetView("agent")}>
        Agent
      </button>
      <button role="tab" aria-selected={view === "editor"}
        className={`view-switch-btn ${view === "editor" ? "active" : ""}`}
        onClick={() => onSetView("editor")}>
        Editor
      </button>
    </div>
  );

  if (view === "editor") {
    return (
      <div className="editor-screen">
        <div className="topbar">
          <button className="back" onClick={onBack}>← All workspaces</button>
          <div className="ws-name">{id}</div>
          <span className="spacer" />
          {switcher}
          <ThemeToggle />
        </div>
        <Suspense fallback={<div className="editor-loading">Loading editor…</div>}>
          <EditorView
            workspaceID={id}
            client={agent.client}
            agentOnline={agent.ready && !agent.offline}
            subscribeIdle={agent.subscribeIdle}
          />
        </Suspense>
      </div>
    );
  }

  return (
    <div className={`agent-shell ${collapsed ? "collapsed" : ""}`}>
      <aside className="sidebar" aria-hidden={collapsed} inert={collapsed}>
        <div className="sidebar-inner">
          <div className="top">
            <button className="back" onClick={onBack}>← All workspaces</button>
            <div className="ws-name">{id}</div>
          </div>
          <div className="sessions">
            <span className="eyebrow">Sessions</span>
            {agent.sessions.map((s) => (
              <button
                key={s.id}
                className={`session-item ${s.id === agent.sessionID ? "active" : ""}`}
                onClick={() => agent.openSession(s.id)}
                title={s.title}
              >
                {s.title || s.id}
              </button>
            ))}
          </div>
          <div className="foot">
            <button className="btn" onClick={() => agent.newSession()}>New session</button>
          </div>
        </div>
      </aside>

      <div className="main">
        <div className="topbar">
          <button
            className="icon-btn"
            onClick={toggleSidebar}
            title={`${collapsed ? "Show" : "Hide"} sidebar (⌘B)`}
            aria-label={`${collapsed ? "Show" : "Hide"} sidebar`}
            aria-expanded={!collapsed}
          >
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
              strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <rect x="3" y="4" width="18" height="16" rx="3" />
              <path d="M9.5 4v16" />
            </svg>
          </button>
          {collapsed && (
            <button className="back topbar-back" onClick={onBack} title="All workspaces">
              ← {id}
            </button>
          )}
          <div className="tabs" role="tablist">
            <button role="tab" aria-selected={tab === "chat"}
              className={`tab ${tab === "chat" ? "active" : ""}`}
              onClick={() => setTab("chat")}>
              Chat
            </button>
            <button role="tab" aria-selected={tab === "changes"}
              className={`tab ${tab === "changes" ? "active" : ""}`}
              onClick={() => setTab("changes")}>
              Changes
            </button>
          </div>
          <span className="spacer" />
          {switcher}
          {agent.models.length > 0 && (
            <ModelPicker models={agent.models} model={agent.model} onPick={agent.setModel} />
          )}
          <ThemeToggle />
        </div>

        {tab === "chat" ? (
          <ChatTab agent={agent} />
        ) : (
          <ChangesTab load={agent.diff} sessionID={agent.sessionID} workspaceID={id} />
        )}
      </div>
    </div>
  );
}

const modelKey = (m: ModelChoice) => `${m.providerID}/${m.modelID}`;

// A searchable model picker: opens a popover with a filter box and models
// grouped under their source (Anthropic, OpenRouter, …).
function ModelPicker({
  models,
  model,
  onPick,
}: {
  models: ModelChoice[];
  model: ModelChoice | null;
  onPick: (m: ModelChoice) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  // Close on outside click or Escape.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  // Focus the search box and reset the filter each time the popover opens.
  useEffect(() => {
    if (open) {
      setQuery("");
      inputRef.current?.focus();
    }
  }, [open]);

  const q = query.trim().toLowerCase();
  const filtered = q
    ? models.filter(
        (m) =>
          m.label.toLowerCase().includes(q) ||
          m.modelID.toLowerCase().includes(q) ||
          m.providerName.toLowerCase().includes(q),
      )
    : models;

  // Group the (filtered) models by source, preserving first-seen order.
  const groups: { name: string; models: ModelChoice[] }[] = [];
  const byName = new Map<string, ModelChoice[]>();
  for (const m of filtered) {
    let bucket = byName.get(m.providerName);
    if (!bucket) {
      bucket = [];
      byName.set(m.providerName, bucket);
      groups.push({ name: m.providerName, models: bucket });
    }
    bucket.push(m);
  }

  const selectedKey = model ? modelKey(model) : "";

  return (
    <div className="model-picker" ref={rootRef}>
      <button
        type="button"
        className="model-select"
        aria-label="Model"
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="model-select-label">{model ? model.label : "Select model"}</span>
        <span className="model-select-caret" aria-hidden="true">▾</span>
      </button>
      {open && (
        <div className="model-pop" role="dialog">
          <input
            ref={inputRef}
            className="model-search"
            type="text"
            placeholder="Search models…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Search models"
          />
          <div className="model-list" role="listbox">
            {groups.length === 0 && <div className="model-empty">No models match.</div>}
            {groups.map((g) => (
              <div className="model-group" key={g.name}>
                <div className="model-group-label">{g.name}</div>
                {g.models.map((m) => {
                  const key = modelKey(m);
                  const active = key === selectedKey;
                  return (
                    <button
                      type="button"
                      key={key}
                      role="option"
                      aria-selected={active}
                      className={`model-option ${active ? "active" : ""}`}
                      onClick={() => {
                        onPick(m);
                        setOpen(false);
                      }}
                    >
                      <span className="model-option-name">{m.label}</span>
                      {active && <span className="model-option-check" aria-hidden="true">✓</span>}
                    </button>
                  );
                })}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function ChatTab({ agent }: { agent: ReturnType<typeof useAgent> }) {
  const [draft, setDraft] = useState("");
  const threadRef = useRef<HTMLDivElement>(null);
  const stickToBottom = useRef(true);

  // Follow the stream unless the reader scrolled up to review something.
  useEffect(() => {
    const el = threadRef.current;
    if (el && stickToBottom.current) el.scrollTop = el.scrollHeight;
  });

  const onScroll = () => {
    const el = threadRef.current;
    if (!el) return;
    stickToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
  };

  const submit = async () => {
    const text = draft.trim();
    if (!text || agent.working) return;
    setDraft("");
    stickToBottom.current = true;
    try {
      await agent.send(text);
    } catch (e) {
      // Surface a send failure where the reply would have appeared.
      console.error(e);
    }
  };

  if (agent.offline) {
    return (
      <div className="empty-thread">
        <div className="big">The agent isn't reachable yet</div>
        <div>{agent.offline}</div>
        <div style={{ marginTop: 8 }}>
          A new workspace takes a few seconds to boot — this view retries automatically.
        </div>
      </div>
    );
  }

  return (
    <>
      <div className="thread" ref={threadRef} onScroll={onScroll}>
        {agent.messages.length === 0 && !agent.working ? (
          <div className="empty-thread">
            <div className="big">Ask the agent to build, fix, or explain something</div>
            <div>It works inside this workspace's files only.</div>
          </div>
        ) : (
          <div className="thread-inner">
            {agent.messages.map((m) => (
              <MessageView key={m.info.id} message={m} />
            ))}
            {agent.permissions.map((p) => (
              <PermissionFence key={p.id} permission={p} respond={agent.respondPermission} />
            ))}
            {agent.working && agent.permissions.length === 0 && (
              <div className="working">
                <span className="dot" /> working — you can keep typing
                <button className="btn quiet" onClick={agent.abort}>Stop</button>
              </div>
            )}
            {agent.error && <div className="thread-error">{agent.error}</div>}
          </div>
        )}
      </div>

      <div className="composer">
        <div className="composer-box">
          <textarea
            rows={1}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                submit();
              }
            }}
            placeholder="Describe what you want done… (Enter to send, Shift+Enter for a new line)"
            disabled={!agent.ready}
          />
          <button
            className="send"
            aria-label="Send message"
            title="Send message"
            onClick={submit}
            disabled={!draft.trim() || agent.working}
          >
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
              strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M12 19V5M5 12l7-7 7 7" />
            </svg>
          </button>
        </div>
      </div>
    </>
  );
}

function MessageView({ message }: { message: ChatMessage }) {
  const role = message.info.role;
  const rendered = message.parts.map((p) => renderPart(p, role)).filter(Boolean);
  if (rendered.length === 0) return null;
  return (
    <div className={`msg ${role}`} aria-label={role === "user" ? "You" : "Agent"}>
      {rendered}
    </div>
  );
}

function renderPart(part: Part, role: string): React.ReactNode {
  switch (part.type) {
    case "text":
      if (part.synthetic || part.ignored || !part.text.trim()) return null;
      if (role === "user") {
        return (
          <div key={part.id} className="body">
            {part.text}
          </div>
        );
      }
      return (
        <Streamdown
          key={part.id}
          className="md"
          plugins={{ code }}
          shikiTheme={["github-light", "github-dark"]}
        >
          {part.text}
        </Streamdown>
      );
    case "reasoning":
      return part.text.trim() ? <ThinkingView key={part.id} part={part} /> : null;
    case "tool": {
      if (part.tool === "bash") return <BashToolView key={part.id} part={part} />;
      const title =
        (part.state && "title" in part.state && part.state.title) || part.tool;
      const status = part.state?.status;
      return (
        <div key={part.id} className="tool-line" title={String(title)}>
          <span className="tool-name">{part.tool}</span>
          {" · "}
          {status === "running" || status === "pending" ? "running…" : String(title)}
        </div>
      );
    }
    default:
      return null;
  }
}

// A model reasoning block: pulses while the model is still thinking, and the
// streamed reasoning text is one click away.
function ThinkingView({ part }: { part: Part & { type: "reasoning" } }) {
  const active = !part.time?.end;
  return (
    <details className="thinking">
      <summary className={active ? "active" : ""}>
        {active ? "Thinking…" : "Thought process"}
      </summary>
      <div className="thinking-text">{part.text}</div>
    </details>
  );
}

// A bash run: the command the agent typed and what came back.
function BashToolView({ part }: { part: Part & { type: "tool" } }) {
  const state = part.state;
  const command =
    state && "input" in state && state.input
      ? String((state.input as { command?: unknown }).command ?? "")
      : "";
  const output =
    state?.status === "completed"
      ? state.output
      : state?.status === "error"
        ? state.error
        : null;
  const running = state?.status === "running" || state?.status === "pending";
  return (
    <div className={`bash-block ${state?.status === "error" ? "failed" : ""}`}>
      <div className="bash-cmd">
        <span className="prompt">$</span> {command || part.tool}
      </div>
      {running && <div className="bash-status">running…</div>}
      {output != null && output.trim() !== "" && (
        <pre className="bash-out">{output.trim()}</pre>
      )}
    </div>
  );
}

// The permission fence: the agent asked to do something the profile gates.
function PermissionFence({
  permission,
  respond,
}: {
  permission: PermissionAsk;
  respond: (p: PermissionAsk, r: "once" | "always" | "reject") => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const verbs: Record<string, string> = {
    edit: "edit these files",
    bash: "run this command",
    webfetch: "fetch this URL",
  };
  const answer = async (r: "once" | "always" | "reject") => {
    setBusy(true);
    try {
      await respond(permission, r);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="fence" role="alertdialog" aria-label="Permission request">
      <span className="eyebrow">Permission — {permission.permission}</span>
      <div className="title">
        The agent wants to {verbs[permission.permission] ?? `use ${permission.permission}`}:
      </div>
      {permission.patterns.length > 0 && (
        <div className="pattern">{permission.patterns.join("\n")}</div>
      )}
      <div className="actions">
        <button className="btn allow" disabled={busy} onClick={() => answer("once")}>
          Allow once
        </button>
        <button className="btn allow" disabled={busy} onClick={() => answer("always")}>
          Always allow
        </button>
        <button className="btn danger" disabled={busy} onClick={() => answer("reject")}>
          Deny
        </button>
      </div>
    </div>
  );
}

function ChangesTab({
  load,
  sessionID,
  workspaceID,
}: {
  load: () => Promise<VcsChange[]>;
  sessionID: string | null;
  workspaceID: string;
}) {
  const [diffs, setDiffs] = useState<VcsChange[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    setDiffs(null);
    load()
      .then(setDiffs)
      .catch((e) => setErr(e instanceof Error ? e.message : "could not load changes"));
  }, [load, sessionID]);

  if (err) return <div className="diff-panel"><div className="none">{err}</div></div>;
  if (diffs === null) return <div className="diff-panel"><div className="none">Loading changes…</div></div>;
  if (diffs.length === 0) {
    return (
      <div className="diff-panel">
        <div className="none">No file changes in this session yet.</div>
      </div>
    );
  }
  return (
    <Suspense fallback={<div className="diff-panel"><div className="none">Loading diff viewer…</div></div>}>
      <DiffView workspaceID={workspaceID} diffs={diffs} />
    </Suspense>
  );
}
