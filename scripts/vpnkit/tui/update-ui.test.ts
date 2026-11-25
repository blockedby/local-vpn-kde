import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { AppState } from "./app-state";
import { FakeBackend, initial } from "./app-test-fixtures";

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

    key(app, "u"); await t.renderOnce();
    expect(app.screen).toBe("confirm"); expect(app.confirmation).toBe("update");
    frame = t.captureCharFrame();
    expect(frame).toContain("Установить версию 0.4.4 (сейчас 0.4.3)?"); expect(frame).toContain("VPN останется подключён");
    expect(backend.calls.some(call => call.action === "update/install")).toBe(false);

    key(app, "y"); for (let i = 0; i < 30 && !app.updatedTo; i++) await Bun.sleep(5);
    await t.renderOnce();
    expect(backend.calls.find(call => call.action === "update/install")?.value).toBe("0.4.4");
    frame = t.captureCharFrame();
    expect(app.updatedTo).toBe("0.4.4");
    expect(frame).toContain("Перезапустить интерфейс");
    expect(frame.replace(/\s+/g, " ")).toContain("Обновлено до 0.4.4, VPN не отключался");
  } finally { await app.close(); }
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
