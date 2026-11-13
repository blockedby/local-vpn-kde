import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend, settle } from "./app-test-fixtures";

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
