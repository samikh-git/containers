// Agent session state for one workspace: message history, live streaming via
// the OpenCode event bus (SSE through the router proxy), permission requests,
// and the model list the profile allows.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Message, Part, Provider, Session } from "@opencode-ai/sdk/client";
import { opencodeClient } from "./opencode";
import { authHeaders } from "./router";

export interface ChatMessage {
  info: Message;
  parts: Part[];
}

// A pending approval, as the live server reports it (GET /permission and the
// permission.asked event). The SDK's static types lag these routes, so the
// shape is pinned here against the server's own OpenAPI spec.
export interface PermissionAsk {
  id: string;
  sessionID: string;
  permission: string;
  patterns: string[];
  always: string[];
  metadata?: Record<string, unknown>;
}

// One changed file from GET /vcs/diff?mode=git: a unified git patch plus
// counters. Also outside the SDK's typed surface.
export interface VcsChange {
  file: string;
  patch: string;
  additions: number;
  deletions: number;
  status: string;
}

export interface ModelChoice {
  providerID: string;
  modelID: string;
  label: string;
  // Human-readable source/provider name used to group models in the picker
  // (e.g. "Anthropic", "OpenRouter"). Falls back to the provider id.
  providerName: string;
}

interface AgentState {
  ready: boolean;
  offline: string | null;
  sessions: Session[];
  sessionID: string | null;
  messages: ChatMessage[];
  permissions: PermissionAsk[];
  working: boolean;
  error: string | null;
  models: ModelChoice[];
  model: ModelChoice | null;
}

// A session is working while its newest assistant message hasn't completed.
// Derived from the messages themselves so a late message.updated event can't
// resurrect the working state after session.idle already cleared it.
function lastAssistantIncomplete(messages: ChatMessage[]): boolean {
  for (let i = messages.length - 1; i >= 0; i--) {
    const info = messages[i].info;
    if (info.role === "assistant") return !info.time?.completed;
  }
  return false;
}

function upsertPart(parts: Part[], part: Part): Part[] {
  const i = parts.findIndex((p) => p.id === part.id);
  if (i === -1) return [...parts, part];
  const next = parts.slice();
  next[i] = part;
  return next;
}

