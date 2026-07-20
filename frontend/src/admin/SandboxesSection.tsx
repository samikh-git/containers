import { useEffect, useState } from "react";
import {
  Badge,
  Button,
  Dialog,
  Empty,
  Input,
  LayerCard,
  Link,
  Table,
} from "@cloudflare/kumo";
import { ApiError, routerAPI, type WorkspaceStatus } from "../lib/router";
import { fmtActivity, fmtDisk, fmtMem, fmtNum } from "./format";
import { SectionTitle } from "./SectionTitle";

type Props = {
  tick: number;
  onLog: (msg: string, kind?: "info" | "ok" | "err") => void;
  onAuthLost: () => void;
};

export function SandboxesSection({ tick, onLog, onAuthLost }: Props) {
  const [statuses, setStatuses] = useState<WorkspaceStatus[]>([]);
  const [fanoutId, setFanoutId] = useState<string | null>(null);
  const [fanoutN, setFanoutN] = useState("3");
  const [destroyId, setDestroyId] = useState<string | null>(null);
  const [destroyConfirm, setDestroyConfirm] = useState("");
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    if (fanoutId || destroyId) return;
    let cancelled = false;
    (async () => {
      try {
        const list = await routerAPI.listWorkspaces(true);
        if (!cancelled) setStatuses(list ?? []);
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) {
          onAuthLost();
          return;
        }
        if (!cancelled) onLog(`refresh: ${e instanceof Error ? e.message : e}`, "err");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [tick, fanoutId, destroyId, onAuthLost, onLog]);

  const runOp = async (op: "down" | "hibernate", id: string) => {
    setBusy(id);
    try {
      if (op === "down") await routerAPI.stopWorkspace(id);
      else await routerAPI.hibernateWorkspace(id);
      onLog(`${op} ${id} done`, "ok");
    } catch (e) {
      onLog(`${op} ${id}: ${e instanceof Error ? e.message : e}`, "err");
    } finally {
      setBusy(null);
    }
  };

  const doFanout = async () => {
    if (!fanoutId) return;
    const n = parseInt(fanoutN, 10);
    if (!n || n < 1) return;
    setBusy(fanoutId);
    onLog(`fanning out ${fanoutId} × ${n} …`);
    try {
      const res = await routerAPI.fanoutWorkspace(fanoutId, n);
      onLog(`branches up: ${(res.branches || []).join(", ")}`, "ok");
    } catch (e) {
      if (e instanceof ApiError) {
        const branches = e.data.branches as string[] | undefined;
        if (branches?.length) {
          onLog(`fanout ${fanoutId} partial: ${e.message} — up: ${branches.join(", ")}`, "err");
        } else {
          onLog(`fanout ${fanoutId}: ${e.message}`, "err");
        }
      } else {
        onLog(`fanout ${fanoutId}: ${e instanceof Error ? e.message : e}`, "err");
      }
    } finally {
      setBusy(null);
      setFanoutId(null);
    }
  };

  const doDestroy = async () => {
    if (!destroyId || destroyConfirm !== destroyId) return;
    setBusy(destroyId);
    try {
      await routerAPI.destroyWorkspace(destroyId);
      onLog(`destroyed ${destroyId}`, "ok");
    } catch (e) {
      onLog(`destroy ${destroyId}: ${e instanceof Error ? e.message : e}`, "err");
    } finally {
      setBusy(null);
      setDestroyId(null);
      setDestroyConfirm("");
    }
  };

  const stateBadge = (state: string) => {
    const s = (state || "").toLowerCase();
    if (s.startsWith("running")) return <Badge variant="success">{state}</Badge>;
    if (s.includes("exit") || s.includes("stop")) return <Badge variant="warning">{state}</Badge>;
    return <Badge variant="neutral">{state || "?"}</Badge>;
  };

  return (
    <LayerCard className="p-4">
      <SectionTitle>Sandboxes</SectionTitle>
      {statuses.length === 0 ? (
        <Empty size="sm" title="No sandboxes" description="Provision a workspace to get started." />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <Table.Header>
              <Table.Row>
                <Table.Head>Workspace</Table.Head>
                <Table.Head>State</Table.Head>
                <Table.Head>Gen</Table.Head>
                <Table.Head>CPU</Table.Head>
                <Table.Head>Memory</Table.Head>
                <Table.Head>Disk</Table.Head>
                <Table.Head>Activity</Table.Head>
                <Table.Head sticky="right"> </Table.Head>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {statuses.map((s) => (
                <Table.Row key={s.ID}>
                  <Table.Cell title={s.Container || undefined}>
                    <span className="font-medium">{s.ID}</span>
                    {s.AgentBusy && (
                      <Badge variant="info" className="ml-2">
                        busy
                      </Badge>
                    )}
                  </Table.Cell>
                  <Table.Cell>{stateBadge(s.State)}</Table.Cell>
                  <Table.Cell className="tabular-nums">{s.Generation}</Table.Cell>
                  <Table.Cell className="tabular-nums text-kumo-subtle">
                    {s.CPUPercent == null ? "—" : `${fmtNum(s.CPUPercent, 1)}%`}
                  </Table.Cell>
                  <Table.Cell className="tabular-nums text-kumo-subtle">
                    {fmtMem(s.MemoryUsedMB, s.MemoryLimitMB)}
                  </Table.Cell>
                  <Table.Cell className="tabular-nums text-kumo-subtle">
                    {fmtDisk(s.DiskUsedMB, s.DiskQuotaGB)}
                  </Table.Cell>
                  <Table.Cell className="tabular-nums text-kumo-subtle">
                    {fmtActivity(s.LastActivityAt)}
                  </Table.Cell>
                  <Table.Cell sticky="right">
                    <div className="flex flex-wrap justify-end gap-1">
                      <Link href={`/#/ws/${encodeURIComponent(s.ID)}`} variant="plain">
                        <Button size="xs" variant="secondary">
                          Open
                        </Button>
                      </Link>
                      <Button
                        size="xs"
                        variant="secondary"
                        disabled={busy === s.ID}
                        onClick={() => {
                          setFanoutN("3");
                          setFanoutId(s.ID);
                        }}
                      >
                        Fanout
                      </Button>
                      <Button
                        size="xs"
                        variant="secondary"
                        loading={busy === s.ID}
                        onClick={() => void runOp("down", s.ID)}
                      >
                        Down
                      </Button>
                      <Button
                        size="xs"
                        variant="secondary"
                        loading={busy === s.ID}
                        onClick={() => void runOp("hibernate", s.ID)}
                      >
                        Hibernate
                      </Button>
                      <Button
                        size="xs"
                        variant="secondary-destructive"
                        onClick={() => {
                          setDestroyConfirm("");
                          setDestroyId(s.ID);
                        }}
                      >
                        Destroy
                      </Button>
                    </div>
                  </Table.Cell>
                </Table.Row>
              ))}
            </Table.Body>
          </Table>
        </div>
      )}

      <Dialog.Root
        open={fanoutId != null}
        onOpenChange={(open) => {
          if (!open) setFanoutId(null);
        }}
      >
        <Dialog className="p-6">
          <Dialog.Title>Fanout {fanoutId}</Dialog.Title>
          <Dialog.Description>Create N snapshot branches from this workspace.</Dialog.Description>
          <div className="mt-4 flex flex-col gap-3">
            <Input
              label="Branches"
              type="number"
              min={1}
              max={100}
              value={fanoutN}
              onChange={(e) => setFanoutN(e.target.value)}
            />
            <div className="flex justify-end gap-2">
              <Dialog.Close
                render={(p) => (
                  <Button {...p} variant="secondary">
                    Cancel
                  </Button>
                )}
              />
              <Button variant="primary" loading={busy === fanoutId} onClick={() => void doFanout()}>
                Go
              </Button>
            </div>
          </div>
        </Dialog>
      </Dialog.Root>

      <Dialog.Root
        role="alertdialog"
        open={destroyId != null}
        onOpenChange={(open) => {
          if (!open) {
            setDestroyId(null);
            setDestroyConfirm("");
          }
        }}
      >
        <Dialog className="p-6">
          <Dialog.Title>Destroy {destroyId}?</Dialog.Title>
          <Dialog.Description>
            This deletes the workspace, snapshots, and branches. Type the workspace id to confirm.
          </Dialog.Description>
          <div className="mt-4 flex flex-col gap-3">
            <Input
              label={`Type ${destroyId}`}
              value={destroyConfirm}
              onChange={(e) => setDestroyConfirm(e.target.value)}
              placeholder={destroyId ?? ""}
            />
            <div className="flex justify-end gap-2">
              <Dialog.Close
                render={(p) => (
                  <Button {...p} variant="secondary">
                    Cancel
                  </Button>
                )}
              />
              <Button
                variant="destructive"
                disabled={destroyConfirm !== destroyId}
                loading={busy === destroyId}
                onClick={() => void doDestroy()}
              >
                Destroy
              </Button>
            </div>
          </div>
        </Dialog>
      </Dialog.Root>
    </LayerCard>
  );
}
