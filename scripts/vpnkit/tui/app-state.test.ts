import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { FakeBackend, reply, rows, settle } from "./app-test-fixtures";
import type { Reply } from "./bridge";

const key = (state: AppState, name: string) => state.onKey({ name, preventDefault() {} });

test("renderer-free state navigates while a mutation is held and confirms disconnect", async () => {
  const backend = new FakeBackend();
  backend.status.networkmanager_active = "yes";
  const state = new AppState(backend);
  let finish!: (reply: Reply) => void;
  try {
    await state.perform("status");
    backend.hold = { action: "start", promise: new Promise(resolve => { finish = resolve; }) };
    const work = state.perform("start");
    key(state, "d");
    expect(state.screen).toBe("diagnostics");
    key(state, "h");
    key(state, "x");
    expect(state.screen).toBe("confirm");
    expect(state.confirmation).toBe("disconnect");
    key(state, "return");
    expect(state.screen).toBe("servers");
    expect(backend.calls.filter(call => call.action === "disconnect")).toHaveLength(0);
    finish(reply(backend.status)); await work;
  } finally { await state.close(); }
});

test("renderer-free requests queue after status and assign request IDs", async () => {
  const backend = new FakeBackend();
  const ids: string[] = [];
  const request = backend.request.bind(backend);
  let finish!: (reply: Reply) => void;
  backend.hold = { action: "status", promise: new Promise(resolve => { finish = resolve; }) };
  const state = new AppState({
    request(action, value, id) { if (id) ids.push(id); return request(action, value); },
    close: () => backend.close(),
  });
  try {
    const status = state.perform("status");
    await state.perform("start");
    expect(backend.calls.map(call => call.action)).toEqual(["status"]);
    finish(reply()); await status;
    expect(backend.calls.map(call => call.action)).toEqual(["status", "start"]);
    expect(ids).toEqual(["ui-1", "ui-2"]);
  } finally { await state.close(); }
});

test("renderer-free deferred subscription read preserves a newer editor value", async () => {
  const backend = new FakeBackend();
  let finish!: (reply: Reply) => void;
  backend.hold = { action: "status", promise: new Promise(resolve => { finish = resolve; }) };
  const state = new AppState(backend);
  try {
    const status = state.perform("status");
    key(state, "c");
    state.edit("https://example.invalid/typed");
    finish(reply()); await status; await settle();
    expect(backend.calls.map(call => call.action)).toEqual(["status"]);
    expect(state.editorValue).toBe("https://example.invalid/typed");
    expect(state.draft).toBe("https://example.invalid/typed");
  } finally { await state.close(); }
});

test("renderer-free subscribers receive scoped updates and can unsubscribe", async () => {
  const backend = new FakeBackend();
  const state = new AppState(backend);
  const regions: string[] = [];
  const unsubscribe = state.subscribe(region => regions.push(region));
  try {
    state.edit("value");
    expect(regions).toEqual(["editor"]);
    regions.length = 0;
    const work = state.perform("start");
    backend.onProgress?.("runtime-wait");
    expect(regions).toContain("progress");
    await work;
    unsubscribe();
    regions.length = 0;
    state.edit("other");
    expect(regions).toEqual([]);
  } finally { await state.close(); }
});

test("action definitions expose identity, availability and the same keyboard path", async () => {
  const backend = new FakeBackend();
  const state = new AppState(backend);
  try {
    state.servers = structuredClone(rows);
    state.serverID = rows[1]!.server_id;
    const speed = state.actions().find(action => action.id === "servers:single-speed")!;
    expect(speed.key).toBe("shift+t");
    expect(speed.keys).toEqual(["shift+t"]);
    expect(speed.scope).toBe("server-table");
    expect(speed.available).toBe(false);
    expect(speed.unavailableReason).toContain("Docker");
    key(state, "T");
    expect(state.notice).toBe(speed.unavailableReason!);
    await state.perform("status");
    expect(state.actions().find(action => action.id === "servers:single-speed")?.available).toBe(true);
    key(state, "v");
    expect(state.screen).toBe("servers");
    expect(state.actions().map(action => action.id)).toContain("servers:p");
  } finally { await state.close(); }
});
