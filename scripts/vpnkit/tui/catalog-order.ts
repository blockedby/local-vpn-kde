import type { Server } from "./bridge";

export type ServerSort = "name" | "ping" | "speed" | "availability";

/** Return an ordered copy without changing the catalog or its selection identity. */
export function sortedServers(servers: readonly Server[], sort: ServerSort, frozenOrder?: readonly string[]): Server[] {
  if (frozenOrder) {
    const ranks = new Map(frozenOrder.map((id, i) => [id, i]));
    return [...servers].sort((a, b) => (ranks.get(a.server_id) ?? Infinity) - (ranks.get(b.server_id) ?? Infinity));
  }
  const compare = (a: Server, b: Server) =>
    a.display_name.localeCompare(b.display_name, "ru", { numeric: true }) || a.server_id.localeCompare(b.server_id);
  return [...servers].sort((a, b) => {
    if (sort === "ping") return (a.latency_ms ?? Infinity) - (b.latency_ms ?? Infinity) || compare(a, b);
    if (sort === "speed") return (b.download_mbps ?? -1) - (a.download_mbps ?? -1) || compare(a, b);
    if (sort === "availability") return Number(b.availability === "ready") - Number(a.availability === "ready") || compare(a, b);
    return compare(a, b);
  });
}

/** Keep the cursor in view while preserving the old scroll anchor when possible. */
export function serverViewport(servers: readonly Server[], serverID: string | undefined, viewStart: number, rowCount: number) {
  const at = Math.max(0, servers.findIndex(s => s.server_id === serverID));
  let start = Math.min(viewStart, Math.max(0, servers.length - rowCount));
  if (at < start) start = at;
  if (at >= start + rowCount) start = at - rowCount + 1;
  return { start, servers: servers.slice(start, start + rowCount) };
}
