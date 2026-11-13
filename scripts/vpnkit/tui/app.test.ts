import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { Renderable, TextRenderable } from "@opentui/core";
import { App } from "./app";
import { AppState } from "./app-state";
import {
  Bridge,
  type Action,
  type Backend,
  type Reply,
  type Status,
  type Server,
  type CheckProgress,
} from "./bridge";
import { initial, rows, reply, FakeBackend, settle } from "./app-test-fixtures";

// Resolve the current action's rendered hit target without relying on its wording or row.
function actionTarget(app: App, root: Renderable, id: string) {
  const action = app.actions().find(item => item.id === id);
  if (!action) throw new Error(`Missing action: ${id}`);
  const visit = (node: Renderable): TextRenderable | undefined => {
    if (node instanceof TextRenderable && node.chunks.map(chunk => chunk.text).join("").includes(action.label)) {
      const parent = node.parent as unknown as { _mouseListeners?: { down?: unknown } } | null;
      if (parent?._mouseListeners?.down) return node;
    }
    for (const child of node.getChildren()) {
      const found = visit(child);
      if (found) return found;
    }
  };
  const target = visit(root);
  if (!target) throw new Error(`Missing rendered action: ${id}`);
  return { x: target.screenX, y: target.screenY };
}

test("connection requires healthy Docker and active KDE", async () => {
  const backend = new FakeBackend();
  const state = new AppState(backend);
  try {
    backend.status.networkmanager_active = "yes";
    await state.perform("status");
    expect(state.ready()).toBe(true);
    expect(state.status?.networkmanager_active).toBe("yes");
    backend.status.networkmanager_active = "no";
    await state.perform("status");
    expect(state.status?.networkmanager_active).toBe("no");
    backend.status.gateway_state = "unhealthy";
    backend.status.networkmanager_active = "yes";
    await state.perform("status");
    expect(state.ready()).toBe(false);
    expect(state.status?.networkmanager_active).toBe("yes");
  } finally { await state.close(); }
});

test("native runtime shows the supported lifecycle preview and native service prompts", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", gateway_state: "absent" };
  const state = new AppState(b);
  const key = (name: string) => state.onKey({ name, preventDefault() {} });
  try {
    await state.perform("status");
    expect(state.nativePreviewOnly()).toBe(true);
    key("s");
    await settle();
    expect(b.calls.at(-1)?.action).toBe("backend/start");
    b.status.gateway_state = "healthy";
    await state.perform("status");
    key("z");
    expect(state.screen).toBe("confirm");
    expect(state.confirmation).toBe("stop");
  } finally { await state.close(); }
});

test("native mode guards unsupported actions before the backend request", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  const app = new AppState(b);
  try {
    await app.perform("status");
    const unsupported: [Action, string?][] = [
      ["servers/list"],
      ["servers/refresh"],
      ["servers/check-batch", JSON.stringify({ ids: [] })],
      ["servers/speed-batch", JSON.stringify({ ids: [], url: "https://example.invalid/" })],
      ["servers/ping", rows[0]!.server_id],
      ["servers/speed", rows[0]!.server_id],
      ["servers/availability", JSON.stringify({ id: rows[0]!.server_id })],
      ["servers/select", rows[0]!.server_id],
      ["retest/select"],
      ["toggle-mode"],
      ["subscription/read"],
      ["subscription", "https://example.invalid/new"],
      ["diagnostics"],
    ];
    for (const [action, value] of unsupported) await app.perform(action, value);
    for (const name of ["v", "c", "d"]) app.onKey({ name, preventDefault() {} });
    await settle();
    expect(b.calls.map((call) => call.action)).toEqual(["status"]);
    expect(app.nativePreviewOnly()).toBe(true);
    expect(app.error).toBe(true);
  } finally {
    await app.close();
  }
});

test("installed native subscription setup uses the private editor and explicit confirmation", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = {
    ...b.status,
    runtime: "native",
    installed: true,
    subscription: "not configured",
  };
  const app = new App(t.renderer, b);
  const value = "https://example.invalid/private-initial-subscription";
  try {
    await app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    await t.renderOnce();
    expect(app.screen).toBe("subscription");
    expect(app.subscriptionAvailable()).toBe(true);
    expect(b.calls.map((call) => call.action)).toEqual(["status", "subscription/read"]);

    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText(value);
    t.mockInput.pressEnter();
    await t.renderOnce();
    expect(app.screen).toBe("confirm");
    expect(app.confirmation).toBe("subscription");
    expect(app.pendingSubscription).toBe(value);
    expect(b.calls.map((call) => call.action)).toEqual(["status", "subscription/read"]);

    t.mockInput.pressKey("y");
    await settle();
    await t.renderOnce();
    expect(b.calls.at(-1)).toEqual({ action: "subscription", value });
    expect(b.calls.map((call) => call.action)).toEqual(["status", "subscription/read", "subscription"]);
    expect(b.calls.some((call) => ["start", "backend/start", "autostart/read", "autostart/set"].includes(call.action))).toBe(false);
    expect(app.noticeIdentity).toEqual({ action: "subscription", reason: "ok", ok: true });
    expect(app.notice).not.toContain(value);
    expect(app.noticeAttempt).not.toContain(value);
  } finally {
    await app.close();
  }
});

test("checkout native and native status without installed=true keep subscription setup disabled", async () => {
  for (const installed of [undefined, false] as const) {
    const b = new FakeBackend();
    b.status = { ...b.status, runtime: "native", installed };
    const state = new AppState(b);
    try {
      await state.perform("status");
      state.onKey({ name: "c", preventDefault() {} });
      await settle();
      expect(state.subscriptionAvailable()).toBe(false);
      expect(state.screen).toBe("servers");
      expect(state.error).toBe(true);
      expect(b.calls.map(call => call.action)).toEqual(["status"]);
    } finally { await state.close(); }
  }
});

test("native setup failures never echo the private editor value", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, subscription: "not configured" };
  const value = "https://example.invalid/private-failure-marker";
  const original = b.request.bind(b);
  b.request = async (action, requestValue) => {
    const result = await original(action, requestValue);
    if (action === "subscription")
      return { ...result, ok: false, reason: value, status: { ...result.status, last_attempt: value } };
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("c");
    await settle();
    app.edit(value);
    key("return");
    key("y");
    await settle();
    expect(app.error).toBe(true);
    expect(app.noticeIdentity).toEqual({ action: "subscription", reason: value, ok: false });
    expect(app.notice).not.toContain(value);
    expect(app.noticeAttempt).not.toContain(value);
    expect(app.recovery?.message).not.toContain(value);
    expect(b.calls.map((call) => call.action)).toEqual(["status", "subscription/read", "subscription"]);
  } finally {
    await app.close();
  }
});

