import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App, cell } from "./app";
import {
  Bridge,
  type Action,
  type Backend,
  type Reply,
  type Status,
  type Server,
  type CheckProgress,
} from "./bridge";
import { connection } from "./model";
const initial: Status = {
  vpn_state: "inactive",
  gateway_state: "healthy",
  subscription: "configured",
  routing_mode: "strict",
  networkmanager_configured: "yes",
  networkmanager_active: "no",
  diagnostics: "not-run",
};
const rows: Server[] = [
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
const reply = (status = initial): Reply => ({
  ok: true,
  reason: "ok",
  code: 0,
  status: { ...status },
});
class FakeBackend implements Backend {
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
    if (action === "servers/check-batch") {
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
const settle = () => Bun.sleep(10);

test("connection requires healthy Docker and active KDE", () => {
  expect(connection({ ...initial, networkmanager_active: "yes" }).title).toBe(
    "VPN подключён",
  );
  expect(connection(initial).title).toBe("VPN отключён");
  expect(
    connection({
      ...initial,
      gateway_state: "unhealthy",
      networkmanager_active: "yes",
    }).title,
  ).not.toBe("VPN подключён");
  expect(cell("東京", 6)).toBe("東京  ");
  expect(Bun.stringWidth(cell("🇫🇮 Helsinki", 8))).toBe(8);
});
test("compact home has state-specific actions, no stale generic hints or refresh timestamp", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame).toContain("Запустить Docker");
    expect(frame).not.toContain("[s] Подключить");
    expect(frame).not.toContain("Обновлено:");
    expect(frame).not.toContain("Нажмите");
    b.status.gateway_state = "healthy";
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Подключить VPN");
    b.status.networkmanager_active = "yes";
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Отключить VPN");
    t.resize(60, 20);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("q выход");
  } finally {
    await app.close();
  }
});
test("navigation and diagnostics Back are immediate during a held mutation", async () => {
  const t = await createTestRenderer({ width: 90, height: 28 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "start", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    const task = app.perform("start");
    t.mockInput.pressKey("d");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
    const before = performance.now();
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    await t.renderOnce();
    expect(performance.now() - before).toBeLessThan(150);
    expect(t.captureCharFrame()).not.toContain("OpenVPN / KDE");
    t.mockInput.pressKey("c");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Enter сохранить");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Enter сохранить");
    expect(b.calls.map((c) => c.action)).toEqual(["status", "start", "subscription/read"]);
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("completed diagnostics never reopen a screen the user left", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "diagnostics", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("d");
    const task = app.perform("diagnostics");
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("OpenVPN / KDE");
    expect(t.captureCharFrame()).toContain("Подключить VPN");
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
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText("https://example.invalid/sxrm");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/sxrm");
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
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "subscription",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    t.mockInput.pressKey("c");
    await settle();
    t.mockInput.pressEnter();
    await settle();
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("Enter сохранить");
    finish({ ...reply(), ok: false, reason: "invalid-request" });
    await settle();
    t.mockInput.pressKey("c");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});
test("server table is sorted by name, keeps columns aligned and exposes three separate batch actions", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame.indexOf("Amsterdam")).toBeLessThan(frame.indexOf("Tokyo"));
    for (const text of [
      "Ping всех",
      "Скорость всех",
      "Ping всех + сайт",
      "Ping мс",
      "Мбит/с",
    ])
      expect(frame).toContain(text);
    t.mockInput.pressKey("o");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("ping ↑");
    t.mockInput.pressArrow("down");
    t.mockInput.pressEnter();
    await t.renderOnce();
    expect(b.calls.at(-1)?.action).toBe("servers/select");
  } finally {
    await app.close();
  }
});
test("batch publishes each result and allows navigation without cancelling", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/check-batch",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    t.mockInput.pressKey("d");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
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
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("2/2");
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
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/speed",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("t");
    await settle();
    t.mockInput.pressKey("k");
    expect(b.cancelled).toBe(1);
    finish({ ...reply(), ok: false, reason: "cancelled" });
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Остановлено");
    expect(b.calls.filter((c) => c.action === "servers/speed")).toHaveLength(1);
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
    t.mockInput.pressKey("a");
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
test("actions during polling are serialized once", async () => {
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
    expect(b.calls.map((c) => c.action)).toEqual(["status", "backend/start"]);
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
    await t.renderOnce();
    const at = t
      .captureCharFrame()
      .split("\n")
      .findIndex((l) => l.includes("[m]"));
    await t.mockMouse.click(5, at, 2);
    expect(b.calls).toHaveLength(1);
    await t.mockMouse.click(5, at, 0);
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
  expect(t.captureCharFrame()).toContain("Завершаем текущую операцию");
  finish(reply());
  await task;
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
  const t = await createTestRenderer({ width: 100, height: 28 });
  const server = {
    ...rows[0],
    status: "failed",
    ping_status: "failed",
    download_mbps: undefined,
  };
  const app = new App(t.renderer, {
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
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("ошибка");
    expect(t.captureCharFrame()).toContain("не пров.");
    await app.perform("servers/ping", server.server_id);
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("ошибка");
    await app.perform("servers/speed", server.server_id);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("ошибка");
  } finally {
    await app.close();
  }
});

test("home has one list of navigation entries and no top tabs", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const app = new App(t.renderer, new FakeBackend());
  try {
    await app.perform("status");
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("[h] Главная");
    for (const label of ["[v] Серверы", "[c] Подписка", "[d] Диагностика"])
      expect(frame.split(label)).toHaveLength(2);
    const row = frame
      .split("\n")
      .findIndex((line) => line.includes("[d] Диагностика"));
    await t.mockMouse.click(5, row);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
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
    t.mockInput.pressTab();
    t.mockInput.pressEnter();
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

test("changing subscription invalidates old rows and refreshes the catalog", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await app.perform("subscription", "https://example.invalid/new");
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("Amsterdam");
    t.mockInput.pressKey("v");
    await settle();
    expect(b.calls.at(-1)?.action).toBe("servers/refresh");
    expect(b.calls.some((c) => c.action === "servers/select")).toBe(false);
  } finally {
    await app.close();
  }
});

test("Docker readiness loads the list on an already-open servers screen", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "backend/start",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    const task = app.perform("backend/start");
    t.mockInput.pressKey("v");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(b.calls.at(-1)?.action).toBe("servers/list");
    expect(t.captureCharFrame()).toContain("Amsterdam");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("a successful site check marks a server ready without claiming a speed result", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    const row = t
      .captureCharFrame()
      .split("\n")
      .find((line) => line.includes("Amsterdam"))!;
    expect(row).toContain("готов");
    expect(row).toContain("да");
    expect(row).toContain("—");
    expect(row).not.toContain("50.0");
  } finally {
    await app.close();
  }
});

test("disconnect confirmation defaults to No and requires an explicit Yes", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status.networkmanager_active = "yes";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("x");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Точно хотите отключить VPN?");
    expect(b.calls).toHaveLength(1);
    t.mockInput.pressEnter();
    await settle();
    expect(b.calls).toHaveLength(1);
    t.mockInput.pressKey("x");
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    expect(b.calls).toHaveLength(1);
    t.mockInput.pressKey("x");
    t.mockInput.pressArrow("down");
    t.mockInput.pressEnter();
    await settle();
    expect(b.calls.filter((c) => c.action === "disconnect")).toHaveLength(1);
    t.mockInput.pressKey("x");
    t.mockInput.pressEnter();
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
    expect(t.captureCharFrame()).toContain(
      "остановить Docker и отключить VPN?",
    );
    const no = t
      .captureCharFrame()
      .split("\n")
      .findIndex((line) => line.includes("[n] Нет"));
    await t.mockMouse.click(5, no);
    expect(b.calls).toHaveLength(1);
    t.mockInput.pressKey("z");
    await t.renderOnce();
    const yes = t
      .captureCharFrame()
      .split("\n")
      .findIndex((line) => line.includes("[y] Да"));
    await t.mockMouse.click(5, yes);
    await settle();
    expect(b.calls.at(-1)?.action).toBe("stop");
  } finally {
    await app.close();
  }
});

