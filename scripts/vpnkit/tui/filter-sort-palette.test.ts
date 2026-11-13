import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { FakeBackend, initial, rows, reply, settle } from "./app-test-fixtures";
import type { Reply } from "./bridge";
import { sortedServers } from "./catalog-order";

const key = (name: string) => ({ name, preventDefault() {} });
function fixture() {
  const backend = new FakeBackend();
  backend.rows = [{ ...rows[0], display_name: "МОСКВА" }, { ...rows[1], display_name: "Berlin" }];
  const state = new AppState(backend); state.status = { ...initial }; state.servers = structuredClone(backend.rows); state.catalogLoaded = true; state.serverID = state.servers[0]!.server_id;
  return { backend, state };
}

test("filter matches displayed names including Cyrillic and restores the cursor on clear", async () => {
  const { state } = fixture();
  state.onKey(key("/")); state.editFilter("мОс");
  expect(state.sortedServers().map(s => s.display_name)).toEqual(["МОСКВА"]);
  state.editFilter("ber"); expect(state.serverID).toBe(rows[1].server_id);
  state.onKey(key("return")); state.onKey(key("escape"));
  expect(state.serverID).toBe(rows[0].server_id); expect(state.filter).toBe("");
  await state.close();
});

test("filtered batches and fastest selection use shown servers; hidden late results survive", async () => {
  const { state, backend } = fixture();
  let finish!: (value: ReturnType<typeof reply> & { catalog: any }) => void;
  backend.hold = { action: "servers/speed-batch", promise: new Promise(resolve => { finish = resolve; }) };
  state.editFilter("ber"); const run = state.runBatch("speed");
  await Bun.sleep(10);
  expect(JSON.parse(backend.calls.find(call => call.action === "servers/speed-batch")!.value!).ids).toEqual([rows[1].server_id]);
  state.editFilter("моск");
  finish({ ...reply(), catalog: { status: "ok", servers: [{ ...backend.rows[1], status: "ready", ping_status: "ready", availability: "ready", download_mbps: 90 }] } });
  await run; expect(state.servers[1]!.download_mbps).toBe(90);
  state.confirmFastest(); expect(state.screen).not.toBe("confirm");
  state.editFilter("ber"); state.confirmFastest(); expect(state.fastestCandidate?.id).toBe(rows[1].server_id);
  await state.close();
});

test("sort direction keeps missing measurements last and preserves cursor", async () => {
  const { state } = fixture(); state.servers.push({ ...rows[0], server_id: "third", latency_ms: 5, download_mbps: 7 });
  state.servers[0]!.latency_ms = 10; state.servers[1]!.latency_ms = undefined;
  state.servers[0]!.download_mbps = 10; state.servers[1]!.download_mbps = undefined;
  for (const sort of ["ping", "speed"] as const) for (const descending of [false, true]) expect(sortedServers(state.servers, sort, undefined, descending).at(-1)?.server_id).toBe(rows[1].server_id);
  const id = state.serverID; state.setSort("ping"); state.setSort("ping"); expect(state.serverID).toBe(id);
  await state.close();
});

test("a queued filtered batch retains its original scope and deduplicates subsequent batches", async () => {
  const { state, backend } = fixture();
  let finish!: (value: ReturnType<typeof reply>) => void;
  backend.hold = { action: "servers/speed-batch", promise: new Promise(resolve => { finish = resolve; }) };
  const running = state.runBatch("speed"); await Bun.sleep(10);
  state.editFilter("ber"); await state.runBatch("ping");
  state.editFilter("моск"); await state.runBatch("ping");
  expect(state.queue).toHaveLength(1);
  expect(JSON.parse(state.queue[0]!.value!).ids).toEqual([rows[1].server_id]);
  finish(reply()); await running; await Bun.sleep(20);
  expect(JSON.parse(backend.calls.find(call => call.action === "servers/check-batch")!.value!).ids).toEqual([rows[1].server_id]);
  await state.close();
});