test("native autostart edits retain the returned runtime and do not connect immediately", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  let options = {
    gateway: false,
    connect: false,
    runtime: "native" as const,
    last_result: "ready",
  };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/set") options = { ...options, ...JSON.parse(value!) };
    if (action === "autostart/read" || action === "autostart/set")
      result.autostart = { ...options };
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("a");
    await settle();
    expect(app.autostart).toMatchObject({ gateway: false, connect: false, runtime: "native" });
    key("g");
    await settle();
    const set = b.calls.find((call) => call.action === "autostart/set");
    expect(set).toBeDefined();
    expect(JSON.parse(set!.value!).runtime).toBe("native");
    expect(options).toMatchObject({ gateway: true, connect: false, runtime: "native" });
    expect(app.autostart).toMatchObject({ gateway: true, connect: false, runtime: "native" });
    expect(b.calls.some((call) => ["start", "backend/start", "disconnect", "stop"].includes(call.action))).toBe(false);
  } finally {
    await app.close();
  }
});

test("navigation and diagnostics Back are immediate during a held mutation", async () => {
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "start", promise: new Promise((r) => (finish = r)) };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    const task = app.perform("start");
    key("d");
    expect(app.screen).toBe("diagnostics");
    const before = performance.now();
    key("escape");
    await Bun.sleep(60);
    expect(performance.now() - before).toBeLessThan(150);
    expect(app.screen).toBe("servers");
    key("c");
    expect(app.screen).toBe("subscription");
    finish(reply());
    await task;
    expect(app.screen).toBe("subscription");
    expect(b.calls.map((c) => c.action)).toEqual(["status", "start", "subscription/read"]);
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("completed diagnostics never reopen a screen the user left", async () => {
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "diagnostics", promise: new Promise((r) => (finish = r)) };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({name, preventDefault() {}});
  try {
    await app.perform("status");
    key("d");
    const task = app.perform("diagnostics");
    key("escape");
    await Bun.sleep(60);
    finish(reply());
    await task;
    expect(app.screen).toBe("servers");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("subscription is visible and editable; paste never dispatches action hotkeys", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    t.mockInput.pressKey("c");
    await settle();
    await t.renderOnce();
    expect(app.editorValue).toBe("https://example.invalid/saved");
    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText("https://example.invalid/sxrm");
    await t.renderOnce();
    expect(app.editorValue).toBe("https://example.invalid/sxrm");
    expect(b.calls.map((c) => c.action)).toEqual(["subscription/read"]);
    t.mockInput.pressEnter();
    await settle();
    expect(b.calls.at(-1)).toEqual({
      action: "subscription",
      value: "https://example.invalid/sxrm",
    });
  } finally {
    await app.close();
  }
});
test("failed subscription save retains input and Back works while saving", async () => {
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "subscription",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    key("c");
    await settle();
    key("return");
    await settle();
    key("escape");
    await Bun.sleep(60);
    expect(app.screen).toBe("servers");
    finish({ ...reply(), ok: false, reason: "invalid-request" });
    await settle();
    key("c");
    expect(app.screen).toBe("subscription");
    expect(app.editorValue).toBe("https://example.invalid/saved");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("server table is sorted by name, keeps columns aligned and exposes combined batch actions", async () => {
  const b = new FakeBackend();
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    expect(app.sortedServers().map(server => server.server_id)).toEqual([rows[1]!.server_id, rows[0]!.server_id]);
    expect(app.actions().map(action => action.id)).toEqual(expect.arrayContaining(["servers:p", "servers:t"]));
    key("o");
    expect(app.sort).toBe("ping");
    key("down");
    key("return");
    await settle();
    expect(b.calls.at(-1)?.action).toBe("servers/select");
  } finally { await app.close(); }
});
test("batch publishes each result and allows navigation without cancelling", async () => {
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/check-batch",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    key("p");
    await settle();
    key("d");
    expect(app.screen).toBe("diagnostics");
    expect(app.batch?.total).toBe(2);
    expect(b.cancelled).toBe(0);
    b.hold = undefined;
    finish({
      ...reply(),
      catalog: {
        status: "ok",
        servers: rows.map((s) => ({
          ...s,
          latency_ms: 24,
          ping_status: "ready",
          availability: "ready",
        })),
      },
    });
    await settle();
    expect(app.batch).toBeUndefined();
    expect(app.servers.map(server => server.ping_status)).toEqual(["ready", "ready"]);
    expect(app.servers.map(server => server.availability)).toEqual(["ready", "ready"]);
    expect(
      b.calls.filter((c) => c.action === "servers/check-batch"),
    ).toHaveLength(1);
    expect(b.calls.some((c) => c.action === "servers/speed")).toBe(false);
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("Cancel stops the remaining batch and preserves its completed rows", async () => {
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/speed-batch",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    key("t");
    await settle();
    key("k");
    expect(b.cancelled).toBe(1);
    finish({ ...reply(), ok: false, reason: "cancelled" });
    await settle();
    expect(app.cancelled).toBe(true);
    expect(app.batch).toBeUndefined();
    expect(b.calls.filter((c) => c.action === "servers/speed-batch")).toHaveLength(1);
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("combined batch uses the configured URL without running the speed benchmark", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("e");
    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText("https://example.com/");
    t.mockInput.pressEnter();
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    const calls = b.calls.filter((c) => c.action === "servers/check-batch");
    expect(calls).toHaveLength(1);
    expect(JSON.parse(calls[0].value!).url).toBe("https://example.com/");
    expect(
      b.calls.some(
        (c) => c.action === "servers/speed" || c.action === "servers/ping",
      ),
    ).toBe(false);
  } finally {
    await app.close();
  }
});
test("connection changes during polling are refused and never replayed", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    const task = app.perform("status");
    t.mockInput.pressKey("s");
    t.mockInput.pressKey("s");
    finish(reply());
    await task;
    expect(b.calls.map((c) => c.action)).toEqual(["status"]);
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("mouse navigation works; non-primary clicks do not mutate", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("i");
    await t.renderOnce();
    const at = actionTarget(app, t.renderer.root, "home:m");
    await t.mockMouse.click(at.x, at.y, 2);
    expect(b.calls).toHaveLength(1);
    await t.mockMouse.click(at.x, at.y, 0);
    await settle();
    expect(b.calls.at(-1)?.action).toBe("toggle-mode");
  } finally {
    await app.close();
  }
});
test("quit cancels and drains in-flight work without destroying the renderer prematurely", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "start", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  await app.perform("status");
  const task = app.perform("start");
  await app.close();
  expect(b.cancelled).toBe(1);
  await app.close();
  expect(b.cancelled).toBe(1);
  await t.renderOnce();
  expect(app.closing).toBe(true);
  expect(app.disposed).toBe(false);
  finish(reply());
  await task;
  expect(app.disposed).toBe(true);
});
test("actual native Go bridge supports new actions and bounded catalog responses", async () => {
  const b = new Bridge(["--test"]);
  try {
    expect((await b.request("status")).status.vpn_state).toBe("unknown");
    expect((await b.request("backend/start")).ok).toBe(true);
    expect((await b.request("servers/list")).catalog?.servers).toEqual([]);
    expect((await b.request("servers/ping", "not-an-id")).ok).toBe(false);
  } finally {
    await b.close();
  }
});

test("cached errors do not masquerade as tests in the current session", async () => {
  const server = {
    ...rows[0],
    status: "failed",
    ping_status: "failed",
    download_mbps: undefined,
  };
  const app = new AppState({
    request: async (action) => ({
      ...reply(),
      catalog:
        action === "servers/list"
          ? { status: "ok", servers: [server] }
          : {
              status: "ok",
              server: { ...server, ping_status: "ready", latency_ms: 15 },
            },
    }),
    close: async () => {},
  });
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();

    expect(app.servers[0]).toMatchObject({ status: "untested", ping_status: "untested" });
    await app.perform("servers/ping", server.server_id);

    expect(app.servers[0]).toMatchObject({ status: "untested", ping_status: "ready", latency_ms: 15 });
    await app.perform("servers/speed", server.server_id);

    expect(app.servers[0]?.status).toBe("failed");
  } finally {
    await app.close();
  }
});

test("server actions are reachable with Tab and Enter, including before Docker startup", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    expect(
      b.calls.filter((c) => c.action === "servers/check-batch"),
    ).toHaveLength(1);
    expect(b.calls.some((c) => c.action === "servers/select")).toBe(false);
  } finally {
    await app.close();
  }
  const u = await createTestRenderer({ width: 80, height: 24 });
  const off = new FakeBackend();
  off.status.gateway_state = "absent";
  const other = new App(u.renderer, off);
  try {
    await other.perform("status");
    u.mockInput.pressKey("v");
    u.mockInput.pressEnter();
    await settle();
    expect(off.calls.some((c) => c.action === "backend/start")).toBe(true);
  } finally {
    await other.close();
  }
});

