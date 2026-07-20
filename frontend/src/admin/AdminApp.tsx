import { useCallback, useEffect, useState } from "react";
import {
  Banner,
  Button,
  LayerCard,
  Link,
  SensitiveInput,
  Text,
  Toasty,
  useKumoToastManager,
} from "@cloudflare/kumo";
import { MoonIcon, SunIcon } from "@phosphor-icons/react";
import { ApiError, authHeaders, getToken, setToken } from "../lib/router";
import { ActivityLog, type LogEntry, useActivityLog } from "./ActivityLog";
import { ProvidersSection } from "./ProvidersSection";
import { ProvisionSection } from "./ProvisionSection";
import { SandboxesSection } from "./SandboxesSection";
import { UsageSection } from "./UsageSection";

export function AdminApp() {
  return (
    <Toasty>
      <AdminShell />
    </Toasty>
  );
}

function AdminShell() {
  const toast = useKumoToastManager();
  const { entries, push } = useActivityLog();
  const [needsToken, setNeedsToken] = useState(false);
  const [ready, setReady] = useState(false);
  const [tick, setTick] = useState(0);
  const [mode, setMode] = useState(
    () => document.documentElement.dataset.mode || "dark",
  );

  const log = useCallback(
    (msg: string, kind: LogEntry["kind"] = "info") => {
      push(msg, kind);
      if (kind === "err") toast.add({ title: msg, variant: "error" });
      else if (kind === "ok") toast.add({ title: msg, variant: "success" });
    },
    [push, toast],
  );

  const toggleMode = () => {
    const next = mode === "dark" ? "light" : "dark";
    document.documentElement.dataset.mode = next;
    localStorage.setItem("admin-mode", next);
    setMode(next);
  };

  const probeAuth = useCallback(async () => {
    const resp = await fetch("/api/workspaces", { headers: authHeaders() });
    if (resp.status === 401) {
      setNeedsToken(true);
      setReady(false);
      return false;
    }
    setNeedsToken(false);
    setReady(true);
    return true;
  }, []);

  useEffect(() => {
    void probeAuth();
  }, [probeAuth]);

  useEffect(() => {
    if (!ready) return;
    const id = setInterval(() => setTick((t) => t + 1), 5000);
    return () => clearInterval(id);
  }, [ready]);

  const refreshAll = () => setTick((t) => t + 1);

  if (needsToken) {
    return (
      <TokenGate
        onDone={() => {
          setNeedsToken(false);
          setReady(true);
        }}
      />
    );
  }

  if (!ready) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-kumo-canvas">
        <Text variant="secondary">Loading…</Text>
      </div>
    );
  }

  return (
    <div className="min-h-screen bg-kumo-canvas text-kumo-default">
      <header className="flex flex-wrap items-center gap-3 border-b border-kumo-fill px-6 py-4">
        <div>
          <Text variant="heading3" as="h1">
            Workspace Router
          </Text>
          <Text variant="secondary" size="sm">
            operator console
          </Text>
        </div>
        <div className="flex-1" />
        <Link href="/" variant="inline">
          End-user app
        </Link>
        <SensitiveInput
          size="sm"
          className="w-48"
          value={getToken()}
          onChange={(e) => {
            setToken(e.target.value);
            void probeAuth();
          }}
          placeholder="API token"
          aria-label="API token"
        />
        <Button
          variant="ghost"
          size="sm"
          shape="square"
          aria-label="Toggle color mode"
          icon={mode === "dark" ? SunIcon : MoonIcon}
          onClick={toggleMode}
        />
        <Button variant="secondary" size="sm" onClick={refreshAll}>
          Refresh
        </Button>
      </header>

      <main className="mx-auto flex max-w-6xl flex-col gap-5 p-6">
        <ProvidersSection tick={tick} onLog={log} />
        <ProvisionSection tick={tick} onLog={log} onProvisioned={refreshAll} />
        <SandboxesSection
          tick={tick}
          onLog={log}
          onAuthLost={() => setNeedsToken(true)}
        />
        <UsageSection tick={tick} />
        <ActivityLog entries={entries} />
      </main>
    </div>
  );
}

function TokenGate({ onDone }: { onDone: () => void }) {
  const [value, setValue] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setErr(null);
    try {
      const resp = await fetch("/api/workspaces", {
        headers: { Authorization: `Bearer ${value}` },
      });
      if (!resp.ok) throw new ApiError("That token was not accepted.", resp.status);
      setToken(value);
      onDone();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "That token was not accepted.");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-kumo-canvas p-4">
      <LayerCard className="w-full max-w-md p-6">
        <div className="mb-2">
          <Text variant="heading2" as="h1">
            Access token
          </Text>
        </div>
        <div className="mb-4">
          <Text variant="secondary">
            This router requires <code className="font-mono text-sm">ROUTER_API_TOKEN</code>.
            Paste it to use the admin console.
          </Text>
        </div>
        <form onSubmit={submit} className="flex flex-col gap-3">
          <SensitiveInput
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="Bearer token"
            autoComplete="current-password"
            required
            autoFocus
          />
          <Button type="submit" variant="primary" loading={loading}>
            Continue
          </Button>
          {err && <Banner variant="error">{err}</Banner>}
        </form>
      </LayerCard>
    </div>
  );
}