test("an unfiltered queued batch keeps the whole shown catalog after filtering", async () => {
  const { state, backend } = fixture(); let finish!: (value: ReturnType<typeof reply>) => void;
  backend.hold = { action: "servers/speed-batch", promise: new Promise(resolve => { finish = resolve; }) };
  const running = state.runBatch("speed"); await Bun.sleep(10); await state.runBatch("ping");
  state.editFilter("ber"); finish(reply()); await running; await Bun.sleep(20);
  expect(JSON.parse(backend.calls.find(call => call.action === "servers/check-batch")!.value!).ids).toHaveLength(2);
  await state.close();
});

test("changing sort during a pass preserves frozen order until completion", async () => {
  const { state, backend } = fixture();
  let finish!: (value: ReturnType<typeof reply>) => void;
  backend.hold = { action: "servers/check-batch", promise: new Promise(resolve => { finish = resolve; }) };
  const running = state.runBatch("ping"); await Bun.sleep(10);
  const before = state.sortedServers().map(s => s.server_id); state.setSort("name");
  expect(state.sortedServers().map(s => s.server_id)).toEqual(before);
  finish(reply()); await running;
  expect(state.sortedServers().map(s => s.server_id)).toEqual([...before].reverse());
  await state.close();
});

test("palette filters actions, ignores text hotkeys, and preserves confirmations and availability", async () => {
  const { state, backend } = fixture(); state.status!.networkmanager_active = "yes";
  state.onKey(key(":")); expect(state.paletteOpen).toBe(true);
  expect(new Set(state.paletteActions().map(v => v.action.id)).size).toBe(state.paletteActions().length);
  const ids = new Set(state.paletteActions().map(v => v.action.id));
  state.paletteOpen = false;
  for (const screen of ["servers", "profiles", "diagnostics", "settings", "autostart", "tasks", "details"] as const) {
    state.screen = screen;
    for (const action of state.actions()) if (!/^(help:|editor:|filter:)/.test(action.id) && action.id !== "global:palette") expect(ids.has(action.id)).toBe(true);
  }
  state.screen = "servers"; state.paletteOpen = true;
  state.onKey(key("q")); expect(state.paletteOpen).toBe(true); expect(state.closing).toBe(false);
  state.paletteQuery = "отключить vpn"; state.paletteIndex = 0; state.onKey(key("return"));
  expect(String(state.screen)).toBe("confirm"); expect(backend.calls.some(call => call.action === "disconnect")).toBe(false);
  state.open("servers", false); state.onKey(key(":")); state.paletteQuery = "отменить";
  const index = state.filteredPaletteActions().findIndex(v => !v.action.available); expect(index).toBeGreaterThanOrEqual(0);
  state.paletteIndex = index; state.onKey(key("return")); expect(state.paletteOpen).toBe(true);
  state.paletteOpen = false; await state.close();
});

test("palette autostart toggle does not start a conflicting screen read", async () => {
  const { state, backend } = fixture();
  state.autostart = { gateway: false, connect: false };
  let finish!: (r: Reply) => void;
  backend.hold = { action: "autostart/read", promise: new Promise(resolve => { finish = resolve; }) };
  try {
    state.paletteOpen = true; state.paletteQuery = "";
    state.paletteIndex = state.filteredPaletteActions().findIndex(entry => entry.action.id === "autostart:g");
    expect(state.paletteIndex).toBeGreaterThanOrEqual(0);
    state.runPaletteAction(); await settle();
    expect(backend.calls.some(call => call.action === "autostart/set")).toBe(true);
    expect(state.notice).not.toContain("конфликт");
  } finally { finish(reply()); await settle(); await state.close(); }
});

test("palette excludes finished task history", async () => {
  const { state } = fixture();
  try {
    state.history.push({ action: "diagnostics", started: Date.now(), ended: Date.now(), phase: "", outcome: "Завершено" });
    state.paletteQuery = "завершено";
    expect(state.filteredPaletteActions()).toHaveLength(0);
  } finally { await state.close(); }
});
