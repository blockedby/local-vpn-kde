import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import type { Action, Reply } from "./bridge";
import { FakeBackend, settle, reply, rows } from "./app-test-fixtures";

test("legacy status without runtime keeps Docker lifecycle wording", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запустить Docker");
    expect(t.captureCharFrame()).not.toContain("NATIVE PREVIEW");
    b.status.gateway_state = "healthy";
    await app.perform("status");
    t.mockInput.pressKey("z");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("остановить Docker и отключить VPN?");
    expect(t.captureCharFrame()).not.toContain("native-службу");
  } finally {
    await app.close();
  }
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
    const lines = t.captureCharFrame().split("\n");
    expect(lines.findIndex(line => line.includes("[x] Отключить VPN"))).toBe(3);
    t.resize(60, 20);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("q выход");
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

test("autostart screen renders failed and masked service states", async () => {
  for (const service of [
    { enabled: "enabled", active: "failed", expected: ["автозапуск включён", "текущее состояние: ошибка"] },
    { enabled: "masked", active: "inactive", expected: ["автозапуск замаскирован", "текущее состояние: неактивна"] },
    { enabled: "not-installed", active: "unknown", expected: ["служба не установлена", "текущее состояние: неизвестно"] },
  ]) {
    const t = await createTestRenderer({ width: 100, height: 28 });
    const b = new FakeBackend();
    const original = b.request.bind(b);
    b.request = async (action, value) => {
      const result = await original(action, value);
      if (action === "autostart/read") {
        result.autostart = { gateway: true, connect: false };
        result.autostart_service = { enabled: service.enabled, active: service.active };
      }
      return result;
    };
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      t.mockInput.pressKey("a");
      await settle();
      await t.renderOnce();
      const frame = t.captureCharFrame();
      for (const expected of service.expected) expect(frame).toContain(expected);
    } finally {
      await app.close();
    }
  }
});

test("missing actual autostart status stays explicit and does not infer from saved intent", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read")
      result.autostart = { gateway: true, connect: false };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Запускать шлюз: да");
    expect(frame).toContain("не проверено");
    expect(frame).toContain("недоступно");
    expect(frame).not.toContain("Фактическое состояние службы: автозапуск включён");
  } finally {
    await app.close();
  }
});

test("presentation: completed diagnostics never reopen a screen the user left", async () => {
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

test("presentation: installed native separates service startup, connection, disconnect and shutdown", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
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
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("s"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("VPN отключён");
    expect(t.captureCharFrame()).toContain("Подключить VPN");
    expect(b.calls.map(call => call.action)).toEqual(["status", "backend/start"]);
    t.mockInput.pressKey("s"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("VPN подключён");
    t.mockInput.pressKey("x"); await settle();
    expect(b.calls.at(-1)?.action).toBe("start");
    t.mockInput.pressKey("n"); await settle();
    expect(b.status.networkmanager_active).toBe("yes");
    t.mockInput.pressKey("x"); t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Native-служба работает");
    expect(b.status.gateway_state).toBe("healthy");
    t.mockInput.pressKey("z"); await settle();
    expect(b.calls.at(-1)?.action).toBe("disconnect");
    t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Native-служба и VPN остановлены");
    expect(b.calls.map(call => call.action)).toEqual(["status", "backend/start", "start", "disconnect", "stop"]);
  } finally { await app.close(); }
});

test("presentation: native runtime shows the supported lifecycle preview and native service prompts", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", gateway_state: "absent" };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame).toContain("NATIVE PREVIEW");
    expect(frame).toContain("Native-служба");
    expect(frame).toContain("Запустить сервис");
    expect(frame).not.toContain("Docker");
    t.mockInput.pressKey("s");
    await settle();

    b.status.gateway_state = "healthy";
    await app.perform("status");
    t.mockInput.pressKey("z");
    await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame).toContain("остановить сервис и отключить VPN?");
    expect(frame).not.toContain("Docker");
  } finally {
    await app.close();
  }
});