test("changing subscription invalidates old rows until an explicit catalog refresh", async () => {
  const b = new FakeBackend();
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    await app.perform("subscription", "https://example.invalid/new");
    expect(app.servers).toEqual([]);
    expect(app.catalogStale).toBe(true);
    key("v");
    await settle();
    expect(b.calls.some(c => c.action === "servers/refresh")).toBe(false);
    key("r"); await settle();
    expect(b.calls.at(-1)?.action).toBe("servers/refresh");
    expect(b.calls.some((c) => c.action === "servers/select")).toBe(false);
  } finally {
    await app.close();
  }
});

test("Docker readiness loads the list on an already-open servers screen", async () => {
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "backend/start",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    const task = app.perform("backend/start");
    key("v");
    finish(reply());
    await task;
    expect(b.calls.at(-1)?.action).toBe("servers/list");
    expect(app.servers.map(server => server.server_id)).toContain(rows[1]!.server_id);
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("a successful site check marks a server ready without claiming a speed result", async () => {
  const b = new FakeBackend();
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    key("p");
    await settle();

    expect(app.servers.find(server => server.server_id === rows[1]!.server_id)).toMatchObject({
      ping_status: "ready", availability: "ready", status: "untested", download_mbps: undefined,
    });
  } finally {
    await app.close();
  }
});

test("disconnect confirmation defaults to No and requires an explicit Yes", async () => {
  const b = new FakeBackend();
  b.status.networkmanager_active = "yes";
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("x");
    expect(app.screen).toBe("confirm");
    expect(app.confirmation).toBe("disconnect");
    expect(app.selected).toBe(0);
    expect(b.calls).toHaveLength(1);
    key("return");
    await settle();
    expect(b.calls).toHaveLength(1);
    key("x");
    key("escape");
    await Bun.sleep(60);
    expect(b.calls).toHaveLength(1);
    key("x");
    key("down");
    key("return");
    await settle();
    expect(b.calls.filter((c) => c.action === "disconnect")).toHaveLength(1);
    key("x");
    key("return");
    await settle();
    expect(b.calls.filter((c) => c.action === "disconnect")).toHaveLength(1);
  } finally {
    await app.close();
  }
});

test("stopping Docker also requires confirmation; mouse No does not disconnect", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("z");
    await t.renderOnce();
    expect(app.screen).toBe("confirm");
    expect(app.confirmation).toBe("stop");
    const no = actionTarget(app, t.renderer.root, "confirm:n");
    await t.mockMouse.click(no.x, no.y);
    expect(b.calls).toHaveLength(1);
    t.mockInput.pressKey("z");
    await t.renderOnce();
    const yes = actionTarget(app, t.renderer.root, "confirm:y");
    await t.mockMouse.click(yes.x, yes.y);
    await settle();
    expect(b.calls.at(-1)?.action).toBe("stop");
  } finally {
    await app.close();
  }
});

test("combined checks submit one snapshot once without restarting on navigation", async () => {
  const b = new FakeBackend();
  b.rows = Array.from({ length: 12 }, (_, i) => ({ ...rows[0], server_id: `srv_${String(i).padStart(27, "0")}` }));
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    key("p");
    await settle();
    const batches = b.calls.filter(c => c.action === "servers/check-batch").map(c => JSON.parse(c.value!).ids);
    expect(batches.map(ids => ids.length)).toEqual([12]);
    expect(new Set(batches.flat()).size).toBe(12);
    key("d");
    key("v");
    await settle();

    expect(b.calls.filter(c => c.action === "servers/check-batch")).toHaveLength(1);
    expect(app.batch).toBeUndefined();
  } finally { await app.close(); }
});

test("batch failure retains the actionable outdated backend explanation", async () => {
  const b = new FakeBackend();
  b.hold = { action: "servers/check-batch", promise: Promise.resolve({ ...reply(), ok: false, reason: "backend-outdated" }) };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    key("p");
    await settle();

    expect(app.error).toBe(true);
    expect(app.recovery).toMatchObject({ action: "servers/check-batch" });
  } finally { await app.close(); }
});


test("ping and site render before batch completion and completed rows stop spinning", async () => {
 const b = new FakeBackend();
 let finish!: (r: Reply) => void;
 b.hold = { action:"servers/check-batch", promise:new Promise(r => finish=r) };
 const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
 try {
  await app.perform("status");
  key("v"); await settle();
  key("p"); await settle();
  const event: CheckProgress = { event:"server-check", server_id:rows[0].server_id, stage:"ping", ping_status:"ready", latency_ms:37, availability:"untested" };
  b.onCheckProgress?.(event);

  expect(app.servers.find(s=>s.server_id===rows[0]!.server_id)).toMatchObject({latency_ms:37, ping_status:"ready", availability:"untested"});
  expect(app.batch).toMatchObject({done:0,total:2});
  b.onCheckProgress?.({...event,stage:"complete",availability:"ready"});
  b.onCheckProgress?.({...event,stage:"complete",availability:"ready"});
  expect(app.servers.find(s=>s.server_id===rows[0]!.server_id)).toMatchObject({latency_ms:37,availability:"ready"});
  expect(app.batch).toMatchObject({done:1,total:2});
  expect(app.batch?.running.has(rows[0]!.server_id)).toBe(false);
  key("d"); expect(app.screen).toBe("diagnostics");
  key("v"); expect(app.servers.find(s=>s.server_id===rows[0]!.server_id)?.latency_ms).toBe(37);
  finish({...reply(),catalog:{status:"ok",servers:rows.map(s=>({...s,ping_status:"ready",availability:"ready",latency_ms:37}))}});
  await settle();
  expect(app.batch).toBeUndefined();
  expect(app.servers.every(s=>s.ping_status==="ready" && s.availability==="ready")).toBe(true);
  expect(b.calls.filter(c=>c.action==="servers/check-batch")).toHaveLength(1);
 } finally { finish(reply()); await settle(); await app.close(); }
});

