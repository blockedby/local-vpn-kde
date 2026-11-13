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
