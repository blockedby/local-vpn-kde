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

test("opening a profile loads its servers while the automatic status refresh is in flight", async () => {
  const backend = new FakeBackend();
  let finish!: (reply: Reply) => void;
  backend.hold = { action: "status", promise: new Promise(resolve => { finish = resolve; }) };
  const state = new AppState(backend);
  state.status = { ...backend.status };
  state.poll = setInterval(() => {}, 60_000);
  try {
    await state.browseProfile("a");
    await settle();
    expect(backend.calls.map(call => call.action)).toContain("servers/list");
    expect(state.catalogLoaded).toBe(true);
  } finally {
    finish(reply());
    await settle();
    await state.close();
  }
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

test("details show live samples and bounded per-server session measurements", async () => {
  const { detailsLines } = await import("./details-view");
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  backend.hold = { action: "servers/speed", promise: new Promise(resolve => { finish = resolve; }) };
  try {
    state.status = { ...backend.status }; state.servers = structuredClone(rows); state.serverID = rows[0]!.server_id;
    const work = state.perform("servers/speed", rows[0]!.server_id, true);
    const taskID = [...state.tasks.keys()][0]!;
    backend.onSpeedProgress?.({ event: "speed-progress", server_id: rows[0]!.server_id, downloaded_bytes: 100, elapsed_seconds: 1, download_mbps: 12 }, taskID);
    backend.onSpeedProgress?.({ event: "speed-progress", server_id: rows[0]!.server_id, downloaded_bytes: 200, elapsed_seconds: 2, download_mbps: 24 }, taskID);
    expect(detailsLines(state).join("\n")).toMatch(/[▁▂▃▄▅▆▇█]{2}/);
    finish({ ...reply(), catalog: { status: "ok", server: { ...rows[0]!, status: "ready", ping_status: "ready", latency_ms: 12, availability: "ready", download_mbps: 24 } } });
    await work;
    expect(detailsLines(state).join("\n")).toContain("Замеры этой сессии");
  } finally { await state.close(); }
});

test("cached reads are bounded and a newer status survives an older reply", async () => {
  for (const action of ["status", "servers/list", "subscription/read"] as const) {
    const backend = new FakeBackend(); const pending: ((value: Reply) => void)[] = [];
    backend.request = (name, value) => { backend.calls.push({ action: name, value }); return new Promise(resolve => pending.push(resolve)); };
    const state = new AppState(backend);
    try {
      const first = state.perform(action);
      const rest = Array.from({ length: 20 }, () => state.perform(action));
      expect(backend.calls).toHaveLength(1);
      pending.shift()!(reply()); await first; await settle();
      expect(backend.calls).toHaveLength(2);
      const newer = reply({ ...backend.status, networkmanager_active: "yes" });
      pending.shift()!(newer); await Promise.all(rest);
      expect(backend.calls).toHaveLength(2);
      if (action === "status") expect(state.status?.networkmanager_active).toBe("yes");
    } finally { await state.close(); }
  }
  const backend = new FakeBackend(); let finish!: (value: Reply) => void;
  backend.hold = { action: "status", promise: new Promise(resolve => { finish = resolve; }) };
  const state = new AppState(backend);
  try {
    const old = state.perform("status");
    state.status = { ...backend.status, networkmanager_active: "yes" }; state.statusRevision = state.nextTask + 1;
    finish(reply({ ...backend.status, networkmanager_active: "no" })); await old;
    expect(state.status.networkmanager_active).toBe("yes");
  } finally { await state.close(); }
});

test("session measurement history stays bounded and scoped to its server", async () => {
  const state = new AppState(new FakeBackend());
  try {
    for (let i = 0; i < 12; i++) state.recordMeasurement({ ...rows[0]!, status: "ready", download_mbps: i });
    state.recordMeasurement({ ...rows[1]!, status: "ready", download_mbps: 99 });
    expect(state.sessionMeasurements.get(rows[0]!.server_id)?.map(entry => entry.speed)).toEqual([11, 10, 9, 8]);
    expect(state.sessionMeasurements.get(rows[1]!.server_id)?.map(entry => entry.speed)).toEqual([99]);
  } finally { await state.close(); }
});

test("secret sources never appear in new details, palette, notices or history", async () => {
  const { detailsLines } = await import("./details-view");
  const backend = new FakeBackend(); const state = new AppState(backend);
  const source = "hy2://private-source-marker@example.invalid#server";
  backend.request = async (action, value) => { backend.calls.push({ action, value }); return { ...reply(), ok: false, reason: source }; };
  try {
    state.status = { ...backend.status }; state.servers = [{ ...rows[0]!, availability: "ready" }]; state.serverID = rows[0]!.server_id;
    state.target = "https://example.invalid/path?token=private-query-marker#fragment";
    await state.perform("profiles/add", JSON.stringify({ name: "Profile", source }));
    const visible = [state.notice, state.noticeAttempt, ...state.history.map(entry => entry.outcome), ...detailsLines(state), ...state.paletteActions().map(entry => entry.action.label)].join("\n");
    expect(visible).not.toContain("private-source-marker");
    expect(visible).not.toContain("private-query-marker");
  } finally { await state.close(); }
});

test("retry confirmation rejects a changed connection target", async () => {
  const { recoveryFor } = await import("./recovery");
  const backend = new FakeBackend(); const state = new AppState(backend);
  try {
    state.status = { ...backend.status, networkmanager_active: "no" };
    state.recovery = { ...recoveryFor("start", "timeout"), action: "start" };
    state.open("recovery"); state.items().find(item => item.key === "r")!.run();
    state.status.networkmanager_active = "yes";
    key(state, "y"); await settle();
    expect(backend.calls.some(call => call.action === "start")).toBe(false);
    expect(state.notice).toContain("изменил");
  } finally { await state.close(); }
});

test("retry confirmation refuses a profile whose active identity changed", async () => {
  const { recoveryFor } = await import("./recovery");
  const backend = new FakeBackend(); const state = new AppState(backend);
  const items = ["a", "b"].map(id => ({ id, name: id, kind: "server" as const, count: 1, stale: false }));
  try {
    state.status = { ...backend.status }; state.profiles = { active: "a", items };
    state.recovery = { ...recoveryFor("profiles/remove", "timeout"), action: "profiles/remove", value: "a" };
    state.open("recovery"); state.items().find(item => item.key === "r")!.run();
    state.profiles.active = "b";
    key(state, "y"); await settle();
    expect(backend.calls.some(call => call.action === "profiles/remove")).toBe(false);
    expect(state.notice).toContain("изменил");
  } finally { await state.close(); }
});

test("retrying connection captures and names its selected server", async () => {
  const { recoveryFor } = await import("./recovery");
  const backend = new FakeBackend(); const state = new AppState(backend);
  try {
    state.status = { ...backend.status }; state.servers = [{ ...rows[0]!, selected: true }];
    state.recovery = { ...recoveryFor("start", "timeout"), action: "start" };
    state.open("recovery"); state.items().find(item => item.key === "r")!.run();
    expect(state.retryTarget?.name).toBe(rows[0]!.display_name);
    state.servers[0]!.display_name = "Changed";
    key(state, "y"); await settle();
    expect(backend.calls.some(call => call.action === "start")).toBe(false);
  } finally { await state.close(); }
});
