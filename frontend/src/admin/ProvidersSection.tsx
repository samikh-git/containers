import { useEffect, useState } from "react";
import {
  Badge,
  Banner,
  Button,
  Empty,
  Input,
  LayerCard,
  Table,
} from "@cloudflare/kumo";
import { routerAPI, type RouteInfo } from "../lib/router";
import { providerOf } from "./providers";
import { SectionTitle } from "./SectionTitle";

type Props = {
  tick: number;
  onLog: (msg: string, kind?: "info" | "ok" | "err") => void;
};

export function ProvidersSection({ tick, onLog }: Props) {
  const [routes, setRoutes] = useState<Record<string, RouteInfo> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const r = await routerAPI.routes();
        if (!cancelled) {
          setRoutes(r);
          setError(null);
        }
      } catch (e) {
        if (!cancelled) {
          setRoutes(null);
          setError(e instanceof Error ? e.message : "routes unavailable");
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [tick]);

  const put = async (route: string, key: string, what: string) => {
    setBusy(route);
    try {
      await routerAPI.setKey(route, key);
      setDrafts((d) => ({ ...d, [route]: "" }));
      onLog(`${what} for ${route}`, "ok");
    } catch (e) {
      onLog(`${what} ${route}: ${e instanceof Error ? e.message : e}`, "err");
    } finally {
      setBusy(null);
    }
  };

  return (
    <LayerCard className="p-4">
      <SectionTitle>Model providers (policy gateway keys)</SectionTitle>
      {error && !routes ? (
        <Banner variant="alert">{error}</Banner>
      ) : !routes || Object.keys(routes).length === 0 ? (
        <Empty size="sm" title="No routes" description="Configure the policy gateway to manage provider keys." />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <Table.Header>
              <Table.Row>
                <Table.Head>Provider</Table.Head>
                <Table.Head>Route</Table.Head>
                <Table.Head>Models</Table.Head>
                <Table.Head>Key</Table.Head>
                <Table.Head sticky="right"> </Table.Head>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {Object.keys(routes)
                .sort()
                .map((route) => {
                  const info = routes[route];
                  const provider = providerOf(route, info);
                  return (
                    <Table.Row key={route}>
                      <Table.Cell>{provider.name}</Table.Cell>
                      <Table.Cell className="text-kumo-subtle">{route}</Table.Cell>
                      <Table.Cell className="text-kumo-subtle">
                        {(info.allowed_models || []).join(", ") || "any"}
                      </Table.Cell>
                      <Table.Cell>
                        {info.key_set ? (
                          <Badge variant="success">
                            configured
                            {info.key_source ? ` (${info.key_source === "env" ? "from environment" : "set via admin"})` : ""}
                          </Badge>
                        ) : (
                          <Badge variant="error">missing</Badge>
                        )}
                      </Table.Cell>
                      <Table.Cell sticky="right">
                        <div className="flex items-center gap-2">
                          <Input
                            size="sm"
                            type="password"
                            className="w-44"
                            placeholder={provider.hint || "paste API key"}
                            value={drafts[route] ?? ""}
                            onChange={(e) =>
                              setDrafts((d) => ({ ...d, [route]: e.target.value }))
                            }
                          />
                          <Button
                            size="sm"
                            variant="secondary"
                            loading={busy === route}
                            disabled={!drafts[route]}
                            onClick={() => void put(route, drafts[route], "key set")}
                          >
                            Save
                          </Button>
                          <Button
                            size="sm"
                            variant="secondary-destructive"
                            loading={busy === route}
                            onClick={() => {
                              if (confirm(`Clear the runtime key for ${provider.name}?`)) {
                                void put(route, "", "runtime key cleared");
                              }
                            }}
                          >
                            Clear
                          </Button>
                        </div>
                      </Table.Cell>
                    </Table.Row>
                  );
                })}
            </Table.Body>
          </Table>
        </div>
      )}
    </LayerCard>
  );
}
