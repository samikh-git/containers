import { useEffect, useState } from "react";
import {
  Banner,
  Empty,
  Grid,
  LayerCard,
  Select,
  Table,
  Text,
} from "@cloudflare/kumo";
import { routerAPI, type UsageBucket, type UsageSummary } from "../lib/router";
import { fmtTokens, fmtUSD } from "./format";
import { SectionTitle } from "./SectionTitle";

type Props = { tick: number };

const WINDOWS = [
  { label: "1h", value: "1h" },
  { label: "6h", value: "6h" },
  { label: "24h", value: "24h" },
  { label: "7d", value: "7d" },
  { label: "30d", value: "30d" },
];

export function UsageSection({ tick }: Props) {
  const [window, setWindow] = useState("24h");
  const [usage, setUsage] = useState<UsageSummary | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const u = await routerAPI.usage(window);
        if (!cancelled) {
          setUsage(u);
          setError(null);
        }
      } catch (e) {
        if (!cancelled) {
          setUsage(null);
          setError(e instanceof Error ? e.message : "usage unavailable");
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [tick, window]);

  const t = usage?.total;
  const priced = t?.priced_requests ?? 0;
  const unpriced = (t?.forwarded ?? 0) - priced;
  let note = usage?.note ?? "";
  if (usage?.available && unpriced > 0) {
    note += `${note ? " · " : ""}${unpriced} forwarded request(s) lack token counts (pre-upgrade or unparseable)`;
  }

  return (
    <LayerCard className="p-4">
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <div className="uppercase tracking-wider">
          <Text variant="secondary" size="sm">
            Cost &amp; usage
          </Text>
        </div>
        <div className="flex-1" />
        <Select
          label="window"
          size="sm"
          value={window}
          onValueChange={(v) => setWindow(String(v))}
          items={WINDOWS}
          className="w-28"
        >
          {WINDOWS.map((w) => (
            <Select.Option key={w.value} value={w.value}>
              {w.label}
            </Select.Option>
          ))}
        </Select>
      </div>

      {error ? (
        <Banner variant="error">{error}</Banner>
      ) : !usage ? (
        <Text variant="secondary">Loading…</Text>
      ) : !usage.available ? (
        <Banner variant="alert">{usage.note || "usage unavailable"}</Banner>
      ) : (
        <>
          <Grid variant="4up" gap="sm" className="mb-4">
            <Stat
              label="Est. cost"
              value={priced ? fmtUSD(t?.cost_usd) : "—"}
              sub={priced ? `${priced} priced reqs` : "no token counts yet"}
            />
            <Stat
              label="Requests"
              value={String(t?.requests ?? 0)}
              sub={`${t?.forwarded ?? 0} fwd · ${t?.blocked ?? 0} blocked · ${t?.errors ?? 0} err`}
            />
            <Stat label="Input tokens" value={fmtTokens(t?.input_tokens ?? 0)} />
            <Stat label="Output tokens" value={fmtTokens(t?.output_tokens ?? 0)} />
          </Grid>
          {note && (
            <div className="mb-4">
              <Text variant="secondary" size="sm">
                {note}
              </Text>
            </div>
          )}
          <div className="grid gap-4 md:grid-cols-2">
            <BucketTable title="By workspace" buckets={usage.by_workspace} />
            <BucketTable title="By model" buckets={usage.by_model} />
          </div>
        </>
      )}
    </LayerCard>
  );
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-lg bg-kumo-recessed p-3 ring-1 ring-kumo-hairline">
      <div className="uppercase tracking-wider">
        <Text variant="secondary" size="xs">
          {label}
        </Text>
      </div>
      <div className="mt-1 text-lg tabular-nums text-kumo-strong">{value}</div>
      {sub && (
        <div className="mt-1">
          <Text variant="secondary" size="xs">
            {sub}
          </Text>
        </div>
      )}
    </div>
  );
}

function BucketTable({ title, buckets }: { title: string; buckets: UsageBucket[] }) {
  return (
    <div>
      <SectionTitle>{title}</SectionTitle>
      {!buckets?.length ? (
        <Empty size="sm" title="No traffic in window" />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <Table.Header>
              <Table.Row>
                <Table.Head>Key</Table.Head>
                <Table.Head>Reqs</Table.Head>
                <Table.Head>Tokens</Table.Head>
                <Table.Head>Est. cost</Table.Head>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {buckets.map((b) => {
                const tok = (b.input_tokens || 0) + (b.output_tokens || 0);
                return (
                  <Table.Row key={b.key}>
                    <Table.Cell>{b.key || "(none)"}</Table.Cell>
                    <Table.Cell className="tabular-nums">{b.requests || 0}</Table.Cell>
                    <Table.Cell className="tabular-nums text-kumo-subtle">
                      {tok
                        ? `${fmtTokens(b.input_tokens || 0)} in / ${fmtTokens(b.output_tokens || 0)} out`
                        : "—"}
                    </Table.Cell>
                    <Table.Cell className="tabular-nums">
                      {b.priced_requests ? fmtUSD(b.cost_usd) : "—"}
                    </Table.Cell>
                  </Table.Row>
                );
              })}
            </Table.Body>
          </Table>
        </div>
      )}
    </div>
  );
}
