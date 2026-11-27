import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { AppState } from "./app-state";
import { FakeBackend, initial, reply, settle } from "./app-test-fixtures";

const key = (app: { onKey: AppState["onKey"] }, name: string, shift = false) => app.onKey({ name, shift, preventDefault() {} });

test("settings check, confirm and install an update for the installed native app", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const backend = new FakeBackend();
  backend.status = { ...initial, runtime: "native", installed: true };
  const app = new App(t.renderer, backend);
  try {
    await app.perform("status");
    app.open("settings"); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Проверить обновления");

    key(app, "u"); for (let i = 0; i < 30 && !app.updateInfo; i++) await Bun.sleep(5);
    await t.renderOnce();
    expect(backend.calls.some(call => call.action === "update/check")).toBe(true);
    let frame = t.captureCharFrame();
    expect(frame).toContain("Обновление: 0.4.3 → 0.4.4");
    expect(frame).toContain("Доступна версия 0.4.4");
    expect(frame).toContain("Установить: Настройки → Обновление");

    key(app, "u"); await t.renderOnce();
    expect(app.screen).toBe("confirm"); expect(app.confirmation).toBe("update");
    frame = t.captureCharFrame();
    expect(frame).toContain("Установить версию 0.4.4 (сейчас 0.4.3)?"); expect(frame).toContain("VPN останется подключён");
    expect(frame).toContain("после установки можно будет его перезапустить");
    expect(backend.calls.some(call => call.action === "update/install")).toBe(false);

    key(app, "y"); for (let i = 0; i < 30 && !app.updatedTo; i++) await Bun.sleep(5);
    await t.renderOnce();
    expect(backend.calls.find(call => call.action === "update/install")?.value).toBe("0.4.4");
    frame = t.captureCharFrame();
    expect(app.updatedTo).toBe("0.4.4");
    expect(frame).toContain("Перезапустить интерфейс");
    expect(frame).not.toContain("Перезапустить шлюз");
    expect(app.notice.toLowerCase()).not.toContain("шлюз");
    expect(frame.replace(/\s+/g, " ")).toContain("Обновлено до 0.4.4, VPN не отключался");
  } finally { await app.close(); }
});

test("a running gateway gets a separate, default-No restart after installation", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const backend = new FakeBackend();
  backend.status = { ...initial, runtime: "native", installed: true };
  backend.updateGatewayRunning = true;
  const app = new App(t.renderer, backend);
  const restart = Promise.withResolvers<ReturnType<typeof reply>>();
  try {
    await app.perform("status"); app.open("settings", false);
    await app.perform("update/install", "0.4.4"); await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame).toContain("Перезапустить шлюз");
    expect(frame.replace(/\s+/g, " ")).toContain("Шлюз пока работает на старой версии");
    key(app, "g"); await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame).toContain("Если VPN подключён, он отключится на несколько секунд");
    expect(frame).toContain("и подключится снова");
    expect(app.items()[app.selected]?.label).toBe("Нет");
    key(app, "return");
    expect(app.screen).toBe("settings");
    expect(backend.calls.some(call => call.action === "gateway/restart")).toBe(false);

    backend.hold = { action: "gateway/restart", promise: restart.promise };
    key(app, "g"); key(app, "y");
    backend.onProgress?.("gateway-restart"); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Перезапускаем шлюз · Задачи");
    expect([...app.tasks.values()].find(task => task.action === "gateway/restart")?.phase).toBe("Переход на новую версию");
    expect(backend.calls.filter(call => call.action === "gateway/restart")).toHaveLength(1);
    restart.resolve(reply(backend.status)); await settle(); await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame).toContain("Шлюз перезапущен на новой версии");
    expect(frame).not.toContain("Перезапустить шлюз");
    expect(frame).toContain("Перезапустить интерфейс");
  } finally { restart.resolve(reply(backend.status)); await app.close(); }
});

test("restart failure shows the helper error and the terminal fallback", async () => {
  const backend = new FakeBackend();
  backend.status = { ...initial, runtime: "native", installed: true };
  backend.updateGatewayRunning = true;
  const state = new AppState(backend);
  try {
    await state.perform("status"); state.open("settings", false);
    await state.perform("update/install", "0.4.4");
    backend.hold = { action: "gateway/restart", promise: Promise.resolve({ ...reply(backend.status), ok: false, reason: "gateway-restart-failed", error: "service refused" }) };
    key(state, "g"); key(state, "y"); await settle();
    expect(state.error).toBe(true);
    expect(state.notice).toContain("service refused");
    expect(state.notice).toContain("kvpn restart");
    expect(state.items().map(item => item.key)).toContain("R");
  } finally { await state.close(); }
});

test("declining the update installs nothing", async () => {
  const backend = new FakeBackend();
  backend.status = { ...initial, runtime: "native", installed: true };
  const state = new AppState(backend);
  try {
    await state.perform("status"); await state.perform("update/check");
    state.open("settings"); state.confirmUpdate();
    expect(state.screen).toBe("confirm");
    key(state, "n");
    expect(state.screen).toBe("settings");
    expect(backend.calls.some(call => call.action === "update/install")).toBe(false);
  } finally { await state.close(); }
});

test("update items are hidden outside the installed native app and relaunch closes first", async () => {
  const backend = new FakeBackend();
  const state = new AppState(backend);
  try {
    await state.perform("status"); state.open("settings");
    expect(state.items().some(item => item.label.includes("обновлен"))).toBe(false);
    state.updatedTo = "0.4.4";
    state.relaunch();
    expect(state.relaunchRequested).toBe(true);
    expect(state.closing || state.disposed).toBe(true);
  } finally { await state.close(); }
});

test("a refused install forgets the stale release so [u] checks again", async () => {
  const backend = new FakeBackend();
  backend.status = { ...initial, runtime: "native", installed: true };
  backend.updateInstallFails = true;
  const state = new AppState(backend);
  try {
    await state.perform("status"); await state.perform("update/check");
    await state.perform("update/install", "0.4.4");
    expect(state.notice).toContain("0.4.5");
    expect(state.updateInfo).toBeUndefined();
    state.open("settings");
    expect(state.items().find(item => item.key === "u")?.label).toBe("Проверить обновления");
  } finally { await state.close(); }
});