test("subscription opened during status loads on the first visit after status completes", async () => {
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    const task = app.perform("status");
    key("c");
    await settle();
    expect(b.calls.map(c => c.action)).toEqual(["status"]);
    finish(reply());
    await task;
    expect(b.calls.map(c => c.action)).toEqual(["status", "subscription/read"]);
    expect(app.editorValue).toBe("https://example.invalid/saved");
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("deferred subscription load does not replace text typed while busy", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    const task = app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    await t.mockInput.pasteBracketedText("https://example.invalid/new");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(app.editorValue).toBe("https://example.invalid/new");
    expect(b.calls.map(c => c.action)).toEqual(["status"]);
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("any listed server can be selected without successful measurements", async () => {
  for (const status of ["untested", "failed"] as const) {
    const t = await createTestRenderer({ width: 100, height: 24 });
    const b = new FakeBackend();
    b.rows = [{ ...rows[0]!, status, ping_status: "failed", availability: "failed" }];
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      t.mockInput.pressKey("v");
      await settle();
      t.mockInput.pressEnter();
      await settle();
      expect(b.calls.at(-1)).toEqual({ action: "servers/select", value: rows[0]!.server_id });
    } finally { await app.close(); }
  }
});

test("failed current ping overrides an earlier successful speed result", async () => {
  const b = new FakeBackend();
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v");
    await settle();
    await app.perform("servers/speed",rows[0]!.server_id);
    let finish!: (r: Reply) => void;
    b.hold = { action: "servers/check-batch", promise: new Promise(r => finish=r) };
    key("p");
    await settle();
    b.onCheckProgress?.({ event:"server-check",server_id:rows[0]!.server_id,stage:"complete",ping_status:"failed",latency_ms:0,availability:"untested" });

    expect(app.servers.find(s=>s.server_id===rows[0]!.server_id)).toMatchObject({download_mbps:50,ping_status:"failed"});
    finish(reply()); await settle();
  } finally { await app.close(); }
});

test("only assigned workers spin while remaining servers wait in the queue", async () => {
 const b = new FakeBackend();
 b.rows = Array.from({length:6},(_,i)=>({...rows[0]!,server_id:`srv_${String.fromCharCode(97+i).repeat(27)}`,display_name:`Node ${i}`}));
 let finish!: (r:Reply)=>void;
 b.hold = {action:"servers/check-batch",promise:new Promise(r=>finish=r)};
 const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
 const event = (i:number,stage:"start"|"complete"): CheckProgress => ({event:"server-check",server_id:b.rows[i]!.server_id,stage,ping_status:stage==="start"?"untested":"ready",latency_ms:stage==="start"?0:12,availability:stage==="start"?"untested":"ready"});
 try {
  await app.perform("status"); key("v"); await settle();
  key("p"); await settle();
  for(let i=0;i<5;i++) b.onCheckProgress?.(event(i,"start"));

  expect(app.batch?.running.size).toBe(5);
  expect(app.batch?.running.has(b.rows[5]!.server_id)).toBe(false);
  b.onCheckProgress?.(event(0,"complete")); b.onCheckProgress?.(event(5,"start"));
  expect(app.batch?.running.size).toBe(5);
  expect(app.batch?.completed.has(b.rows[0]!.server_id)).toBe(true);
  expect(app.batch?.running.has(b.rows[5]!.server_id)).toBe(true);
 } finally { finish(reply()); await settle(); await app.close(); }
});

test("speed batch requests all IDs and configured URL once, merges mixed outcomes", async () => {
 const b = new FakeBackend();
 b.hold = {action:"servers/speed-batch", promise:Promise.resolve({...reply(),catalog:{status:"ok",servers:[
 {...rows[0]!,ping_status:"ready",latency_ms:20,availability:"failed",status:"ready",download_mbps:44},
 {...rows[1]!,ping_status:"ready",latency_ms:30,availability:"ready",status:"failed"},
 ]}})};
 const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
 try { await app.perform("status"); key("v"); await settle();
 app.target="https://example.invalid/custom";
 key("t"); await settle();
 expect(b.calls.filter(c=>c.action==="servers/speed-batch").map(c=>JSON.parse(c.value!))).toEqual([{ids:[rows[1]!.server_id,rows[0]!.server_id],url:"https://example.invalid/custom"}]);
 expect(b.calls.some(c=>c.action==="servers/ping"||c.action==="servers/speed")).toBe(false);
 expect(app.servers.find(s=>s.server_id===rows[0]!.server_id)).toMatchObject({ availability:"failed", status:"ready", download_mbps:44 });
 expect(app.servers.find(s=>s.server_id===rows[1]!.server_id)).toMatchObject({ availability:"ready", status:"failed" });
 } finally {await app.close();}
});

test("speed batch shows parallel preparation and speed stage, cancellation sends no subsequent request", async () => {
 const b = new FakeBackend(); let finish!:(r:Reply)=>void;
 b.hold={action:"servers/speed-batch",promise:new Promise(r=>finish=r)}; const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
 try {await app.perform("status");key("v");await settle();key("t");await settle();
 const event=(i:number,stage:CheckProgress["stage"]):CheckProgress=>({event:"server-check",server_id:rows[i]!.server_id,stage,ping_status:stage==="start"?"untested":"ready",latency_ms:20,availability:"untested"});
 b.onCheckProgress?.(event(0,"start"));b.onCheckProgress?.(event(1,"start"));
 expect(app.batch?.running.size).toBe(2);
 expect([...app.batch!.stages.values()]).toEqual(["start", "start"]);
 b.onCheckProgress?.(event(0,"speed"));expect(app.batch?.stages.get(rows[0]!.server_id)).toBe("speed");expect(app.batch?.stages.get(rows[1]!.server_id)).toBe("start");
 b.onCheckProgress?.({...event(0,"complete"),availability:"ready",status:"ready",download_mbps:31});
 expect(app.batch).toMatchObject({ done: 1, total: 2 });
 key("k");finish({...reply(),ok:false,reason:"cancelled"});await settle();
 expect(b.cancelled).toBe(1);expect(b.calls.filter(c=>c.action==="servers/speed-batch")).toHaveLength(1);
 expect(b.calls.some(c=>c.action==="servers/ping"||c.action==="servers/speed")).toBe(false);
 expect(app.servers.find(s=>s.server_id===rows[0]!.server_id)?.download_mbps).toBe(31);
 }finally{finish(reply());await settle();await app.close();}
});

test("held speed batch blocks every other measurement even after navigating home", async () => {
 const t = await createTestRenderer({width:110,height:30}); const b = new FakeBackend();
 let finish!:(r:Reply)=>void; b.hold={action:"servers/speed-batch",promise:new Promise(r=>finish=r)};
 const app = new App(t.renderer,b);
 try {
  await app.perform("status"); t.mockInput.pressKey("v"); await settle(); t.mockInput.pressKey("t"); await settle();
  const initialCalls=b.calls.length;
  t.mockInput.pressKey("t"); t.mockInput.pressKey("p"); await settle();
  await app.perform("servers/speed", rows[0]!.server_id);
  await app.perform("servers/speed-batch",JSON.stringify({ids:[rows[0]!.server_id],url:"https://example.invalid/"}));
  // An internal caller cannot bypass task compatibility either.
  await app.perform("servers/speed", rows[0]!.server_id, true);
  t.mockInput.pressKey("escape");t.mockInput.pressKey("t");await settle();
  expect(b.calls).toHaveLength(initialCalls);
  expect(b.calls.filter(c=>c.action==="servers/speed-batch")).toHaveLength(1);
  finish(reply());await settle();
 } finally {finish(reply());await settle();await app.close();}
});

test("home speedometer measures the selected server after ping and removes redundant labels", async () => {

  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 1 }));
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v"); await settle();
    key("T"); await settle();
    expect(b.calls.filter(c => c.action.startsWith("servers/")).map(c => c.action)).toEqual(["servers/list", "servers/list", "servers/ping", "servers/speed"]);
    expect(b.calls.find(c => c.action === "servers/speed")?.value).toBe(rows[1]!.server_id);
    expect(app.homeSpeed).toBe(50);
    expect(app.homePing).toBe(24);
    expect(app.homeSpeedServer?.id).toBe(rows[1]!.server_id);

  } finally { await app.close(); }
});