test("combined checks submit one snapshot once without restarting on navigation", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  b.rows = Array.from({ length: 12 }, (_, i) => ({ ...rows[0], server_id: `srv_${String(i).padStart(27, "0")}` }));
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    const batches = b.calls.filter(c => c.action === "servers/check-batch").map(c => JSON.parse(c.value!).ids);
    expect(batches.map(ids => ids.length)).toEqual([12]);
    expect(new Set(batches.flat()).size).toBe(12);
    t.mockInput.pressKey("d");
    t.mockInput.pressKey("v");
    await settle();
    await t.renderOnce();
    expect(b.calls.filter(c => c.action === "servers/check-batch")).toHaveLength(1);
    expect(t.captureCharFrame()).not.toContain("5 потоков");
  } finally { await app.close(); }
});

test("batch failure retains the actionable outdated backend explanation", async () => {
  const t = await createTestRenderer({ width: 120, height: 30 });
  const b = new FakeBackend();
  b.hold = { action: "servers/check-batch", promise: Promise.resolve({ ...reply(), ok: false, reason: "backend-outdated" }) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Нужен обновлённый Docker-бэкенд");
    expect(t.captureCharFrame()).not.toContain("Результаты сохранены");
  } finally { await app.close(); }
});


test("ping and site render before batch completion and completed rows stop spinning", async () => {
 const t = await createTestRenderer({ width:120, height:30 });
 const b = new FakeBackend();
 let finish!: (r: Reply) => void;
 b.hold = { action:"servers/check-batch", promise:new Promise(r => finish=r) };
 const app = new App(t.renderer,b);
 try {
  await app.perform("status");
  t.mockInput.pressKey("v"); await settle();
  t.mockInput.pressKey("p"); await settle();
  const event: CheckProgress = { event:"server-check", server_id:rows[0].server_id, stage:"ping", ping_status:"ready", latency_ms:37, availability:"untested" };
  b.onCheckProgress?.(event);
  await t.renderOnce();
  let row=t.captureCharFrame().split("\n").find(s=>s.includes("Tokyo"))!;
  expect(row).toContain("37");
  expect(row).not.toContain("да");
  expect(t.captureCharFrame()).toContain("0/2");
  b.onCheckProgress?.({...event,stage:"complete",availability:"ready"});
  b.onCheckProgress?.({...event,stage:"complete",availability:"ready"});
  await t.renderOnce();
  row=t.captureCharFrame().split("\n").find(s=>s.includes("Tokyo"))!;
  expect(row).toContain("37"); expect(row).toContain("да");
  expect(row).not.toMatch(/[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/);
  expect(t.captureCharFrame()).toContain("1/2");
  t.mockInput.pressKey("d"); await t.renderOnce();
  expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
  t.mockInput.pressKey("v"); await t.renderOnce();
  expect(t.captureCharFrame()).toContain("37");
  finish({...reply(),catalog:{status:"ok",servers:rows.map(s=>({...s,ping_status:"ready",availability:"ready",latency_ms:37}))}});
  await settle(); await t.renderOnce();
  expect(t.captureCharFrame()).toContain("2/2");
  expect(t.captureCharFrame()).not.toContain("3/2");
  expect(b.calls.filter(c=>c.action==="servers/check-batch")).toHaveLength(1);
 } finally { finish(reply()); await settle(); await app.close(); }
});

test("subscription opened during status loads on the first visit after status completes", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    const task = app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    expect(b.calls.map(c => c.action)).toEqual(["status"]);
    finish(reply());
    await task;
    await t.renderOnce();
    expect(b.calls.map(c => c.action)).toEqual(["status", "subscription/read"]);
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
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
    expect(t.captureCharFrame()).toContain("https://example.invalid/new");
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
  const t = await createTestRenderer({ width: 110, height: 24 });
  const b = new FakeBackend();
  const app = new App(t.renderer,b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await app.perform("servers/speed",rows[0]!.server_id);
    let finish!: (r: Reply) => void;
    b.hold = { action: "servers/check-batch", promise: new Promise(r => finish=r) };
    t.mockInput.pressKey("a");
    await settle();
    b.onCheckProgress?.({ event:"server-check",server_id:rows[0]!.server_id,stage:"complete",ping_status:"failed",latency_ms:0,availability:"untested" });
    await t.renderOnce();
    const row = t.captureCharFrame().split("\n").find(line => line.includes("Tokyo"))!;
    expect(row).toContain("50.0");
    expect(row).toMatch(/ошибка\s*$/);
    finish(reply()); await settle();
  } finally { await app.close(); }
});