export function useAgent(workspaceID: string) {
  const client = useMemo(() => opencodeClient(workspaceID), [workspaceID]);
  // Raw proxy calls for routes the SDK doesn't wrap yet (pending
  // permissions and their replies).
  const ocFetch = useCallback(
    (path: string, init?: RequestInit) =>
      fetch(`/api/workspaces/${workspaceID}/opencode${path}`, {
        ...init,
        headers: { "Content-Type": "application/json", ...authHeaders() },
      }),
    [workspaceID],
  );
  const [state, setState] = useState<AgentState>({
    ready: false,
    offline: null,
    sessions: [],
    sessionID: null,
    messages: [],
    permissions: [],
    working: false,
    error: null,
    models: [],
    model: null,
  });
  // The session the event loop should apply events to; a ref so the SSE
  // effect doesn't restart on every selection change.
  const sessionRef = useRef<string | null>(null);
  sessionRef.current = state.sessionID;
  // Fired whenever ANY session in this workspace goes idle — the editor uses
  // it to refresh files the agent may have touched, so it must not be
  // filtered to the open session.
  const idleListeners = useRef(new Set<() => void>());
  const subscribeIdle = useCallback((fn: () => void) => {
    idleListeners.current.add(fn);
    return () => {
      idleListeners.current.delete(fn);
    };
  }, []);

  const loadSessions = useCallback(async () => {
    const res = await client.session.list();
    const sessions = (res.data ?? []).sort(
      (a, b) => (b.time?.updated ?? 0) - (a.time?.updated ?? 0),
    );
    return sessions;
  }, [client]);

  const loadPermissions = useCallback(
    async (sessionID: string): Promise<PermissionAsk[]> => {
      try {
        const resp = await ocFetch("/permission");
        if (!resp.ok) return [];
        const all = (await resp.json()) as PermissionAsk[];
        return all.filter((p) => p.sessionID === sessionID);
      } catch {
        return [];
      }
    },
    [ocFetch],
  );

  const openSession = useCallback(
    async (id: string) => {
      const [res, permissions] = await Promise.all([
        client.session.messages({ path: { id } }),
        loadPermissions(id),
      ]);
      const messages = (res.data ?? []) as ChatMessage[];
      setState((s) => ({
        ...s,
        sessionID: id,
        messages,
        permissions,
        working: lastAssistantIncomplete(messages),
        error: null,
      }));
    },
    [client, loadPermissions],
  );

  const newSession = useCallback(async () => {
    const res = await client.session.create({ body: {} });
    const session = res.data;
    if (!session) throw new Error("could not create a session");
    setState((s) => ({
      ...s,
      sessions: [session, ...s.sessions],
      sessionID: session.id,
      messages: [],
      permissions: [],
      error: null,
    }));
    return session.id;
  }, [client]);

  // Initial load: models, sessions, most recent session's history. A fresh
  // workspace needs a few seconds before OpenCode answers, so keep retrying
  // until it does.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      for (let attempt = 0; !cancelled; attempt++) {
        try {
          const [providersRes, configRes, sessions] = await Promise.all([
            client.config.providers(),
            client.config.get(),
            loadSessions(),
          ]);
          if (cancelled) return;
          let providers = providersRes.data?.providers ?? [];
          const defaults = providersRes.data?.default ?? {};
          // "Allowed options" means the providers this workspace's profile
          // configures — not OpenCode's whole catalog. (The gateway enforces
          // its model allowlist regardless; this keeps the picker honest.)
          const configured = Object.keys(configRes.data?.provider ?? {});
          if (configured.length > 0) {
            providers = providers.filter((p) => configured.includes(p.id));
          }
          const models = providers.flatMap((p: Provider) =>
            Object.values(p.models ?? {}).map((m) => ({
              providerID: p.id,
              modelID: m.id,
              label: m.name || m.id,
              providerName: p.name || p.id,
            })),
          );
          // Right after boot the catalog can be momentarily empty; treat
          // that as "still starting" rather than a workspace with no models.
          if (models.length === 0 && attempt < 10) {
            throw new Error("model catalog not ready");
          }
          const firstProvider = providers[0];
          const profileDefault = configRes.data?.model; // "provider/model"
          const defaultModel =
            models.find((m) => `${m.providerID}/${m.modelID}` === profileDefault) ??
            models.find(
              (m) => firstProvider && m.modelID === defaults[firstProvider.id],
            ) ??
            models[0] ??
            null;
          setState((s) => ({
            ...s,
            ready: true,
            offline: null,
            sessions,
            models,
            model: s.model ?? defaultModel,
          }));
          if (sessions.length > 0) await openSession(sessions[0].id);
          return;
        } catch (e) {
          if (cancelled) return;
          setState((s) => ({
            ...s,
            offline:
              attempt < 3
                ? "starting up…"
                : e instanceof Error
                  ? e.message
                  : "agent unreachable",
          }));
          await new Promise((r) => setTimeout(r, 2500));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, loadSessions, openSession]);

  // Event stream: one subscription per workspace, filtered to the open session.
  useEffect(() => {
    const controller = new AbortController();
    (async () => {
      while (!controller.signal.aborted) {
        try {
          const events = await client.event.subscribe({ signal: controller.signal });
          // The running server emits event types beyond the SDK's static
          // union (notably permission.asked/replied) — widen and dispatch.
          for await (const event of events.stream as AsyncGenerator<{
            type: string;
            properties: never;
          }>) {
            const sid = sessionRef.current;
            switch (event.type) {
              case "message.updated": {
                const info = (event.properties as { info: Message }).info;
                if (info.sessionID !== sid) break;
                setState((s) => {
                  const i = s.messages.findIndex((m) => m.info.id === info.id);
                  const messages =
                    i === -1
                      ? [...s.messages, { info, parts: [] }]
                      : s.messages.map((m, j) => (j === i ? { ...m, info } : m));
                  return { ...s, messages, working: lastAssistantIncomplete(messages) };
                });
                break;
              }
              case "message.part.updated": {
                // Part events can trail session.idle; they never change
                // whether the agent is working.
                const part = (event.properties as { part: Part }).part;
                if (part.sessionID !== sid) break;
                setState((s) => ({
                  ...s,
                  messages: s.messages.map((m) =>
                    m.info.id === part.messageID
                      ? { ...m, parts: upsertPart(m.parts, part) }
                      : m,
                  ),
                }));
                break;
              }
              case "session.status": {
                const p = event.properties as {
                  sessionID: string;
                  status: { type: string };
                };
                if (p.sessionID !== sid) break;
                setState((s) => ({ ...s, working: p.status.type !== "idle" }));
                break;
              }
              case "permission.asked": {
                const perm = event.properties as PermissionAsk;
                if (perm.sessionID !== sid) break;
                setState((s) => ({
                  ...s,
                  permissions: [
                    ...s.permissions.filter((p) => p.id !== perm.id),
                    perm,
                  ],
                }));
                break;
              }
              case "permission.replied": {
                const done = event.properties as {
                  requestID?: string;
                  permissionID?: string;
                };
                const id = done.requestID ?? done.permissionID;
                setState((s) => ({
                  ...s,
                  permissions: s.permissions.filter((p) => p.id !== id),
                }));
                break;
              }
              case "session.idle": {
                const idle = event.properties as { sessionID: string };
                idleListeners.current.forEach((fn) => fn());
                if (idle.sessionID !== sid) break;
                setState((s) => ({ ...s, working: false }));
                break;
              }
              case "session.error": {
                const err = event.properties as { error?: { data?: { message?: string } } };
                setState((s) => ({
                  ...s,
                  working: false,
                  error: err.error?.data?.message ?? "the agent hit an error",
                }));
                break;
              }
            }
          }
        } catch {
          // Stream dropped (sandbox restarting, network blip): retry shortly.
        }
        if (!controller.signal.aborted) {
          await new Promise((r) => setTimeout(r, 2000));
        }
      }
    })();
    return () => controller.abort();
  }, [client]);

  const send = useCallback(
    async (text: string) => {
      let sid = sessionRef.current;
      if (!sid) sid = await newSession();
      setState((s) => ({ ...s, working: true, error: null }));
      const model = state.model;
      try {
        await client.session.promptAsync({
          path: { id: sid },
          body: {
            parts: [{ type: "text", text }],
            ...(model ? { model: { providerID: model.providerID, modelID: model.modelID } } : {}),
          },
        });
      } catch (e) {
        // The prompt never reached the agent — don't leave the UI working.
        setState((s) => ({
          ...s,
          working: false,
          error: e instanceof Error ? e.message : "could not send the message",
        }));
        throw e;
      }
    },
    [client, newSession, state.model],
  );

  const respondPermission = useCallback(
    async (perm: PermissionAsk, response: "once" | "always" | "reject") => {
      const resp = await ocFetch(`/permission/${perm.id}/reply`, {
        method: "POST",
        body: JSON.stringify({ reply: response }),
      });
      if (!resp.ok) throw new Error(`reply failed (${resp.status})`);
      setState((s) => ({
        ...s,
        permissions: s.permissions.filter((p) => p.id !== perm.id),
      }));
    },
    [ocFetch],
  );

  const abort = useCallback(async () => {
    const sid = sessionRef.current;
    if (sid) await client.session.abort({ path: { id: sid } });
    setState((s) => ({ ...s, working: false }));
  }, [client]);

  const setModel = useCallback((model: ModelChoice) => {
    setState((s) => ({ ...s, model }));
  }, []);

  const diff = useCallback(async (): Promise<VcsChange[]> => {
    // Everything uncommitted in the workspace repo = what the agent changed
    // since the workspace baseline. (The SDK has no wrapper for this route.)
    const resp = await ocFetch("/vcs/diff?mode=git");
    if (!resp.ok) throw new Error(`changes unavailable (${resp.status})`);
    return (await resp.json()) as VcsChange[];
  }, [ocFetch]);

  return { ...state, client, openSession, newSession, send, respondPermission, abort, setModel, diff, subscribeIdle };
}