test("home speed test skips download after failed ping and allows navigation", async () => {
  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 0 }));
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/ping", promise: new Promise(r => { finish = r; }) };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v"); await settle();
    key("T"); await settle();
    expect(app.homeTesting).toBe(true);
    key("d");
    expect(app.screen).toBe("diagnostics");
    finish({ ...reply(), ok: false, reason: "failed" }); await settle();
    expect(b.calls.some(c => c.action === "servers/speed")).toBe(false);
    key("escape");
    expect(app.screen).toBe("servers");
    expect(app.homeSpeed).toBeUndefined();
    expect(app.homeTesting).toBe(false);
    expect(app.error).toBe(true);
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("switching completes during a held home speed test; late result belongs to original server", async () => {

  const b = new FakeBackend();
  b.rows = rows.map((row, i) => ({ ...row, selected: i === 0 }));
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/speed", promise: new Promise(r => finish = r) };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v"); await settle();
    key("T");
    await settle();
    expect(b.calls.some(c => c.action === "servers/speed")).toBe(true);
    key("v");
    await settle();
    await app.perform("servers/select", rows[1]!.server_id);

    expect(app.servers.find(s => s.server_id === rows[1]!.server_id)?.selected).toBe(true);
    finish({ ...reply(), catalog: { status: "ok", server: { ...rows[0]!, selected: true, status: "ready", download_mbps: 77 } } });
    await settle();

    expect(app.servers.find(s => s.server_id === rows[0]!.server_id)).toMatchObject({ selected: false, download_mbps: 77 });
    expect(app.servers.find(s => s.server_id === rows[1]!.server_id)?.selected).toBe(true);
    expect(b.cancelled).toBe(0);
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("measurement cancellation is scoped and never cancels concurrent server selection", async () => {

  const b = new FakeBackend();
  let finishCheck!: (r: Reply) => void, finishSelect!: (r: Reply) => void;
  const check = new Promise<Reply>(r => finishCheck = r);
  const select = new Promise<Reply>(r => finishSelect = r);
  const tasks = new Map<Action, string | undefined>();
  const cancelled: (string | undefined)[] = [];
  const original = b.request.bind(b);
  b.request = async (action: Action, value?: string, id?: string) => {
    tasks.set(action, id);
    if (action === "servers/check-batch") return check;
    if (action === "servers/select") return select;
    return original(action, value);
  };
  b.cancel = (id?: string) => { cancelled.push(id); };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v"); await settle();
    key("p"); await settle();
    const switching = app.perform("servers/select", rows[1]!.server_id);
    await settle();
    expect(tasks.get("servers/select")).toBeDefined();
    key("k"); await settle();
    key("1"); await settle();
    expect(cancelled).toEqual([tasks.get("servers/check-batch")]);
    expect(cancelled).not.toContain(tasks.get("servers/select"));
    finishCheck({ ...reply(), ok: false, reason: "cancelled" });
    await settle();

    expect(app.tasks.has(tasks.get("servers/select")!)).toBe(true);
    finishSelect(reply()); await switching;
    key("v"); await settle();

    expect(app.servers.find(s => s.server_id === rows[1]!.server_id)?.selected).toBe(true);
  } finally { finishCheck(reply()); finishSelect(reply()); await settle(); await app.close(); }
});

test("conflicting refresh and second measurement queue while selection is allowed", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/check-batch", promise: new Promise(r => finish = r) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("p"); await settle();
    await app.perform("servers/refresh");
    await app.perform("subscription", "https://example.invalid/replacement");
    await app.perform("stop");
    t.mockInput.pressKey("t"); await settle();
    expect(b.calls.some(c => ["servers/refresh", "subscription", "stop", "servers/speed"].includes(c.action))).toBe(false);
    expect(app.queue.map(entry => entry.action)).toEqual(["servers/refresh", "servers/speed-batch"]);
    await app.perform("servers/select", rows[0]!.server_id);
    expect(b.calls.some(c => c.action === "servers/select")).toBe(true);
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("quit drains both selection and measurement before closing backend", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  let finishCheck!: (r: Reply) => void, finishSelect!: (r: Reply) => void;
  const check = new Promise<Reply>(r => finishCheck = r);
  const select = new Promise<Reply>(r => finishSelect = r);
  let closed = false;
  const original = b.request.bind(b);
  b.request = async (action, value) => action === "servers/check-batch" ? check : action === "servers/select" ? select : original(action, value);
  b.close = async () => { closed = true; };
  const app = new App(t.renderer, b);
  await app.perform("status");
  t.mockInput.pressKey("v"); await settle();
  t.mockInput.pressKey("p"); await settle();
  const switching = app.perform("servers/select", rows[0]!.server_id);
  await app.close();
  expect(b.cancelled).toBe(2);
  expect(closed).toBe(false);
  finishCheck({ ...reply(), ok: false, reason: "cancelled" }); await settle();
  expect(closed).toBe(false);
  finishSelect(reply()); await switching;
  expect(closed).toBe(true);
});

test("autostart is opt-in, keeps gateway/connect consistent and never starts VPN immediately", async () => {
  const b = new FakeBackend();
  let options = { gateway: false, connect: false };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/set") options = JSON.parse(value!);
    if (action.startsWith("autostart/")) result.autostart = { ...options };
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("a"); await settle();
    expect(app.autostart).toEqual({ gateway: false, connect: false });
    expect(b.calls.some(call => call.action === "autostart/set")).toBe(false);
    key("a"); await settle();
    expect(options).toEqual({ gateway: true, connect: true });
    key("g"); await settle();
    expect(options).toEqual({ gateway: false, connect: false });
    expect(app.autostart).toEqual({ gateway: false, connect: false });
    expect(b.calls.some(call => ["start", "backend/start", "stop", "disconnect"].includes(call.action))).toBe(false);
  } finally { await app.close(); }
});