test("only assigned workers spin while remaining servers wait in the queue", async () => {
 const t = await createTestRenderer({ width:120, height:30 });
 const b = new FakeBackend();
 b.rows = Array.from({length:6},(_,i)=>({...rows[0]!,server_id:`srv_${String.fromCharCode(97+i).repeat(27)}`,display_name:`Node ${i}`}));
 let finish!: (r:Reply)=>void;
 b.hold = {action:"servers/check-batch",promise:new Promise(r=>finish=r)};
 const app = new App(t.renderer,b);
 const event = (i:number,stage:"start"|"complete"): CheckProgress => ({event:"server-check",server_id:b.rows[i]!.server_id,stage,ping_status:stage==="start"?"untested":"ready",latency_ms:stage==="start"?0:12,availability:stage==="start"?"untested":"ready"});
 try {
  await app.perform("status"); t.mockInput.pressKey("v"); await settle();
  t.mockInput.pressKey("a"); await settle();
  for(let i=0;i<5;i++) b.onCheckProgress?.(event(i,"start"));
  await t.renderOnce();
  let lines=t.captureCharFrame().split("\n").filter(s=>s.includes("Node "));
  expect(lines.filter(s=>s.includes("проверка"))).toHaveLength(5);
  expect(lines.find(s=>s.includes("Node 5"))).toContain("в очереди");
  expect(lines.find(s=>s.includes("Node 5"))).not.toMatch(/[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/);
  b.onCheckProgress?.(event(0,"complete")); b.onCheckProgress?.(event(5,"start"));
  await t.renderOnce();
  lines=t.captureCharFrame().split("\n").filter(s=>s.includes("Node "));
  expect(lines.filter(s=>s.includes("проверка"))).toHaveLength(5);
  expect(lines.find(s=>s.includes("Node 0"))).toContain("готов");
  expect(lines.find(s=>s.includes("Node 5"))).toContain("проверка");
 } finally { finish(reply()); await settle(); await app.close(); }
});

test("speed batch continues after one failed server and clears the previous error", async () => {
 const t=await createTestRenderer({width:110,height:30});
 const b=new FakeBackend(); const original=b.request.bind(b);let probes=0;
 b.request=async(action,value)=>{
  if(action==="servers/speed" && probes++===0) {
   b.calls.push({action,value});
   return {...reply(),ok:false,reason:"failed",catalog:{status:"failed",server:{...rows[0]!,server_id:value!,status:"failed"}}};
  }
  return original(action,value);
 };
 const app=new App(t.renderer,b);
 try {
  await app.perform("status");t.mockInput.pressKey("v");await settle();
  t.mockInput.pressKey("t");await settle();await t.renderOnce();
  expect(b.calls.filter(c=>c.action==="servers/speed")).toHaveLength(2);
  expect(t.captureCharFrame()).toContain("2/2");
  expect(t.captureCharFrame()).not.toContain("Служба управления недоступна");
 }finally{await app.close();}
});

test("speed batch pings each server before downloading and skips unreachable servers", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  const original = b.request.bind(b);
  let firstID = "";
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "servers/ping" && !firstID) {
      firstID = value!;
      return { ...result, ok: false, reason: "failed",
        catalog: { status: "failed", server: {
          ...result.catalog!.server!, ping_status: "failed", latency_ms: undefined,
        } } };
    }
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("t"); await settle();
    await t.renderOnce();
    const measurements = b.calls.filter(c =>
      c.action === "servers/ping" || c.action === "servers/speed");
    expect(measurements.map(c => c.action)).toEqual([
      "servers/ping", "servers/ping", "servers/speed",
    ]);
    expect(measurements[2]!.value).toBe(measurements[1]!.value);
    expect(measurements[2]!.value).not.toBe(firstID);
    expect(b.calls.some(c => c.action === "servers/check-batch")).toBe(false);
    expect(t.captureCharFrame()).toContain("2/2");
    expect(t.captureCharFrame()).toContain("ошибок 1");
    const failedRow = t.captureCharFrame().split("\n").find(line => line.includes("Amsterdam"))!;
    expect(failedRow).toContain("ошибка");
    expect(failedRow).not.toContain("40.0");
    expect(failedRow).not.toContain("50.0");
  } finally { await app.close(); }
});

