export function fmtNum(n: number | null | undefined, digits: number): string | null {
  if (n == null || Number.isNaN(n)) return null;
  return Number(n).toFixed(digits);
}

export function fmtUSD(n: number | null | undefined): string {
  if (n == null || Number.isNaN(n)) return "—";
  if (n === 0) return "$0";
  if (n < 0.01) return `$${n.toFixed(4)}`;
  return `$${n.toFixed(2)}`;
}

export function fmtTokens(n: number | null | undefined): string {
  if (n == null) return "—";
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}k`;
  return String(n);
}

export function fmtActivity(iso?: string): string {
  if (!iso) return "—";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "—";
  const sec = Math.max(0, (Date.now() - t.getTime()) / 1000);
  if (sec < 60) return `${Math.floor(sec)}s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
  return t.toLocaleString();
}

export function fmtMem(used?: number, limit?: number): string {
  if (used == null) return "—";
  const u = fmtNum(used, 0);
  if (limit == null) return `${u} MB`;
  return `${u} / ${fmtNum(limit, 0)} MB`;
}

export function fmtDisk(usedMB?: number, quotaGB?: number): string {
  if (usedMB == null) return "—";
  const used =
    usedMB >= 1024 ? `${fmtNum(usedMB / 1024, 2)} GB` : `${fmtNum(usedMB, 0)} MB`;
  if (quotaGB == null) return used;
  return `${used} / ${fmtNum(quotaGB, 1)} GB`;
}