test("failed autostart save requires rereading authoritative settings", async () => {
  const b = new FakeBackend();
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read") result.autostart = { gateway: false, connect: false };
    if (action === "autostart/set") return { ...result, ok: false, reason: "autostart-unavailable" };
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("a"); await settle();
    key("a"); await settle();
    expect(app.autostart).toBeUndefined();
    expect(app.noticeIdentity).toEqual({ action: "autostart/set", reason: "autostart-unavailable", ok: false });
  } finally { await app.close(); }
});

test("autostart screen keeps saved intent separate from the actual disabled unit", async () => {
  const b = new FakeBackend();
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read") {
      result.autostart = { gateway: true, connect: true };
      result.autostart_service = { enabled: "disabled", active: "inactive" };
    }
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("a");
    await settle();

    expect(app.autostart).toEqual({ gateway: true, connect: true });
    expect(app.serviceState).toEqual({ enabled: "disabled", active: "inactive" });
  } finally {
    await app.close();
  }
});

test("successful autostart saves refresh actual state and preserve native runtime payload", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  let options = { gateway: true, connect: false, runtime: "native" as const };
  let service = { enabled: "enabled", active: "active" };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/set") {
      options = { ...options, ...JSON.parse(value!) };
      service = { enabled: options.gateway ? "enabled" : "disabled", active: "inactive" };
    }
    if (action === "autostart/read" || action === "autostart/set") {
      result.autostart = { ...options };
      result.autostart_service = { ...service };
    }
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("a");
    await settle();

    expect(app.autostart?.runtime).toBe("native");
    key("g");
    await settle();

    const set = b.calls.find((call) => call.action === "autostart/set");
    expect(set).toBeDefined();
    expect(JSON.parse(set!.value!).runtime).toBe("native");
    expect(app.autostart).toMatchObject({ gateway: false, connect: false, runtime: "native" });
    expect(app.serviceState).toEqual({ enabled: "disabled", active: "inactive" });
  } finally {
    await app.close();
  }
});

test("autostart errors clear a previously healthy actual state", async () => {
  const b = new FakeBackend();
  let reads = 0;
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read") {
      reads++;
      if (reads === 1) {
        result.autostart = { gateway: true, connect: false };
        result.autostart_service = { enabled: "enabled", active: "active" };
      } else return { ...result, ok: false, reason: "autostart-unavailable" };
    }
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("a");
    await settle();

    expect(app.serviceState).toEqual({ enabled: "enabled", active: "active" });
    await app.perform("autostart/read");

    expect(app.serviceState).toBeUndefined();
    expect(app.autostart).toEqual({ gateway: true, connect: false });
    expect(app.noticeIdentity).toEqual({ action: "autostart/read", reason: "autostart-unavailable", ok: false });
  } finally {
    await app.close();
  }
});

test("installed native reuses the server table, selection, checks and mode controls", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true };
  const original=b.request.bind(b);
  b.request=async(action,value)=>{const r=await original(action,value);if(action.startsWith("profiles/"))r.profiles={active:"profile_legacy1",items:[{id:"profile_legacy1",name:"Existing",kind:"subscription",count:2,stale:false}]};return r};
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("v"); await settle();
    key("1"); await settle();
    expect(app.servers.map(server => server.server_id)).toContain(rows[1]!.server_id);
    expect(app.nativeInstalled()).toBe(true);
    key("return"); await settle();
    expect(b.calls.at(-1)?.action).toBe("servers/select");
    await app.perform("servers/ping",rows[0]!.server_id);
    expect(b.calls.at(-1)?.action).toBe("servers/ping");
    key("h"); await Bun.sleep(60);
    key("m"); await settle();
    expect(b.calls.at(-1)?.action).toBe("toggle-mode");
    expect(app.status?.runtime).toBe("native");
  } finally { await app.close(); }
});

test("native profiles show separate server lists and adding a link does not connect", async () => {
 const t=await createTestRenderer({width:100,height:28});const b=new FakeBackend();
 b.status={...b.status,runtime:"native",installed:true};
 let active="profile_one";
 const items=[{id:"profile_one",name:"Subscription",kind:"subscription" as const,count:1,stale:true},{id:"profile_two",name:"My VPS",kind:"server" as const,count:1,stale:false}];
 b.rows=[{...rows[0]!,profile_id:"profile_one"},{...rows[1]!,profile_id:"profile_two"}];
 const original=b.request.bind(b);
 b.request=async(action,value)=>{const r=await original(action,value);if(action==="profiles/open")active=value!;if(action.startsWith("profiles/"))r.profiles={active,items};return r};
 const app=new App(t.renderer,b);
 try{
  await app.perform("status");t.mockInput.pressKey("c");await settle();await t.renderOnce();
  expect(app.profiles?.items).toMatchObject([{ id: "profile_one", stale: true }, { id: "profile_two" }]);
  t.mockInput.pressKey("2");await settle();await t.renderOnce();
  expect(app.profiles?.active).toBe("profile_two");
  expect(app.servers.map(server => server.server_id)).toEqual([rows[1]!.server_id]);
  expect(b.calls.some(c=>c.action==="start" || c.action==="servers/select")).toBe(false);
  t.mockInput.pressKey("c");await settle();t.mockInput.pressKey("n");await settle();
  t.mockInput.typeText("Other VPS");t.mockInput.pressEnter();await settle();
  t.mockInput.typeText("hy2://private-marker@example.invalid#VPS");t.mockInput.pressEnter();await settle();await t.renderOnce();
  const added=b.calls.find(c=>c.action==="profiles/add");expect(JSON.parse(added!.value!).name).toBe("Other VPS");
  expect(app.editorValue).not.toContain("private-marker");
  expect(app.notice).not.toContain("private-marker");
  expect(app.noticeAttempt).not.toContain("private-marker");
 }finally{await app.close()}
});

test("failed profile open retains the current source and avoids catalog reload", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true };
  b.rows = [{ ...rows[0]!, profile_id: "profile_one" }, { ...rows[1]!, profile_id: "profile_two" }];
  const items = [
    { id: "profile_one", name: "First profile", kind: "subscription" as const, count: 1, stale: false },
    { id: "profile_two", name: "Second profile", kind: "server" as const, count: 1, stale: false },
  ];
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "profiles/open" && value === "profile_two")
      return { ...result, ok: false, reason: "profiles-failed" };
    if (action.startsWith("profiles/")) result.profiles = { active: "profile_one", items };
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("c"); await settle();
    key("1"); await settle();
    expect(app.servers.map(server => server.server_id)).toEqual([rows[0]!.server_id]);
    key("c"); await settle();
    const listsBefore = b.calls.filter(call => call.action === "servers/list").length;
    key("2"); await settle();
    expect(app.noticeIdentity).toEqual({ action: "profiles/open", reason: "profiles-failed", ok: false });
    expect(app.profiles?.active).toBe("profile_one");
    expect(b.calls.filter(call => call.action === "servers/list")).toHaveLength(listsBefore);
    key("1"); await settle();
    expect(app.servers.map(server => server.server_id)).toEqual([rows[0]!.server_id]);
    expect(b.calls.some(call => ["start", "stop", "servers/select"].includes(call.action))).toBe(false);
  } finally { await app.close(); }
});