test("cancelling the speed preflight never starts a download", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/ping", promise: new Promise(r => { finish = r; }) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("t"); await settle();
    t.mockInput.pressKey("k");
    finish({ ...reply(), ok: false, reason: "cancelled" });
    await settle();
    expect(b.calls.filter(c => c.action === "servers/ping")).toHaveLength(1);
    expect(b.calls.some(c => c.action === "servers/speed")).toBe(false);
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("home speedometer measures the selected server after ping and removes redundant labels", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 1 }));
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("t"); await settle(); await t.renderOnce();
    expect(b.calls.filter(c => c.action.startsWith("servers/")).map(c => c.action)).toEqual(["servers/list", "servers/ping", "servers/speed"]);
    expect(b.calls.find(c => c.action === "servers/speed")?.value).toBe(rows[1]!.server_id);
    const frame = t.captureCharFrame();
    expect(frame).toContain("50.0 Мбит/с");
    expect(frame).not.toContain("Подписка: есть");
    expect(frame).not.toContain("Проверки и выбор серверов");

  } finally { await app.close(); }
});

test("home speed test skips download after failed ping and allows navigation", async () => {
  const t = await createTestRenderer({ width: 60, height: 24 });
  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 0 }));
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/ping", promise: new Promise(r => { finish = r; }) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("t"); await settle();
    t.mockInput.pressKey("d"); await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("СКОРОСТЬ СЕРВЕРА");
    finish({ ...reply(), ok: false, reason: "failed" }); await settle();
    expect(b.calls.some(c => c.action === "servers/speed")).toBe(false);
    t.mockInput.pressEscape(); await Bun.sleep(60); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("— Мбит/с");
    expect(t.captureCharFrame()).toContain("q выход");
  } finally { finish(reply()); await settle(); await app.close(); }
});
