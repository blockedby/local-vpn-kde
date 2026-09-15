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
    expect(b.calls.map((c) => c.action)).toEqual(["status", "start"]);
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
    expect(t.captureCharFrame()).toContain("сначала выполните тест скорости");
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
test("quit drains in-flight work without destroying the renderer prematurely", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "start", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  await app.perform("status");
  const task = app.perform("start");
  await app.close();
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

test("combined checks split twelve nodes into bounded groups of five", async () => {
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
    expect(batches.map(ids => ids.length)).toEqual([5, 5, 2]);
    expect(new Set(batches.flat()).size).toBe(12);
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