test("removing the active source browses its fallback without changing the VPN", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, networkmanager_active: "yes" };
  b.rows = [{ ...rows[0]!, profile_id: "profile_one" }, { ...rows[1]!, profile_id: "profile_two" }];
  let items = [
    { id: "profile_one", name: "First profile", kind: "subscription" as const, count: 1, stale: false },
    { id: "profile_two", name: "Second profile", kind: "server" as const, count: 1, stale: false },
  ];
  let active = "profile_one";
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "profiles/remove") {
      expect(value).toBe("profile_one");
      items = items.slice(1);
      active = "profile_two";
    }
    if (action.startsWith("profiles/")) result.profiles = { active, items };
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("c"); await settle();
    key("X"); await settle();
    expect(b.calls.some(call => call.action === "profiles/remove")).toBe(false);
    key("y"); await settle();
    expect(app.profiles?.active).toBe("profile_two");
    expect(app.profiles?.items.map(profile => profile.id)).toEqual(["profile_two"]);
    key("1"); await settle();
    expect(app.servers.map(server => server.server_id)).toEqual([rows[1]!.server_id]);
    expect(b.calls.filter(call => call.action === "profiles/remove")).toHaveLength(1);
    expect(b.calls.some(call => ["start", "stop", "disconnect", "servers/select"].includes(call.action))).toBe(false);
  } finally { await app.close(); }
});

test("installed native separates service startup, connection, disconnect and shutdown", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, gateway_state: "absent" };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    if (action === "backend/start") b.status.gateway_state = "healthy";
    if (action === "start") b.status.networkmanager_active = "yes";
    if (action === "disconnect") b.status.networkmanager_active = "no";
    if (action === "stop") b.status.gateway_state = "stopped";
    return original(action, value);
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({name, preventDefault() {}});
  try {
    await app.perform("status");
    key("s"); await settle();
    expect(app.status?.networkmanager_active).toBe("no");
    expect(app.status?.gateway_state).toBe("healthy");
    expect(b.calls.map(call => call.action)).toEqual(["status", "backend/start", "servers/list"]);
    key("s"); await settle();
    expect(app.status?.networkmanager_active).toBe("yes");
    key("x"); await settle();
    expect(b.calls.at(-1)?.action).toBe("start");
    key("n"); await settle();
    expect(b.status.networkmanager_active).toBe("yes");
    key("x"); key("y"); await settle();
    expect(app.status?.networkmanager_active).toBe("no");
    expect(app.noticeIdentity).toEqual({action:"disconnect",reason:"ok",ok:true});
    expect(b.status.gateway_state).toBe("healthy");
    key("z"); await settle();
    expect(b.calls.at(-1)?.action).toBe("disconnect");
    key("y"); await settle();
    expect(app.status?.gateway_state).toBe("stopped");
    expect(app.noticeIdentity).toEqual({action:"stop",reason:"ok",ok:true});
    expect(b.calls.map(call => call.action)).toEqual(["status", "backend/start", "servers/list", "start", "disconnect", "stop"]);
  } finally { await app.close(); }
});

test("installed native mode failure preserves strict state and retry shows smart then strict", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true };
  let fail = true;
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    if (action === "toggle-mode") {
      if (fail) { fail = false; return { ...reply(b.status), ok: false, reason: "dns-failed" }; }
      b.status.routing_mode = b.status.routing_mode === "strict" ? "smart" : "strict";
    }
    return original(action, value);
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("m"); await settle();
    expect(app.noticeIdentity).toEqual({ action: "toggle-mode", reason: "dns-failed", ok: false });
    expect(app.status?.routing_mode).toBe("strict");
    key("m"); await settle();
    expect(app.status?.routing_mode).toBe("smart");
    expect(app.noticeIdentity).toEqual({ action: "toggle-mode", reason: "ok", ok: true });
    key("m"); await settle();
    expect(app.status?.routing_mode).toBe("strict");
    expect(b.calls.some(call => ["start", "backend/start", "disconnect", "stop"].includes(call.action))).toBe(false);
  } finally { await app.close(); }
});

test("installed native diagnostics failure can be retried without changing connectivity", async () => {
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, networkmanager_active: "yes" };
  let attempts = 0;
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "diagnostics") {
      if (++attempts === 1) return { ...result, ok: false, reason: "diagnostics-unavailable" };
      b.status.diagnostics = "available";
      return reply(b.status);
    }
    return result;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    key("d"); await settle();
    key("u"); await settle();
    expect(app.noticeIdentity).toEqual({ action: "diagnostics", reason: "diagnostics-unavailable", ok: false });
    key("u"); await settle();
    expect(app.status?.diagnostics).toBe("available");
    expect(app.noticeIdentity).toEqual({ action: "diagnostics", reason: "ok", ok: true });
    key("h"); await Bun.sleep(60);
    expect(app.status?.networkmanager_active).toBe("yes");
    expect(app.screen).toBe("servers");
    expect(b.calls.some(call => ["start", "backend/start", "disconnect", "stop"].includes(call.action))).toBe(false);
  } finally { await app.close(); }
});

test("failed native status polls retain native server screen without stale readiness", async () => {
  const b = new FakeBackend();
  b.status = { ...initial, runtime: "native", installed: true, vpn_state: "healthy", networkmanager_active: "yes" };
  const request = b.request.bind(b);
  b.request = async (action, value) => {
    const r = await request(action, value);
    if (action.startsWith("profiles/")) r.profiles = { active: "profile_legacy1", items: [{ id: "profile_legacy1", name: "Fixture", kind: "subscription", count: 2, stale: false }] };
    return r;
  };
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");

    expect(app.status?.networkmanager_active).toBe("yes");
    key("v");
    await settle();
    key("1");
    await settle();
    b.request = async () => { throw new Error("backend-unavailable"); };
    for (let attempt = 0; attempt < 2; attempt++) {
      await app.perform("status");

      expect(app.screen).toBe("servers");
      expect(app.status).toMatchObject({ runtime: "native", installed: true, gateway_state: "unknown", networkmanager_active: "unknown" });
      expect(app.ready()).toBe(false);
      expect(app.backendOffline).toBe(true);
    }
  } finally {
    await app.close();
  }
});

test("an initial status failure does not invent native identity", async () => {
  const b = new FakeBackend();
  b.request = async () => { throw new Error("backend-unavailable"); };
  const app = new AppState(b);
  try {
    await app.perform("status");

    expect(app.status).toBeUndefined();
    expect(app.backendOffline).toBe(true);
    expect(app.recovery).toMatchObject({ action: "status" });
  } finally {
    await app.close();
  }
});

