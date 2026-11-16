import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { FakeBackend, initial, reply, rows, settle } from "./app-test-fixtures";
import { detailsLines } from "./details-view";
import { compatible, queueable, requestClasses } from "./request-policy";
import { serverMenuActions } from "./action-registry";

function fixture() {
  const backend = new FakeBackend(), state = new AppState(backend);
  state.screen = "servers"; state.tab = "servers"; state.status = { ...initial };
  state.servers = structuredClone(backend.rows); state.serverID = rows[0]!.server_id; state.catalogLoaded = true;
  const key = (name: string) => state.onKey({ name, preventDefault() {} });
  return { state, backend, key };
}
function openRename(state: AppState, key: (name: string) => void) {
  key("return"); key("end"); key("return");
  expect(state.screen).toBe("server-rename");
}

test("country sort in both directions keeps unknown last and name ties stable", async () => {
  const { state } = fixture();
  state.servers = [
    { ...rows[0]!, server_id: "c", display_name: "Zulu", country: "NL" },
    { ...rows[0]!, server_id: "b", display_name: "Alpha", country: "NL" },
    { ...rows[0]!, server_id: "a", display_name: "Beta", country: "DE" },
    { ...rows[0]!, server_id: "d", display_name: "Unknown", country: undefined },
  ];
  state.serverID = "c"; state.setSort("country");
  expect(state.sortedServers().map(s => s.server_id)).toEqual(["a", "b", "c", "d"]);
  state.setSort("country"); expect(state.sortedServers().map(s => s.server_id)).toEqual(["b", "c", "a", "d"]); expect(state.serverID).toBe("c");
  state.sort = "availability"; state.onKey({ name: "o", preventDefault() {} }); expect(String(state.sort)).toBe("country");
  state.onKey({ name: "o", preventDefault() {} }); expect(String(state.sort)).toBe("name");
  await state.close();
});

test("filter matches country and subscription name and details explain both", async () => {
  const { state } = fixture();
  state.servers[0]!.original_name = "Original Tokyo"; state.servers[0]!.display_name = "Custom";
  state.editFilter("jP"); expect(state.filteredServers().map(s => s.display_name)).toEqual(["Custom"]);
  state.editFilter("ORIGINAL toK"); expect(state.filteredServers().map(s => s.display_name)).toEqual(["Custom"]);
  expect(detailsLines(state)).toContain("Страна: JP"); expect(detailsLines(state)).toContain("Имя в подписке: Original Tokyo");
  state.servers[0]!.country = undefined; expect(detailsLines(state)).toContain("Страна: неизвестна"); await state.close();
});

test("rename uses captured row, preserves id and session data, then resets", async () => {
  const { state, backend, key } = fixture(); const id = rows[0]!.server_id;
  state.homeSpeedServer = { id, name: rows[0]!.display_name };
  state.measurementTimes.set(id, 42); state.speedSamples.set(id, [7]); state.liveSpeeds.set(id, 9); state.sessionMeasurements.set(id, [{ at: 1, site: "ready" }]);
  key("return"); state.serverID = rows[1]!.server_id; key("end"); key("return");
  expect(state.editorValue).toBe(rows[0]!.display_name); expect(state.renameTarget?.original).toBe(rows[0]!.display_name);
  state.edit("Custom"); key("return"); await settle();
  expect(backend.calls.at(-1)).toEqual({ action: "servers/rename", value: JSON.stringify({ id, name: "Custom" }) });
  expect(state.serverID).toBe(id); expect(state.servers.find(s => s.server_id === id)?.display_name).toBe("Custom");
  expect(state.servers.find(s => s.server_id === id)?.original_name).toBe(rows[0]!.display_name); expect(state.notice).toContain("сохранено");
  expect(state.homeSpeedServer?.name).toBe("Custom");
  expect(state.measurementTimes.get(id)).toBe(42); expect(state.speedSamples.get(id)).toEqual([7]); expect(state.liveSpeeds.get(id)).toBe(9); expect(state.sessionMeasurements.get(id)).toHaveLength(1);
  openRename(state, key); expect(state.editorValue).toBe("Custom"); expect(state.renameTarget?.original).toBe(rows[0]!.display_name);
  state.edit(""); key("return"); await settle(); expect(state.servers.find(s => s.server_id === id)?.display_name).toBe(rows[0]!.display_name); expect(state.servers.find(s => s.server_id === id)?.original_name).toBeUndefined();
  await state.close();
});

for (const [reason, message] of [["invalid-name", "Недопустимое имя"], ["too-many-names", "предел"], ["server-names-unavailable", "Не удалось сохранить имена"], ["invalid-request", "Не удалось"]] as const) test(`rename failure ${reason} retains input`, async () => {
  const { state, backend, key } = fixture();
  backend.hold = { action: "servers/rename", promise: Promise.resolve({ ...reply(), ok: false, reason }) };
  openRename(state, key); state.edit("Typed name"); key("return"); await settle();
  expect(state.screen).toBe("server-rename"); expect(state.editorValue).toBe("Typed name"); expect(state.notice).toContain(message); await state.close();
});

test("escape, stale server and incompatible operation cannot submit or queue rename", async () => {
  const { state, backend, key } = fixture();
  openRename(state, key); state.edit("discard"); key("escape"); expect(state.screen).toBe("servers"); expect(backend.calls).toHaveLength(0);
  openRename(state, key); state.updateCatalog({ ...reply(), catalog: { status: "ok", servers: state.servers.slice(1) } }, "servers/list"); key("return"); await settle(); expect(state.screen).toBe("servers"); expect(state.notice).toContain("больше не найден"); expect(backend.calls).toHaveLength(0);
  state.servers = structuredClone(rows); state.serverID = rows[0]!.server_id; openRename(state, key);
  state.edit("x".repeat(80)); expect([...state.editorValue]).toHaveLength(60);
  state.tasks.set("busy", { action: "start", measurement: false, started: 0, phase: "" }); key("return"); await settle();
  expect(state.screen).toBe("server-rename"); expect(state.notice).toContain("конфликтует"); expect(state.queue).toHaveLength(0); expect(backend.calls).toHaveLength(0);
  state.tasks.clear(); state.drainQueue(); expect(backend.calls).toHaveLength(0); await state.close();
});

test("rename is shared like a cached read but cannot be queued", () => {
  expect(requestClasses["servers/rename"]).toBe("read");
  expect(compatible("servers/rename", "servers/ping")).toBe(true); expect(compatible("servers/rename", "start")).toBe(false);
  expect(queueable.has("servers/rename")).toBe(false);
});

test("help registry includes the row rename action", async () => {
  const { state, key } = fixture(); key("return");
  expect(serverMenuActions(state).map(a => a.label)).toContain("Переименовать");
  expect(state.groupedActions().flatMap(g => g.actions).map(a => a.id)).toContain("servers:rename"); await state.close();
});
