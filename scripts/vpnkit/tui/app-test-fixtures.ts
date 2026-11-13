import type { Action, Backend, Reply, Status, Server, CheckProgress } from "./bridge";
export const initial: Status = {
  vpn_state: "inactive",
  gateway_state: "healthy",
  subscription: "configured",
  routing_mode: "strict",
  networkmanager_configured: "yes",
  networkmanager_active: "no",
  diagnostics: "not-run",
};
export const rows: Server[] = [
  {
    server_id: `srv_${"a".repeat(27)}`,
    display_name: "Tokyo 東京",
    status: "untested",
    selected: false,
  },
  {
    server_id: `srv_${"b".repeat(27)}`,
    display_name: "Amsterdam",
    status: "ready",
    selected: false,
    download_mbps: 40,
  },
];
export const reply = (status = initial): Reply => ({
  ok: true,
  reason: "ok",
  code: 0,
  status: { ...status },
});
export class FakeBackend implements Backend {
  calls: { action: Action; value?: string }[] = [];
  status = { ...initial };
  rows = rows;
  hold?: { action: Action; promise: Promise<Reply> };
  cancelled = 0;
  onProgress?: (phase: string) => void;
  onCheckProgress?: (result: CheckProgress) => void;
  async request(action: Action, value?: string): Promise<Reply> {
    this.calls.push({ action, value });
    if (this.hold?.action === action) return this.hold.promise;
    const r = reply(this.status);
    if (action === "subscription/read")
      r.value = "https://example.invalid/saved";
    if (action === "servers/list" || action === "servers/refresh")
      r.catalog = { status: "ok", servers: structuredClone(this.rows) };
    if (action === "servers/check-batch" || action === "servers/speed-batch") {
      const ids: string[] = JSON.parse(value!).ids;
      r.catalog = {
        status: "ok",
        servers: this.rows
          .filter((s) => ids.includes(s.server_id))
          .map((s) => ({
            ...s,
            ping_status: "ready",
            latency_ms: 24,
            availability: "ready",
            ...(action === "servers/speed-batch" ? {status: "ready", download_mbps: 40, download_seconds: 3, downloaded_bytes: 15000000} : {}),
          })),
      };
    }
    if (
      action === "servers/ping" ||
      action === "servers/speed" ||
      action === "servers/availability"
    ) {
      const id =
        action === "servers/availability" ? JSON.parse(value!).id : value;
      r.catalog = {
        status: "ok",
        server: {
          ...rows.find((s) => s.server_id === id)!,
          ping_status: "ready",
          latency_ms: 24,
          status: "ready",
          download_mbps: 50,
          availability: "ready",
        },
      };
    }
    return r;
  }
  cancel() {
    this.cancelled++;
  }
  async close() {}
}
export const settle = () => Bun.sleep(10);
