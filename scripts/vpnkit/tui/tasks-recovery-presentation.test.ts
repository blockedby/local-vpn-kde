import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import type { Action, Backend, Reply, SpeedProgress } from "./bridge";

const status = { vpn_state: "healthy", gateway_state: "healthy", subscription: "configured", routing_mode: "smart", networkmanager_configured: "yes", networkmanager_active: "yes", diagnostics: "not-run" };
const rows = ["Alpha", "Beta"].map((display_name, i) => ({ display_name, server_id: "srv_" + String(i).repeat(27), selected: i === 0, status: "untested" }));
const reply = (): Reply => ({ ok: true, reason: "ok", code: 0, status: { ...status } });
const settle = () => Bun.sleep(50);
class BackendFixture implements Backend {
  calls: { action: Action; id?: string }[] = [];
  cancelled: (string | undefined)[] = [];
  held = new Map<Action, { resolve: (r: Reply) => void; reject: (e: Error) => void }>();
  holds = new Set<Action>();
  reconnects = 0;
  reconnectError = false;
  onDisconnect?: () => void;
  onSpeedProgress?: (row: SpeedProgress, id?: string) => void;
  request(action: Action, value?: string, id?: string): Promise<Reply> {
    this.calls.push({ action, id });
    if (this.holds.has(action)) return new Promise((resolve, reject) => this.held.set(action, { resolve, reject }));
    const r = reply();
    if (action === "servers/list") r.catalog = { status: "ok", servers: structuredClone(rows) };
    if (action === "servers/ping") r.catalog = { status: "ok", server: { ...rows[0], ping_status: "ready", latency_ms: 20 } };
    if (action === "servers/select") r.catalog = { status: "ok", server: { ...rows.find(r => r.server_id === value)!, selected: true } };
    return Promise.resolve(r);
  }
  cancel(id?: string) { this.cancelled.push(id); }
  async reconnect() { this.reconnects++; if (this.reconnectError) throw new Error("offline"); }
  async close() {}
}
async function fixture() {
  const t = await createTestRenderer({ width: 100, height: 35 });
  const b = new BackendFixture();
  const app = new App(t.renderer, b);
  await app.perform("status");
  const key = async (key: string) => { t.mockInput.pressKey(key); await settle(); await t.renderOnce(); };
  const frame = async () => { await t.renderOnce(); return t.captureCharFrame(); };
  const close = async () => { for (const h of b.held.values()) h.resolve(reply()); await settle(); await app.close(); };
  return { t, b, app, key, frame, close };
}

test("presentation: task chooser cancels only the chosen measurement and confirms mutation cancellation", async () => {
  const f = await fixture();
  f.b.holds = new Set(["servers/check-batch", "servers/select"]);
  try {
    const a = f.app.perform("servers/check-batch", "{}", true);
    const b = f.app.perform("servers/select", rows[1].server_id);
    await f.key("k");
    expect(await f.frame()).toContain("Состояние Операция Этап Время");
    expect(await f.frame()).toContain("Выполняется       Применяем сервер");
    await f.key("1");
    await f.key("2");
    expect(await f.frame()).toContain("Точно отменить");
    await f.key("return");
    await f.key("2");
    f.t.mockInput.pressEscape(); await settle();
    await f.key("j"); await f.key("2"); await f.key("y");
    f.b.held.get("servers/select")!.resolve(reply()); await b;
    expect(await f.frame()).not.toContain("Выполняется       Применяем сервер");
    expect(await f.frame()).toContain("Ping");
    f.b.held.get("servers/check-batch")!.resolve(reply()); await a;
    expect([...f.app.tasks.values()].filter(task => task.action !== "status")).toHaveLength(0);
  } finally { await f.close(); }
});

test("presentation: DNS failure offers server choice without replaying failed connection", async () => {
  const f = await fixture();
  f.b.holds.add("start");
  try {
    const task = f.app.perform("start");
    f.b.held.get("start")!.resolve({ ...reply(), ok: false, reason: "dns-failed" }); await task;
    await f.key("f");
    expect(await f.frame()).toContain("Выбрать другой сервер");
    await f.key("v");
    expect(await f.frame()).toContain("Alpha");
  } finally { await f.close(); }
});

test("presentation: explicit recovery rereads status and catalog without replaying interrupted mutation", async () => {
  const f = await fixture();
  f.b.holds.add("servers/select");
  try {
    const task = f.app.perform("servers/select", rows[1].server_id);
    f.b.onDisconnect?.();
    f.b.held.get("servers/select")!.reject(new Error("backend-unavailable")); await task;
    await f.key("f"); await f.key("u");
    expect(await f.frame()).toContain("Связь восстановлена");
  } finally { await f.close(); }
});

test("presentation: failed reconnect leaves explicit retry available without issuing actions", async () => {
  const f = await fixture();
  try {
    f.b.reconnectError = true; f.b.onDisconnect?.();
    await f.key("f"); await f.key("u");
    expect(await f.frame()).toContain("Не удалось восстановить связь");
    expect(await f.frame()).toContain("Восстановить связь");
    await f.key("u");
  } finally { await f.close(); }
});

test("presentation: live speed belongs to measured server and late samples cannot overwrite final result", async () => {
  const f = await fixture();
  f.b.holds.add("servers/speed");
  try {
    await f.key("v");
    await f.key("T");
    const id = f.b.calls.find(c => c.action === "servers/speed")!.id;
    const sample: SpeedProgress = { event: "speed-progress", server_id: rows[0].server_id, downloaded_bytes: 2000000, elapsed_seconds: 1, download_mbps: 16 };
    f.b.onSpeedProgress?.(sample, id);
    expect(await f.frame()).toContain("16.0");
    f.b.onSpeedProgress?.({ ...sample, server_id: rows[1].server_id, download_mbps: 999 }, id);
    expect(await f.frame()).not.toContain("999");
    expect(await f.frame()).toContain("16.0");
    await f.app.perform("servers/select", rows[1].server_id);
    f.b.onSpeedProgress?.({ ...sample, download_mbps: 24 }, id);
    expect(await f.frame()).toContain("24.0");
    f.b.held.get("servers/speed")!.resolve({ ...reply(), catalog: { status: "ok", server: { ...rows[0], status: "ready", download_mbps: 32 } } });
    await settle();
    f.b.onSpeedProgress?.({ ...sample, download_mbps: 999 }, id);
    const frame = await f.frame();
    expect(frame).not.toContain("999");
    expect(frame).toContain("32.0");
  } finally { await f.close(); }
});
