import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { FakeBackend, reply, rows, settle } from "./app-test-fixtures";
import type { Reply } from "./bridge";

const key = (state: AppState, name: string) => state.onKey({ name, preventDefault() {} });

test("queued work is ordered, deduplicated and cancellable; mutations never enter it", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  try {
    await state.perform("status"); await state.perform("servers/list");
    backend.hold = { action: "start", promise: new Promise(resolve => { finish = resolve; }) };
    const active = state.perform("start");
    await state.perform("diagnostics"); await state.perform("diagnostics");
    await state.perform("servers/refresh");
    await state.perform("stop"); await state.perform("servers/select", rows[0]!.server_id);
    expect(state.queue.map(item => item.action)).toEqual(["diagnostics", "servers/refresh"]);
    state.cancelTask(state.queue[1]!.id);
    expect(state.queue.map(item => item.action)).toEqual(["diagnostics"]);
    expect(state.history[0]?.outcome).toBe("Отменено в очереди");
    finish(reply()); await active; await settle();
    expect(backend.calls.filter(call => ["diagnostics", "servers/refresh", "stop", "servers/select"].includes(call.action)).map(call => call.action)).toEqual(["diagnostics"]);
  } finally { finish(reply()); await settle(); await state.close(); }
});

test("queued batch is dropped when its catalog or service precondition changes", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  try {
    await state.perform("status"); await state.perform("servers/list");
    backend.hold = { action: "servers/speed-batch", promise: new Promise(resolve => { finish = resolve; }) };
    const batch = state.runBatch("speed"); await settle();
    await state.runBatch("ping"); await state.runBatch("ping");
    expect(state.queue.map(item => item.action)).toEqual(["servers/check-batch"]);
    state.servers.pop();
    finish(reply()); await batch; await settle();
    expect(backend.calls.some(call => call.action === "servers/check-batch")).toBe(false);
    expect(state.notice).toContain("каталог изменились");
  } finally { finish(reply()); await settle(); await state.close(); }
});

test("cancellation remains requested until the target reply and history is bounded", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  try {
    await state.perform("status");
    backend.hold = { action: "servers/ping", promise: new Promise(resolve => { finish = resolve; }) };
    const work = state.perform("servers/ping", rows[0]!.server_id);
    const id = [...state.tasks.keys()][0]!;
    key(state, "k"); expect(state.tasks.get(id)?.cancelRequested).toBe(true);
    expect(state.history.some(item => item.action === "servers/ping")).toBe(false);
    finish({ ...reply(), ok: false, reason: "cancelled" }); await work;
    expect(state.tasks.has(id)).toBe(false); expect(state.history[0]?.outcome).toBe("Отменено");
    backend.hold = undefined;
    for (let i = 0; i < 25; i++) await state.perform("diagnostics");
    expect(state.history).toHaveLength(20);
  } finally { finish(reply()); await state.close(); }
});

test("queued exclusive reads start in FIFO order after the blocking request", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finishStart!: (result: Reply) => void, finishDiagnostics!: (result: Reply) => void;
  const original = backend.request.bind(backend);
  backend.request = (action, value) => {
    if (action === "start") { backend.calls.push({ action, value }); return new Promise(resolve => { finishStart = resolve; }); }
    if (action === "diagnostics") { backend.calls.push({ action, value }); return new Promise(resolve => { finishDiagnostics = resolve; }); }
    return original(action, value);
  };
  try {
    await state.perform("status"); await state.perform("servers/list");
    const start = state.perform("start");
    await state.perform("diagnostics"); await state.perform("servers/refresh");
    finishStart(reply()); await start; await settle();
    expect(backend.calls.at(-1)?.action).toBe("diagnostics");
    expect(backend.calls.some(call => call.action === "servers/refresh")).toBe(false);
    finishDiagnostics(reply()); await settle();
    expect(backend.calls.at(-1)?.action).toBe("servers/refresh");
  } finally { finishStart(reply()); finishDiagnostics?.(reply()); await settle(); await state.close(); }
});

test("cancelled connection mutation rereads status and catalog without replaying it", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  try {
    await state.perform("status"); await state.perform("servers/list");
    backend.hold = { action: "servers/select", promise: new Promise(resolve => { finish = resolve; }) };
    const work = state.perform("servers/select", rows[0]!.server_id);
    const id = [...state.tasks.keys()][0]!;
    state.cancelTask(id);
    finish({ ...reply(), ok: false, reason: "cancelled" }); await work;
    expect(backend.calls.slice(-3).map(call => call.action)).toEqual(["servers/select", "status", "servers/list"]);
    expect(backend.calls.filter(call => call.action === "servers/select")).toHaveLength(1);
  } finally { finish(reply()); await state.close(); }
});

test("failed speed replies containing cached values do not count as a session measurement", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  try {
    await state.perform("status"); await state.perform("servers/list");
    backend.hold = { action: "servers/speed", promise: Promise.resolve({ ...reply(), ok: false, reason: "stale", catalog: { status: "ok", server: { ...rows[1]!, status: "ready", download_mbps: 100 } } }) };
    await state.perform("servers/speed", rows[1]!.server_id);
    state.confirmFastest();
    expect(state.notice).toContain("[t]");
    expect(state.measurementTimes.has(rows[1]!.server_id)).toBe(false);
  } finally { await state.close(); }
});

test("queued measurement is dropped if service stops, but status can still be read", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  try {
    await state.perform("status"); await state.perform("servers/list");
    backend.hold = { action: "stop", promise: new Promise(resolve => { finish = resolve; }) };
    const stop = state.perform("stop");
    await state.perform("servers/ping", rows[0]!.server_id); await state.perform("status");
    backend.status.gateway_state = "stopped";
    finish(reply(backend.status)); await stop; await settle();
    expect(backend.calls.some(call => call.action === "servers/ping")).toBe(false);
    expect(backend.calls.filter(call => call.action === "status")).toHaveLength(2);
    expect(state.history.some(item => item.outcome.includes("условия изменились"))).toBe(true);
  } finally { finish(reply()); await state.close(); }
});

test("cancelled catalog refresh invalidates queued snapshots and rereads cached state", async () => {
  const backend = new FakeBackend(); const state = new AppState(backend);
  let finish!: (result: Reply) => void;
  try {
    await state.perform("status"); await state.perform("servers/list");
    backend.hold = { action: "servers/refresh", promise: new Promise(resolve => { finish = resolve; }) };
    const refresh = state.perform("servers/refresh");
    await state.perform("servers/ping", rows[0]!.server_id);
    state.cancelTask([...state.tasks.keys()][0]!);
    finish({ ...reply(), ok: false, reason: "cancelled" }); await refresh; await settle();
    expect(backend.calls.filter(call => call.action === "servers/refresh")).toHaveLength(1);
    expect(backend.calls.some(call => call.action === "servers/ping")).toBe(false);
    expect(backend.calls.filter(call => call.action === "servers/list")).toHaveLength(2);
    expect(state.history.some(item => item.outcome.includes("условия изменились"))).toBe(true);
  } finally { finish(reply()); await state.close(); }
});