test("presentation: native mode guards unsupported actions before the backend request", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  const app = new App(t.renderer, b);
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
    t.mockInput.pressKey("v");
    t.mockInput.pressKey("c");
    t.mockInput.pressKey("d");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("native preview");
    expect(t.captureCharFrame()).not.toContain("Docker");
  } finally {
    await app.close();
  }
});


test("presentation: checkout native and native status without installed=true keep subscription setup disabled", async () => {
  for (const installed of [undefined, false] as const) {
    const t = await createTestRenderer({ width: 100, height: 28 });
    const b = new FakeBackend();
    b.status = { ...b.status, runtime: "native", installed };
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      t.mockInput.pressKey("c");
      await settle();
      await t.renderOnce();
      expect(t.captureCharFrame()).toContain("недоступна в native preview");
      expect(t.captureCharFrame()).not.toContain("Enter сохранить");
    } finally {
      await app.close();
    }
  }
});

test("presentation: installed native subscription setup uses the private editor and explicit confirmation", async () => {
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
    let frame = t.captureCharFrame();
    expect(frame).toContain("настройка native-пакета");
    expect(frame).toContain("Polkit");
    expect(frame).toContain("несколько минут");
    expect(frame).toContain("Подключить VPN");
    expect(frame).toContain("автозапуск");
    expect(frame).not.toContain("недоступна в native preview");

    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText(value);
    t.mockInput.pressEnter();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("подтверждение администратора");

    t.mockInput.pressKey("y");
    await settle();
    await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame).toContain("Native-пакет настроен");
    expect(frame).toContain("нажмите «Подключить VPN» отдельно");
    expect(frame).not.toContain(value);
  } finally {
    await app.close();
  }
});

test("presentation: native setup failures never echo the private editor value", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
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
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText(value);
    t.mockInput.pressEnter();
    t.mockInput.pressKey("y");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Не удалось настроить native-пакет");
    expect(frame).not.toContain(value);
  } finally {
    await app.close();
  }
});

test("presentation: native autostart edits retain the returned runtime and do not connect immediately", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
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
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запускать native-службу: нет");
    t.mockInput.pressKey("g");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запускать native-службу: да");
  } finally {
    await app.close();
  }
});

test("presentation: navigation and diagnostics Back are immediate during a held mutation", async () => {
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
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("OpenVPN / KDE");
    t.mockInput.pressKey("c");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Enter сохранить");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Enter сохранить");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: subscription is visible and editable; paste never dispatches action hotkeys", async () => {
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
    t.mockInput.pressEnter();
    await settle();
  } finally {
    await app.close();
  }
});

test("presentation: failed subscription save retains input and Back works while saving", async () => {
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

test("presentation: server table is sorted by name, keeps columns aligned and exposes combined batch actions", async () => {
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
      "Скорость + сайт",
      "Ping всех + сайт",
      "Ping мс",
      "Мбит/с",
    ])
      expect(frame).toContain(text);
    t.mockInput.pressKey("o");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("ping ↑");
    t.resize(90, 18); await t.renderOnce();
    const compact = t.captureCharFrame();
    expect(compact).not.toContain("Docker:");
    expect(compact).not.toContain("Сортировка [o]");
    expect(compact).not.toContain("Сайт: https:");
    expect(compact).toContain("Amsterdam");
    t.resize(100, 30); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Сортировка [o]");
    expect(t.captureCharFrame()).toContain("Docker:");
    t.mockInput.pressArrow("down");
    t.mockInput.pressEnter();
    await t.renderOnce();
  } finally {
    await app.close();
  }
});

test("presentation: batch publishes each result and allows navigation without cancelling", async () => {
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
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: Cancel stops the remaining batch and preserves its completed rows", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/speed-batch",
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
    finish({ ...reply(), ok: false, reason: "cancelled" });
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Остановлено");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: changing subscription invalidates old rows and refreshes the catalog", async () => {
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
  } finally {
    await app.close();
  }
});

test("presentation: Docker readiness loads the list on an already-open servers screen", async () => {
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
    expect(t.captureCharFrame()).toContain("Amsterdam");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: subscription opened during status loads on the first visit after status completes", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    const task = app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: deferred subscription load does not replace text typed while busy", async () => {
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
  } finally { finish(reply()); await settle(); await app.close(); }
});
