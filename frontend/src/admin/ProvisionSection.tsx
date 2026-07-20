import { useEffect, useState } from "react";
import {
  Button,
  Collapsible,
  Input,
  LayerCard,
  Select,
  Text,
} from "@cloudflare/kumo";
import { routerAPI, type ProvisionSpec, type ServeDefaults } from "../lib/router";
import { SectionTitle } from "./SectionTitle";

type Props = {
  tick: number;
  onLog: (msg: string, kind?: "info" | "ok" | "err") => void;
  onProvisioned: () => void;
};

export function ProvisionSection({ tick, onLog, onProvisioned }: Props) {
  const [defaults, setDefaults] = useState<ServeDefaults>({});
  const [routes, setRoutes] = useState<string[]>([]);
  const [id, setId] = useState("");
  const [route, setRoute] = useState("");
  const [image, setImage] = useState("");
  const [cpus, setCpus] = useState("");
  const [memoryMb, setMemoryMb] = useState("");
  const [quotaGb, setQuotaGb] = useState("");
  const [network, setNetwork] = useState("");
  const [profileDir, setProfileDir] = useState("");
  const [gatewayUrl, setGatewayUrl] = useState("");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [d, r] = await Promise.all([
          routerAPI.defaults(),
          routerAPI.routes().catch(() => ({}) as Record<string, unknown>),
        ]);
        if (cancelled) return;
        setDefaults(d);
        const keys = Object.keys(r).sort();
        setRoutes(keys);
        if (d.route && !route) setRoute(d.route);
      } catch {
        /* defaults optional */
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only re-sync on poll tick
  }, [tick]);

  const hint = (() => {
    const bits: string[] = [];
    if (defaults.image) bits.push(`image=${defaults.image}`);
    if (defaults.cpus) bits.push(`cpus=${defaults.cpus}`);
    if (defaults.memory_mb) bits.push(`mem=${defaults.memory_mb}MB`);
    if (defaults.quota_gb) bits.push(`quota=${defaults.quota_gb}GB`);
    if (defaults.network) bits.push(`net=${defaults.network}`);
    if (defaults.route) bits.push(`route=${defaults.route}`);
    if (defaults.profile_dir) bits.push(`profile=${defaults.profile_dir}`);
    if (defaults.gateway_url) bits.push(`gateway=${defaults.gateway_url}`);
    return bits.length ? `Serve defaults: ${bits.join(" · ")}` : "No serve defaults set.";
  })();

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const body: ProvisionSpec = { id: id.trim() };
    if (image) body.image = image;
    if (cpus) body.cpus = parseFloat(cpus);
    if (memoryMb) body.memory_mb = parseInt(memoryMb, 10);
    if (quotaGb) body.quota_gb = parseInt(quotaGb, 10);
    if (route) body.route = route;
    if (network) body.network = network;
    if (profileDir) body.profile_dir = profileDir;
    if (gatewayUrl) body.gateway_url = gatewayUrl;

    setLoading(true);
    onLog(`provisioning ${body.id} …`);
    try {
      const res = await routerAPI.createWorkspace(body);
      onLog(`workspace ${res.id} up, volume at ${res.mount}`, "ok");
      setId("");
      setImage("");
      setCpus("");
      setMemoryMb("");
      setQuotaGb("");
      setNetwork("");
      setProfileDir("");
      setGatewayUrl("");
      onProvisioned();
    } catch (ex) {
      onLog(`up ${body.id}: ${ex instanceof Error ? ex.message : ex}`, "err");
    } finally {
      setLoading(false);
    }
  };

  const routeItems = [
    { label: "(default)", value: "" },
    ...routes.map((r) => ({ label: r, value: r })),
  ];

  return (
    <LayerCard className="p-4">
      <SectionTitle>Provision workspace</SectionTitle>
      <form onSubmit={submit} className="flex flex-col gap-3">
        <div className="flex flex-wrap items-end gap-3">
          <Input
            label="id"
            size="sm"
            required
            pattern="[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}"
            placeholder="ws1"
            value={id}
            onChange={(e) => setId(e.target.value)}
            className="w-40"
          />
          <Select
            label="route"
            size="sm"
            value={route}
            onValueChange={(v) => setRoute(String(v ?? ""))}
            items={routeItems}
            className="min-w-48"
          >
            {routeItems.map((it) => (
              <Select.Option key={it.value || "default"} value={it.value}>
                {it.label}
              </Select.Option>
            ))}
          </Select>
          <Button type="submit" variant="primary" loading={loading}>
            Up
          </Button>
        </div>

        <Collapsible>
          <Collapsible.DefaultTrigger>Advanced (image, resources, network, profile)</Collapsible.DefaultTrigger>
          <Collapsible.DefaultPanel>
            <div className="mt-3 flex flex-wrap gap-3">
              <Input label="image" size="sm" placeholder="(default)" value={image} onChange={(e) => setImage(e.target.value)} className="w-48" />
              <Input label="cpus" size="sm" type="number" step={0.5} min={0} placeholder="dflt" value={cpus} onChange={(e) => setCpus(e.target.value)} className="w-24" />
              <Input label="memory MB" size="sm" type="number" min={0} placeholder="dflt" value={memoryMb} onChange={(e) => setMemoryMb(e.target.value)} className="w-28" />
              <Input label="quota GB" size="sm" type="number" min={0} placeholder="dflt" value={quotaGb} onChange={(e) => setQuotaGb(e.target.value)} className="w-24" />
              <Input label="network" size="sm" placeholder="(default)" value={network} onChange={(e) => setNetwork(e.target.value)} className="w-36" />
              <Input label="profile dir" size="sm" placeholder="(default)" value={profileDir} onChange={(e) => setProfileDir(e.target.value)} className="w-56" />
              <Input label="gateway URL" size="sm" placeholder="(default)" value={gatewayUrl} onChange={(e) => setGatewayUrl(e.target.value)} className="w-64" />
            </div>
            <div className="mt-2">
              <Text variant="secondary" size="sm">
                {hint}
              </Text>
            </div>
          </Collapsible.DefaultPanel>
        </Collapsible>
      </form>
    </LayerCard>
  );
}