test("held native and Docker lifecycle actions use their own runtime labels", async () => {
  for (const runtime of ["native", "docker"] as const) {
    const b = new FakeBackend();
    b.status = { ...initial, runtime, installed: runtime === "native" };
    const app = new AppState(b);
    try {
      await app.perform("status");
      for (const action of ["backend/start", "stop"] as const) {
        let finish!: (r: Reply) => void;
        b.hold = { action, promise: new Promise(resolve => { finish = resolve; }) };
        const work = app.perform(action);
        expect(app.busy).toBe(action);
        expect(app.status?.runtime).toBe(runtime);
        finish(reply(b.status));
        await work;
        b.hold = undefined;
      }
    } finally {
      await app.close();
    }
  }
});

for (const width of [48, 100, 140]) {
  test(`native network repair is offered inline and f runs once at ${width} columns`, async () => {
    const b = new FakeBackend();
    b.status = { ...b.status, runtime: "native", installed: true, gateway_state: "inactive", last_attempt: "private-attempt" };
    const app = new AppState(b);
    const key = (name: string) => app.onKey({ name, preventDefault() {} });
    try {
      await app.perform("status");
      b.hold = { action: "backend/start", promise: Promise.resolve({ ...reply(b.status), ok: false, reason: "native-network-settings-changed" }) };
      await app.perform("backend/start");
      expect(app.noticeIdentity).toEqual({ action: "backend/start", reason: "native-network-settings-changed", ok: false });
      expect(app.recovery?.actions).toContain("reapply");
      expect(app.notice).not.toContain("private-attempt");
      expect(app.noticeAttempt).not.toContain("private-attempt");
      expect(b.calls.filter(c => c.action === "network/reapply")).toHaveLength(0);
      let resolve!: (r: Reply) => void;
      b.hold = { action: "network/reapply", promise: new Promise(r => resolve = r) };
      key("f"); await settle();
      key("f"); await settle();
      expect(b.calls.filter(c => c.action === "network/reapply")).toHaveLength(1);
      expect(b.calls.some(c => c.action === "start" || c.action === "disconnect")).toBe(false);
      resolve(reply(b.status)); await settle();
      expect(app.noticeIdentity).toEqual({ action: "network/reapply", reason: "ok", ok: true });
      key("f"); await settle();
      expect(b.calls.filter(c => c.action === "network/reapply")).toHaveLength(1);
      b.hold = { action: "backend/start", promise: Promise.resolve({ ...reply(b.status), ok: false, reason: "native-network-settings-changed" }) };
      await app.perform("backend/start");
      await app.perform("disconnect");
      key("f"); await settle();
      expect(b.calls.filter(c => c.action === "network/reapply")).toHaveLength(1);
    } finally { await app.close(); }
  });
}

test("failed reapplication replaces drift instruction with the actual busy failure", async () => {
  const b = new FakeBackend();
  b.status = {...b.status,runtime:"native",installed:true,gateway_state:"inactive"};
  const app = new AppState(b);
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    await app.perform("status");
    b.hold = {action:"backend/start",promise:Promise.resolve({...reply(b.status),ok:false,reason:"native-network-settings-changed"})};
    await app.perform("backend/start");
    b.hold = {action:"network/reapply",promise:Promise.resolve({...reply(b.status),ok:false,reason:"native-network-busy"})};
    key("f"); await settle();
    expect(app.noticeIdentity).toEqual({ action: "network/reapply", reason: "native-network-busy", ok: false });
    expect(app.recovery?.actions).not.toContain("reapply");
    key("f"); await settle();
    expect(b.calls.filter(c=>c.action==="network/reapply")).toHaveLength(1);
  } finally { await app.close(); }
});

test("home speed starts during status polling and reports other conflicts", async () => {
  const b = new FakeBackend();
  b.rows = rows.map((row, i) => ({ ...row, selected: i === 0 }));
  const app = new AppState(b);
  const state = app;
  try {
    await app.perform("status");
    let finish!: (result: Reply) => void;
    b.hold = { action: "status", promise: new Promise(resolve => { finish = resolve; }) };
    const poll = app.perform("status");
    await state.testHomeSpeed();
    expect(b.calls.some(call => call.action === "servers/speed")).toBe(true);
    finish(reply()); await poll;
    b.hold = { action: "start", promise: new Promise(resolve => { finish = resolve; }) };
    const mutation = app.perform("start");
    const callsBefore = b.calls.length;
    await state.testHomeSpeed();
    expect(b.calls).toHaveLength(callsBefore);
    expect(state.homeTesting).toBe(false);
    finish(reply()); await mutation;
  } finally { await app.close(); }
});

test("profiles and autostart reads wait for conflicting work while their screen remains open", async () => {
  for (const [screen, read] of [["profiles", "profiles/list"], ["autostart", "autostart/read"]] as const) {
    const t = await createTestRenderer({ width: 100, height: 30 });
    const b = new FakeBackend(); const app = new App(t.renderer, b); const state = app as any;
    try {
      await app.perform("status");
      let finish!: (result: Reply) => void;
      b.hold = { action: "start", promise: new Promise(resolve => { finish = resolve; }) };
      const mutation = app.perform("start");
      state.open(screen);
      expect(b.calls.filter(call => call.action === read)).toHaveLength(0);
      finish(reply()); await mutation;
      expect(b.calls.filter(call => call.action === read)).toHaveLength(1);
    } finally { await app.close(); }
  }
});

test("unknown routing mode is not presented as strict", async () => {
  const b = new FakeBackend(); b.status.routing_mode = "unknown";
  const app = new AppState(b);
  try {
    await app.perform("status");
    expect(app.status?.routing_mode).toBe("unknown");
  } finally { await app.close(); }
});

test("idle stops animation and progress updates do not repaint the whole screen", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend(); const app = new App(t.renderer, b); const state = app as any;
  const paint = state.paint; let paints = 0;
  state.paint = () => { paints++; paint(); };
  try {
    app.start(); await settle();
    expect(state.timer).toBeUndefined();
    let finish!: (result: Reply) => void;
    b.hold = { action: "start", promise: new Promise(resolve => { finish = resolve; }) };
    const work = app.perform("start");
    expect(state.timer).toBeDefined();
    const before = paints;
    b.onProgress?.("runtime-wait");
    await Bun.sleep(130);
    expect(paints).toBe(before);
    finish(reply()); await work;
    expect(state.timer).toBeUndefined();
  } finally { await app.close(); }
});

test("deferred screen read runs after a batch catalog failure", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend(); const app = new App(t.renderer, b); const state = app as any;
  try {
    await app.perform("status");
    let finish!: (result: Reply) => void;
    b.hold = { action: "servers/list", promise: new Promise(resolve => { finish = resolve; }) };
    const batch = state.runBatch("availability");
    state.open("profiles");
    expect(b.calls.some(call => call.action === "profiles/list")).toBe(false);
    finish({ ...reply(), ok: false, reason: "unavailable" });
    await batch;
    expect(b.calls.filter(call => call.action === "profiles/list")).toHaveLength(1);
  } finally { await app.close(); }
});
