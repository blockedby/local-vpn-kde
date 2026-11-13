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

test("renderer-free mutations are refused during status and never replayed", async () => {
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
    expect(backend.calls.map(call => call.action)).toEqual(["status"]);
    expect(ids).toEqual(["ui-1"]);
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

test("polling overlaps measurements, avoids duplicate polls and resumes after exclusive work", async () => {
  const backend = new FakeBackend();
  const original = backend.request.bind(backend);
  let finishBatch!: (reply: Reply) => void, finishStatus!: (reply: Reply) => void, finishExclusive!: (reply: Reply) => void;
  const batch = new Promise<Reply>(resolve => { finishBatch = resolve; });
  const status = new Promise<Reply>(resolve => { finishStatus = resolve; });
  const exclusive = new Promise<Reply>(resolve => { finishExclusive = resolve; });
  let holdStatus = false;
  backend.request = (action, value) => {
    if (action === "servers/check-batch") { backend.calls.push({ action, value }); return batch; }
    if (action === "diagnostics") { backend.calls.push({ action, value }); return exclusive; }
    if (action === "status" && holdStatus) { backend.calls.push({ action, value }); return status; }
    return original(action, value);
  };
  const state = new AppState(backend);
  try {
    await state.perform("status");
    const work = state.perform("servers/check-batch", "{}", true);
    holdStatus = true;
    state.pollStatus(); state.pollStatus();
    expect(backend.calls.filter(call => call.action === "status")).toHaveLength(2);
    finishStatus(reply()); await settle();
    finishBatch(reply()); await work;
    holdStatus = false;
    const busy = state.perform("diagnostics");
    state.poll = setInterval(() => {}, 60000);
    state.pollStatus();
    expect(backend.calls.filter(call => call.action === "status")).toHaveLength(3);
    finishExclusive(reply()); await busy; await settle();
    expect(backend.calls.filter(call => call.action === "status")).toHaveLength(4);
  } finally { await state.close(); }
});

test("fastest confirmation uses session results, rejects unreachable sites and retains the named choice", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  try {
    await state.perform("status");
    state.servers = [
      { ...rows[0]!, status: "ready", download_mbps: 80, availability: "failed" },
      { ...rows[1]!, status: "ready", download_mbps: 50, availability: "ready" },
      { ...rows[0]!, server_id: "srv_z", display_name: "Other", status: "ready", download_mbps: 50, availability: "untested" },
    ];
    state.catalogLoaded = true;
    key(state, "b"); expect(state.notice).toContain("[t]");
    for (const server of state.servers) state.measurementTimes.set(server.server_id, Date.now());
    key(state, "b");
    expect(state.fastestCandidate?.id).toBe(rows[1]!.server_id);
    expect(state.selected).toBe(0);
    key(state, "return");
    expect(backend.calls.some(call => call.action === "servers/select")).toBe(false);
    state.servers[1]!.selected = true;
    key(state, "b"); expect(state.notice).toContain("уже выбран");
    state.servers[1]!.selected = false;
    key(state, "b");
    // A late batch reply must not change the identity already shown to the user.
    state.updateCatalog({ ...reply(), catalog: { status: "ok", servers: [{ ...state.servers[2]!, download_mbps: 100 }] } }, "servers/speed-batch");
    key(state, "y"); await settle();
    expect(backend.calls.filter(call => call.action === "servers/select").at(-1)?.value).toBe(rows[1]!.server_id);
  } finally { await state.close(); }
});

test("repeated status reads coalesce into one follow-up", async () => {
  const backend = new FakeBackend();
  const pending: ((value: Reply) => void)[] = [];
  backend.request = (action, value) => {
    backend.calls.push({ action, value });
    return new Promise(resolve => pending.push(resolve));
  };
  const state = new AppState(backend);
  try {
    const first = state.perform("status");
    const rest = Array.from({ length: 25 }, () => state.perform("status"));
    expect(backend.calls).toHaveLength(1);
    pending.shift()!(reply()); await first; await settle();
    expect(backend.calls).toHaveLength(2);
    pending.shift()!(reply()); await Promise.all(rest);
    expect(backend.calls).toHaveLength(2);
  } finally { await state.close(); }
});

test("profile removal rejects a changed active profile while confirmation is open", async () => {
  const backend = new FakeBackend();
  let finish!: (reply: Reply) => void;
  backend.hold = { action: "profiles/list", promise: new Promise(resolve => { finish = resolve; }) };
  const items = [
    { id: "a", name: "Alpha", kind: "server" as const, count: 1, stale: false },
    { id: "b", name: "Beta", kind: "server" as const, count: 1, stale: false },
  ];
  const state = new AppState(backend);
  try {
    state.profiles = { active: "a", items };
    state.open("profiles");
    state.items().find(item => item.key === "X")!.run();
    finish({ ...reply(), profiles: { active: "b", items } }); await settle();
    state.items().find(item => item.key === "y")!.run(); await settle();
    expect(backend.calls.some(call => call.action === "profiles/remove")).toBe(false);
    expect(state.notice).toContain("изменил");
  } finally { await state.close(); }
});

test("late profile open leaves a newer confirmation in place", async () => {
  const backend = new FakeBackend();
  let finish!: (reply: Reply) => void;
  backend.hold = { action: "profiles/open", promise: new Promise(resolve => { finish = resolve; }) };
  const state = new AppState(backend);
  try {
    const work = state.browseProfile("a");
    state.confirm("disconnect");
    finish(reply()); await work;
    expect(state.screen).toBe("confirm");
    expect(state.confirmation).toBe("disconnect");
  } finally { await state.close(); }
});

test("site check address hides query and fragment outside its editor", async () => {
  const { detailsLines } = await import("./details-view");
  const state = new AppState(new FakeBackend());
  try {
    state.target = "https://example.invalid/path?token=private-marker#fragment-marker";
    state.servers = [{ ...rows[0]!, availability: "ready" }]; state.serverID = rows[0]!.server_id;
    state.open("settings", false);
    const shown = [...detailsLines(state), ...state.items().map(item => item.label), ...state.paletteActions().map(entry => entry.action.label)].join("\n");
    expect(shown).toContain("https://example.invalid/path");
    expect(shown).not.toContain("private-marker");
    expect(shown).not.toContain("fragment-marker");
    state.open("target"); expect(state.editorValue).toContain("private-marker");
  } finally { await state.close(); }
});

test("disconnect confirmation refuses a connection that changed while open", async () => {
  const backend = new FakeBackend(); backend.status.networkmanager_active = "yes";
  const state = new AppState(backend);
  try {
    await state.perform("status"); state.confirm("disconnect");
    state.status!.networkmanager_active = "no";
    key(state, "y"); await settle();
    expect(backend.calls.filter(call => call.action === "disconnect")).toHaveLength(0);
    expect(state.notice).toContain("изменил");
  } finally { await state.close(); }
});

test("poll during an in-flight status schedules one fresh follow-up", async () => {
  const backend = new FakeBackend();
  const pending: ((value: Reply) => void)[] = [];
  backend.request = (action, value) => { backend.calls.push({ action, value }); return new Promise(resolve => pending.push(resolve)); };
  const state = new AppState(backend);
  try {
    const first = state.perform("status");
    state.pollStatus(); state.pollStatus();
    expect(backend.calls).toHaveLength(1);
    pending.shift()!(reply()); await first; await settle();
    expect(backend.calls).toHaveLength(2);
  } finally { pending.shift()?.(reply()); await settle(); await state.close(); }
});
